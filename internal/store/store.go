// Package store is the persistence boundary for yaad-index.
//
// Handlers depend only on the Store interface; SQL strings, drivers, and
// schema details live behind it (per the architectural decision in step 8's
// dispatch — no SQL strings outside this package, ever). The default
// implementation uses modernc.org/sqlite, but the interface is shaped so a
// future Postgres / other backend swap requires no handler changes.
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Entity is the canonical entity record. Edges, when populated, come from
// the optional with-edges expansion (see GetEntity / GetEntities; later
// PRs wire the param plumbing).
//
// GapCallDoneAt tracks whether the AI has been gap-called for the
// entity's current fetch-cycle (per ADR-0013 §4 + §5).
// Set on a successful fill (any 2xx response from
// `POST /v1/entities/{id}/fill`, full or partial); cleared by an
// upstream refetch (`force_refetch=true` or TTL-driven). nil when
// the AI has NOT yet been gap-called this fetch-cycle. Read-side
// suppression on cache-hit `needs_fill` checks this flag.
//
// **DB-only** — the vault is unaware of this column. Wipe DB + reindex
// re-derives the entity from vault but leaves this flag NULL, by
// design (ADR-0013 §5: regen invariant; gap-call replay against
// unchanged content is acceptable).
type Entity struct {
	ID string
	Kind string
	Data map[string]any
	Provenance []ProvenanceEntry
	Edges []EdgeRef
	CreatedAt time.Time
	UpdatedAt time.Time
	GapCallDoneAt *time.Time
	// ArchivedAt marks when ArchiveEntity moved this row to the
	// archived state per ADR-0018. nil = active (default); non-nil
	// = archived at that moment. Cleared by RestoreEntity. Default
	// list/search surfaces filter `archived_at IS NULL`; lookup-by-
	// id (GetEntity) returns archived rows normally so an operator
	// can still read the row.
	ArchivedAt *time.Time

	// GapState carries per-field metadata for the operator-fill
	// state machine per ADR-0019 §Storage. Field VALUES live under
	// `Data`; this map carries metadata about how each field moved
	// through the agent-or-operator fill path. Keyed by field name
	// (the same key under `Data`).
	//
	// Empty map / nil → no metadata for any field (the default
	// state on existing rows pre-ADR-0019). The DB column is NULL
	// in that case; readers degrade cleanly.
	GapState map[string]GapStateEntry
}

// GapStateEntry is one entry in Entity.GapState per ADR-0019
// §Storage. Records who filled a gap (agent vs. operator), when it
// was filled, and whether the operator deferred it.
//
// Two semantically-distinct shapes coexist in this struct:
//
// - Filled entries set Source + FilledAt (Deferred=false,
// DeferredAt=nil). Source is the load-bearing field —
// downstream UI / endpoint surfaces branch on it to attribute
// the value.
// - Deferred entries set Deferred=true + DeferredAt (Source="",
// FilledAt=nil). A deferred field can later be filled — the
// deferred flag goes back to false and Source/FilledAt land.
//
// JSON wire shape (vault frontmatter mirror; same shape as the DB
// column):
//
//	{"source": "operator", "filled_at": "2026-05-08T16:30:00Z"}
//	{"deferred": true, "deferred_at": "2026-05-08T16:30:00Z"}
type GapStateEntry struct {
	Source string `json:"source,omitempty"`
	FilledAt *time.Time `json:"filled_at,omitempty"`
	Deferred bool `json:"deferred,omitempty"`
	DeferredAt *time.Time `json:"deferred_at,omitempty"`
	// DataSchema is the per-key extraction guidance the agent's
	// fill prompt sees for canonical_type gaps carrying the
	// optional per-entry `data` map. Workflow `add_gap` injects
	// this when the workflow knows the shape of data its events
	// produce; canonical_kinds config provides only the gap's
	// global type/strategy/range. Map key = data-field name; map
	// value = natural-language extraction instruction for the
	// LLM. Empty / nil omits from the wire.
	DataSchema map[string]string `json:"data_schema,omitempty"`

	// Type / Description / FillStrategy / Range / MaxLength /
	// Values / Kinds carry the workflow-injected GapSpec per
	// #142. /v1/needs-fill surfaces these alongside the
	// operator-config-derived metadata so the agent's fill
	// prompt builder sees one unified spec regardless of which
	// surface declared the gap.
	Type         string   `json:"type,omitempty"`
	Description  string   `json:"description,omitempty"`
	FillStrategy string   `json:"fill_strategy,omitempty"`
	Range        []int    `json:"range,omitempty"`
	MaxLength    int      `json:"max_length,omitempty"`
	Values       []string `json:"values,omitempty"`
	Kinds        []string `json:"kinds,omitempty"`
}

// ArchivedFilter selects which archive-state subset of entities a
// listing surface returns per ADR-0018 step 2.
type ArchivedFilter int

const (
	// ArchivedExclude is the default-filter — only `archived_at IS
	// NULL` rows. Hides archived entities from list/search.
	ArchivedExclude ArchivedFilter = iota
	// ArchivedInclude returns active + archived rows together —
	// callers passing `?include_archived=true`.
	ArchivedInclude
	// ArchivedOnly returns ONLY rows whose `archived_at IS NOT
	// NULL` — callers passing `?archived_only=true`.
	ArchivedOnly
)

// Edge is a typed relationship between two entities. Metadata is opaque
// JSON; provenance records who/what/when created or last touched the row.
type Edge struct {
	Type string
	From string
	To string
	Metadata map[string]any
	Provenance []ProvenanceEntry
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EdgeRef is the abbreviated edge form embedded inside Entity.Edges.
type EdgeRef struct {
	Type string
	To string
}

// ProvenanceEntry records a single fetch or fill attempt against an entity
// or edge. Plugin-fetch entries set FetchedAt and leave FilledAt nil;
// agent-fill entries set FilledAt and leave FetchedAt nil. Each entry has
// exactly one of the two date fields populated.
type ProvenanceEntry struct {
	Source string
	FetchedAt *time.Time
	FilledAt *time.Time
	OK bool
	Error string
	ErrorMessage string

	// FetchAttachments records the (role, uri) pairs that landed on
	// disk for this fetch attempt (per ADR-0014 §4). Optional;
	// populated only on plugin-fetch entries that emitted attachments.
	// Read-side compares this against the next fetch's Attachments
	// to short-circuit re-fetches when the URI hasn't changed.
	//
	// Wire shape on the vault YAML side:
	//
	//	fetch_attachments:
	//	 - role: thumb
	//	 uri: "https://cf.geekdo-images.com/.../thumb.jpg"
	//
	// Pre-ADR-0014 entries have FetchAttachments nil; the next
	// ingest's comparison treats nil as "no prior attachments" and
	// performs every fetch. Backward-compatible with existing rows.
	FetchAttachments []FetchAttachmentRef
}

// FetchAttachmentRef is one (role, uri) pair stamped on a
// ProvenanceEntry's FetchAttachments. Mirrors the FetchResult.
// Attachments wire shape minus the Extension (which is preserved on
// disk via the filename). Per ADR-0014 §4: re-fetch comparison is a
// pure string compare on (role, uri) against the freshest provenance.
type FetchAttachmentRef struct {
	Role string
	URI string
}

// DroppedCanonicalKindCount records how many times a plugin's
// emitted canonical-kind stub was dropped at the orchestrator's
// config-filter (per ADR-0013 §3). The count
// accumulates across the daemon's lifetime; it persists in the
// `dropped_canonical_kinds` table so a restart doesn't reset the
// drift signal. `/v1/cv-status` reads this for the
// `kinds_emitted_not_enabled[].would_materialize_count` field.
type DroppedCanonicalKindCount struct {
	Plugin string
	Kind string
	Count int64
	FirstSeenAt time.Time
	LastSeenAt time.Time
}

// DroppedCanonicalEdgeCount is the edge-type counterpart of
// DroppedCanonicalKindCount — same shape, but keyed by
// (plugin, edge_type) for canonical edge-type emissions the
// operator's `canonical_edge_types:` config didn't enable.
type DroppedCanonicalEdgeCount struct {
	Plugin string
	EdgeType string
	Count int64
	FirstSeenAt time.Time
	LastSeenAt time.Time
}

// Hit is one search result. Snippet and Score are populated by the search
// backend (FTS5 today, possibly external later — see ADR-0002 line 467).
// Data is the entity's deserialised data column — included on every hit
// so the API layer can derive snippets from per-kind data fields without
// a second roundtrip per result.
type Hit struct {
	ID string
	Kind string
	Data map[string]any
	Snippet string
	Score float64
}

// ContextNeighbor is one entry in a context-traversal result
// (`GET /v1/entities/{id}/context`). Carries the
// edge that introduced the neighbor, the neighbor entity itself, and
// the BFS depth at which it was first reached. Entities are visited
// at most once across a traversal — back-edges that would re-introduce
// an already-visited entity are dropped (the entity is already in the
// result set under its earlier depth).
type ContextNeighbor struct {
	Edge Edge
	Entity Entity
	Depth int
}

// ReindexFile is one row of reindex bookkeeping — per-vault-file state
// the incremental reindexer reads to decide whether a file has changed
// since the last walk. Path is the absolute file path (the primary key).
// Mtime + ContentHash are the change-detection signal; LastIndexedAt is
// the wall-clock at which this row was written. EntityID + EntityKind
// record the entity that the file produced — needed when a file is
// removed from disk and the bookkeeping row drives the entity delete.
type ReindexFile struct {
	Path string
	Mtime time.Time
	ContentHash string
	LastIndexedAt time.Time
	EntityID string
	EntityKind string
}

// CachedPluginCapabilities is one row from the plugin_capabilities
// table — the cached --init document for a single plugin. Version is
// the plugin-emitted version string (per the capabilities document);
// CapabilitiesJSON is the verbatim JSON the plugin wrote to stdout on
// --init, opaque to the store. Callers compare Version against a
// freshly probed version to decide cache validity.
type CachedPluginCapabilities struct {
	Version string
	CapabilitiesJSON []byte
	CachedAt time.Time
}

// Notation maps an input form (URL, `<plugin>: <id>` shorthand,
// future input shapes) to the canonical entity slug it resolves to.
// The orchestrator uses this to short-circuit the plugin
// Fetch when an inbound URL/shorthand is already known. Vault
// frontmatter persists the same list per entity so reindex
// can re-derive the table.
//
// - Notation — exact input string the caller passed.
// - EntityID — slug the notation resolves to (FK on entities).
// - Kind — discriminator (`url`, `shorthand`, …) — drives
// reindex re-formatting back into frontmatter.
type Notation struct {
	Notation string
	EntityID string
	Kind string
}

// NotationKind* are the canonical discriminator values that
// notation_kind uses today. Plugins MAY emit kinds outside this set
// — the schema doesn't constrain — but the orchestrator + reindex
// only know about these.
const (
	NotationKindURL = "url"
	NotationKindShorthand = "shorthand"
)

// Alias is the persisted shape for a single entity_aliases row
// per #3. Same axis as Notation but for outbound display labels
// rather than inbound input forms.
//
//   - Alias    — the full alias text (including any `<edge-type>:`
//                prefix for typed aliases). Operator + agent see
//                this verbatim in search hits.
//   - EntityID — slug the alias points at (FK on entities,
//                ON DELETE CASCADE).
//   - Kind     — discriminator in {'bare', 'typed'} per #3. 'bare'
//                is the Obsidian wikilink target shape; 'typed'
//                marks `<edge-type>: <label>` strings agents
//                reverse-lookup-filter on.
type Alias struct {
	Alias    string
	EntityID string
	Kind     string
}

// AliasKind* are the canonical discriminator values entity_aliases
// uses per #3. The orchestrator derives 'typed' when an alias
// matches the `<edge-type>: <label>` shape AND the prefix is in
// the operator's canonical_edge_types; everything else lands as
// 'bare'.
const (
	AliasKindBare  = "bare"
	AliasKindTyped = "typed"
)

// TypedAliasPrefix splits a candidate typed-alias string into its
// edge-type prefix + label half per #3 §"Bare-string AND typed-
// prefix shapes". Returns (prefix, label, true) for inputs that
// match the `<prefix>: <label>` shape with a single colon-space
// separator; (-, -, false) for inputs without that shape OR with
// a prefix containing whitespace / colons. The caller is
// responsible for validating the prefix against the operator's
// canonical_edge_types registry — the store doesn't see the
// registry. Bare aliases that happen to contain `: ` (e.g. a
// title like "Brass: Birmingham") aren't typed and the caller's
// registry check rejects them; this split is purely a shape
// gate.
func TypedAliasPrefix(alias string) (prefix, label string, ok bool) {
	idx := strings.Index(alias, ": ")
	if idx <= 0 || idx+2 >= len(alias) {
		return "", "", false
	}
	prefix = alias[:idx]
	for _, r := range prefix {
		if r == ':' || r == ' ' || r == '\t' || r == '\n' {
			return "", "", false
		}
	}
	return prefix, alias[idx+2:], true
}

// TypedAliasEntries builds entity_aliases rows from a flat alias-string
// slice, deriving each row's Kind: AliasKindTyped when the alias matches
// the `<prefix>: <label>` shape AND prefix is in the operator's
// canonicalEdgeTypes registry, AliasKindBare otherwise. Empty alias
// strings are skipped, duplicates de-duplicated (first occurrence wins),
// and nil/empty input returns nil (a ReplaceAliases clear).
//
// This is the single source of truth for the alias-kind derivation
// shared by reindex (vault frontmatter), ingest (plugin-emitted), and
// the #405 creation surfaces (canonical.MirrorAliases) so all three
// agree on alias typing (#445). EntityID is stamped on each row for
// callers that consume it; ReplaceAliases scopes by its own id arg.
func TypedAliasEntries(entityID string, aliases, canonicalEdgeTypes []string) []Alias {
	if len(aliases) == 0 {
		return nil
	}
	edgeSet := make(map[string]struct{}, len(canonicalEdgeTypes))
	for _, t := range canonicalEdgeTypes {
		edgeSet[t] = struct{}{}
	}
	seen := make(map[string]struct{}, len(aliases))
	out := make([]Alias, 0, len(aliases))
	for _, a := range aliases {
		if a == "" {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		kind := AliasKindBare
		if prefix, _, ok := TypedAliasPrefix(a); ok {
			if _, registered := edgeSet[prefix]; registered {
				kind = AliasKindTyped
			}
		}
		out = append(out, Alias{
			Alias:    a,
			EntityID: entityID,
			Kind:     kind,
		})
	}
	return out
}

// Store is the persistence interface used by the API handlers.
//
// Implementations must be safe for concurrent use. Methods take a context
// so cancellation propagates from the HTTP layer down into the driver.
//
// SaveEntity vs. UpsertEntity + AppendProvenance vs. ReplaceProvenance:
// - SaveEntity is "this is the current full state" — it upserts the
// entity row and replaces (wipe + reinsert) the entity's
// provenance entries. Used by tests / admin paths.
// - UpsertEntity upserts the entity row only and leaves existing
// provenance untouched. AppendProvenance adds new entries.
// Together they form the ingest semantic: re-ingest of the same
// URL updates `data` while accumulating one new provenance row
// per fetch attempt (ADR-0002 line 281).
// - ReplaceProvenance overwrites an entity's provenance with the
// given list (transactional DELETE + INSERTs). Used by reindex
// to re-derive DB provenance from vault frontmatter per ADR-0009.
// The vault list is canonical; ReplaceProvenance enforces the
// DB-mirrors-vault contract.
type Store interface {
	GetEntity(ctx context.Context, id string) (*Entity, error)
	GetEntities(ctx context.Context, ids []string) (matched []Entity, missing []string, err error)
	SaveEntity(ctx context.Context, e *Entity) error
	UpsertEntity(ctx context.Context, e *Entity) error
	AppendProvenance(ctx context.Context, entityID string, entries []ProvenanceEntry) error
	ReplaceProvenance(ctx context.Context, entityID string, entries []ProvenanceEntry) error

	// MarkGapCallDone stamps `gap_call_done_at = NOW()` on the
	// entity (per ADR-0013 §4 + §5). Idempotent — re-stamps with
	// current timestamp on subsequent calls. ErrNotFound returned
	// when the entity doesn't exist.
	MarkGapCallDone(ctx context.Context, entityID string) error
	// ClearGapCallDone resets the flag to NULL (per ADR-0013 §4):
	// called on `force_refetch=true` and TTL-driven refetch when
	// the upstream produces a fresh fetch. Idempotent — clearing
	// an already-NULL flag is a no-op success. ErrNotFound returned
	// when the entity doesn't exist.
	ClearGapCallDone(ctx context.Context, entityID string) error
	// IncDroppedCanonicalKind bumps the per-(plugin, kind) drop
	// counter (per ADR-0013 §3). Called by
	// the ingest orchestrator at the moment a plugin-emitted
	// canonical stub is filtered out by the operator's
	// `canonical_kinds:` config — the same site that fires the
	// existing startup WARN. Idempotent at the row level: the
	// first call inserts with count=1; subsequent calls increment
	// and refresh `last_seen_at`. `/v1/cv-status` reads
	// these via ListDroppedCanonicalKinds.
	IncDroppedCanonicalKind(ctx context.Context, plugin, kind string) error
	// IncDroppedCanonicalEdge is the edge-type counterpart —
	// bumps `(plugin, edge_type)` for canonical edge-type
	// emissions the operator's `canonical_edge_types:` config
	// didn't enable.
	IncDroppedCanonicalEdge(ctx context.Context, plugin, edgeType string) error
	// ListDroppedCanonicalKinds returns every (plugin, kind) row
	// in the counter table, ordered by (plugin, kind) for
	// deterministic output. Used by `/v1/cv-status` to
	// surface the drift signal — `would_materialize_count` per
	// row maps to the persisted Count.
	ListDroppedCanonicalKinds(ctx context.Context) ([]DroppedCanonicalKindCount, error)
	// ListDroppedCanonicalEdges is the edge-type counterpart.
	ListDroppedCanonicalEdges(ctx context.Context) ([]DroppedCanonicalEdgeCount, error)
	// ClearDroppedCanonicalKinds wipes the per-(plugin, kind) drop
	// counter table. Called by reindex.Run after a successful walk.
	// Reindex is the operator's "consume drift
	// signal" action, so post-reindex the drift surface zeroes and
	// any new drops from subsequent ingest accrue fresh under their
	// originating plugin's tag (preserving attribution that would
	// blur if we cleared at the start of reindex instead).
	ClearDroppedCanonicalKinds(ctx context.Context) error
	// ClearDroppedCanonicalEdges is the edge-type counterpart.
	ClearDroppedCanonicalEdges(ctx context.Context) error

	// ListGapCallableCandidates returns entities whose gap-call-done
	// flag is NULL (per ADR-0013 §4 + §6),
	// ordered by `id ASC` for deterministic pagination. afterID,
	// when non-empty, filters to ids strictly greater (cursor
	// resume). limit caps the returned slice. The "actually has
	// unfilled gaps" predicate is NOT enforced here — vault is the
	// canonical source for that and the handler vault-reads each
	// candidate. Provenance is NOT loaded (the caller drops it
	// before serializing).
	//
	// kind, when non-empty, filters candidates to that entity kind
	// (the indexed `entities.kind` column) per #385. The source
	// filter is NOT applied here — source lives in the vault
	// frontmatter, not a DB column, so the handler filters it on the
	// vault entity it already reads.
	ListGapCallableCandidates(ctx context.Context, afterID string, limit int, kind string) ([]Entity, error)
	// ListGapCallableUnfilledCandidates is the gap-state-aware variant of
	// ListGapCallableCandidates — same predicate as CountGapCallableCandidates
	// (at least one unfilled, non-deferred gap). Used where the listed set
	// must agree with the count (#439).
	ListGapCallableUnfilledCandidates(ctx context.Context, afterID string, limit int, kind string) ([]Entity, error)
	// ListGapCallableSurfaceableCandidates is the needs-fill page query
	// (#523): it drops rows that can never surface — pure-pointer stubs
	// (nil-data rows, `data` = JSON "null", no vault file) and all-filled
	// rows — so they don't consume the handler's per-request scan bound
	// before the real candidates. It KEEPS NULL-gap_state rows (config-gap
	// entities the list still surfaces but the count omits), so it is the
	// superset the page wants: callable AND not-a-stub AND (no-gap_state OR
	// has-unfilled-gap).
	ListGapCallableSurfaceableCandidates(ctx context.Context, afterID string, limit int, kind string) ([]Entity, error)
	// CountGapCallableCandidates returns the count of entities the
	// `/v1/needs-fill` listing would surface — the queue-depth
	// surface for the response's `total` field per #338 with the
	// gap-state-aware filter from #350. Predicate:
	//
	//   gap_call_done_at IS NULL                     (callable)
	//   AND gap_state IS NOT NULL AND length(gap_state) > 2
	//                                                (gap_state non-empty)
	//   AND ∃ entry ∈ gap_state. filled_at IS NULL
	//                            AND COALESCE(deferred, 0) = 0
	//                                                (actually unfilled)
	//
	// The JSON1 EXISTS filter walks the gap_state map and counts the
	// entity only when at least one entry is genuinely unfilled (no
	// filled_at stamp, not deferred). Entities whose gap_state was
	// populated by `add_gap` but later fully filled / deferred don't
	// inflate the queue-depth report. Pre-#338 the predicate over-
	// counted by ~99% on real data; the staging instance reporting
	// motivated this fix (#350).
	//
	// Caveats still in effect: pure-pointer canonical rows have no
	// vault file; auth-filtered entries with all gaps gated by the
	// caller's role would still inflate by the gap_state-filled-but-
	// auth-hidden count. Both surfaced as docstring caveats on the
	// `total` field. kind, when non-empty, scopes the count to that
	// entity kind per #385 so `total` matches the kind-filtered
	// listing. The source filter is vault-side and intentionally NOT
	// reflected in this count (an exact source count would require
	// vault-reading every candidate); under a source filter `total`
	// is the kind-filtered anchor.
	CountGapCallableCandidates(ctx context.Context, kind string) (int, error)
	GetEdgesFor(ctx context.Context, entityID string, types []string) ([]Edge, error)
	// GetEdgesTo is the inbound mirror of GetEdgesFor — edges whose
	// to_id matches the supplied id. The new
	// GET /v1/edges?direction=in path reads through this helper.
	GetEdgesTo(ctx context.Context, entityID string, types []string) ([]Edge, error)
	// GetEdgesForMany is GetEdgesFor over a frontier of source ids in
	// a single SQL query. Used by the BFS context traversal so a
	// depth-3 walk doesn't fan out into
	// N round-trips on each frontier. Empty fromIDs → empty result
	// (not an error). Empty types → no type filter.
	GetEdgesForMany(ctx context.Context, fromIDs []string, types []string) ([]Edge, error)
	CreateEdge(ctx context.Context, e *Edge) error

	// UpdateEdgeTarget atomically rewrites an edge's target per
	// #304 Cut B. Deletes (fromID, edgeType, oldTargetID) and
	// creates (fromID, edgeType, newTargetID) in one transaction,
	// preserving created_at on the new row so the audit trail
	// shows "this edge existed since T, target finalized at T'"
	// rather than a delete+create with two separate timestamps.
	// updated_at advances to the rewrite moment.
	//
	// Stale-safety per the v1 cut framing: returns ErrEdgeStale
	// (handlers → 409) when current state doesn't permit the
	// rewrite. Two cases — both surface as ErrEdgeStale because
	// both signal "your view of the edge graph is stale":
	//
	//   - Old tuple missing: (fromID, edgeType, oldTargetID)
	//     doesn't match a current row.
	//   - New tuple already exists: (fromID, edgeType,
	//     newTargetID) is already a current row — rewriting
	//     onto it would silently merge two distinct edges.
	//
	// Concurrent rewrites collapse cleanly: the loser sees
	// ErrEdgeStale on one of the two checks and re-reads state.
	//
	// newTargetID must reference an existing entity; absent →
	// ErrMissingEntity (handlers → 422). The metadata + edge type
	// on the new row mirror the old row (same payload, swap
	// target). A no-op rewrite (newTargetID == oldTargetID)
	// rejects with a plain error — the routing layer should
	// short-circuit before calling.
	UpdateEdgeTarget(ctx context.Context, fromID, edgeType, oldTargetID, newTargetID string) (*Edge, error)

	// DeleteEdgesByTypeFrom removes every edge of the given type
	// originating at fromID. Used by the canonical_type fill path
	// to implement idempotent re-fill semantics:
	// before creating new edges from a re-filled canonical_type
	// gap, the prior fill's edges are deleted so the post-fill
	// edge set is exactly the new fill's labels. Returns the
	// number of rows removed (purely informational).
	DeleteEdgesByTypeFrom(ctx context.Context, fromID, edgeType string) (int64, error)

	// GetContextNeighbors walks outbound edges from rootID up to
	// maxDepth hops in BFS order. The
	// returned root is the canonical store.Entity for rootID;
	// neighbors are flattened (depth-major; arbitrary within a
	// depth) and capped at maxResults — when capped, truncated is
	// true and the prefix that fit is returned.
	//
	// edgeTypes filters traversal: only edges of the named types are
	// walked AND only those edges appear in neighbors. Empty
	// edgeTypes → walk all edge types.
	//
	// Cycle handling: each entity appears at most once across root +
	// neighbors. Back-edges that would re-introduce an already-
	// visited entity are dropped — the entity is already present
	// under its earlier-depth visit.
	//
	// Returns ErrNotFound (wrapped) when rootID resolves to nothing.
	GetContextNeighbors(ctx context.Context, rootID string, maxDepth int, edgeTypes []string, maxResults int) (root *Entity, neighbors []ContextNeighbor, truncated bool, err error)
	// tags AND-filters the result set per #453: each entry requires
	// the entity's `data.tags` JSON array to contain that value, so a
	// multi-element tags slice is an intersection (every tag must be
	// present). Empty-string entries are skipped; a nil/empty slice
	// applies no tag predicate.
	Search(ctx context.Context, q, kind string, limit, offset int, archived ArchivedFilter, journalOnly bool, tags ...string) (results []Hit, total int, err error)

	// ArchiveEntity flips an entity into the archived state per
	// ADR-0018: sets `archived_at = now` and bumps `updated_at`.
	// Idempotent on already-archived rows (no-op when archived_at
	// is already non-NULL — same row stays at the original archive
	// timestamp). Returns ErrNotFound when no entity with the given
	// id exists.
	ArchiveEntity(ctx context.Context, id string) error

	// RestoreEntity is the inverse: clears `archived_at = NULL` and
	// bumps `updated_at`. Idempotent on already-active rows.
	// Returns ErrNotFound when no entity with the given id exists.
	RestoreEntity(ctx context.Context, id string) error

	// Reindex bookkeeping (per ADR-0008 /). Operator-only — the
	// reindexer (CLI + POST /v1/reindex) uses these to compare vault
	// state against last-walk state. ListReindexFiles returns every
	// row; UpsertReindexFile is idempotent on Path; DeleteReindexFile
	// returns true if a row was dropped, false if absent.
	// DeleteEntityCascade removes an entity along with its inbound +
	// outbound edges and provenance — used when a vault file's
	// disappearance prompts an entity removal in incremental mode.
	// WipeDerivedState is the --full reset: drops every entity, edge,
	// provenance row, and reindex_files row in a single transaction.
	// (Plugin capabilities cache, fill tokens, and schema_migrations
	// are preserved — those aren't derived from the vault.)
	//
	// LastReindexAt returns the most recent reindex timestamp —
	// `MAX(last_indexed_at)` across the reindex_files table. The
	// second return is `false` when no reindex has ever run (the
	// table is empty). Used by `/v1/cv-status` per ADR-0013 §3
	// to surface "when was the last full
	// re-derive?" alongside the drift counters.
	LastReindexAt(ctx context.Context) (time.Time, bool, error)
	ListReindexFiles(ctx context.Context) ([]ReindexFile, error)
	UpsertReindexFile(ctx context.Context, f ReindexFile) error
	DeleteReindexFile(ctx context.Context, path string) (bool, error)
	DeleteEntityCascade(ctx context.Context, id string) error
	// RenameEntity re-keys an entity from oldID to newID in a single
	// transaction: it inserts the new entities row (carrying over
	// kind / created_at / gap state / archived state from the old row,
	// with newData as the new payload), re-points every table that
	// references the old id — edges (from_id + to_id), provenance
	// (entity + edge-endpoint rows), entity_notations, and
	// entity_aliases — guarantees the bare old slug resolves to
	// newID via an alias row, then deletes the old entities row. The
	// derived view stays whole (inbound/outbound edges + provenance
	// survive, re-pointed); the old `<kind>:<old-slug>` reference still
	// resolves through the alias resolver. There is no in-place id
	// rename in SQLite (no ON UPDATE CASCADE on the FKs), so this is the
	// supported path for changing an entity's id.
	RenameEntity(ctx context.Context, oldID, newID string, newData map[string]any) error
	WipeDerivedState(ctx context.Context) error

	// Notation lookup. entity_notations is
	// the input-form → entity-slug index used by the lookup-first
	// ingest path and reindex. Methods are operator-
	// adjacent — invoked from the ingest handler and the reindex
	// helper, not from agent-facing surfaces.
	//
	// - GetNotation: one-shot lookup; returns ErrNotFound if absent.
	// - UpsertNotation: insert-or-replace. Same notation string is
	// allowed to point at a different entity_id only via explicit
	// overwrite — the PRIMARY KEY on notation enforces the
	// uniqueness; this method is the supported path for moving
	// a notation between entities.
	// - DeleteNotationsForEntity: drop every row for one entity.
	// Used by reindex re-derive (DELETE-then-INSERT). Cascade
	// from entity deletion is handled at the schema layer.
	// - ReplaceNotations: transactional DELETE+INSERTs for one
	// entity, mirroring ReplaceProvenance's contract (per ADR-
	// 0009 re-derive pattern). Empty entries permitted — clears
	// the entity's notations.
	GetNotation(ctx context.Context, notation string) (Notation, error)
	UpsertNotation(ctx context.Context, n Notation) error
	DeleteNotationsForEntity(ctx context.Context, entityID string) (int, error)
	ReplaceNotations(ctx context.Context, entityID string, entries []Notation) error

	// Alias lookup per #3. entity_aliases is the alternative-label
	// index used by /v1/search (LEFT JOIN at query time so a
	// `q` LIKE-matches alias text alongside id + data) and
	// reindex re-derive. Methods mirror the notation surface
	// — same DELETE-then-INSERT pattern under ReplaceAliases;
	// same FK ON DELETE CASCADE.
	//
	//   - ListAliasesForEntity returns every alias pointing at
	//     a given entity (ordered by alias for stable output).
	//   - ReplaceAliases transactionally wipes + rewrites the
	//     entity's alias rows. Empty entries permitted — clears
	//     the entity's aliases. Called by the ingest path after
	//     vault write + by reindex re-derive.
	ListAliasesForEntity(ctx context.Context, entityID string) ([]Alias, error)
	ReplaceAliases(ctx context.Context, entityID string, entries []Alias) error
	// ResolveAlias reverse-looks-up an exact alias to its entity id,
	// optionally scoped to a kind (#392). "" when no match.
	ResolveAlias(ctx context.Context, alias, kind string) (string, error)

	// Plugin capabilities cache . Operator-only — these
	// methods aren't reachable from the agent-facing /v1 API surface;
	// they're called by the server-startup registration path
	// (cmd/yaad-index/main.go) and the `yaad-index plugins clear-cache`
	// CLI subcommand. Version-driven invalidation: a freshly probed
	// version mismatch on registration triggers an Upsert; the CLI
	// path force-drops the row regardless.
	GetPluginCapabilities(ctx context.Context, name string) (CachedPluginCapabilities, bool, error)
	UpsertPluginCapabilities(ctx context.Context, name, version string, capabilitiesJSON []byte) error
	DeletePluginCapabilities(ctx context.Context, name string) (bool, error)
	ClearAllPluginCapabilities(ctx context.Context) (int, error)

	Close() error
}

// ErrNotFound is returned by GetEntity when no row matches the requested id.
var ErrNotFound = errors.New("not found")

// ErrMissingEntity is returned by CreateEdge when from or to references an
// entity id that doesn't exist in the entities table. Handlers use
// errors.Is to detect this and translate it into the canonical
// 422 missing_entity envelope per ADR-0002 (POST /v1/edges; RFC 9110
// §15.5.21 unprocessable content — request well-formed, can't be
// processed because of a referential-integrity gap).
var ErrMissingEntity = errors.New("missing entity")

// ErrAliasConflict is returned by RenameEntity when the renamed entity's
// bare old slug is already a live alias owned by a DIFFERENT same-kind
// entity. Completing the rename would delete the old row and leave the
// old `<kind>:<old-slug>` reference falling through the kind-scoped
// resolver to that foreign entity. Handlers translate it into a 409.
var ErrAliasConflict = errors.New("alias conflict")

// ErrEdgeStale is returned by UpdateEdgeTarget when current state
// doesn't permit the requested rewrite per #304 Cut B. Two failure
// modes both map to this sentinel + the same 409 wire shape:
//
//   - **Old tuple missing.** (fromID, edgeType, oldTargetID)
//     doesn't match a current row — the edge was already deleted,
//     already resolved to a different target, or never existed.
//   - **New tuple already exists.** (fromID, edgeType,
//     newTargetID) is already a current row — rewriting onto it
//     would silently merge two distinct edges (and the existing
//     target's metadata + created_at would diverge from what the
//     caller expects).
//
// Both surface "current state has moved on; the caller's view of
// the edge graph is stale". Handlers map to 409 so callers
// re-read state + retry with a fresh tuple.
var ErrEdgeStale = errors.New("edge tuple does not match current state")

// ErrNotImplemented is returned by stub methods until a follow-up PR
// implements them. Tests in this package assert against it to lock the
// staged-rollout contract.
var ErrNotImplemented = errors.New("not implemented")
