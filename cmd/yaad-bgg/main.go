// Command yaad-bgg is the standalone BoardGameGeek extractor binary
// for yaad-index, implementing the subprocess plugin protocol from
// yaad-index's ADR-0006.
//
// Two CLI modes:
//
// - `yaad-bgg --init` — write the capabilities document
// (name, version, url_patterns, entity_kinds, cache_ttl_seconds)
// as JSON to stdout and exit 0. yaad-index calls this once at
// server startup.
//
// - `yaad-bgg` (no args) — read a JSON request
// (`{"operation": "ingest", "url": "..."}`) from stdin, fetch
// the boardgame via the BGG API, write the response document
// (`{"ok": true, "structured": {...}, "attachments": [...]}`)
// to stdout, and exit 0. On failure: write a human-readable
// message to stderr and exit non-zero.
//
// The wire shapes mirror yaad-index's
// internal/plugins/subprocess/subprocess.go (Capabilities,
// fetchRequest, fetchResponse). Keeping the marshalling here — and
// the fetcher in internal/bgg/ — means the fetcher stays
// straightforwardly unit-testable while the wire concerns are
// exercised by main_test.go. Same split as yaad-wikipedia.
//
// Per ADR-0014 (plugin attachment contract), a successful fetch
// stages the BGG thumbnail under attach.StagingDir() and emits an
// `attachments[]` entry pointing at the staged path; yaad-index's
// daemon copies/hardlinks it into the vault next to the entity .md
// file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/pkg/plugin/attach"
	"github.com/yaad-index/yaad-index/pkg/plugin/data"

	"github.com/yaad-index/yaad-index/internal/bgg"
	"github.com/yaad-index/yaad-index/internal/slug"
)

// EnvAPIKey is the env var operators set to supply the BGG API key.
// Required at runtime — yaad-bgg fails closed when it's missing
// rather than falling back to anonymous BGG access (the anonymous
// path is rate-limited + missing fields per the operator's spec).
const EnvAPIKey = "BGG_API_KEY"

// EnvUsername / EnvPassword are the optional BGG credential env
// vars per #282. Both empty → per-game collection enrichment is
// silently off (the legacy /thing-only behavior). Both non-empty
// + YAAD_PLUGIN_DATA_DIR set → enrichment on. Operators source
// these from yaad-index.env via systemd's EnvironmentFile +
// reference them in config.yaml via #256's `${NAME}` expansion.
const (
	EnvUsername = "BGG_USERNAME"
	EnvPassword = "BGG_PASSWORD"
)

// thumbDownloadTimeout caps the wall-clock budget for the thumbnail
// staging download. Separate from the BGG-API budget so a slow
// thumbnail CDN doesn't trip the upstream-API timeout.
const thumbDownloadTimeout = 30 * time.Second

// requestTimeout caps the wall-clock budget for the whole fetch
// invocation (request read + BGG API call + thumbnail download +
// response write). Generous because BGG's `thing` endpoint is
// often slow.
const requestTimeout = 60 * time.Second

func main() {
	exit := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(exit)
}

// run is the testable entry point. Returns the exit code rather than
// calling os.Exit so tests can drive runInit / runFetch directly with
// a buffer pair. Exit codes: 0 success, 1 runtime error, 2 bad flags.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("yaad-bgg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	initMode := fs.Bool("init", false,
		"emit the capabilities document on stdout and exit (called by yaad-index at startup)")
	versionMode := fs.Bool("version", false,
		"print the plugin version and exit (called by yaad-index's cache-key probe)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *versionMode {
		_, _ = fmt.Fprintln(stdout, bgg.PluginVersion)
		return 0
	}

	if *initMode {
		if err := runInit(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-bgg --init: %v\n", err)
			return 1
		}
		return 0
	}

	if err := runFetch(context.Background(), stdin, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "yaad-bgg: %v\n", err)
		return 1
	}
	return 0
}

// --- capabilities (--init) ---

type capabilitiesDoc struct {
	Name string `json:"name"`
	Version string `json:"version"`
	URLPatterns []string `json:"url_patterns"`
	EntityKinds []kindSpecJSON `json:"entity_kinds"`
	EdgeKinds []kindSpecJSON `json:"edge_kinds"`
	CanonicalKindsEmitted []string `json:"canonical_kinds_emitted,omitempty"`
	// ResolvesCanonicalKinds names the subset of CanonicalKindsEmitted
	// for which this plugin's name-search primitive (BGG's
	// search-by-name) can resolve a free-text name to a concrete
	// canonical entity per yaad-index #304 Cut A. The daemon's
	// edgewrite Service routes resolver-aware edge writes through
	// this plugin for declared kinds; #325 additionally routes the
	// fill-API gate and post-CreateEdge auto-fetch through the
	// same plugin.
	ResolvesCanonicalKinds []string `json:"resolves_canonical_kinds,omitempty"`
	CanonicalEdgeTypesEmitted []string `json:"canonical_edge_types_emitted,omitempty"`
	// SupportsSearch declares opt-in to /v1/search/upstream per
	// #457. yaad-bgg delegates to BGG's xmlapi2 name-search
	// endpoint via bgg.Plugin.Search.
	SupportsSearch bool `json:"supports_search,omitempty"`
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
	CanonicalKindsExtras map[string]canonicalKindExtras `json:"canonical_kinds_extras,omitempty"`
	// SourceNamespace declares the per-plugin vault path prefix
	// and entity-ID namespace under the ADR-0021 universal `kind:
	// source` contract. Daemon derives `<source_namespace>:<slug.
	// Slug(name)>` for every emission with `structured.kind:
	// "source"`.
	SourceNamespace string `json:"source_namespace,omitempty"`
	// ConfigSchema declares the JSON Schema the operator's
	// `plugins[N].config:` block must satisfy per ADR-0006's
	// 2026-05-22 amendment (#192). yaad-bgg has no operator-side
	// config surface today — the API key (the only secret it
	// needs) stays in env-passthrough — so the schema rejects
	// any operator-supplied properties beyond the daemon-injected
	// `_name`.
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`
}

// configSchemaJSON declares the operator-side `config:` shape for
// yaad-bgg per ADR-0006's 2026-05-22 amendment. The plugin reads
// its only secret (BGG_API_KEY) from the daemon-process env, so
// the operator yaml has no structured-config surface beyond the
// reserved daemon-injected `_name` field.
const configSchemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "_name": {"type": "string"}
  }
}`

// canonicalKindExtras mirrors the wire shape yaad-index's
// plugins.CanonicalKindExtras decodes per yaad-index (typed
// gaps in plugin Capabilities) — the per-kind gap-extras the
// plugin contributes on top of the daemon's universal +
// kind-specific built-in gap defaults. Layered as Layer 2 in
// MergeCanonicalRegistry; operator config (Layers 3-4) overrides.
type canonicalKindExtras struct {
	Gaps map[string]gapSpecJSON `json:"gaps,omitempty"`
}

// gapSpecJSON mirrors yaad-index plugins.GapSpec (post- typed
// shape). Field VALUES live under entity `data`; this declares the
// metadata about how each gap is expected to be filled. ADR-0019
// adds FillStrategy, Range, MaxLength, Values to the pre-existing
// {type, description} pair so the operator-fill endpoint can run
// per-field validation against this declaration.
type gapSpecJSON struct {
	Type string `json:"type,omitempty"`
	Description string `json:"description"`
	FillStrategy string `json:"fill_strategy,omitempty"`
	Range []int `json:"range,omitempty"`
	MaxLength int `json:"max_length,omitempty"`
	Values []string `json:"values,omitempty"`
}

type kindSpecJSON struct {
	Name string `json:"name"`
	DefaultTTLDays int `json:"default_ttl_days,omitempty"`
}

func runInit(stdout io.Writer) error {
	doc := capabilitiesDoc{
		Name: bgg.PluginName,
		Version: bgg.PluginVersion,
		URLPatterns: []string{bgg.URLPattern, bgg.ShorthandPattern},
		// ADR-0021: yaad-bgg emits the universal `kind: source`
		// shape; per-kind entity_kinds collapses to a single
		// source kind. Daemon's vault-path routing uses the
		// SourceNamespace declaration below.
		EntityKinds: []kindSpecJSON{
			{
				Name: bgg.UniversalSourceKind,
				DefaultTTLDays: bgg.DefaultTTLDays,
			},
		},
		EdgeKinds:    []kindSpecJSON{},
		ConfigSchema: json.RawMessage(configSchemaJSON),
		CanonicalKindsEmitted: []string{bgg.CanonicalKind, bgg.ExpansionCanonicalKind, "person", "company"},
		// #332 + #334: declare resolver capability for the two
		// canonical kinds whose name-search BGG handles reliably.
		// boardgame: search-by-name returns base games. Adding
		// boardgame-expansion (#334 Cut 1): the same search
		// surface returns expansion entries when their names
		// match, and the plugin now accepts them on fetchByID.
		// person and company stay off the resolver claim until
		// BGG's search proves out for those kinds.
		ResolvesCanonicalKinds: []string{bgg.CanonicalKind, bgg.ExpansionCanonicalKind},
		// Edge types yaad-bgg emits in the source-shape edges
		// block (per ADR-0021 + this PR's plugin-emitted edge
		// design). `is_a` is the universal source-type edge;
		// `is_about` is the canonical-kind edge to the boardgame
		// label; `designed_by` / `artist_by` carry per-game
		// designer/artist credits to person canonical labels;
		// `published_by` carries the primary publisher to a
		// company canonical label. Operator's
		// `canonical_edge_types:` config still gates each at
		// edge-creation time.
		CanonicalEdgeTypesEmitted: []string{
			bgg.SourceTypeEdgeType,
			bgg.CanonicalEdgeType,
			bgg.ExpansionEdgeType,
			"designed_by",
			"artist_by",
			"published_by",
		},
		// SupportsSearch declares opt-in to /v1/search/upstream per
		// #457. yaad-bgg delegates to BGG's xmlapi2 name-search via
		// bgg.Plugin.Search.
		SupportsSearch: true,
		CacheTTLSeconds: bgg.DefaultCacheTTLSeconds,
		SourceNamespace: bgg.SourceNamespace,

		// Per yaad-index ADR-0019 step 8: declare the five
		// operator-strategy gaps the boardgame canonical kind needs.
		// Field VALUES live under entity `data`; the daemon's typed
		// fill endpoint validates writes against these declarations
		// (type, range, fill_strategy).
		//
		// All five carry FillStrategy="operator" — the value's source
		// is operator input, so no clean_content ingest signal fills
		// them automatically; the agent surfaces them out-of-band and
		// writes the operator's confirmed value via the unified fill
		// endpoint POST /v1/entities/{id}/fill. Per yaad-index
		// ADR-0029 §3 (#521) a bare agent token may perform that
		// write — fill_strategy governs the value's source, not
		// write-permission.
		//
		// The daemon's Layer 1.5 BuiltinKindGaps (per yaad-index
		//) already carries the same five for boardgame; this
		// plugin declaration is Layer 2 of the merge so the contract
		// is also explicit at the plugin layer. Operator config
		// (Layers 3-4) can still override any of these — e.g. an
		// operator who wants `played` as fill_strategy=both can
		// declare it under `canonical_kinds.boardgame.gaps.played`
		// and the operator block wins.
		CanonicalKindsExtras: map[string]canonicalKindExtras{
			bgg.CanonicalKind: {
				Gaps: map[string]gapSpecJSON{
					"rating": {
						Type: "int",
						Description: "How do you rate this on a 1-10 scale?",
						Range: []int{1, 10},
						FillStrategy: "operator",
					},
					"owned": {
						Type: "bool",
						Description: "Do you own this?",
						FillStrategy: "operator",
					},
					"want": {
						Type: "bool",
						Description: "Do you want this?",
						FillStrategy: "operator",
					},
					"played": {
						Type: "bool",
						Description: "Have you played this?",
						FillStrategy: "operator",
					},
					"knows_how_to_play": {
						Type: "bool",
						Description: "Do you know how to play this?",
						FillStrategy: "operator",
					},
				},
			},
		},
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", " ")
	return enc.Encode(doc)
}

// --- ingest (default mode) ---

type fetchRequest struct {
	Operation string `json:"operation"`
	URL string `json:"url"`
	// Query + Limit carry the search-operation parameters per
	// #457. Ignored on operation=ingest.
	Query string `json:"query,omitempty"`
	Limit int `json:"limit,omitempty"`
}

// searchResponseDoc is the stdout JSON shape for operation=search
// per #457. Mirrors yaad-wikipedia's searchResponseDoc (and
// yaad-index's subprocess.searchResponse) — a single object, no
// NDJSON wrap.
type searchResponseDoc struct {
	OK bool `json:"ok"`
	Candidates []bgg.SearchResultCandidate `json:"candidates,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// fetchResponse mirrors the wire shape yaad-index's
// subprocess.fetchResponse decodes (per ADR-0005 + ADR-0008 +
// ADR-0014 §1). Optional fields use omitempty / pointer shapes so
// "absent" and "explicit zero" are distinguishable.
type fetchResponse struct {
	OK bool `json:"ok"`
	Structured *structuredResponse `json:"structured,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Notations []string `json:"notations,omitempty"`
	CacheTTLSeconds *int `json:"cache_ttl_seconds,omitempty"`
	Attachments []attach.Attachment `json:"attachments,omitempty"`

	// Options is the disambiguation-candidate map per ADR-0006 +
	//. Keyed by the candidate's BGG numeric id (as a
	// string — the agent re-ingests via `bgg: <id>`); value is the
	// human-readable label + an optional summary. Mutually exclusive
	// with Structured: a single Fetch call emits one or the other,
	// never both. yaad-index's subprocess wrapper reads this into
	// FetchResult.Options and translates to `state: disambiguation`
	// on the API surface.
	Options map[string]optionJSON `json:"options,omitempty"`

	// RawContent is the markdown body the daemon writes under the
	// frontmatter. Per ADR-0015 the daemon wraps it in
	// `<!-- yaad:plugin start/end -->` markers automatically — this
	// plugin emits plain markdown (title H1 + image embed +
	// description prose; see renderBody).
	RawContent string `json:"raw_content,omitempty"`
}

// optionJSON mirrors yaad-wikipedia's optionJSON (per ADR-0006). The
// summary field is currently unused on the bgg side (BGG search
// returns no description; the rich data lives behind xmlapi2/thing,
// which the agent fetches via re-ingest). Keeping it on the wire
// shape matches the cross-plugin disambiguation surface the agent
// already knows about.
type optionJSON struct {
	Label string `json:"label"`
	Summary string `json:"summary,omitempty"`
}

// structuredResponse mirrors yaad-index's subprocess.structuredResponse
// under the ADR-0021 universal-source-shape contract: `kind:
// "source"` + descriptive `name` + `data` + `edges` block. Daemon
// derives the source-node ID from `<source_namespace>:<slug.Slug(name)>`
// and resolves each edge target to a canonical-label endpoint.
type structuredResponse struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Data map[string]any `json:"data,omitempty"`
	Edges map[string][]edgeTargetJSON `json:"edges,omitempty"`
	Provenance []provenanceJSONEntry `json:"provenance,omitempty"`
}

// edgeTargetJSON is one descriptive `{name, kind}` reference in
// the ADR-0021 edges block. The daemon's slug.Slug derives the
// canonical-label slug (`<kind>:<slug.Slug(name)>`).
type edgeTargetJSON struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type provenanceJSONEntry struct {
	Source string `json:"source"`
	FetchedAt string `json:"fetched_at,omitempty"`
	OK bool `json:"ok"`
}

func runFetch(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	apiKey := strings.TrimSpace(os.Getenv(EnvAPIKey))
	if apiKey == "" {
		return fmt.Errorf("%s env is not set; yaad-bgg fails closed (no anonymous fallback)", EnvAPIKey)
	}

	body, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var req fetchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	// Operation dispatch mirrors yaad-wikipedia per #457. `ingest`
	// falls through to the existing fetch path below; `search` runs
	// BGG's free-text name search after the plugin is constructed
	// (it needs the bggo client). The default message keeps the
	// `unsupported operation` substring an existing test asserts.
	switch req.Operation {
	case "ingest":
		// fall through to the existing ingest path below
	case "search":
		// handled after plugin construction below
	default:
		return fmt.Errorf("unsupported operation %q (supported: \"ingest\", \"search\")", req.Operation)
	}
	if req.Operation == "ingest" && req.URL == "" {
		return errors.New("request missing `url`")
	}

	// #282: optional per-game collection enrichment credentials.
	// Both empty → silent off, behaves as today. Both non-empty
	// + YAAD_PLUGIN_DATA_DIR set → enrichment on.
	username := strings.TrimSpace(os.Getenv(EnvUsername))
	password := os.Getenv(EnvPassword) // raw — passwords can carry leading/trailing spaces

	pluginOpts := []bgg.Option{
		bgg.WithWarnLogger(func(format string, args ...any) {
			_, _ = fmt.Fprintf(stderr, "yaad-bgg WARN: "+format+"\n", args...)
		}),
	}
	if username != "" && password != "" {
		pluginOpts = append(pluginOpts, bgg.WithCredentials(username, password))
		// #284: daemon-managed per-instance data dir. Read via
		// pkg/plugin/data.DataDir() so the plugin honors the
		// daemon's resolved + provisioned path.
		if dir := data.DataDir(); dir != "" {
			pluginOpts = append(pluginOpts, bgg.WithDataDir(dir))
		} else {
			// Creds without a data dir means the plugin is
			// running outside yaad-index (or against a daemon
			// predating #284). Per #282 acceptance "if either
			// is missing, enrichment is silently off" — the
			// data-dir is required for cookie-jar persistence,
			// so missing it disables enrichment without an
			// error. WARN to stderr so the operator notices.
			_, _ = fmt.Fprintln(stderr, "yaad-bgg WARN: BGG_USERNAME + BGG_PASSWORD set but YAAD_PLUGIN_DATA_DIR is empty; per-game collection enrichment disabled (cookie-jar persistence requires the daemon-managed data dir from #284)")
		}
	}

	plugin, err := bgg.New(apiKey, pluginOpts...)
	if err != nil {
		return err
	}

	// #457: search operation runs BGG's free-text name search and
	// returns the full candidate list (no auto-resolve / disambiguation
	// collapse — that's the ingest path's job).
	if req.Operation == "search" {
		return runSearch(ctx, plugin, req, stdout)
	}

	outcome, err := plugin.Fetch(ctx, req.URL)
	if err != nil {
		if errors.Is(err, bgg.ErrNotFoundUpstream) {
			return fmt.Errorf("%w: %s", err, req.URL)
		}
		return err
	}

	// Disambiguation path: name-shorthand resolved to multiple BGG
	// search candidates. Emit options-only (no Structured / no
	// attachments / no body) — yaad-index's subprocess wrapper
	// translates this to `state: disambiguation` on the API surface,
	// per ADR-0006 +. Agent picks one and re-ingests via
	// `bgg: <id>` (the option key).
	//
	// Branching on `Options != nil` rather than `Boardgame == nil`:
	// today fetchByName guarantees these mirror (Boardgame nil iff
	// Options non-empty), but the affirmative-on-Options check reads
	// more obviously correct and won't bite if a future code path
	// adds a third terminal state.
	if outcome.Options != nil {
		options := make(map[string]optionJSON, len(outcome.Options))
		for _, o := range outcome.Options {
			options[o.ID] = optionJSON{Label: o.Label, Summary: o.Summary}
		}
		resp := fetchResponse{OK: true, Options: options}
		return writeFetchResponse(stdout, resp)
	}

	bg := outcome.Boardgame
	// ADR-0021 source-shape: kind="source" + descriptive name +
	// edges block. ID is omitted on the wire — daemon derives via
	// `<source_namespace>:<slug.Slug(name)>`.
	resp := fetchResponse{
		OK: true,
		Structured: &structuredResponse{
			Kind: bgg.UniversalSourceKind,
			Name: bg.Name,
			Data: bg.Data,
			Edges: marshalEdges(bg.Edges),
			Provenance: marshalProvenance(outcome.Provenance),
		},
		Aliases: bg.Aliases,
		Notations: bg.Notations,
	}
	cacheTTL := bgg.DefaultCacheTTLSeconds
	resp.CacheTTLSeconds = &cacheTTL

	// Thumbnail attachment per + ADR-0014. Stage to
	// attach.StagingDir() and emit a `file://` attachment via the
	// SDK; yaad-index's dispatcher copies into the vault and
	// deletes the staged source on success. Empty Thumbnail (BGG
	// returned no image) skips the attachment with a stderr WARN
	// per the operator's spec — entity still lands.
	//
	// thumbExt carries the on-disk extension when the staging
	// succeeded; renderBody uses it to compose the image embed
	// path. Empty when staging was skipped or failed — the body
	// renderer omits the image line in that case.
	//
	// stagingLabel for staging-file naming uses the BGG numeric
	// id — stable across re-fetches AND distinct per game without
	// reaching into the daemon-derived source slug. Pre-ADR-0021
	// the entity ID's local part doubled as the staging label;
	// post-rewrite the BGG numeric is the closest equivalent.
	var thumbExt string
	stagingLabel := bg.BGGID
	if bg.ThumbnailURL != "" {
		stagedPath, ext, terr := stageThumbnail(ctx, bg.ThumbnailURL, stagingLabel)
		if terr != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-bgg: skipping thumbnail attachment: %v\n", terr)
		} else {
			thumbExt = ext
			resp.Attachments = []attach.Attachment{
				attach.File("thumb", stagedPath, ext),
			}
		}
	} else {
		_, _ = fmt.Fprintln(stderr, "yaad-bgg: BGG returned no thumbnail; skipping thumb attachment")
	}

	// Body rendering per. Plain markdown — title H1,
	// optional image embed referencing the ADR-0014 thumb file by
	// its on-disk filename, optional description prose. Operator
	// hand-edits to the body survive re-ingest because the daemon
	// (ADR-0015) wraps this content in marker pair on write; the
	// plugin doesn't see or care about the markers.
	//
	// #365: the markdown image-ref uses the daemon-derived entity
	// slug (not the BGG numeric id) so the path resolves to the
	// actual on-disk attachment at <kind>/<slug>/attachments/
	// thumb.<ext> rather than the bggID-shaped path that pre-#365
	// emitted (no such directory existed; thumbnails rendered
	// broken on every BGG entity). The slug is derived locally
	// via slug.Slug(bg.Name); the daemon's slug derivation matches
	// per ADR-0021, so the markdown ref + the daemon-placed
	// attachment dir converge.
	resp.RawContent = renderBody(bg, slug.Slug(bg.Name), thumbExt)

	return writeFetchResponse(stdout, resp)
}

// runSearch handles the operation=search subprocess call per #457.
// Reads the operator/agent query from the fetchRequest's Query
// field, dispatches to bgg.Plugin.Search, and emits the
// searchResponseDoc shape on stdout. Mirrors yaad-wikipedia's
// runSearch one-for-one.
//
// Empty query → ok:false with an error_message; the daemon's
// federation handler surfaces this on the per_plugin_status block
// without failing the federated call.
//
// Upstream / transport errors → ok:false + error_message. A
// successful empty result set → ok:true + empty candidates list.
func runSearch(ctx context.Context, p *bgg.Plugin, req fetchRequest, stdout io.Writer) error {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return json.NewEncoder(stdout).Encode(searchResponseDoc{
			OK: false,
			ErrorMessage: "request missing `query`",
		})
	}
	candidates, err := p.Search(ctx, query, req.Limit)
	if err != nil {
		return json.NewEncoder(stdout).Encode(searchResponseDoc{
			OK: false,
			ErrorMessage: err.Error(),
		})
	}
	return json.NewEncoder(stdout).Encode(searchResponseDoc{
		OK: true,
		Candidates: candidates,
	})
}

// writeFetchResponse emits resp to stdout as NDJSON-shape per
// ADR-0023: single-line JSON terminated by exactly one `\n`
// (Encoder.Encode appends one). The pre-ADR-0023 pretty-printed
// multi-line shape is retired so each invocation produces exactly
// one envelope line — the canonical wire format for yaad-index's
// streaming reader (a prior PR) + the future N-line consumer.
func writeFetchResponse(stdout io.Writer, resp fetchResponse) error {
	return json.NewEncoder(stdout).Encode(resp)
}

func marshalProvenance(entries []bgg.ProvenanceEntry) []provenanceJSONEntry {
	out := make([]provenanceJSONEntry, 0, len(entries))
	for _, e := range entries {
		entry := provenanceJSONEntry{
			Source: e.Source,
			OK: e.OK,
		}
		if !e.FetchedAt.IsZero() {
			entry.FetchedAt = e.FetchedAt.Format(time.RFC3339Nano)
		}
		out = append(out, entry)
	}
	return out
}

// marshalEdges translates the plugin's internal edges map into
// the wire-shape `{<edge_type>: [{name, kind}, ...]}`. Empty/nil
// returns nil so omitempty drops the field on the wire.
func marshalEdges(edges map[string][]bgg.EdgeTarget) map[string][]edgeTargetJSON {
	if len(edges) == 0 {
		return nil
	}
	out := make(map[string][]edgeTargetJSON, len(edges))
	for edgeType, targets := range edges {
		row := make([]edgeTargetJSON, len(targets))
		for i, t := range targets {
			row[i] = edgeTargetJSON{Name: t.Name, Kind: t.Kind}
		}
		out[edgeType] = row
	}
	return out
}

// renderBody composes the markdown body emitted on FetchResult.
// RawContent per. Shape:
//
//	# <title>
//
//	![thumbnail](<local-id>/attachments/thumb.<ext>)
//
//	<description>
//
// Each section is independently optional:
//
// - title H1 always emitted (entity always has a name).
// - image embed only when thumbExt is non-empty (BGG returned a
// thumb AND staging succeeded). The path matches the daemon's
// ADR-0018 §Attachments aggregate-root vault placement —
// `<kind>/<localID>/attachments/<role>.<ext>` — relative from
// the .md file at `<kind>/<localID>.md`, so Obsidian + standard
// markdown renderers resolve the image inline.
// - description prose only when bg.Data["description"] is non-
// empty (BGG occasionally has games without description).
//
// Per ADR-0015 the daemon wraps this output in `<!-- yaad:plugin
// start/end -->` markers on write; the plugin emits PLAIN markdown
// without the markers. Operator hand-edits to the body (e.g.
// `## My playthrough notes` appended below the description)
// survive re-ingest because the daemon's splice replaces only the
// content between the markers.
func renderBody(bg *bgg.Boardgame, localID, thumbExt string) string {
	title, _ := bg.Data["title"].(string)
	if title == "" {
		// Defensive: a fetched-but-titleless boardgame would emit
		// only the bare prose body. Empty title also means the H1
		// is meaningless; skip it so we don't render `# `.
		return descriptionFromData(bg.Data)
	}

	var sb strings.Builder
	sb.WriteString("# ")
	sb.WriteString(title)
	sb.WriteString("\n")

	if thumbExt != "" {
		// Two-newline gap above the image so the H1 + image render
		// as separate blocks in Obsidian. Path is
		// `<localID>/attachments/thumb.<ext>` — relative from the
		// .md file, mirroring the daemon's ADR-0018 nested layout.
		sb.WriteString("\n![thumbnail](")
		sb.WriteString(localID)
		sb.WriteString("/attachments/thumb.")
		sb.WriteString(thumbExt)
		sb.WriteString(")\n")
	}

	if desc := descriptionFromData(bg.Data); desc != "" {
		sb.WriteString("\n")
		sb.WriteString(desc)
		if !strings.HasSuffix(desc, "\n") {
			sb.WriteString("\n")
		}
	}

	// #367: surface the operator-side fields (rating, plays,
	// status, acquisition, comments) as a visible "My Experience"
	// section so the operator's own data isn't invisible in the
	// rendered page. Renders nothing when no operator_* field is
	// populated — the section only appears for entities the
	// operator has actually touched.
	if exp := renderOperatorExperience(bg.Data); exp != "" {
		sb.WriteString("\n")
		sb.WriteString(exp)
	}
	return sb.String()
}

// descriptionFromData extracts the description string from the
// entity data map. Returns "" when missing OR when the field isn't
// a string (defensive against future buildData shape changes).
func descriptionFromData(data map[string]any) string {
	desc, _ := data["description"].(string)
	return desc
}

// renderOperatorExperience composes the "My Experience" body
// section from the operator_* fields the bgg enrichment path
// stamps into entity.data (per #282 / mergeOperatorFields). Each
// field is optional — only present ones render their line. Returns
// "" when no operator_* field is populated so the caller can skip
// emitting the section header entirely.
//
// Field shapes match mergeOperatorFields' writes:
//
//   - operator_rating: int 1-10
//   - operator_num_plays: int
//   - operator_status: []string of status flags (own, played, etc.)
//   - operator_comment: string
//   - operator_acquisition_date: string (YYYY-MM-DD per BGG)
//   - operator_acquired_from: string
//   - operator_price_paid + operator_price_currency: strings
//   - operator_inventory_location + operator_private_comment kept
//     in frontmatter only — operator-side notes that don't belong
//     in a public-shaped page section.
func renderOperatorExperience(data map[string]any) string {
	var lines []string
	if v, ok := data["operator_rating"].(int); ok && v > 0 {
		lines = append(lines, fmt.Sprintf("- **Rating:** %d/10", v))
	}
	if v, ok := data["operator_num_plays"].(int); ok && v > 0 {
		lines = append(lines, fmt.Sprintf("- **Plays:** %d", v))
	}
	if v, ok := data["operator_status"].([]string); ok && len(v) > 0 {
		lines = append(lines, fmt.Sprintf("- **Status:** %s", strings.Join(v, ", ")))
	}
	if acq := operatorAcquisitionLine(data); acq != "" {
		lines = append(lines, acq)
	}
	if v, ok := data["operator_comment"].(string); ok && v != "" {
		lines = append(lines, fmt.Sprintf("- **Comment:** %s", v))
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## My Experience\n\n")
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// operatorAcquisitionLine merges the acquisition-related fields
// into one "Acquired:" line. Renders only the parts present:
// "Acquired: <date>", "Acquired: <date> from <source>",
// "Acquired: <date> from <source> for <currency-symbol><price>",
// etc. Returns "" when no acquisition field is populated.
func operatorAcquisitionLine(data map[string]any) string {
	date, _ := data["operator_acquisition_date"].(string)
	from, _ := data["operator_acquired_from"].(string)
	pricePaid, _ := data["operator_price_paid"].(string)
	currency, _ := data["operator_price_currency"].(string)
	if date == "" && from == "" && pricePaid == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("- **Acquired:**")
	if date != "" {
		sb.WriteString(" ")
		sb.WriteString(date)
	}
	if from != "" {
		sb.WriteString(" from ")
		sb.WriteString(from)
	}
	if pricePaid != "" {
		sb.WriteString(" for ")
		sb.WriteString(currencySymbol(currency))
		sb.WriteString(pricePaid)
	}
	return sb.String()
}

// currencySymbol maps a 3-letter currency code to a visible
// symbol when known; falls back to the bare code (with trailing
// space) for currencies without a familiar single-character form.
func currencySymbol(code string) string {
	switch strings.ToUpper(code) {
	case "EUR":
		return "€"
	case "USD":
		return "$"
	case "GBP":
		return "£"
	case "JPY":
		return "¥"
	case "":
		return ""
	default:
		return code + " "
	}
}

// stageThumbnail downloads thumbURL into attach.StagingDir() and
// returns the staged absolute path + the on-disk extension (no
// leading dot, lowercase). The extension is derived from the URL
// path's suffix and validated against ADR-0014 §5b's regex via the
// daemon-side guard — anything weird here gets skipped at dispatch.
//
// The staged file's lifetime is one Fetch call: the daemon copies
// (or hardlinks) it into the vault and deletes the staged source
// per ADR-0014 §2.file. We do NOT need to clean up here; the
// daemon owns lifecycle on success, and a daemon-side failure
// leaves the staged file for operator inspection.
func stageThumbnail(ctx context.Context, thumbURL, bggID string) (stagedPath, extension string, err error) {
	ctx, cancel := context.WithTimeout(ctx, thumbDownloadTimeout)
	defer cancel()

	ext, err := extensionFromURL(thumbURL)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, thumbURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build thumbnail request: %w", err)
	}
	req.Header.Set("User-Agent", "yaad-bgg/"+bgg.PluginVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch thumbnail %s: %w", thumbURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("thumbnail upstream returned %d", resp.StatusCode)
	}

	stagedDir := attach.StagingDir()
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir staging dir %q: %w", stagedDir, err)
	}
	stagedPath = filepath.Join(stagedDir, "yaad-bgg-"+bggID+"-thumb."+ext)
	out, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", "", fmt.Errorf("create staged file %q: %w", stagedPath, err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(stagedPath)
		return "", "", fmt.Errorf("write thumbnail to %q: %w", stagedPath, err)
	}
	if err := out.Close(); err != nil {
		return "", "", fmt.Errorf("close staged file %q: %w", stagedPath, err)
	}
	return stagedPath, ext, nil
}

// extensionFromURL derives the on-disk file extension from the URL
// path's suffix. Returns the lowercase extension (no leading dot)
// or an error when the URL has no `.<ext>` suffix or the suffix
// doesn't satisfy ADR-0014 §5b's `^[a-z0-9]{1,10}$` shape.
//
// We don't sniff the response body's Content-Type — BGG serves
// thumbnails with stable extensions in the URL path, and trusting
// the URL suffix matches what yaad-wikipedia does for its image
// future work. If BGG ever ships a thumbnail without an extension
// in the path (it doesn't today), this surfaces as a clean WARN +
// skip rather than a malformed dispatch.
func extensionFromURL(thumbURL string) (string, error) {
	u, err := url.Parse(thumbURL)
	if err != nil {
		return "", fmt.Errorf("parse thumbnail URL: %w", err)
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
	if ext == "" {
		return "", fmt.Errorf("thumbnail URL %q has no extension", thumbURL)
	}
	// Drop common query-string remnants — path.Ext shouldn't see
	// them since we walk u.Path, but defensive.
	if i := strings.IndexAny(ext, "?#"); i >= 0 {
		ext = ext[:i]
	}
	if len(ext) < 1 || len(ext) > 10 {
		return "", fmt.Errorf("thumbnail extension %q outside ADR-0014 1..10 char shape", ext)
	}
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			// ok
		default:
			return "", fmt.Errorf("thumbnail extension %q contains non-alphanumeric character", ext)
		}
	}
	return ext, nil
}
