// Package plugins is the substrate for /v1/ingest extractors.
//
// Per ADR-0005 (invocation) + ADR-0006 (discovery), plugins are
// standalone binaries registered via an explicit config allowlist.
// This package owns the interface every plugin implementation
// satisfies (subprocess wrappers, fixture plugins for tests, …) and
// the Registry that the /v1/ingest handler consults.
//
// **Registration discipline.** All Register calls happen at server
// startup (NewHandler / config load), single-goroutine. Lookup is
// read-only after that point. The Registry uses no locks — adding
// one would be premature for a single-writer-at-init / many-reader
// pattern. If runtime registration ever lands, the Registry will
// need a mutex; until then the discipline is the contract.
package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/yaad-index/yaad-index/internal/store"
)

// Plugin extracts entities from URLs of one or more known shapes.
// Implementations include subprocess.Plugin (wraps a binary on disk)
// and fixture.Plugin (in-memory, used by tests).
//
// Match must be cheap and deterministic — it runs once per ingest
// call per registered plugin. Fetch may block on network I/O or
// subprocess wall-clock; implementations must respect ctx for
// cancellation and apply their own upstream timeout.
type Plugin interface {
	// Name is a stable identifier for logs, provenance, and the
	// future /v1/kinds source_plugins surface.
	Name() string

	// Match reports whether this plugin handles rawURL.
	Match(rawURL string) bool

	// Fetch performs the extraction. The returned FetchResult takes one
	// of two shapes — Entity (with optional Gaps for needs_fill) OR
	// Options (for disambiguation). Two paths, not three. The tracker
	// synthesizes the wire-level `state` field from which is populated:
	// Options non-empty → disambiguation; Entity set with non-empty
	// Gaps → needs_fill; Entity alone → complete. Plugins do not know
	// about protocol state names — the contract is "emit data, let
	// yaad-index label." When Entity is set it must have stable ID +
	// Kind; Provenance is recommended (the tracker synthesizes a
	// fallback if a plugin omits it).
	//
	// Fetch is the single-envelope contract preserved post-ADR-0023
	// for callers that only consume the first source emission (the
	// existing /v1/ingest URL-shape path). Implementations MAY route
	// Fetch through Stream internally — see subprocess.Plugin for
	// the canonical pattern: a Stream callback captures the first
	// source emission, the rest are drained.
	Fetch(ctx context.Context, rawURL string) (*FetchResult, error)

	// Stream performs the extraction with the post-ADR-0023 N-envelope
	// contract. The plugin emits zero or more source envelopes per
	// invocation; each is delivered to onEnvelope as it arrives,
	// before the next is read from the wire. Control packets (`_error`,
	// `_summary`) are surfaced via onControl; the implementation logs
	// them when onControl is nil.
	//
	// Callbacks are invoked synchronously from the read loop so the
	// caller's persist work (vault write, attachment dispatch, DB
	// upsert) completes before the next envelope is consumed —
	// "write-as-you-go" per ADR-0023 §recovery. A non-nil error from
	// onEnvelope or onControl terminates the stream; Stream returns
	// that error wrapped with the plugin name.
	//
	// Stream returns nil on a clean stream close (plugin exited zero
	// after emitting all envelopes), regardless of how many envelopes
	// or control packets were emitted. Plugin non-zero exit AFTER
	// envelopes already committed surfaces as a non-nil return —
	// callers MUST NOT roll back already-persisted envelopes (the
	// write-as-you-go contract guarantees on-disk state survives).
	//
	// Implementations:
	// - subprocess.Plugin runs the binary subprocess-per-request
	// and parses NDJSON / pretty-printed stdout via the same
	// value-by-value json.Decoder as Fetch.
	// - fixture.Plugin emits a script of envelopes for tests.
	Stream(ctx context.Context, rawURL string, onEnvelope EnvelopeFunc, onControl ControlFunc) error

	// Capabilities returns the entity / edge kinds + url_patterns this
	// plugin advertises. The /v1/kinds handler walks every registered
	// plugin's capabilities and aggregates the union (deduped by kind
	// name, source_plugins unioned). Implementations that don't carry
	// kinds (e.g. fixture plugins in tests) may return a zero value;
	// the kinds handler treats that as "no kinds contributed."
	Capabilities() Capabilities
}

// Capabilities mirrors the JSON document a plugin binary writes to
// stdout in response to `--init` (ADR-0005 §"Discovery"). Defined on
// the plugins package (not subprocess) so the Plugin interface can
// expose it without the api package picking up a subprocess import.
type Capabilities struct {
	Name string `json:"name"`
	Version string `json:"version"`
	URLPatterns []string `json:"url_patterns"`
	EntityKinds []KindSpec `json:"entity_kinds"`
	EdgeKinds []KindSpec `json:"edge_kinds"`

	// CanonicalKindsEmitted names the canonical-shape entity kinds
	// this plugin MAY emit alongside its source-shape entities (per
	// ADR-0008 + the cold-reviewer's a prior PR review note 2). Plugins declare these
	// at --init time so yaad-index startup can warn operators when
	// a plugin proposes a canonical kind that isn't enabled in the
	// operator's `canonical_kinds:` config — surfacing the
	// discoverability gap that's otherwise silent.
	//
	// Empty / absent → plugin emits no canonical stubs.
	CanonicalKindsEmitted []string `json:"canonical_kinds_emitted,omitempty"`

	// CanonicalEdgeTypesEmitted names the canonical-shape edge types
	// this plugin MAY emit between its source entity and a canonical
	// stub (e.g. `is_about` from `wikipedia:martin-wallace` to
	// `person:martin-wallace`). Same gating + warning semantic as
	// CanonicalKindsEmitted.
	CanonicalEdgeTypesEmitted []string `json:"canonical_edge_types_emitted,omitempty"`

	// SupportsSearch declares that the plugin opts in to the
	// upstream-search dispatch surface (per yaad-index the source issue
	// a prior PR). When `POST /v1/search/upstream` fans a query out
	// across registered plugins (a prior PR), only plugins with this
	// flag set are invoked. Plugins not opting in are silently
	// skipped — the operator's local-search surface (`/v1/search`)
	// continues to work for them.
	//
	// Default false — explicit opt-in. Plugins predating issue
	// emit no `supports_search` field and decode as false
	// (Go zero value), preserving current behavior.
	SupportsSearch bool `json:"supports_search,omitempty"`

	// CanonicalKindsExtras carries the plugin's per-kind gap
	// additions/overrides per ADR-0016 §2. Map keyed by
	// canonical-kind name; each entry's `gaps` map contributes to
	// the merged effective registry's Layer 2 (after code defaults,
	// before operator overrides).
	//
	// Plugin-side `instruction` declarations are forbidden (per
	// ADR-0016 §2). The subprocess wire decoder strips any
	// instruction field with a WARN naming the plugin; the daemon
	// never sees plugin-supplied instruction.
	//
	// Empty / nil → plugin contributes no per-kind extras; it
	// activates its declared canonical_kinds with built-in
	// defaults only. Plugins predating ADR-0016 emit no
	// `canonical_kinds_extras` field on the wire and decode as
	// nil — the merged registry then falls through to the
	// operator-config layers without plugin contribution.
	CanonicalKindsExtras map[string]CanonicalKindExtras `json:"canonical_kinds_extras,omitempty"`

	// CacheTTLSeconds is the plugin-level TTL declaration in the
	// three-level resolution chain (per yaad-index). Used when
	// no per-entry override exists in the entity's frontmatter and
	// the operator's global `cache_ttl_seconds` config is also 0.
	// Sentinel rules — identical at every level:
	//
	// - 0 (default / absent) → no opinion; fall through to the
	// next level (global config).
	// - positive N → entities from this plugin expire
	// after N seconds.
	// - negative → entities from this plugin never
	// expire (always cache hit).
	//
	// Resolution at ingest time picks the first non-zero value
	// across {entry-frontmatter > plugin > global}; all-zero falls
	// back to the documented default (no TTL stamped → cache hit
	// forever). See internal/api/cache_ttl.go::resolveCacheTTL.
	//
	// Plugins predating emit no `cache_ttl_seconds` field and
	// decode as 0 (Go zero value), preserving the legacy behavior
	// where the global config alone determined freshness.
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`

	// SourceNamespace declares the vault-path prefix and entity-ID
	// namespace for source nodes this plugin emits under the
	// ADR-0021 universal `kind: source` contract. When set + the
	// plugin emits `structured.kind: "source"`, the daemon derives:
	//
	// - entity ID: `<source_namespace>:<slug.Slug(structured.name)>`
	// - vault path: `<vault>/<source_namespace>/<slug>.md`
	//
	// Required for every plugin post-ADR-0021: the
	// daemon's wire-shape validator rejects responses without
	// this field, so plugins must emit it at --init.
	SourceNamespace string `json:"source_namespace,omitempty"`

	// Commands is the list of imperative command names this plugin
	// exposes per ADR-0022. Bare names, no `!` sigil — the sigil
	// lives only in the invocation surface (`<plugin>: !<name>`),
	// the advertised vocabulary is the bare string. Parallel to
	// URLPatterns: a plugin is typically URL-shape OR command-shape,
	// not both, but the protocol allows either or both.
	//
	// Empty / absent → plugin exposes no command-shape invocations
	// (e.g. yaad-wikipedia, yaad-bgg today). Plugins predating
	// ADR-0022 emit no `commands` field on the wire and decode as
	// nil, preserving back-compat — the daemon's command-routing
	// path simply never resolves to them.
	Commands []string `json:"commands,omitempty"`
}

// CanonicalKindExtras is the plugin-side declaration block per
// ADR-0016 §2 — a plugin's gap additions/overrides for a single
// canonical kind. The daemon strips any plugin-supplied
// instruction field at parse time per ADR-0016 §2 (instruction is
// operator-only end-to-end), so this struct deliberately omits an
// `Instruction` field; a plugin that inserts one onto the wire
// gets a WARN log and the field is ignored.
type CanonicalKindExtras struct {
	// Gaps maps gap-field-name → GapSpec ({type, description}).
	// Layered into the effective registry as Layer 2 (after code
	// defaults, before operator overrides).
	Gaps map[string]GapSpec `json:"gaps,omitempty"`
}

// GapSpec is re-exported from the config package so plugin
// capabilities decoders + the orchestrator's merge function speak
// the same gap-shape type without an internal/config dependency
// from the subprocess wrapper. The two definitions are kept in
// lock-step manually; canonical source is `internal/config`.
//
// ADR-0019 step 3 extends the JSON wire shape with the typed-gap
// fields the operator-config side already accepts (per ADR-0019
// step 2 / yaad-index):
//
// - FillStrategy: "agent" | "operator" | "both" (default "both")
// - Range: [min, max] for type=int
// - MaxLength: cap for type=string
// - Values: enumeration for type=enum
//
// The string-shape Capabilities entry (a bare description string
// in JSON) still parses — UnmarshalJSON below falls through to the
// long form. Plugins predating ADR-0019 emit only Description and
// optional Type; that shape continues to work unchanged.
type GapSpec struct {
	Type string `json:"type,omitempty"`
	Description string `json:"description"`
	FillStrategy string `json:"fill_strategy,omitempty"`
	Range []int `json:"range,omitempty"`
	MaxLength int `json:"max_length,omitempty"`
	Values []string `json:"values,omitempty"`
	// Kinds is the canonical-kind allowlist for the
	// `type: "canonical_type"` gap shape per yaad-index.
	// Two wire shapes (resolved by UnmarshalJSON):
	//
	// - Bare string `"*"`: wildcard — any canonical kind in the
	// operator's `canonical_kinds` registry per ADR-0008. Decodes
	// as []string{"*"}.
	// - List `["person", "boardgame"]`: explicit allowlist.
	//
	// Empty / nil → not a canonical_type gap; rejected when Type ==
	// "canonical_type" by the daemon's Validate at startup.
	Kinds []string `json:"kinds,omitempty"`
}

// gapSpecJSON mirrors GapSpec plus the `prompt` alias so a plugin
// emitting the operator-side spelling (`prompt: "..."` instead of
// `description: "..."`) parses cleanly. The two distinct fields →
// unambiguous decode → merge logic in UnmarshalJSON picks
// whichever was set.
type gapSpecJSON struct {
	Type string `json:"type"`
	Description string `json:"description"`
	Prompt string `json:"prompt"`
	FillStrategy string `json:"fill_strategy"`
	Range []int `json:"range"`
	MaxLength int `json:"max_length"`
	Values []string `json:"values"`
	Kinds json.RawMessage `json:"kinds"`
}

// UnmarshalJSON accepts the bare-string shorthand AND the typed
// long form. Mirrors config.GapSpec.UnmarshalYAML (yaad-index)
// so a plugin's Capabilities document and the operator's YAML
// config decode through the same value-shape rules.
//
// - JSON string ("Short summary") → GapSpec{Type: "string",
// Description: "Short summary"} — pre-ADR-0019 plugin shape.
// - JSON object (typed) → field-for-field decode.
// - The `prompt` alias reads into Description when `description`
// is absent.
//
// Type defaults to "string" when omitted (matches the YAML side).
func (g *GapSpec) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("decode GapSpec string shorthand: %w", err)
		}
		g.Type = "string"
		g.Description = s
		return nil
	}
	var p gapSpecJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("decode GapSpec object: %w", err)
	}
	if p.Type == "" {
		p.Type = "string"
	}
	desc := p.Description
	if desc == "" {
		desc = p.Prompt
	}
	kinds, err := decodeKindsJSON(p.Kinds)
	if err != nil {
		return fmt.Errorf("decode GapSpec.kinds: %w", err)
	}
	*g = GapSpec{
		Type: p.Type,
		Description: desc,
		FillStrategy: p.FillStrategy,
		Range: p.Range,
		MaxLength: p.MaxLength,
		Values: p.Values,
		Kinds: kinds,
	}
	return nil
}

// decodeKindsJSON resolves the polymorphic `kinds:` field per
// yaad-index: scalar `"*"` decodes to []string{"*"}; list
// `["person", "boardgame"]` decodes verbatim. Absent / null
// returns nil; any other shape rejects loudly so plugin
// capability typos surface at server start.
func decodeKindsJSON(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("decode kinds scalar: %w", err)
		}
		return []string{s}, nil
	case '[':
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("decode kinds list: %w", err)
		}
		return list, nil
	default:
		return nil, fmt.Errorf("kinds must be a scalar `\"*\"` or a list of strings")
	}
}

// validGapFillStrategies mirrors the operator-side enum from
// internal/config/canonical_kinds.go. Empty string is treated as
// the `both` default by callers; explicit non-empty values must
// be one of {agent, operator, both}.
var validGapFillStrategies = map[string]struct{}{
	"": {},
	"agent": {},
	"operator": {},
	"both": {},
}

// Validate enforces the ADR-0019 typed-gap rules for plugin-side
// declarations. Mirrors config.GapSpec.Validate (yaad-index)
// so a plugin's Capabilities document fails the same shape rules
// as an operator yaml that declared the same gap.
//
// Path names the gap field for error messages (e.g.
// "<plugin>.kinds.<kind>.gaps.<field>").
func (g GapSpec) Validate(path string) error {
	if _, ok := validGapFillStrategies[g.FillStrategy]; !ok {
		return fmt.Errorf("%s.fill_strategy=%q not in {agent, operator, both}", path, g.FillStrategy)
	}
	switch g.Type {
	case "int":
		if len(g.Range) > 0 {
			if len(g.Range) != 2 {
				return fmt.Errorf("%s.range must be [min, max] (length 2), got length %d", path, len(g.Range))
			}
			if g.Range[0] > g.Range[1] {
				return fmt.Errorf("%s.range: min %d > max %d", path, g.Range[0], g.Range[1])
			}
		}
		if g.MaxLength != 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=int does not accept max_length or values", path)
		}
	case "string":
		if g.MaxLength < 0 {
			return fmt.Errorf("%s.max_length=%d is negative", path, g.MaxLength)
		}
		if len(g.Range) > 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=string does not accept range or values", path)
		}
	case "text", "bool":
		if g.MaxLength != 0 || len(g.Range) > 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=%s does not accept max_length, range, or values", path, g.Type)
		}
	case "enum":
		if len(g.Values) == 0 {
			return fmt.Errorf("%s: type=enum requires non-empty values list", path)
		}
		seen := make(map[string]struct{}, len(g.Values))
		for _, v := range g.Values {
			if v == "" {
				return fmt.Errorf("%s.values: empty string not allowed", path)
			}
			if _, dup := seen[v]; dup {
				return fmt.Errorf("%s.values: duplicate %q", path, v)
			}
			seen[v] = struct{}{}
		}
		if g.MaxLength != 0 || len(g.Range) > 0 {
			return fmt.Errorf("%s: type=enum does not accept max_length or range", path)
		}
	default:
		// Unknown / pre-ADR-0019 type — passes through without
		// extra-field enforcement (back-compat with plugins
		// emitting types like "date" that pre-date ADR-0019).
	}
	return nil
}

// KindSpec is the per-kind metadata the capabilities document carries.
// Fields here are a subset of the full ADR-0005 shape (lines 58–66) —
// the parts /v1/kinds advertises. Description is operator-facing prose
// that the kinds handler propagates onto the wire response when the
// plugin supplies it; absent / empty descriptions render as empty
// strings on the response (clients tolerate the empty case).
//
// SnippetFields names the entity.data keys /v1/search should walk in
// order to derive a non-empty snippet for hits of this kind. The
// search handler picks the first non-empty field, truncates to
// SearchSnippetMaxChars (env-overridable), and surfaces it on the
// response. Plugins SHOULD populate this for every entity kind that
// has a natural prose-y data field; absent / empty SnippetFields
// falls back to a default chain (description → summary → extract →
// content) — see internal/api/snippet.go.
type KindSpec struct {
	Name string `json:"name"`
	Description string `json:"description,omitempty"`
	DefaultTTLDays int `json:"default_ttl_days,omitempty"`
	FromKind string `json:"from_kind,omitempty"`
	ToKind string `json:"to_kind,omitempty"`
	SnippetFields []string `json:"snippet_fields,omitempty"`
}

// EnvelopeFunc is the per-envelope callback shape Plugin.Stream
// invokes for each source emission. Returning a non-nil error
// terminates the stream early — Stream surfaces the error wrapped
// with the plugin name. Returning nil signals "envelope persisted;
// continue."
//
// The callback is invoked synchronously from the stream's read
// loop; the next envelope is NOT read until the callback returns.
// This is the load-bearing detail behind write-as-you-go: the
// tracker's per-envelope vault write + attachment dispatch + DB
// upsert all complete before the next envelope is consumed.
type EnvelopeFunc func(*FetchResult) error

// ControlPacketKind discriminates the two control-packet shapes per
// ADR-0023 §2.
type ControlPacketKind int

const (
	// ControlPacketError is the `_error` sentinel: a per-envelope
	// failure the plugin chose to log + skip rather than abort the
	// invocation. Slug is optional (the plugin may not have a slug
	// to attribute the error to).
	ControlPacketError ControlPacketKind = iota
	// ControlPacketSummary is the `_summary` aggregate-stats packet
	// (typically terminal, but a misplaced mid-stream summary doesn't
	// truncate the stream).
	ControlPacketSummary
)

// ControlPacket carries one decoded `_error` or `_summary` packet
// from a Plugin.Stream invocation. Only the fields valid for the
// declared Kind are populated; other fields are zero-valued.
type ControlPacket struct {
	Kind ControlPacketKind

	// Error fields (valid when Kind == ControlPacketError).
	ErrorSlug string
	ErrorKind string
	ErrorMessage string

	// Summary fields (valid when Kind == ControlPacketSummary).
	Ingested int
	Errors int
	DurationMs int
}

// ControlFunc is the per-control-packet callback shape Plugin.Stream
// invokes for each `_error` or `_summary` line. May be nil — the
// implementation logs the packet when no callback is wired.
// Returning a non-nil error terminates the stream early (same
// shape as EnvelopeFunc).
type ControlFunc func(ControlPacket) error

// FetchResult is what a plugin returns on a successful Fetch. Two
// shapes only — Entity (with optional Gaps for needs_fill) OR Options
// (for disambiguation). The tracker synthesizes the wire-level
// `state` from which field is populated:
//
// - Options non-empty → disambiguation
// - Entity set, Gaps non-empty → needs_fill
// - Entity set, Gaps empty → complete
// - all empty → 404 not_found
//
// A plugin author never types the literal strings "complete",
// "needs_fill", or "disambiguation" — they populate the right field
// and yaad-index labels the response.
// SourceEdgeTarget is one edge target in an ADR-0021 source-shape
// emission's `edges` block. Plugins emit `{name, kind}` only — the
// daemon's slug.Slug derives the canonical-label slug at ingest
// time (`<kind>:<slug.Slug(name)>`).
type SourceEdgeTarget struct {
	Name string
	Kind string
}

type FetchResult struct {
	// Entity is the canonical entity row to upsert. Set when the
	// upstream resolved to a single match.
	Entity *store.Entity

	// SourceName is the descriptive name a plugin emits under the
	// ADR-0021 universal-source contract (`structured.kind:
	// "source"` + `structured.name`). Daemon slugifies via
	// slug.Slug to derive the source-node entity ID. Empty when
	// the plugin uses the legacy ADR-0008 shape (Entity.ID
	// plugin-formed); the dual-shape switch lives in
	// internal/api/ingest_tracker.go.
	SourceName string

	// SourceEdges is the ADR-0021 `edges` block keyed by edge
	// type. Each value is one or more `{name, kind}` targets the
	// daemon resolves to canonical-label edge endpoints
	// (`<kind>:<slug.Slug(name)>`) at ingest time. Empty when the
	// plugin uses the legacy ADR-0008 CanonicalEntities/
	// CanonicalEdges shape.
	SourceEdges map[string][]SourceEdgeTarget

	// Provenance records this fetch attempt — typically one entry
	// stamped with the plugin's source identifier and `fetched_at`.
	// Optional but recommended; the tracker synthesizes a fallback
	// entry naming the plugin if Provenance is empty.
	Provenance []store.ProvenanceEntry

	// RawContent is the plugin-cleaned source content the agent's
	// AI extracts gap fields from. Populated when Gaps is non-empty.
	// Renamed to `clean_content` on the API response wire (per
	// ADR-0008 / docs/plugin-flow.md §2c) — the plugin's term for
	// "what the upstream returned" maps to the agent's term for
	// "the cleaned body I read." Same bytes, different layer name.
	RawContent string
	RawContentTruncated bool

	// Gaps maps each entity.data field name the agent's AI is
	// expected to fill to a short human-readable description that
	// helps the AI produce the right value (e.g. "summary": "one
	// paragraph summary based on long_summary"). Empty / nil means no
	// needs_fill transition.
	Gaps map[string]string

	// Options is the disambiguation candidate map keyed by the
	// plugin's canonical id. Set when the upstream returned multiple
	// plausible matches (e.g. Wikipedia's `Go` page listing the
	// language, the board game, the verb, etc.). Per ADR-0006,
	// populating Options signals the disambiguation state — the
	// tracker emits a 200 response with state="disambiguation" +
	// the options object. **Empty Options is NOT "disambiguation
	// with zero candidates"**; an empty FetchResult overall maps to
	// 404 not_found.
	//
	// Each key is the plugin-canonical identifier. The caller picks
	// one key and re-invokes /v1/ingest with the plugin's shorthand
	// (`<plugin>: <id>`) to fetch that candidate. Plugins emitting
	// Options MUST support that shorthand input shape — see ADR-0006
	// "Disambiguation responses".
	Options map[string]DisambiguationOption

	// Edges optionally records typed relationships alongside the
	// entity (e.g. boardgame → designer). Today the tracker doesn't
	// persist these (edges land via POST /v1/edges); reserved on
	// the type for the future-PR that wires plugin-emitted edges
	// into the auto-create path.
	Edges []store.Edge

	// CanonicalEdges carries the canonical-label edges the daemon
	// derived from `structured.edges` per ADR-0021. Each entry's
	// `To` is `<kind>:<slug.Slug(name)>` — a label that the
	// thin-row materialize step turns into an entity row before
	// the FK-constrained CreateEdge runs. Subprocess.toFetchResult
	// populates this field; plugins do not write it directly.
	//
	// CanonicalEntities (top-level legacy stubs per ADR-0008) was
	// removed in once both in-fleet plugins ( yaad-bgg,
	// yaad-wikipedia) migrated to the source-shape edges
	// block. The thin-row materialize path covers the entity-row
	// side that legacy CanonicalEntities used to carry.
	CanonicalEdges []*store.Edge

	// Notations is the list of every input form the plugin knows
	// resolves to this entity — canonical URL, shorthand
	// `<plugin>: <id>`, with/without underscore in the title,
	// mobile subdomain URL, etc. The orchestrator (per yaad-index
	// the source issue a prior PR) writes these to the `entity_notations`
	// lookup table after a successful Fetch so subsequent ingests
	// of any equivalent form short-circuit on the cache without
	// re-invoking the plugin.
	//
	// Plugins SHOULD include the input notation that triggered the
	// Fetch in this list — the orchestrator's lookup-first path
	// matches on exact-string equality, so the input form must be
	// present for a self-roundtrip to register.
	//
	// Empty / nil is permitted (plugin opts out of the cache
	// pre-registration; ingests still work via Match→Fetch on
	// every call). Existing plugins predating the source issue emit no
	// `notations` field on the wire and surface here as nil.
	Notations []string

	// Aliases is the list of alternative labels the plugin knows
	// for this entity (per yaad-index the source issue). Used by Obsidian
	// wikilink resolution + agent reverse-lookup.
	//
	// Two shapes coexist in the same flat slice:
	// - Bare strings ("Susanna Clarke", "S. Clarke") — render as
	// plain wikilink targets. Obsidian's [[label]] resolves to
	// the entity when label matches any alias.
	// - Typed prefixes (`<edge-type>: <label>`, e.g.
	// `designed_by: Martin Wallace`) — carry a reverse-lookup
	// hint agents can filter on. The label is the wikilink
	// target; the prefix is the typed-relationship hint.
	//
	// Order doesn't matter; the orchestrator dedupes against ADR-
	// 0011's title-synthesized alias at vault-write time and
	// preserves a deterministic merged order (synthesized first,
	// then plugin-emitted in input order).
	//
	// Empty / nil is permitted — plugins predating the source issue emit
	// no `aliases` field and the only alias on the resulting vault
	// file is the ADR-0011 title-synthesized one (current
	// behavior). Adopting plugin-emitted aliases is one slice
	// write at the FetchResult layer.
	Aliases []string

	// CacheTTLSeconds is an OPTIONAL per-fetch override for this
	// entity's cache-freshness contract (per yaad-index). Pointer
	// shape distinguishes absent (nil = no override; resolver falls
	// through to plugin-level / global-level) from explicit-zero
	// (*=0 = same as nil; explicit "no opinion") from a positive or
	// negative sentinel value the plugin wants to attach to THIS
	// fetch specifically (e.g. "this article was just edited
	// upstream, give it a 1-hour TTL even though my plugin default
	// is 7 days").
	//
	// At ingest persistence time, the orchestrator participates this
	// value as the entry-level input to resolveCacheTTL — same
	// sentinel semantics (positive=N seconds, negative=infinite,
	// zero=no opinion). The resolved TTL is baked into the entity's
	// vault frontmatter (`cache_ttl_seconds:`) and re-derives via
	// reindex per ADR-0008.
	//
	// Plugins predating leave this nil and the resolver walks
	// straight to plugin-level / global-level — preserves current
	// behavior.
	CacheTTLSeconds *int

	// Attachments is the OPTIONAL list of binary attachments the
	// plugin emits alongside the structured Entity (per ADR-0014).
	// Each entry is a `{Role, URI, Extension}` triple; the daemon
	// dispatches on URI scheme (`file://`, `https://`, `base64://`)
	// and writes the resolved bytes to
	// `<vault>/<kind>/<local-id>.<role>.<extension>` next to the
	// entity's `.md` file.
	//
	// Role + Extension are validated against the regexes from
	// ADR-0014 §5 before any filesystem access; per-attachment
	// failures (bad shape, scheme handler error, traversal attempt)
	// log at WARN and the offending attachment is skipped — the
	// rest of the entity proceeds.
	//
	// Empty / nil is the silent-preserves shape per ADR-0014 §4:
	// existing on-disk attachments for the entity are PRESERVED
	// when the plugin emits no attachments on a re-ingest. Plugins
	// predating ADR-0014 emit no `attachments` field on the wire
	// and surface here as nil (current behavior preserved).
	Attachments []Attachment
}

// Attachment is one entry in FetchResult.Attachments — the validated
// shape of an ADR-0014 attachment emission. Role + Extension are
// regex-validated before any filesystem access; URI dispatches by
// scheme to the file:// / https:// / base64:// handlers in
// internal/attachments.
//
// Plugins predating ADR-0014 emit no attachments field at all; this
// type only carries data when a plugin opts in by populating the
// FetchResult.Attachments slice.
type Attachment struct {
	// Role is the plugin-defined semantic identifier (`thumb`,
	// `cover`, `rules`, etc.). Becomes part of the vault filename.
	// MUST match `^[a-z0-9][a-z0-9-]{0,31}$`.
	Role string

	// URI is the source. One of `file://`, `https://`, `http://`,
	// `base64://`. The daemon dispatches on the scheme prefix.
	URI string

	// Extension is the on-disk file extension WITHOUT the leading
	// dot, lowercase. MUST match `^[a-z0-9]{1,10}$`. Required (no
	// inference from URI).
	Extension string
}

// DisambiguationOption is the lightweight metadata for one candidate
// in a disambiguation response. The map key on FetchResult.Options
// carries the canonical id; this struct just decorates that key with
// a human-readable label and an optional summary. No URL field —
// callers re-invoke ingest via the plugin's shorthand input shape
// (`<plugin>: <id>`) using the map key directly.
//
// - Label — human-readable name for the option. Plugin-provided,
// NOT derived from the id.
// - Summary — a few words to a short sentence so the agent picking
// among options can disambiguate semantically. Plugin SHOULD
// include but MAY omit.
//
// Wire shape (per ADR-0006):
//
//	"options": {
//	 "Go_(programming_language)": {"label":"Go (programming language)", "summary":"Open-source language by Google"},
//	 "Go_(game)": {"label":"Go (game)", "summary":"Ancient Chinese strategy board game"}
//	}
type DisambiguationOption struct {
	Label string
	Summary string
}

// Registry is an ordered list of plugins. The first plugin that
// Match()es a URL handles it (first-match-wins). Order is set at
// registration time; registration order encodes priority.
type Registry struct {
	plugins []Plugin
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a plugin to the end of the dispatch list. Must be
// called at server startup, before any Lookup calls (see package
// godoc).
func (r *Registry) Register(p Plugin) {
	r.plugins = append(r.plugins, p)
}

// Lookup returns the first plugin whose Match returns true for
// rawURL, or (nil, false) if none match.
func (r *Registry) Lookup(rawURL string) (Plugin, bool) {
	for _, p := range r.plugins {
		if p.Match(rawURL) {
			return p, true
		}
	}
	return nil, false
}

// LookupByName returns the registered plugin whose Name() equals
// name, or (nil, false) if no plugin with that name is registered.
// Used by routing-time validation (yaad-index) to target a
// specific plugin's url_patterns / commands list when the input
// carries a `<plugin>:` namespace prefix.
func (r *Registry) LookupByName(name string) (Plugin, bool) {
	for _, p := range r.plugins {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// Plugins returns the registered plugins in registration order.
func (r *Registry) Plugins() []Plugin {
	out := make([]Plugin, len(r.plugins))
	copy(out, r.plugins)
	return out
}
