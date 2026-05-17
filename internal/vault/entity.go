// Package vault is the markdown-vault persistence boundary for ADR-0008.
//
// The vault is the source of truth for entity state; the SQLite store is a
// derived index regenerable via reindex. This package provides the
// serialization layer (Entity ↔ markdown file) and the atomic-write Writer
// + parsing Reader. It does not own the runtime ingest path or the reindex
// CLI — those land in a prior PR and a prior PR respectively.
//
// The frontmatter schema is locked by ADR-0008's "Frontmatter schema (v1)"
// table; the body shape (clean_content + `## Edges` + `## Notes`) is
// generated from frontmatter on write.
//
// Read/write asymmetry. The frontmatter is the canonical source on write —
// the writer regenerates the body sections from frontmatter every time, so
// hand-edits made to a `## Edges` or `## Notes` block in the body
// disappear on the next write that doesn't first read-merge them in. The
// reader compensates by merging body-section parses on top of frontmatter
// (additional wikilinks, additional dated note blocks) when it returns
// the entity. The intended workflow is therefore: hand-edits land in the
// body → reindex (or any vault-aware writer that read-modifies-writes)
// picks them up via the reader's body→frontmatter merge → next write
// dumps them back out from the now-canonical frontmatter. A writer that
// fires before any reader sees the body edit will overwrite the edit;
// the v1 design accepts that staleness window (ADR-0008 deferred a file
// watcher).
package vault

import "time"

// Entity is the in-memory shape of a vault file. Mirrors store.Entity
// today but extends it with the v1 frontmatter fields (Plugin, Summary,
// Tags, Notes, Gaps) — separate type until a prior PR converges the two
// shapes at the ingest boundary.
//
// Required for serialization: ID, Kind, Plugin. Everything else is allowed
// to be empty/nil — partial entities (post-ingest, pre-fill) are valid
// vault state.
type Entity struct {
	ID string // e.g. "wikipedia:martin-wallace"
	Kind string // e.g. "wikipedia-article"
	Plugin string // emitting plugin name, e.g. "wikipedia"
	Data map[string]any // kind-specific fields (title, lang, etc.)
	Provenance []ProvenanceEntry
	Summary string // agent-filled gap; empty until fill
	Tags []string // plugin-emitted + agent-filled
	Edges []Edge
	Notes []Note
	// Dataview is the body-section list of agent-appended
	// dataview-inline paragraphs per yaad-index #119. Each
	// paragraph is a `key → value` map representing one
	// canonical-type fill event on this entity (the target).
	// Append-only at the storage layer; dedup-on-append is
	// implemented at the handler level (paragraph identity =
	// sorted-key equality of every (key, value) pair).
	Dataview []DataviewParagraph
	Gaps []string // currently-unfilled gap field names

	// Aliases is the navigation overlay surfaced in frontmatter —
	// alternative labels Obsidian wikilinks resolve through, plus
	// the agent-facing reverse-lookup hint set.
	//
	// Two contributing sources, merged by Marshal at write time
	// (per yaad-index the source issue a prior PR):
	//
	// 1. ADR-0011 title-synthesized — a single entry derived from
	// `data.title` (source-shape entities) or `data.name`
	// (canonical-shape entities, kind ∈ operator's
	// canonical_kinds). Skipped when the title equals the slug
	// or is empty.
	// 2. Plugin-emitted (`FetchResult.Aliases`) — the
	// orchestrator threads any plugin-returned slice through
	// to this field. Two shapes coexist in the flat list:
	// - Bare strings ("Susanna Clarke") render as wikilink
	// targets.
	// - `<edge-type>: <label>` prefixes carry a typed
	// reverse-lookup hint that agents filter on.
	//
	// Marshal order: synthesized first (deterministic), then
	// plugin-emitted in input order. Duplicates are deduped on
	// case-sensitive string match. Empty merged result drops the
	// frontmatter field via omitempty.
	//
	// Not user-authored — reindex re-synthesizes from the entity's
	// current state per ADR-0008's vault-canonical write-from-DB
	// model. Hand-edits to the `aliases:` list survive only until
	// the next ingest re-writes the file.
	Aliases []string

	// Notations is the cache-key list per yaad-index the source issue a prior PR.
	// Mirrors the entity_notations DB table — every input form
	// (canonical URL, shorthand `<plugin>: <id>`, mobile-subdomain
	// URL, etc.) the plugin knows resolves to this entity. The vault
	// is the source of truth (ADR-0008); reindex re-derives the DB
	// table from this frontmatter list.
	//
	// Order matches `FetchResult.Notations` (input notation first,
	// deduped). Empty/nil → frontmatter omits the field entirely
	// (no `notations: []` artifact).
	Notations []string

	// CleanContent is the verbatim plugin-emitted body — the agent's
	// raw material for the fill pass. Written verbatim into the file
	// body before the `## Edges` / `## Notes` mirror sections; not
	// stored in frontmatter.
	//
	// Mirrors the plugin's `RawContent` body (per
	// `internal/plugins/plugin.go` `FetchResult.RawContent`) and is
	// surfaced under `clean_content` on the API response wire and on
	// the vault file body. Same bytes; the rename happens at the
	// plugin-vs-agent boundary because the plugin's "raw" (what
	// upstream returned) is the agent's "clean" (what to read).
	CleanContent string

	// CacheExpires is the absolute-date cache freshness stamp per
	// yaad-index (replaces's CacheTTLSeconds). Resolved
	// at ingest time as `fetched_at + resolveCacheTTL(...)` and
	// baked into vault frontmatter. Lookup is just `now <
	// CacheExpires.Time` (or true on Never sentinel).
	//
	// nil pointer → no opinion at any resolution level (cache
	// forever; preserves legacy behavior). Non-nil with
	// Never=true → infinite (replaces's *=-1 sentinel).
	// Non-nil with Time set → finite expiry.
	//
	// Stamped in operator-TZ via clock.Now()-derived fetched_at
	// per yaad-index. Hand-edits to the frontmatter date
	// survive a single read but get overwritten on the next
	// ingest — same write-only-by-orchestrator contract as
	// CacheTTLSeconds had.
	CacheExpires *CacheExpires

	// Attachments is the entity's attachment manifest per ADR-0018
	// §Attachments and ownership cascade. Aggregate-root model: each
	// file lives under the entity's directory
	// (`<kind>/<slug>/attachments/<name>` for active, mirrored under
	// `_archive/<kind>/<slug>/attachments/` when archived). The
	// archive/restore/destroy lifecycle moves the subdir alongside
	// the .md file; there are no standalone attachment endpoints and
	// no sharing across entities (parents each get their own copy).
	//
	// Empty/nil → frontmatter omits the `attachments:` field via
	// omitempty. Manifest serialization is a write-time concern;
	// the read endpoint streams from the resolved on-disk path,
	// so a manifest entry whose file is missing surfaces as a 404
	// at the HTTP layer (vault-DB drift bookkeeping is the same as
	// for the .md body).
	Attachments []Attachment

	// GapState carries per-field metadata for the operator-fill
	// state machine per ADR-0019 §Storage. Field VALUES live under
	// `Data`; this map carries metadata about how each field moved
	// through the agent-or-operator fill path. Keyed by field name
	// (the same key under `Data`).
	//
	// Empty / nil → frontmatter omits the `gap_state:` field. The
	// store mirrors this map (see store.Entity.GapState) so reindex
	// can reconstitute the DB column from the vault.
	GapState map[string]GapStateEntry
}

// GapStateEntry mirrors store.GapStateEntry per ADR-0019 §Storage —
// per-field metadata about how a gap moved through the agent-or-
// operator fill path. Two semantically-distinct shapes coexist:
//
// - Filled entries set Source + FilledAt.
// - Deferred entries set Deferred=true + DeferredAt.
//
// YAML wire shape (under the top-level `gap_state:` block):
//
//	gap_state:
//	 rating: { source: operator, filled_at: 2026-05-08T16:30:00Z }
//	 played: { deferred: true, deferred_at: 2026-05-08T16:35:00Z }
//	 summary: { source: agent, filled_at: 2026-05-07T12:00:00Z }
type GapStateEntry struct {
	Source string `yaml:"source,omitempty"`
	FilledAt *time.Time `yaml:"filled_at,omitempty"`
	Deferred bool `yaml:"deferred,omitempty"`
	DeferredAt *time.Time `yaml:"deferred_at,omitempty"`
	// DataSchema is the per-key extraction guidance the agent's
	// fill prompt sees for canonical_type gaps carrying the
	// optional per-entry `data` map. Workflow `add_gap` injects
	// this when the workflow knows the shape of data its events
	// produce; canonical_kinds config provides only the gap's
	// global type/strategy. Map key = data-field name; map value
	// = natural-language extraction instruction for the LLM.
	// Empty / nil omits from frontmatter.
	DataSchema map[string]string `yaml:"data_schema,omitempty"`

	// Type / Description / FillStrategy / Range / MaxLength /
	// Values / Kinds carry the workflow-injected GapSpec per
	// #142. When set, /v1/needs-fill surfaces the workflow's
	// shape to the agent's fill prompt builder exactly as it
	// does for an operator-config-registered gap. Empty fields
	// fall back to the operator's canonical_kinds registration
	// (when present) or the runtime defaults. Persisted to
	// vault frontmatter alongside the gap-state audit trail.
	Type         string   `yaml:"type,omitempty"`
	Description  string   `yaml:"description,omitempty"`
	FillStrategy string   `yaml:"fill_strategy,omitempty"`
	Range        []int    `yaml:"range,omitempty"`
	MaxLength    int      `yaml:"max_length,omitempty"`
	Values       []string `yaml:"values,omitempty"`
	Kinds        []string `yaml:"kinds,omitempty"`
}

// Attachment is one entry in the entity's manifest per ADR-0018
// §Attachments. Carries the metadata an HTTP read endpoint or a
// reindex walk needs without re-statting the file. The file lives
// at `<entity-dir>/<Path>`, where `<entity-dir>` is the entity's
// own subdir (active or archive). `Path` is therefore relative to
// the entity's subdir, not to the vault root.
type Attachment struct {
	// Name is the file basename — what `GET /v1/entities/{id}/
	// attachments/{name}` will serve. e.g. "thumbnail.jpg".
	Name string `yaml:"name"`

	// Kind is the MIME type, e.g. "image/jpeg". Optional on parse;
	// when absent the read endpoint falls back to extension-based
	// content-type detection.
	Kind string `yaml:"kind,omitempty"`

	// Path is relative to the entity's own subdir. The canonical
	// shape today is "attachments/<name>" so the on-disk layout is
	// `<kind>/<slug>/attachments/<name>` for active entities and
	// `_archive/<kind>/<slug>/attachments/<name>` for archived ones.
	// Stored explicitly so a future split (e.g. "thumbnails/" vs
	// "attachments/") can carve subspaces without changing the
	// manifest shape.
	Path string `yaml:"path"`

	// Bytes is the file size in bytes — informational; the read
	// endpoint stat()s the file at serve time. Optional on parse.
	Bytes int64 `yaml:"bytes,omitempty"`
}

// Edge is a typed relationship to another entity, stored in frontmatter
// as `{type, to, metadata?}`. Mirrored to the body `## Edges` section as
// a wikilink (`[[<to>]]`) with an inline type annotation.
type Edge struct {
	Type string `yaml:"type"`
	To string `yaml:"to"`
	Metadata map[string]any `yaml:"metadata,omitempty"`
}

// Note is one user-authored entry on an entity. Stored in frontmatter
// as `{date, text, author?, operator?}` and mirrored to the body
// `## Notes` section as a dated block. Append-only in v1 (edit/delete
// is a follow-up per ADR-0008's open questions).
//
// Per yaad-index a prior PR (auth pair-claim model): Author is the agent
// that posted the note (mapped to JWT `sub`); Operator is the human
// resource owner (JWT `operator`). Both fields are optional on parse —
// legacy vault files omit Operator, and the parser leaves it empty
// rather than inventing a value. New notes stamp both.
type Note struct {
	Date time.Time `yaml:"date"`
	Text string `yaml:"text"`
	Author string `yaml:"author,omitempty"`
	Operator string `yaml:"operator,omitempty"`
}

// DataviewParagraph is one canonical-type fill event recorded on a
// target canonical entity per yaad-index #119. Each paragraph
// renders as a single line in the body's yaad:dataview region:
//
//	role:: Staff Platform Engineer  salary:: 150k+  source_email:: gmail:...
//
// Sorted-key rendering is the dedup contract — two paragraphs
// with the same key→value set produce the same line, so a
// re-fill of an already-recorded event is a content-hash no-op
// at write time and the parser inverts cleanly.
type DataviewParagraph struct {
	// Fields maps the Obsidian dataview-inline key → value
	// pairs for this paragraph. Keys are operator-meaningful
	// field names (e.g. `role`, `salary`, `source_email`).
	// Values are free-form strings — the daemon does not type
	// them.
	Fields map[string]string
}

// ProvenanceEntry records one fetch or fill attempt. Plugin-fetch entries
// set FetchedAt and leave FilledAt nil; agent-fill entries do the
// reverse. Same shape as store.ProvenanceEntry — kept separate so the
// vault package doesn't import store directly.
type ProvenanceEntry struct {
	Source string `yaml:"source"`
	FetchedAt *time.Time `yaml:"fetched_at,omitempty"`
	FilledAt *time.Time `yaml:"filled_at,omitempty"`
	OK bool `yaml:"ok"`
	Error string `yaml:"error,omitempty"`
	ErrorMessage string `yaml:"error_message,omitempty"`

	// FetchAttachments records the (role, uri) pairs the plugin
	// emitted on this fetch and the daemon successfully wrote to
	// disk (per ADR-0014 §4). Optional; populated only on
	// plugin-fetch entries that emitted attachments.
	//
	// Vault YAML shape:
	//
	//	fetch_attachments:
	//	 - role: thumb
	//	 uri: "https://cf.geekdo-images.com/.../thumb.jpg"
	//
	// Read-side: the orchestrator compares this list against the
	// next ingest's emitted Attachments to skip re-fetches when the
	// (role, uri) pair is unchanged.
	FetchAttachments []FetchAttachmentRef `yaml:"fetch_attachments,omitempty"`
}

// FetchAttachmentRef is one entry in ProvenanceEntry.FetchAttachments.
// Same shape as store.FetchAttachmentRef — kept separate to preserve
// the no-import rule between vault and store. The Extension field
// from the FetchResult-side Attachment is intentionally absent: it's
// preserved on disk via the filename, not in the provenance row.
type FetchAttachmentRef struct {
	Role string `yaml:"role"`
	URI string `yaml:"uri"`
}
