// Shared types matching yaad-index's response shapes (per ADR-0002 +
// ADR-0008). These are intentionally narrow — only the fields the
// MCP surface actually returns to the agent. yaad-index's full
// response carries more (raw HTTP response metadata, tracker IDs,
// etc.); we project to what the agent needs.

/**
 * Entity is the wire shape returned by yaad-index's
 * `GET /v1/entities/{id}` (per `internal/api/entities.go::toAPIEntity`):
 * `{id, kind, data, provenance, edges}`. The fetch is one-hop — the
 * server emits no `canonical_entities[]` field; the agent discovers
 * canonical-shape entities by walking edges (typically `is_about`)
 * and calling `get_entity` on the target ids.
 *
 * `edges` is populated only when the request passes `?with_edges=`
 * — yaad-mcp's `getEntity` always passes it (the source issue).
 *
 * `gaps` and `raw_content` ride on the `IngestResponse` envelope
 * (`gaps` and `clean_content` fields), not on the entity itself —
 * agents read them off the ingest response, not via `get_entity`.
 */
export interface Entity {
 id: string;
 kind: string;
 data?: Record<string, unknown>;
 edges?: EdgeRef[];
 provenance?: ProvenanceEntry[];
}

export interface EdgeRef {
 type: string;
 to: string;
}

export interface ProvenanceEntry {
 source: string;
 fetched_at?: string;
 filled_at?: string;
 ok: boolean;
 error?: string;
 error_message?: string;
}

/**
 * IngestState mirrors yaad-index's `state` enum on the ingest
 * response. The wire field is also called `status` for legacy
 * client-compat (same value); we surface only `state` to the agent
 * since `status` is just a duplicate.
 */
export type IngestState =
 | "complete"
 | "needs_fill"
 | "disambiguation"
 | "queued";

export interface IngestResponse {
 state: IngestState;
 status?: IngestState; // legacy alias on yaad-index — keep for parsing tolerance
 entity?: Entity;
 /** Disambiguation candidates per ADR-0006. Keyed by the plugin's
 * canonical id; agent picks one and re-invokes ingest with the
 * `<plugin>: <id>` shorthand input form. */
 options?: Record<string, DisambiguationOption>;
 /** When `state === "queued"`: the entity id the orchestrator predicts
 * the fetch will resolve to (best-effort; can be empty). */
 estimated_entity_id?: string;
 /** When `state === "needs_fill"`: the article body the agent reads
 * to derive gap values. */
 clean_content?: string;
 clean_content_truncated?: boolean;
 gaps?: Record<string, string>;
}

export interface DisambiguationOption {
 label: string;
 summary?: string;
}

/**
 * CommentEntry mirrors yaad-index's wire shape for one comment on
 * `POST /v1/entities/{id}/comments` (per yaad-index a prior PR /).
 *
 * - `date` — RFC3339 UTC, server-stamped (clients never send it).
 * - `text` — the comment body (server-trims whitespace).
 * - `author` — the agent identity. Server stamps from JWT `sub` when
 * the request omits the field; an explicit non-matching value is
 * rejected upstream with 403 author_mismatch.
 * - `operator` — the human resource owner from the pair-claim,
 * stamped server-side from JWT `operator`. Empty on legacy comments
 * (legacy vault entries).
 */
export interface CommentEntry {
 date: string;
 text: string;
 author?: string;
 operator?: string;
}

/**
 * CommentsResponse mirrors `POST /v1/entities/{id}/comments` 201 envelope:
 * the just-appended comment plus the freshly merged entity, so the
 * caller can refresh local state without a follow-up GET. Per yaad-index
 *.
 */
export interface CommentsResponse {
 ok: boolean;
 comment: CommentEntry;
 entity: Entity;
}

/**
 * UpstreamErrorEnvelope is the canonical 4xx/5xx shape emitted by
 * yaad-index per ADR-0002 (`{ok: false, error: "...", message: "..."}`).
 * Surfaced verbatim by `add_comment` and `edit_user_content_section`
 * so agents observe the structured error code (`missing_authorization`,
 * `author_mismatch`, `precondition_failed`, `precondition_required`, …)
 * directly — without parsing it out of an exception message.
 *
 * `current_etag` is populated by yaad-mcp ONLY on the 412
 * precondition_failed path: the server includes the current ETag in
 * the response header so the agent can refetch + retry without an
 * extra GET — yaad-mcp lifts that header onto the envelope so agents
 * don't have to inspect HTTP headers. Absent on every other error
 * shape.
 */
export interface UpstreamErrorEnvelope {
 ok: false;
 error: string;
 message: string;
 current_etag?: string;
}

export interface FillResponse {
 ok: boolean;
 entity: Entity;
 /** Remaining unfilled gap field names. Empty array (NOT null) when
 * this fill closed every open gap — yaad-index emits `[]` for
 * encoding clarity. */
 gaps: string[];
}

/**
 * ListEntitiesResponse mirrors yaad-index's `/v1/search` response
 * (per `internal/api/search.go`) — that endpoint is what powers
 * BOTH the `list_entities` tool (kind-filtered listing, no query)
 * and the `search_local` tool (text query, optional kind filter).
 * The shape is search-style (id/snippet/score), not full entities;
 * agents fetch full state via `get_entity(id)` for any returned id.
 *
 * yaad-index has no plain `GET /v1/entities` list-all route; the
 * search endpoint requires either a `q=` or `kind=` parameter, so
 * `list_entities` mandates `kind`.
 */
export interface ListEntitiesResponse {
 results: SearchResultEntry[];
 total: number;
 limit: number;
 offset: number;
}

/**
 * SearchLocalResponse is the same wire shape as ListEntitiesResponse
 * — both call the same `/v1/search` endpoint. Aliased for clarity at
 * the tool boundary so the agent-facing type names match the tool
 * names; future shape divergence (per yaad-index's later PRs)
 * lands as separate types.
 */
export type SearchLocalResponse = ListEntitiesResponse;

export interface SearchResultEntry {
 id: string;
 kind: string;
 snippet: string;
 score: number;
}

/**
 * SearchUpstreamRequest is the POST /v1/search/upstream wire body
 * per yaad-index #2 — federated search across plugins that opted
 * in via Capabilities.SupportsSearch=true.
 *
 * `plugins` empty / omitted → federate to every opted-in plugin in
 * registry order. Explicit list selects exactly the named plugins;
 * an explicit name that isn't registered → 400, an explicit name
 * whose plugin SupportsSearch=false → 422.
 */
export interface SearchUpstreamRequest {
 query: string;
 plugins?: string[];
 limit?: number;
 per_plugin_timeout_seconds?: number;
}

/**
 * SearchUpstreamCandidate is one merged result on the federated
 * response. Carries plugin attribution alongside the plugin-emitted
 * SearchCandidate fields.
 */
export interface SearchUpstreamCandidate {
 plugin: string;
 id: string;
 label: string;
 summary?: string;
}

/**
 * SearchUpstreamPluginStatus surfaces per-plugin outcome so the
 * caller sees exactly which plugins returned vs timed out vs
 * errored. Partial-results semantic — the federated call returns
 * 200 even when individual plugins fail; per-plugin error_message
 * carries the reason.
 */
export interface SearchUpstreamPluginStatus {
 plugin: string;
 ok: boolean;
 candidates: number;
 duration_ms: number;
 error_message?: string;
}

export interface SearchUpstreamResponse {
 ok: boolean;
 results: SearchUpstreamCandidate[];
 per_plugin_status: SearchUpstreamPluginStatus[];
 query: string;
 limit: number;
 per_plugin_timeout_seconds: number;
}

/**
 * ReindexResponse mirrors yaad-index's `reindex.Summary`
 * (`internal/reindex/reindex.go`) — returned by POST /v1/reindex.
 * The daemon walks the markdown vault, re-parses files, and updates
 * the derived index (entities + edges from frontmatter). The summary
 * is the count block agents read after the call completes; yaad-mcp
 * adds no client-side polling or progress reporting (the request
 * blocks until the walk finishes).
 *
 * `mode` echoes the requested mode ("incremental" by default;
 * "full" forces re-parse of every file). `errors` is omitted when
 * the walk completed cleanly; populated with non-fatal parse / store
 * errors when the walk continued past an issue.
 */
export interface ReindexResponse {
 mode: "incremental" | "full";
 scanned: number;
 skipped: number;
 parsed: number;
 entities_created: number;
 entities_updated: number;
 entities_deleted: number;
 edge_rows_written: number;
 errors?: string[];
 started_at: string;
 finished_at: string;
 duration_ms: number;
}

/**
 * EntitiesBatchResponse mirrors yaad-index's
 * `internal/api/entities_batch.go::batchResponse` — returned by
 * `POST /v1/entities/batch`. Splits matched ids into `entities`
 * (with the same per-entity wire shape as `GET /v1/entities/{id}`)
 * and unmatched ids into `missing` (string ids that no entity row
 * exists for).
 *
 * Per ADR-0002 §"in-flight rule": placeholder entities created by
 * an in-flight ingest land in `entities` (not `missing`), with
 * sparse `data`. `missing` is reserved for ids the server has
 * never seen at all.
 *
 * `entities[i].edges` is empty in the v1 endpoint regardless of
 * the `with_edges` request param — the daemon accepts the param
 * for forward compat but doesn't yet expand inline edges on the
 * batch surface (see `entities_batch.go` comment on the request
 * shape: real expansion lands with the edge-side cutover).
 */
export interface EntitiesBatchResponse {
 ok: boolean;
 entities: Entity[];
 missing: string[];
}

/**
 * EntityKindEntry mirrors yaad-index's per-entity-kind row in
 * `GET /v1/kinds` (`internal/api/kinds.go::entityKind`). Carries the
 * plugin-declared kind name, description, and the union of plugin
 * names that emit it (`source_plugins` is sorted alphabetically by
 * the daemon for deterministic output).
 */
export interface EntityKindEntry {
 name: string;
 description: string;
 source_plugins: string[];
}

/**
 * EdgeKindEntry mirrors `internal/api/kinds.go::edgeKind` — the
 * plugin-declared edge-kind row. Same shape as `EntityKindEntry`
 * plus `from_kind` / `to_kind` declaring the typed-relationship
 * endpoints. When two plugins disagree on description / from /
 * to for the same edge name, the first-loaded plugin's metadata
 * wins (deterministic but non-merging — operator config issue
 * to surface).
 */
export interface EdgeKindEntry {
 name: string;
 description: string;
 from_kind: string;
 to_kind: string;
 source_plugins: string[];
}

/**
 * KindsResponse mirrors `GET /v1/kinds` (per ADR-0002 §"GET /v1/kinds"
 * + yaad-mcp). Plugin-emitted source-kinds registry — distinct
 * from `StructureResponse.kinds` which is the canonical-shape
 * registry post-ADR-0016 four-layer merge.
 */
export interface KindsResponse {
 ok: boolean;
 entity_kinds: EntityKindEntry[];
 edge_kinds: EdgeKindEntry[];
}

/**
 * PluginKindEntry mirrors yaad-index's per-plugin entity-kind row on
 * `GET /v1/plugins` (`internal/api/plugins.go::pluginKindEntry`).
 * Scoped to one plugin, so there's no `source_plugins` cross-ref —
 * the plugin's identity is the enclosing PluginEntry.
 */
export interface PluginKindEntry {
 name: string;
 description?: string;
}

/**
 * PluginEdgeEntry mirrors `internal/api/plugins.go::pluginEdgeEntry` —
 * the per-plugin edge-kind row with from_kind / to_kind endpoints.
 */
export interface PluginEdgeEntry {
 name: string;
 description?: string;
 from_kind?: string;
 to_kind?: string;
}

/**
 * PluginEntry mirrors `internal/api/plugins.go::pluginEntry` — the
 * per-plugin slice of Capabilities surfaced on `GET /v1/plugins`:
 * `name + version + url_patterns + commands + entity_kinds +
 * edge_kinds + source_namespace`. Inverse of `EntityKindEntry`:
 * KindsResponse is "kind → plugins"; PluginEntry is "plugin → kinds
 * + everything else."
 *
 * Per yaad-index #13, `url_patterns` and `commands` always marshal
 * as `[]` (not absent) so the consumer doesn't have to nil-guard.
 */
export interface PluginEntry {
 name: string;
 version?: string;
 url_patterns: string[];
 commands: string[];
 entity_kinds: PluginKindEntry[];
 edge_kinds: PluginEdgeEntry[];
 source_namespace?: string;
}

/**
 * PluginsResponse mirrors `GET /v1/plugins` (per yaad-index #13).
 * Plugins are listed in registry order (matching the first-match-
 * wins dispatch order /v1/ingest walks); within each plugin,
 * `entity_kinds` + `edge_kinds` sort alphabetically by name.
 */
export interface PluginsResponse {
 ok: boolean;
 plugins: PluginEntry[];
}

/**
 * EntityContextEdge is the full edge triple {type, from, to} carried
 * by every neighbor entry in a context-stitch response. Distinct
 * from EdgeRef (the inline single-hop body shape) because at depth
 * ≥ 2 the source `from` is one of the previously visited neighbors,
 * not the root.
 */
export interface EntityContextEdge {
 type: string;
 from: string;
 to: string;
}

/**
 * EntityContextNeighbor is one entry in the flattened neighbors
 * list returned by `GET /v1/entities/{id}/context` (per yaad-index
 * the source issue). Carries the edge that introduced the neighbor, the
 * neighbor entity itself, and the BFS depth at which it was first
 * reached.
 */
export interface EntityContextNeighbor {
 edge: EntityContextEdge;
 entity: Entity;
 depth: number;
}

/**
 * EntityContextResponse mirrors the yaad-index endpoint shape
 * verbatim (`internal/api/entities_context.go::contextResponse`).
 * `truncated: true` means the result was clipped at `max_results`
 * — callers decide whether to re-call with a narrower
 * `edge_types` filter or accept the partial.
 */
export interface EntityContextResponse {
 root: Entity;
 neighbors: EntityContextNeighbor[];
 truncated: boolean;
}

/**
 * CVDriftKindRow is one entry in `/v1/cv-status` drift sections
 * keyed by canonical kind (per yaad-index ADR-0013 §3, a prior PR/177).
 * `would_materialize_count` is the cumulative count of canonical
 * entity stubs the plugin emitted but the operator's config-filter
 * dropped at ingest time. Persisted in DB so it survives daemon
 * restart.
 */
export interface CVDriftKindRow {
 plugin: string;
 kind: string;
 would_materialize_count: number;
}

/**
 * CVDriftEdgeRow is the canonical-edge counterpart of
 * CVDriftKindRow — same shape, but keyed by edge_type for
 * canonical edge-type emissions the operator's
 * `canonical_edge_types:` config didn't enable.
 */
export interface CVDriftEdgeRow {
 plugin: string;
 edge_type: string;
 would_materialize_count: number;
}

/**
 * CVDrift carries the four drift sections per ADR-0013 §3.
 * `*_enabled_not_emitted` arrays are stubbed empty in v1 of the
 * yaad-index endpoint — that's a different signal that lands
 * later (a plugin that USED to emit kind X but no longer does).
 */
export interface CVDrift {
 kinds_emitted_not_enabled: CVDriftKindRow[];
 kinds_enabled_not_emitted: CVDriftKindRow[];
 edge_types_emitted_not_enabled: CVDriftEdgeRow[];
 edge_types_enabled_not_emitted: CVDriftEdgeRow[];
}

/**
 * CVStatusResponse mirrors `GET /v1/cv-status` verbatim per
 * yaad-index ADR-0013 §3 (a prior PR/177).
 *
 * - `config_hash` is a deterministic SHA-256 truncated to 16 hex
 * chars over the canonical-vocabulary subset of the config
 * (canonical_kinds + canonical_edge_types). Distinct from
 * `/v1/structure`'s `version` (which covers the full structural
 * signature including plugins). Edge-types sorted before
 * hashing — same contract as `/v1/structure` per yaad-index
 * a prior PR fold-in.
 * - `last_reindex_at` is RFC-3339 UTC when reindex has run,
 * `null` when never reindexed.
 * - `reindex_hint` is a static guidance string per the spec.
 */
export interface CVStatusResponse {
 ok: boolean;
 config_hash: string;
 drift: CVDrift;
 last_reindex_at: string | null;
 reindex_hint: string;
}

/**
 * StructureKind is one entry in the `/v1/structure` `kinds` map
 * (per yaad-index ADR-0013 §7 / yaad-mcp). `is_canonical` is
 * locked to true in v1 — reserved for future "passthrough" kinds.
 * `instruction` carries the operator's per-kind override verbatim
 * (omitted on the wire when unset; absent here).
 */
export interface StructureKind {
 is_canonical: boolean;
 gaps: Record<string, string>;
 instruction?: string;
}

/**
 * StructurePlugin is one loaded plugin's metadata snapshot — name +
 * version + url_patterns + supports_search + emits_kinds + emits_edges.
 * The yaad-index server caches each plugin's `--init` capabilities
 * at startup, so the snapshot is stable across calls within a single
 * server lifetime.
 */
export interface StructurePlugin {
 name: string;
 version: string;
 url_patterns: string[];
 supports_search: boolean;
 emits_kinds: string[];
 emits_edges: string[];
}

/**
 * CanonicalKindRegistryEntry is one entry of the
 * `canonical_vocabulary` map surfaced on `needs_fill` /
 * `/v1/needs-fill` responses. Mirrors yaad-index
 * `config.CanonicalKindConfig` verbatim — `{gaps,
 * instruction?}`. Distinct from StructureKind (which adds
 * `is_canonical: true` at the /v1/structure layer); the
 * needs_fill envelope surfaces the operator's CV registry
 * directly without the structure-layer wrapper.
 */
export interface CanonicalKindRegistryEntry {
 gaps: Record<string, string>;
 instruction?: string;
}

/**
 * GapMetadataEntry carries per-gap structured metadata that doesn't
 * fit the plain `{<field>: <prompt-string>}` shape of `gaps`. Today
 * only `kinds` is populated, surfacing the canonical-kind allowlist
 * for `canonical_type` gaps (per ADR-0019 + yaad-index /) so
 * agents can render the allowlist at fill-prompt time.
 *
 * - `kinds` — present on `canonical_type` gaps. The operator-declared
 * `Kinds` array on the gap spec — e.g. `["person", "boardgame"]`
 * constrains canonical-edge targets to those kinds; the wildcard
 * `["*"]` accepts any kind in the operator's `canonical_kinds:`
 * registry. Absent on plain scalar/list gaps (where there is no
 * canonical-kind constraint).
 *
 * Forward-compat shape: future gap families may add fields here.
 * Decoders should ignore unknown keys.
 */
export interface GapMetadataEntry {
 kinds?: string[];
}

/**
 * NeedsFillEntry is one entry on the `entities` array of the
 * `GET /v1/needs-fill` response (per yaad-index ADR-0013 §6,
 * a prior PR/172). Mirrors yaad-index `internal/api/needs_fill.go`'s
 * `needsFillEntry` shape verbatim:
 *
 * - `id` / `kind` — the entity being surfaced.
 * - `gaps` — `{<field>: <AI-prompt>}` map of unfilled gaps.
 * Cache-hit entries surface empty-string prompts (the prompt-
 * unavailable sentinel) since gap-call prompts originate from
 * the plugin and aren't stored in the vault.
 * - `gap_metadata` — `{<field>: GapMetadataEntry}` map carrying
 * per-gap structured metadata. Present on `canonical_type` gaps
 * (`{kinds: ["person", "boardgame"]}` or wildcard `["*"]`);
 * absent on plain scalar gaps. Decoders should treat the field
 * as opt-in: legacy `needs_fill` entries omit it entirely.
 * - `clean_content` — the article body (always present on the
 * wire; matches the cache-hit ingest envelope shape).
 * - `clean_content_truncated` — boolean parity field; v1 always
 * false in this surface (truncation context lives plugin-side).
 * - `instruction` — operator's resolved instruction
 * (per-kind override → global → omit per ADR-0013 §2).
 * - `canonical_vocabulary` — operator's CV registry verbatim;
 * omitted when registry is empty/nil.
 */
export interface NeedsFillEntry {
 id: string;
 kind: string;
 gaps: Record<string, string>;
 gap_metadata?: Record<string, GapMetadataEntry>;
 clean_content: string;
 clean_content_truncated: boolean;
 instruction?: string;
 canonical_vocabulary?: Record<string, CanonicalKindRegistryEntry>;
}

/**
 * NeedsFillResponse mirrors `GET /v1/needs-fill` verbatim per
 * yaad-index a prior PR/172. `next_cursor` is omitted (absent on the
 * wire) when the candidate stream is exhausted; present as an
 * opaque base64(last-seen-id) string when more pages are
 * available. The agent passes the cursor back as-is on the
 * subsequent call.
 */
export interface NeedsFillResponse {
 ok: boolean;
 entities: NeedsFillEntry[];
 next_cursor?: string;
}

/**
 * UserContentCreateRequest is the POST /v1/user-content body
 * (per yaad-index PR-C / yaad-mcp).
 *
 * - `title` — slugified server-side to derive `id =
 * "user-content:" + slug`. Empty / whitespace-only / unslugifiable
 * titles return 400 invalid_argument.
 * - `tags` — non-empty list per ADR-0012.
 * - `body` — markdown body. Empty body is allowed (the agent can
 * fill content via edit_user_content_section later).
 * - `data` — optional frontmatter map. When the operator's
 * `user_content_frontmatter_edges:` config declares mappings,
 * fields named in those mappings produce canonical-label edges
 * per yaad-index. UGC is operator-authored so both object
 * form (`{name, kind}` or list-of) AND pre-formed
 * `<kind>:<slug>` strings (or list-of) are accepted. Other
 * fields land verbatim under `vault.Entity.Data` without edge
 * derivation.
 */
export interface UserContentCreateRequest {
 title: string;
 tags: string[];
 body: string;
 data?: Record<string, unknown>;
}

/**
 * UserContentDeleteResponse mirrors the DELETE /v1/user-content/{id}
 * 200 envelope (per yaad-index PR-C). The server returns JSON
 * (not 204) so clients can branch uniformly on `ok` across the
 * surface.
 */
export interface UserContentDeleteResponse {
 ok: boolean;
 id: string;
 deleted: boolean;
}

/**
 * EntityDeleteResponse mirrors the DELETE /v1/entities/{id} 200
 * envelope (per yaad-index / a prior PR). Identical shape to
 * UserContentDeleteResponse — kept as a separate interface so a
 * future divergence (e.g. attachments cleanup count) doesn't have to
 * mutate the UGC type.
 */
export interface EntityDeleteResponse {
 ok: boolean;
 id: string;
 deleted: boolean;
}

/**
 * EntityArchiveResponse mirrors POST /v1/entities/{id}/archive and
 * POST /v1/entities/{id}/restore (per yaad-index / ADR-0018
 * step 2). `archived` echoes the new state — `true` after archive,
 * `false` after restore. Same shape for both endpoints so the MCP
 * client + tool layer can share types.
 */
export interface EntityArchiveResponse {
 ok: boolean;
 id: string;
 archived: boolean;
}

/**
 * UserContentSection is one parsed unit of a UGC entity body per
 * yaad-index PR-A's containment model (ADR-0012). Every ATX
 * heading (`#` … `######`) is one addressable section in a flat
 * list; deeper headings are TEXTUALLY INCLUDED in the parent's body
 * so the granularity choice IS the section choice.
 *
 * - `index` — 0-based positional address. Always usable as the
 * `{sec}` URL parameter (the disambiguating fallback when two
 * headings slugify identically).
 * - `depth` — 0 for pre-heading body; 1..6 for `#`..`######`.
 * - `heading` — heading text minus leading `#+ ` and trailing
 * closing-`#` sequences per CommonMark §4.2. Empty for
 * pre-heading sections.
 * - `heading_slug` — URL-addressable slug (lowercase, ASCII
 * alphanumeric runs hyphen-separated, formatting chars stripped).
 * Empty when the section has no heading.
 * - `body` — section content INCLUDING any nested deeper headings
 * textually contained within it, EXCLUDING the section's own
 * heading line.
 * - `byte_offset` — start of the section's address in the original
 * body (heading line for headed sections, 0 for pre-heading).
 * Useful for cursor-based diffing client-side.
 */
export interface UserContentSection {
 index: number;
 depth: number;
 heading?: string;
 heading_slug?: string;
 body: string;
 byte_offset: number;
}

/**
 * UserContentSectionsPage is the paginated section list shape used
 * by both the embedded `sections` field on the entity GET and the
 * standalone `/sections` endpoint. `next_cursor` is OMITTED on the
 * wire when the prior page was the last one — the agent stops
 * paginating when the field is missing (not when it's null).
 */
export interface UserContentSectionsPage {
 entries: UserContentSection[];
 next_cursor?: string;
}

/**
 * UserContentEntityResponse mirrors `GET /v1/user-content/{id}`
 * (per yaad-index a prior PR) — entity envelope + first-page sections.
 * The `etag` field (ADDED CLIENT-SIDE by yaad-mcp from the HTTP
 * `ETag` response header) carries the concurrency token the agent
 * needs to pass back as `If-Match` on a subsequent edit. Server
 * doesn't put it in the JSON body — yaad-mcp lifts it onto the
 * response object so the agent doesn't have to peek at HTTP headers.
 */
export interface UserContentEntityResponse {
 ok: boolean;
 id: string;
 kind: string;
 data?: Record<string, unknown>;
 tags?: string[];
 provenance: ProvenanceEntry[];
 sections: UserContentSectionsPage;
 /** Lifted from the HTTP `ETag` response header by yaad-mcp.
 * Pass back as the `etag` argument of `edit_user_content_section`. */
 etag?: string;
}

/**
 * UserContentSectionsListResponse mirrors `GET /v1/user-content/{id}/sections`
 * — the paginated list endpoint (no entity envelope). `etag` is
 * lifted from the HTTP `ETag` response header same as
 * UserContentEntityResponse.
 */
export interface UserContentSectionsListResponse {
 ok: boolean;
 entries: UserContentSection[];
 next_cursor?: string;
 etag?: string;
}

/**
 * UserContentSectionResponse mirrors `GET /v1/user-content/{id}/sections/{sec}`
 * — one section by positional index OR heading slug. `etag` is
 * lifted from the HTTP `ETag` response header.
 */
export interface UserContentSectionResponse {
 ok: boolean;
 id: string;
 section: UserContentSection;
 etag?: string;
}

/**
 * StructureResponse mirrors `GET /v1/structure` verbatim per
 * ADR-0013 §7 (a prior PR). The agent reads this shape
 * directly — yaad-mcp does not reshape, summarize, or cache.
 *
 * `version` is a deterministic SHA-256 truncated to 16 hex chars,
 * computed over the canonical JSON of (kinds, sorted edge_types,
 * sorted-by-name plugins, schema-discrim sentinel). Same config
 * → same hash; reorder-stable. Agents that want to cache results
 * key on this string and re-fetch when the value changes.
 */
export interface StructureResponse {
 ok: boolean;
 version: string;
 kinds: Record<string, StructureKind>;
 edge_types: string[];
 plugins: StructurePlugin[];
}

/**
 * EdgeListEntry is one entry on the GET /v1/edges response per
 * yaad-index. Distinct from EdgeRef (the inline `{type, to}`
 * shape that lives inside Entity.edges via ?with_edges= expansion):
 * GET /v1/edges surfaces from_id/to_id explicitly so the agent
 * can distinguish inbound vs outbound from a directionless query
 * (direction=both).
 */
export interface EdgeListEntry {
 type: string;
 from_id: string;
 to_id: string;
 metadata?: Record<string, unknown>;
}

/**
 * EdgeListResponse mirrors the GET /v1/edges envelope. `next_cursor`
 * is reserved on the wire for forward-compat but always emitted as
 * null today (yaad-index explicitly defers cursor traversal —
 * single-hop edge counts per entity are bounded). The shape stays
 * consistent so a future cursor implementation doesn't break
 * agent-side decoders.
 */
export interface EdgeListResponse {
 ok: boolean;
 edges: EdgeListEntry[];
 next_cursor: string | null;
}
