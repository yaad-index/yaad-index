// Command yaad-wikipedia is the standalone Wikipedia extractor binary
// for yaad-index, implementing the subprocess plugin protocol from
// ADR-0006.
//
// Two CLI modes:
//
// - `yaad-wikipedia --init` — write the capabilities document
// (name, version, url_patterns, entity_kinds) as JSON to stdout
// and exit 0. yaad-index calls this once at server startup.
//
// - `yaad-wikipedia` (no args) — read a JSON request
// (`{"operation": "ingest", "url": "..."}`) from stdin, fetch the
// article, write the response document (`{"ok": true,
// "structured": {...}}`) to stdout, and exit 0. On failure: write
// a human-readable message to stderr and exit non-zero. yaad-index
// calls this once per /v1/ingest request that matches the
// url_patterns from --init.
//
// The wire shapes mirror yaad-index's
// internal/plugins/subprocess/subprocess.go (Capabilities,
// fetchRequest, fetchResponse). Keeping the marshalling here — and the
// parser in internal/wikipedia/ — means the parser stays
// straightforwardly unit-testable while the wire concerns are
// exercised by main_test.go.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/wikipedia"
)

// EnvUserAgent is the env var operators set to override the default
// User-Agent header. yaad-index spawns this binary subprocess-per-
// request and inherits its environment, so an env var is the natural
// integration point — the index's config allowlist takes a path only,
// not args (per ADR-0006).
const EnvUserAgent = "YAAD_WIKIPEDIA_USER_AGENT"

// EnvLang is the env var operators set to override the Wikipedia
// language code used when resolving the shorthand input form
// (`wikipedia: <topic>`). Same env-passthrough rationale as
// EnvUserAgent — yaad-index inherits the env into the subprocess.
const EnvLang = "YAAD_WIKIPEDIA_LANG"

// EnvAPIHostOverride redirects every upstream Wikipedia API call
// (action API, REST summary, Wikidata) to the named host. Unset /
// empty → production behavior (canonical Wikipedia hosts). Set →
// every call routes through the value; pass a full origin like
// `http://127.0.0.1:NNNN` for local mocks.
const EnvAPIHostOverride = "YAAD_WIKIPEDIA_API_HOST_OVERRIDE"

// requestTimeout caps the wall-clock budget for a single fetch
// invocation, including reading the request from stdin and the
// upstream HTTP call. The internal upstream timeout
// (DefaultUpstreamTimeout) is the dominant component; this outer
// budget exists to make stuck stdin reads (e.g. yaad-index pipe
// went away) not hang the binary forever.
const requestTimeout = 10 * time.Second

func main() {
	exit := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(exit)
}

// run is the testable entry point. Returns the exit code rather than
// calling os.Exit so tests can drive runInit / runFetch directly with
// a buffer pair. Exit codes: 0 success, 1 runtime error, 2 bad flags.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("yaad-wikipedia", flag.ContinueOnError)
	fs.SetOutput(stderr)
	initMode := fs.Bool("init", false,
		"emit the capabilities document on stdout and exit (called by yaad-index at startup)")
	versionMode := fs.Bool("version", false,
		"print the plugin version and exit (called by yaad-index's cache-key probe)")
	userAgent := fs.String("user-agent", os.Getenv(EnvUserAgent),
		"override the User-Agent header sent on upstream Wikipedia requests. "+
			"Also settable via the "+EnvUserAgent+" env var (the env var is the "+
			"natural integration point under yaad-index, whose config takes a path only). "+
			"Default identifies yaad-wikipedia + a contact URL per Wikimedia's User-Agent policy.")
	lang := fs.String("lang", os.Getenv(EnvLang),
		"Wikipedia language code used to resolve shorthand input (`wikipedia: <topic>`) into "+
			"a canonical URL. Also settable via "+EnvLang+". Default \"en\". Full URL inputs "+
			"are not affected — the URL's host already names the language.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *versionMode {
		// Bare version string + newline. Matches yaad-index's
		// subprocess.RunVersion bare-string parse path. The probe is
		// the cache-key check that runs on every yaad-index startup
		// (Per the prior design,'s INFO breadcrumb on this exact path); answering
		// it cleanly skips the full --init handshake when the cached
		// capabilities row's version matches.
		_, _ = fmt.Fprintln(stdout, wikipedia.PluginVersion)
		return 0
	}

	if *initMode {
		if err := runInit(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-wikipedia --init: %v\n", err)
			return 1
		}
		return 0
	}

	var opts []wikipedia.Option
	if *userAgent != "" {
		opts = append(opts, wikipedia.WithUserAgent(*userAgent))
	}
	if *lang != "" {
		opts = append(opts, wikipedia.WithLang(*lang))
	}
	if host := os.Getenv(EnvAPIHostOverride); host != "" {
		opts = append(opts, wikipedia.WithAPIHostOverride(host))
	}
	if err := runFetch(context.Background(), wikipedia.New(opts...), stdin, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "yaad-wikipedia: %v\n", err)
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
	CanonicalEdgeTypesEmitted []string `json:"canonical_edge_types_emitted,omitempty"`
	SupportsSearch bool `json:"supports_search,omitempty"`
	// SourceNamespace declares the per-plugin vault path prefix
	// and entity-ID namespace under the ADR-0021 universal `kind:
	// source` contract. Daemon derives `<source_namespace>:<slug.
	// Slug(name)>` for every emission with `structured.kind:
	// "source"`.
	SourceNamespace string `json:"source_namespace,omitempty"`
	// CacheTTLSeconds is the plugin-level TTL declaration that
	// participates in yaad-index's three-level cache resolution
	//. 31536000s = 365d. omitempty so older yaad-index
	// builds that don't know the field decode cleanly (default 0
	// → "no opinion" → falls through to global config).
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
}

type kindSpecJSON struct {
	Name string `json:"name"`
	DefaultTTLDays int `json:"default_ttl_days,omitempty"`
}

func runInit(stdout io.Writer) error {
	doc := capabilitiesDoc{
		Name: wikipedia.PluginName,
		Version: wikipedia.PluginVersion,
		// URLPatterns is ordered: yaad-index dispatches first-match
		// across plugins (ADR-0006), but within one plugin's own
		// patterns the order is informational. Listing the canonical
		// URL form first matches operator intuition (URLs are the
		// primary input shape; shorthand is a convenience).
		URLPatterns: []string{wikipedia.URLPattern, wikipedia.ShorthandPattern},
		// ADR-0021: yaad-wikipedia emits the universal `kind:
		// source` shape; per-kind entity_kinds collapses to a
		// single source kind. Daemon's vault-path routing uses the
		// SourceNamespace declaration below.
		EntityKinds: []kindSpecJSON{
			{
				Name: wikipedia.UniversalSourceKind,
				DefaultTTLDays: wikipedia.DefaultTTLDays,
			},
		},
		EdgeKinds: []kindSpecJSON{},
		SourceNamespace: wikipedia.SourceNamespace,
		// Canonical-kinds declaration per ADR-0008. The runtime list
		// of kinds is in internal/wikipedia.KnownCanonicalKinds —
		// one source of truth so a kind added there auto-flows into
		// the capabilities doc. yaad-wikipedia emits two edge types
		// today: `is_about` (article → inferred canonical kind, when
		// Wikidata Q-id resolves) and `is_a` (universal source-type
		// label per ADR-0021).
		CanonicalKindsEmitted: wikipedia.KnownCanonicalKinds,
		CanonicalEdgeTypesEmitted: []string{
			wikipedia.CanonicalEdgeType,
			wikipedia.SourceTypeEdgeType,
		},
		// SupportsSearch declares opt-in to /v1/search/upstream per
		// yaad-index #2. yaad-wikipedia delegates to the action API's
		// `?list=search` endpoint via wikipedia.Plugin.Search.
		SupportsSearch: true,
		// Plugin-level cache TTL declaration per yaad-index (and
		//: 365 days. Wikipedia article cadence is
		// slow enough that a yearly default is the right hands-off
		// contract. Surfaces in yaad-index's resolveCacheTTL chain at
		// the plugin level; operators with tighter freshness needs
		// override per-entry or globally.
		CacheTTLSeconds: wikipedia.DefaultCacheTTLSeconds,
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
	// yaad-index #2. Ignored on operation=ingest.
	Query string `json:"query,omitempty"`
	Limit int `json:"limit,omitempty"`
}

// searchResponseDoc is the stdout JSON shape for operation=search
// per yaad-index #2. Mirrors yaad-index's subprocess.searchResponse
// — single object, no NDJSON wrap.
type searchResponseDoc struct {
	OK bool `json:"ok"`
	Candidates []wikipedia.SearchResultCandidate `json:"candidates,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type fetchResponse struct {
	OK bool `json:"ok"`
	Structured *structuredResponse `json:"structured,omitempty"`

	// RawContent is the article body in plaintext (per ADR-0008's
	// FetchResult shape). yaad-index's subprocess wrapper reads this
	// at the top level (sibling of `structured`), translates it onto
	// FetchResult.RawContent, and the orchestrator persists it as
	// the vault file's body.
	RawContent string `json:"raw_content,omitempty"`

	// Gaps is the {field-name → AI-prompt} map per ADR-0002's
	// universal-state amendment. yaad-wikipedia declares the
	// gap-name set (`summary` + `tags` universally) with
	// empty-string placeholder prompt values — post yaad-index #4
	// the daemon's canonical-kind registry is the authoritative
	// source for the AI-prompt text (operator's `canonical_kinds:`
	// config supplies the per-gap Description). The agent fills via
	// POST /v1/entities/{id}/fill.
	Gaps map[string]string `json:"gaps,omitempty"`

	// Options is the disambiguation-candidate map per ADR-0006.
	// Mutually exclusive with Structured: a single Fetch call emits
	// one or the other, never both. yaad-index's subprocess wrapper
	// reads this into FetchResult.Options and translates to
	// `state: disambiguation` on the API surface.
	Options map[string]optionJSON `json:"options,omitempty"`

	// Notations is every input form yaad-wikipedia knows resolves to
	// the entity in `structured` (per yaad-index the source issue a prior PRv2).
	// yaad-index's subprocess wrapper reads this into
	// FetchResult.Notations; the orchestrator writes them to the
	// entity_notations cache after a successful Fetch (a prior PR) so
	// subsequent ingests of any equivalent form short-circuit on
	// the cache. Always includes the originating notation in
	// addition to derived equivalents (canonical URL, mobile URL,
	// shorthand).
	Notations []string `json:"notations,omitempty"`

	// Aliases is the alternative-label list (per yaad-index issue
	// a prior PR). yaad-index's subprocess wrapper reads this into
	// FetchResult.Aliases; Marshal there merges with the ADR-0011
	// title-synthesized alias and dedupes. Today this is just the
	// article title; future PRs may add multi-language aliases.
	Aliases []string `json:"aliases,omitempty"`

	// CacheTTLSeconds is the optional per-fetch cache TTL override
	// (per yaad-index +. Pointer-shape on
	// the wire so absent / explicit-zero / positive / negative are
	// all distinguishable. Defaults to wikipedia.DefaultCacheTTLSeconds
	// (365d) on every successful fetch — same value as the plugin-
	// level Capabilities default. The dual-emission lets yaad-index's
	// resolveCacheExpires read the per-fetch value at the entry
	// level of the three-level chain without re-reading capabilities.
	CacheTTLSeconds *int `json:"cache_ttl_seconds,omitempty"`
}

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

func runFetch(ctx context.Context, p *wikipedia.Plugin, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	var req fetchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	switch req.Operation {
	case "ingest":
		// fall through to the existing ingest path below
	case "search":
		return runSearch(ctx, p, req, stdout)
	default:
		return fmt.Errorf("unsupported operation %q (supported: \"ingest\", \"search\")", req.Operation)
	}
	if req.URL == "" {
		return errors.New("request missing `url`")
	}

	outcome, err := p.Fetch(ctx, req.URL)
	if err != nil {
		// Wrap every fetch error with the URL so operators can grep
		// for the offending request in stderr-tagged logs — applies
		// to ErrNotFoundUpstream + parse errors + upstream HTTP
		// failures + timeouts uniformly. yaad-index's subprocess
		// wrapper surfaces these as fetch_failed; errors.Is downstream
		// still resolves ErrNotFoundUpstream through the %w chain so
		// the daemon's not-found-vs-other-failure differentiation is
		// preserved.
		return fmt.Errorf("%w: %s", err, req.URL)
	}

	resp := fetchResponse{OK: true}

	// Disambiguation path: search returned multi-match (or the URL
	// resolved to a disambiguation page that triggered the search-
	// fallback). Emit options-only — yaad-index's subprocess wrapper
	// translates this into `state: disambiguation`.
	if outcome.Article == nil {
		resp.Options = make(map[string]optionJSON, len(outcome.Options))
		for _, o := range outcome.Options {
			resp.Options[o.ID] = optionJSON{
				Label: o.Label,
				Summary: o.Summary,
			}
		}
		// NDJSON shape per ADR-0023: single-line JSON +
		// trailing `\n` (the Encoder's Encode method appends
		// one). SetIndent is intentionally NOT called — the
		// pre-ADR-0023 pretty-printed multi-line shape is
		// retired so each invocation produces exactly one
		// envelope line.
		return json.NewEncoder(stdout).Encode(resp)
	}

	// Article path: standard fetched-entity. Emit the universal gap
	// name set (summary + tags). Post yaad-index #4 the daemon's
	// canonical-kind registry is the authoritative source for the
	// AI-prompt text per gap; the values here are vestigial empty
	// placeholders kept so the wire shape's gaps map stays present.
	article := outcome.Article
	gaps := map[string]string{
		"summary": "",
		"tags":    "",
	}

	// ADR-0021 source-shape: kind="source" + descriptive name +
	// edges block. ID is omitted on the wire — daemon derives via
	// `<source_namespace>:<slug.Slug(name)>`.
	resp.Structured = &structuredResponse{
		Kind: wikipedia.UniversalSourceKind,
		Name: article.Name,
		Data: article.Data,
		Edges: marshalEdges(article.Edges),
		Provenance: marshalProvenance(article.Provenance),
	}
	resp.RawContent = article.RawContent
	resp.Gaps = gaps
	resp.Notations = article.Notations
	resp.Aliases = article.Aliases
	// Per-fetch cache TTL override: same value
	// as the plugin-level Capabilities default. Pointer-shape on
	// the wire so future PRs can specialize per-article (e.g. a
	// freshly-edited article might warrant a shorter TTL than the
	// 365d default).
	cacheTTL := wikipedia.DefaultCacheTTLSeconds
	resp.CacheTTLSeconds = &cacheTTL
	// NDJSON shape per ADR-0023 (see disambiguation branch above
	// for the same rationale): single-line JSON + trailing `\n`,
	// no SetIndent.
	return json.NewEncoder(stdout).Encode(resp)
}

// runSearch handles the operation=search subprocess call per
// yaad-index #2. Reads the operator/agent query from the
// fetchRequest's Query field, dispatches to wikipedia.Plugin.Search,
// and emits the searchResponseDoc shape on stdout.
//
// Empty query → ok:false with an error_message; the daemon's
// federation handler surfaces this on the per_plugin_status block
// without failing the federated call.
//
// Network / upstream errors → ok:false + error_message (the
// federation handler logs the message verbatim). Successful empty
// results → ok:true + empty candidates list.
func runSearch(ctx context.Context, p *wikipedia.Plugin, req fetchRequest, stdout io.Writer) error {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return json.NewEncoder(stdout).Encode(searchResponseDoc{
			OK:           false,
			ErrorMessage: "request missing `query`",
		})
	}
	candidates, err := p.Search(ctx, query, req.Limit)
	if err != nil {
		return json.NewEncoder(stdout).Encode(searchResponseDoc{
			OK:           false,
			ErrorMessage: err.Error(),
		})
	}
	return json.NewEncoder(stdout).Encode(searchResponseDoc{
		OK:         true,
		Candidates: candidates,
	})
}

// marshalEdges translates the plugin's internal edges map into
// the wire-shape `{<edge_type>: [{name, kind}, ...]}`. Empty/nil
// returns nil so omitempty drops the field on the wire.
func marshalEdges(edges map[string][]wikipedia.EdgeTarget) map[string][]edgeTargetJSON {
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

func marshalProvenance(entries []wikipedia.ProvenanceEntry) []provenanceJSONEntry {
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
