// Thin HTTP client around yaad-index's REST API. Uses bun's built-in
// fetch (no extra deps). Designed to be injectable: the constructor
// takes a `fetch` impl so tests can swap in a mock without monkey-
// patching globals.

import type {
 CommentsResponse,
 CVStatusResponse,
 EdgeListResponse,
 EntitiesBatchResponse,
 Entity,
 EntityArchiveResponse,
 EntityContextResponse,
 EntityDeleteResponse,
 FillResponse,
 IngestResponse,
 KindsResponse,
 ListEntitiesResponse,
 NeedsFillResponse,
 PluginsResponse,
 ReindexResponse,
 SearchLocalResponse,
 SearchUpstreamRequest,
 SearchUpstreamResponse,
 StructureResponse,
 TaskListResponse,
 TaskLoadResponse,
 TaskResolveResponse,
 UpstreamErrorEnvelope,
 UserContentCreateRequest,
 UserContentDeleteResponse,
 UserContentEntityResponse,
 UserContentSectionResponse,
 UserContentSectionsListResponse,
 WorkflowDiscoverResponse,
 WorkflowListResponse,
 WorkflowTriggerResponse,
} from "../types.js";

// FetchLike is the structural shape the client + tests need from a
// fetch implementation: just `(url, init?) => Promise<Response>`. Not
// `typeof fetch` because bun's fetch declares additional methods
// (`preconnect`) that a plain function fixture can't satisfy — we
// don't use those in this client.
export type FetchLike = (
 input: string | URL,
 init?: RequestInit,
) => Promise<Response>;

export interface YaadIndexClientConfig {
 /** Base URL for yaad-index, e.g. "http://localhost:7433". No trailing slash. */
 baseUrl: string;
 /** Optional bearer token. Sent as `Authorization: Bearer <token>` when set. */
 authToken?: string;
 /** Override fetch — tests inject a mock. Defaults to globalThis.fetch (bun-native). */
 fetchImpl?: FetchLike;
}

export class YaadIndexClient {
 private readonly baseUrl: string;
 private readonly authToken?: string;
 private readonly fetchImpl: FetchLike;

 constructor(cfg: YaadIndexClientConfig) {
 this.baseUrl = cfg.baseUrl.replace(/\/+$/, "");
 this.authToken = cfg.authToken;
 this.fetchImpl = cfg.fetchImpl ?? ((input, init) => globalThis.fetch(input, init));
 }

 async ingest(url: string): Promise<IngestResponse> {
 return this.request<IngestResponse>("POST", "/v1/ingest", { url });
 }

 async getEntity(id: string): Promise<Entity> {
 // `?with_edges=*` expands every outgoing edge type inline. The
 // daemon's parseWithEdges treats `*` (and `all`) as the
 // wildcard sentinel that returns nil filter — meaning every
 // edge type the entity has, returned in one call. Legacy the
 // MCP layer pinned this to `is_about` only; that left
 // plugin-emitted (`is_a`, `designed_by`, `artist_by`,
 // `published_by`) and agent-filled cross-canonical edges
 // hidden from `get_entity`. The wildcard restores parity with
 // the daemon's full edge graph for one-hop reads.
 return this.request<Entity>(
 "GET",
 `/v1/entities/${encodeURIComponent(id)}?with_edges=*`,
 );
 }

 /**
 * Append a comment to an existing entity (per yaad-index a prior PR
 * / — POST /v1/entities/{id}/comments). Server stamps `date`
 * UTC + `Operator` from the JWT pair-claim's operator. `author`:
 *
 * - omitted (or empty) → server fills from JWT `sub`.
 * - non-empty matching JWT `sub` → accepted, stored verbatim.
 * - non-empty disagreeing → upstream 403 `author_mismatch`.
 *
 * Unlike most client methods, addComment surfaces the upstream
 * error envelope verbatim instead of throwing — the agent observes
 * structured `{ok:false, error, message}` directly so they can
 * branch on `error === "author_mismatch"` without parsing an
 * exception message. The discriminant is `ok`.
 */
 async addComment(
 id: string,
 text: string,
 author?: string,
 ): Promise<CommentsResponse | UpstreamErrorEnvelope> {
 const body: Record<string, string> = { text };
 if (author) {
 body.author = author;
 }
 const headers: Record<string, string> = {
 Accept: "application/json",
 "Content-Type": "application/json",
 };
 if (this.authToken) {
 headers["Authorization"] = `Bearer ${this.authToken}`;
 }
 const path = `/v1/entities/${encodeURIComponent(id)}/comments`;
 const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
 method: "POST",
 headers,
 body: JSON.stringify(body),
 });
 const text2 = await res.text();
 if (text2 === "") {
 // 204 / empty body — surface a synthetic envelope so the
 // discriminant stays consistent. Not produced by current
 // yaad-index (every error path emits a JSON envelope) but
 // defensive against silent upstream changes.
 return res.ok
 ? ({ ok: true } as unknown as CommentsResponse)
 : { ok: false, error: "empty_response", message: `${res.status}` };
 }
 try {
 return JSON.parse(text2) as CommentsResponse | UpstreamErrorEnvelope;
 } catch {
 // Non-JSON 5xx body (e.g. an upstream proxy error page) —
 // surface as a generic envelope so the agent can route on
 // `ok === false` consistently.
 return {
 ok: false,
 error: "non_json_response",
 message: `${res.status}: ${text2.slice(0, 200)}`,
 };
 }
 }

 /**
 * Single-hop edge query per yaad-index. Wraps
 * GET /v1/edges?entity_id=X[&edge_types=...][&direction=...].
 *
 * Distinct from `getEntityWithContext` (multi-hop BFS): this is
 * the "who does this game cite" / "what cites this person"
 * single-hop surface. Direction defaults to "out" for backward-
 * compat with the existing per-entity edge-expansion semantic.
 *
 * Empty edge_types → no type filter (returns every type).
 * direction="both" surfaces inbound + outbound combined.
 */
 async edges(
 id: string,
 edgeTypes?: string[],
 direction?: "out" | "in" | "both",
 ): Promise<EdgeListResponse> {
 if (!id) {
 throw new YaadIndexError(400, "edges requires `entity_id`");
 }
 const params = new URLSearchParams({ entity_id: id });
 if (edgeTypes && edgeTypes.length > 0) {
 params.set("edge_types", edgeTypes.join(","));
 }
 if (direction) {
 params.set("direction", direction);
 }
 return this.request<EdgeListResponse>(
 "GET",
 `/v1/edges?${params.toString()}`,
 );
 }

 async fill(id: string, fields: Record<string, unknown>): Promise<FillResponse> {
 return this.request<FillResponse>(
 "POST",
 `/v1/entities/${encodeURIComponent(id)}/fill`,
 { fields },
 );
 }

 /**
 * Operator-fill endpoint per yaad-index / ADR-0019 step 5.
 * Wraps POST /v1/entities/{id}/operator-fill — sends per-field
 * ops where each value can be:
 *
 * - scalar (number / boolean / string / ...) → set the field +
 * stamp gap_state.source=operator + gap_state.filled_at.
 * - explicit `null` → clear: remove from data + drop the
 * gap_state entry (back to "untouched"). Sending `null` is
 * the path to un-fill before deferring (see defer_gap doc).
 * - `{defer: true}` → mark the gap deferred. Field MUST be
 * unfilled; defer-on-filled returns 409
 * `deferred_requires_unfilled`.
 * - `{defer: false}` → un-defer (drop the deferred flag).
 *
 * Auth: operator-only (Subject == Operator). Agent-on-behalf-of-
 * operator JWTs reject with 403 `agent_not_allowed`.
 *
 * The 409 envelope on `deferred_requires_unfilled` AND any other
 * 4xx/5xx surface as a thrown YaadIndexError today (consistent
 * with deleteEntity's pattern); the agent caller branches on
 * `err instanceof YaadIndexError && err.status === 409` to detect
 * the filled-and-defer mistake. Wire-shape recovery from there is
 * a two-step path: clear the value (`null`), then defer.
 */
 async setOperatorFill(
 id: string,
 fields: Record<string, unknown>,
 ): Promise<FillResponse> {
 return this.request<FillResponse>(
 "POST",
 `/v1/entities/${encodeURIComponent(id)}/operator-fill`,
 fields,
 );
 }

 /**
 * Pull-based batch gap-call queue (per yaad-index ADR-0013 §6,
 * a prior PR/172). Returns gap-callable entities — those with
 * `gap_call_done_at` NULL AND vault frontmatter carrying
 * unfilled gaps — paginated via opaque base64(last-seen-id)
 * cursor over `id ASC` ordering.
 *
 * `limit` is server-clamped (default 50, cap 200, lenient on
 * bad values); the client passes whatever the agent provided
 * and lets the server validate. `cursor` is opaque to the
 * client — the agent receives `next_cursor` from a previous
 * page and passes it back as-is. Empty / undefined → first
 * page from the beginning.
 *
 * Returns the response verbatim — yaad-mcp adds no
 * client-side auto-pagination loop; the agent calls back
 * with the cursor to fetch the next page.
 */
 async getNeedsFill(args: { limit?: number; cursor?: string } = {}): Promise<NeedsFillResponse> {
 const params = new URLSearchParams();
 if (args.limit !== undefined) {
 params.set("limit", String(args.limit));
 }
 if (args.cursor !== undefined && args.cursor !== "") {
 params.set("cursor", args.cursor);
 }
 const query = params.toString();
 const path = query === "" ? "/v1/needs-fill" : `/v1/needs-fill?${query}`;
 return this.request<NeedsFillResponse>("GET", path);
 }

 /**
 * Introspection snapshot of the running yaad-index instance (per
 * ADR-0013 §7, a prior PR / yaad-mcp). Returns
 * the operator's enabled canonical kinds + edge types + active
 * plugin metadata + a deterministic config-hash version, all
 * verbatim from `/v1/structure`. yaad-mcp adds no client-side
 * caching — the agent keys on `version` if it wants to cache.
 */
 async getStructure(): Promise<StructureResponse> {
 return this.request<StructureResponse>("GET", "/v1/structure");
 }

 /**
 * Trigger a vault-rebuild of the derived index (per yaad-index
 * ADR-0008 §reindex / yaad-mcp). Wraps POST /v1/reindex.
 *
 * `mode` is optional — omit (or pass undefined) to send no body
 * at all, which the daemon treats as the default `incremental`
 * mode (re-parse only files whose mtime/hash changed since last
 * walk). Pass `"full"` for a hard rebuild (re-parse every file).
 *
 * The daemon route is unregistered (404) when `vault.path` isn't
 * configured operator-side — the request() helper surfaces that
 * as a YaadIndexError with status 404; the tool layer maps it to
 * the standard error envelope.
 *
 * The call blocks until the walk completes. yaad-mcp adds no
 * client-side polling or progress reporting; the response is the
 * full daemon Summary verbatim.
 */
 async reindex(mode?: "incremental" | "full"): Promise<ReindexResponse> {
 if (mode === undefined) {
 return this.request<ReindexResponse>("POST", "/v1/reindex");
 }
 return this.request<ReindexResponse>("POST", "/v1/reindex", { mode });
 }

 /**
 * Plugin-emitted source-kinds registry (per ADR-0002 §"GET /v1/kinds"
 * + yaad-mcp). Returns the union of every registered plugin's
 * declared entity-kinds and edge-kinds — distinct from
 * `getStructure()` which returns the canonical-shape registry.
 *
 * No client-side caching, no name filtering at the client layer —
 * the daemon endpoint doesn't accept a name filter, so any per-name
 * narrowing is implemented at the tool boundary on the response.
 */
 async getKinds(): Promise<KindsResponse> {
 return this.request<KindsResponse>("GET", "/v1/kinds");
 }

 /**
 * Per-plugin capability view (per yaad-index #13). Returns each
 * registered plugin's --init capabilities subset: `name + version
 * + url_patterns + commands + entity_kinds + edge_kinds +
 * source_namespace`. Inverse of `getKinds()`: `getKinds()`
 * aggregates kind → plugins, `getPlugins()` enumerates plugin →
 * kinds + URL patterns + commands.
 *
 * Used by the `plugins` tool to give the agent a live view of
 * what plugins are loaded + what each one accepts, so SKILL.md
 * doesn't have to carry per-plugin tables.
 */
 async getPlugins(): Promise<PluginsResponse> {
 return this.request<PluginsResponse>("GET", "/v1/plugins");
 }

 /**
 * Canonical-vocabulary drift snapshot (per yaad-index ADR-0013
 * §3, a prior PR/177 / yaad-mcp). Returns drift counters of
 * plugin emissions the operator's config dropped at ingest
 * time, plus a deterministic `config_hash` for change
 * detection and `last_reindex_at` for the operator-prompted
 * reindex hint. Verbatim pass-through; yaad-mcp adds no
 * client-side caching keyed by `config_hash` — agents handle
 * that themselves.
 */
 async getCVStatus(): Promise<CVStatusResponse> {
 return this.request<CVStatusResponse>("GET", "/v1/cv-status");
 }

 /**
 * Multi-hop context stitch (per yaad-index the source issue / yaad-mcp
 * the source issue). Returns the entity at `id` plus all entities reachable
 * within `depth` outbound edge-hops, in one round-trip. The server
 * caps depth at 3, applies cycle detection, and truncates results
 * past `max_results` (default 200, cap 1000).
 *
 * `edgeTypes`, when non-empty, filters traversal: only edges of the
 * named types are walked AND surfaced. Empty / undefined → no filter.
 * `maxResults` is plumbed when defined; the server applies its
 * default otherwise.
 *
 * Returns the response verbatim — yaad-mcp adds no client-side
 * traversal logic.
 */
 async getEntityContext(
 id: string,
 depth: number,
 edgeTypes?: string[],
 maxResults?: number,
 ): Promise<EntityContextResponse> {
 const params = new URLSearchParams({ depth: String(depth) });
 if (edgeTypes && edgeTypes.length > 0) {
 params.set("edge_types", edgeTypes.join(","));
 }
 if (maxResults !== undefined) {
 params.set("max_results", String(maxResults));
 }
 return this.request<EntityContextResponse>(
 "GET",
 `/v1/entities/${encodeURIComponent(id)}/context?${params.toString()}`,
 );
 }

 /**
 * Batch-fetch multiple entities in one round-trip (per yaad-index
 * ADR-0002 §"GET /v1/entities/batch" + yaad-mcp). Wraps
 * `POST /v1/entities/batch`. Trade-off vs N×getEntity loops: one
 * HTTP request for up to `batchMaxIDs` (= 100 today) ids, at the
 * cost of a single failure surface (any per-id resolution issue
 * lands in `missing` — placeholder entities from in-flight ingest
 * land in `entities` with sparse data per ADR-0002).
 *
 * `withEdges` is plumbed through to the daemon's `with_edges`
 * request field, but per the daemon's current implementation the
 * param is accepted-and-ignored — `entities[i].edges` returns
 * empty regardless. Forward-compat for when the edge-side cutover
 * lands on the batch surface.
 *
 * 400 `too_many_ids` when the batch exceeds `batchMaxIDs` (server
 * surfaces the cap in the message). Bubbles as a `YaadIndexError`
 * so the caller can react by splitting + retrying. Empty `ids`
 * input rejected at the tool boundary upstream of this method.
 */
 async getEntitiesBatch(
 ids: string[],
 withEdges?: string[],
 ): Promise<EntitiesBatchResponse> {
 const body: Record<string, unknown> = { ids };
 if (withEdges !== undefined) {
 body.with_edges = withEdges;
 }
 return this.request<EntitiesBatchResponse>(
 "POST",
 "/v1/entities/batch",
 body,
 );
 }

 async listEntities(kind: string): Promise<ListEntitiesResponse> {
 if (!kind) {
 throw new YaadIndexError(
 400,
 "list_entities requires a kind — yaad-index has no list-all route",
 );
 }
 // `/v1/search?kind=` is the kind-filtered listing endpoint per
 // yaad-index `internal/api/search.go`. There is NO plain
 // `GET /v1/entities` (list-all); the search endpoint requires
 // either `q=` or `kind=`. a prior PR's scaffold pointed at the wrong
 // path (the source issue). Default `limit=100` is fine for v0.
 const path = `/v1/search?kind=${encodeURIComponent(kind)}&limit=100`;
 return this.request<ListEntitiesResponse>("GET", path);
 }

 /**
 * UGC read endpoints (per yaad-index PR-B / yaad-mcp).
 *
 * All three methods lift the `ETag` HTTP response header onto the
 * returned object as `etag` so the agent can pass it back as
 * `If-Match` on the eventual `edit_user_content_section` call (a prior PR).
 * yaad-mcp puts the etag on the JSON object — agents don't have to
 * inspect HTTP headers.
 */
 async getUserContent(
 id: string,
 opts: { limit?: number; cursor?: string } = {},
 ): Promise<UserContentEntityResponse> {
 if (!id) {
 throw new YaadIndexError(400, "get_user_content requires an `id`");
 }
 const path = userContentReadPath(`/v1/user-content/${encodeURIComponent(id)}`, opts);
 return this.requestWithEtag<UserContentEntityResponse>("GET", path);
 }

 async listUserContentSections(
 id: string,
 opts: { limit?: number; cursor?: string } = {},
 ): Promise<UserContentSectionsListResponse> {
 if (!id) {
 throw new YaadIndexError(400, "list_user_content_sections requires an `id`");
 }
 const path = userContentReadPath(
 `/v1/user-content/${encodeURIComponent(id)}/sections`,
 opts,
 );
 return this.requestWithEtag<UserContentSectionsListResponse>("GET", path);
 }

 async getUserContentSection(
 id: string,
 sec: string,
 ): Promise<UserContentSectionResponse> {
 if (!id) {
 throw new YaadIndexError(400, "get_user_content_section requires an `id`");
 }
 if (!sec) {
 throw new YaadIndexError(
 400,
 "get_user_content_section requires a `sec` (heading slug OR positional index)",
 );
 }
 const path = `/v1/user-content/${encodeURIComponent(id)}/sections/${encodeURIComponent(sec)}`;
 return this.requestWithEtag<UserContentSectionResponse>("GET", path);
 }

 /**
 * Create a new UGC entity (per yaad-index PR-C / yaad-mcp).
 * Server stamps `author` from JWT subject and `operator` from the
 * pair-claim. 409 conflict on slug collision (caller picks a new
 * title). Returns the created entity envelope with `etag` lifted
 * from the HTTP ETag header — the agent can chain edits without
 * an extra GET.
 *
 * Throws on non-2xx (read-side semantics for create — the only
 * structured 4xx the agent would branch on is 409 conflict, which
 * comes through the YaadIndexError shape with status=409 and the
 * message containing the colliding id).
 */
 async createUserContent(
 req: UserContentCreateRequest,
 ): Promise<UserContentEntityResponse> {
 if (!req.title) {
 throw new YaadIndexError(400, "create_user_content requires `title`");
 }
 if (!req.tags || req.tags.length === 0) {
 throw new YaadIndexError(400, "create_user_content requires non-empty `tags`");
 }
 return this.requestWithEtag<UserContentEntityResponse>(
 "POST",
 "/v1/user-content",
 req,
 );
 }

 /**
 * Replace a single section's body on a UGC entity (per yaad-index
 * PR-C / yaad-mcp). The novel piece: If-Match etag
 * concurrency.
 *
 * The agent reads the etag from a prior `get_user_content` /
 * `list_user_content_sections` / `get_user_content_section` call
 * (lifted onto the response object) and passes it back here. Server
 * compares it against the current entity body's etag:
 *
 * - match → 200 with the post-edit section + new etag
 * - mismatch → 412 precondition_failed; envelope returned with
 * `current_etag` lifted from the response header so the agent
 * can retry without re-GETing
 * - missing → 428 precondition_required (the agent forgot to
 * pass etag)
 * - cross-author → 403 author_mismatch
 *
 * Unlike most client methods, this returns the upstream envelope
 * verbatim on 4xx (mirrors `addComment`'s passthrough pattern) so
 * the agent branches on `ok === false && error === "..."` without
 * parsing exception messages. 5xx still throws.
 */
 async editUserContentSection(
 id: string,
 sec: string,
 body: string,
 etag: string,
 ): Promise<UserContentSectionResponse | UpstreamErrorEnvelope> {
 if (!id) {
 throw new YaadIndexError(400, "edit_user_content_section requires `id`");
 }
 if (!sec) {
 throw new YaadIndexError(400, "edit_user_content_section requires `sec`");
 }
 if (!etag) {
 throw new YaadIndexError(400, "edit_user_content_section requires `etag` (If-Match)");
 }
 const headers: Record<string, string> = {
 Accept: "application/json",
 "Content-Type": "application/json",
 "If-Match": etag,
 };
 if (this.authToken) {
 headers["Authorization"] = `Bearer ${this.authToken}`;
 }
 const path = `/v1/user-content/${encodeURIComponent(id)}/sections/${encodeURIComponent(sec)}`;
 const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
 method: "PUT",
 headers,
 body: JSON.stringify({ body }),
 });
 const text = await res.text();
 const responseEtag = res.headers.get("ETag") ?? undefined;

 if (res.ok) {
 const parsed = text === "" ? ({} as UserContentSectionResponse) : (JSON.parse(text) as UserContentSectionResponse);
 if (responseEtag) {
 return { ...parsed, etag: responseEtag };
 }
 return parsed;
 }

 // Non-2xx → passthrough envelope (4xx) or throw (5xx). The 412
 // path lifts the ETag header onto the envelope as `current_etag`.
 if (res.status >= 500) {
 throw new YaadIndexError(res.status, `PUT ${path}: ${res.status} ${text}`);
 }
 let envelope: UpstreamErrorEnvelope;
 try {
 envelope = JSON.parse(text) as UpstreamErrorEnvelope;
 } catch {
 envelope = {
 ok: false,
 error: "non_json_response",
 message: `${res.status}: ${text.slice(0, 200)}`,
 };
 }
 if (res.status === 412 && responseEtag) {
 envelope.current_etag = responseEtag;
 }
 return envelope;
 }

 /**
 * Delete a UGC entity (per yaad-index PR-C / yaad-mcp).
 * Removes the vault file (with auto-commit) and the store row.
 * 403 author_mismatch when the JWT claim doesn't match the entity's
 * stored author or operator.
 *
 * Throws on non-2xx — the structured branch agents care about is
 * 403 author_mismatch which comes through the YaadIndexError shape
 * with status=403 and the message containing `author_mismatch`. 404
 * (already-deleted) is a real failure (NOT silent ok); agents
 * surface it.
 */
 /**
 * Archive an entity (per yaad-index / ADR-0018 step 2). Wraps
 * POST /v1/entities/{id}/archive. The vault file moves from the
 * active layout `<kind>/<slug>.md` to `_archive/<kind>/<slug>.md`,
 * and the DB `archived_at` timestamp is set. Idempotent: re-archive
 * on an already-archived entity preserves the original timestamp
 * and is a no-op vault-side.
 *
 * Inverse of `restoreEntity`. ADR-0018 step 4 makes archive a hard
 * prerequisite for delete — see `deleteEntity` for the state-machine.
 *
 * Throws on non-2xx as YaadIndexError. 404 (id not found), 503
 * (vault not configured), 401 (auth) are the typical branches.
 */
 async archiveEntity(id: string): Promise<EntityArchiveResponse> {
 if (!id) {
 throw new YaadIndexError(400, "archive_entity requires `id`");
 }
 return this.requestArchiveTransition(id, "archive");
 }

 /**
 * Restore an archived entity (per yaad-index / ADR-0018 step 2).
 * Wraps POST /v1/entities/{id}/restore. Inverse of `archiveEntity`:
 * the vault file moves back from `_archive/<kind>/<slug>.md` to the
 * active layout, and the DB `archived_at` is cleared.
 */
 async restoreEntity(id: string): Promise<EntityArchiveResponse> {
 if (!id) {
 throw new YaadIndexError(400, "restore_entity requires `id`");
 }
 return this.requestArchiveTransition(id, "restore");
 }

 private async requestArchiveTransition(
 id: string,
 op: "archive" | "restore",
 ): Promise<EntityArchiveResponse> {
 const headers: Record<string, string> = { Accept: "application/json" };
 if (this.authToken) {
 headers["Authorization"] = `Bearer ${this.authToken}`;
 }
 const path = `/v1/entities/${encodeURIComponent(id)}/${op}`;
 const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
 method: "POST",
 headers,
 });
 if (!res.ok) {
 const text = await res.text();
 throw new YaadIndexError(res.status, `POST ${path}: ${res.status} ${text}`);
 }
 return (await res.json()) as EntityArchiveResponse;
 }

 /**
 * Delete an entity (per yaad-index + ADR-0018 step 4). Wraps
 * DELETE /v1/entities/{id} where `id` is the full
 * `<kind>:<local-id>` identifier.
 *
 * State-machine: DELETE only hard-destroys an *archived* entity. On
 * an *active* entity the daemon returns 409 with a wire error code
 * `must archive before delete` and a hint pointing at the
 * archive-first path. This client surfaces that 409 verbatim as a
 * structured `UpstreamErrorEnvelope` (so the MCP tool can return it
 * to the caller without an exception); other non-2xx (401, 404,
 * 503) still throw as YaadIndexError.
 *
 * On an archived entity, the call removes the `_archive/...` vault
 * file (with auto-commit producing a `destroy: <id> [<kind>] by
 * <agent>` git commit) and cascades the DB row + edges + provenance.
 */
 async deleteEntity(
 id: string,
 ): Promise<EntityDeleteResponse | UpstreamErrorEnvelope> {
 if (!id) {
 throw new YaadIndexError(400, "delete_entity requires `id`");
 }
 return this.requestDeleteWithStateMachine(
 `/v1/entities/${encodeURIComponent(id)}`,
 ) as Promise<EntityDeleteResponse | UpstreamErrorEnvelope>;
 }

 /**
 * Delete a UGC entity (per yaad-index PR-C + ADR-0018 step 4).
 * Same archive-first state-machine as `deleteEntity`: an active
 * entity returns 409 surfaced as the structured envelope; an
 * archived entity is hard-destroyed. 403 author_mismatch (caller
 * isn't the author or co-operator) still throws as YaadIndexError.
 */
 async deleteUserContent(
 id: string,
 ): Promise<UserContentDeleteResponse | UpstreamErrorEnvelope> {
 if (!id) {
 throw new YaadIndexError(400, "delete_user_content requires `id`");
 }
 return this.requestDeleteWithStateMachine(
 `/v1/user-content/${encodeURIComponent(id)}`,
 ) as Promise<UserContentDeleteResponse | UpstreamErrorEnvelope>;
 }

 private async requestDeleteWithStateMachine<T>(
 path: string,
 ): Promise<T | UpstreamErrorEnvelope> {
 const headers: Record<string, string> = { Accept: "application/json" };
 if (this.authToken) {
 headers["Authorization"] = `Bearer ${this.authToken}`;
 }
 const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
 method: "DELETE",
 headers,
 });
 if (res.status === 409) {
 // ADR-0018 step 4 archive-first state-machine. Surface the
 // upstream envelope verbatim so the MCP tool layer can return
 // a structured agent-readable hint instead of a thrown error.
 const text = await res.text();
 try {
 return JSON.parse(text) as UpstreamErrorEnvelope;
 } catch {
 return {
 ok: false,
 error: "non_json_response",
 message: `409: ${text.slice(0, 200)}`,
 };
 }
 }
 if (!res.ok) {
 const text = await res.text();
 throw new YaadIndexError(res.status, `DELETE ${path}: ${res.status} ${text}`);
 }
 return (await res.json()) as T;
 }

 async searchLocal(
 query: string,
 kind?: string,
 limit?: number,
 ): Promise<SearchLocalResponse> {
 if (!query) {
 throw new YaadIndexError(400, "search_local requires a non-empty query");
 }
 // `/v1/search?q=...&kind=...&limit=...` per yaad-index
 // `internal/api/search.go`. The endpoint accepts either q OR
 // kind (or both); search_local always passes q + optionally a
 // kind filter. The default limit=20 is the agent-friendly
 // shape — bigger result sets are paginatable in a follow-up.
 const params = new URLSearchParams({ q: query });
 if (kind) {
 params.set("kind", kind);
 }
 params.set("limit", String(limit ?? 20));
 return this.request<SearchLocalResponse>(
 "GET",
 `/v1/search?${params.toString()}`,
 );
 }

 /**
 * searchUpstream wraps POST /v1/search/upstream — the plugin-
 * federated search surface per yaad-index #2. Daemon dispatches
 * to each opted-in plugin (Capabilities.SupportsSearch=true) in
 * parallel with a per-plugin timeout and returns merged
 * candidates plus a per-plugin status block.
 *
 * Partial-results semantic: a single plugin failure / timeout
 * doesn't fail the call; the per-plugin status block surfaces
 * the error. Explicit-allowlist names that aren't registered →
 * 400; names whose plugin opted out → 422.
 */
 async searchUpstream(
 req: SearchUpstreamRequest,
 ): Promise<SearchUpstreamResponse> {
 if (!req.query) {
 throw new YaadIndexError(
 400,
 "search_upstream requires a non-empty query",
 );
 }
 return this.request<SearchUpstreamResponse>(
 "POST",
 "/v1/search/upstream",
 req,
 );
 }

 /**
 * Workflow surface per ADR-0024 §"Agent surface" — list every
 * registered workflow with metadata (name/version/status/
 * trigger_type/dedup_policy). Sorted by name; the daemon's
 * snapshot — yaad-mcp adds no client-side caching.
 */
 async listWorkflows(): Promise<WorkflowListResponse> {
 return this.request<WorkflowListResponse>("GET", "/v1/workflows");
 }

 /**
 * List workflows whose condition predicate matches the given
 * entity per ADR-0024 §"workflow.discover". Walks every
 * registered workflow + evaluates each condition against the
 * resolved entity; returns the matching workflow names
 * (sorted). Server returns 404 not_found when the entity has
 * no store row — surfaced as YaadIndexError(404).
 */
 async discoverWorkflows(entityID: string): Promise<WorkflowDiscoverResponse> {
 const path = `/v1/workflows/discover?entity=${encodeURIComponent(entityID)}`;
 return this.request<WorkflowDiscoverResponse>("GET", path);
 }

 /**
 * Manual workflow trigger per ADR-0024 §"workflow.trigger(input)
 * input semantics". `input` accepts: empty (target-less for
 * trigger.type=manual workflows), canonical entity id
 * (`<kind>:<slug>`), or URL (routes through the ingest-or-
 * lookup pipeline). Returns the recorded Decision envelope
 * verbatim.
 */
 async triggerWorkflow(name: string, input?: string): Promise<WorkflowTriggerResponse> {
 return this.request<WorkflowTriggerResponse>("POST", "/v1/workflows/trigger", {
 name,
 input: input ?? "",
 });
 }

 /**
 * List workflow-produced tasks per ADR-0024 §"task.list".
 * Optional `errored` filter routes to ?errored=true|false on
 * the wire — true returns only err-tasks (per ADR-0024
 * §"Runtime errors" err-task surface); false returns only
 * normal tasks; omitted returns both.
 */
 async listTasks(args: { errored?: boolean } = {}): Promise<TaskListResponse> {
 let path = "/v1/tasks";
 if (args.errored !== undefined) {
 path += `?errored=${args.errored ? "true" : "false"}`;
 }
 return this.request<TaskListResponse>("GET", path);
 }

 /**
 * Load one workflow-produced task by id per ADR-0024
 * §"task.load". Returns the summary + the raw markdown body
 * (post-frontmatter). 404 when the id doesn't resolve.
 */
 async loadTask(id: string): Promise<TaskLoadResponse> {
 const path = `/v1/tasks/${encodeURIComponent(id)}`;
 return this.request<TaskLoadResponse>("GET", path);
 }

 /**
 * Mark a workflow-produced task done per ADR-0024 §"task.resolve".
 * Stamps `resolved_at` on the task's frontmatter; auto-archives
 * (moves the file to tasks/_archive/<id>.md) when the
 * originating workflow has `auto_archive_on_done: true` (the
 * default). Err-tasks always auto-archive regardless of the
 * workflow opt-out.
 */
 async resolveTask(id: string): Promise<TaskResolveResponse> {
 const path = `/v1/tasks/${encodeURIComponent(id)}/resolve`;
 return this.request<TaskResolveResponse>("POST", path);
 }

 private async request<T>(
 method: "GET" | "POST",
 path: string,
 body?: unknown,
 ): Promise<T> {
 const headers: Record<string, string> = { Accept: "application/json" };
 if (body !== undefined) {
 headers["Content-Type"] = "application/json";
 }
 if (this.authToken) {
 headers["Authorization"] = `Bearer ${this.authToken}`;
 }
 const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
 method,
 headers,
 body: body === undefined ? undefined : JSON.stringify(body),
 });
 if (!res.ok) {
 // Read the body once for the error message; yaad-index returns
 // {error, message} on non-2xx per the API contract.
 const text = await res.text();
 throw new YaadIndexError(res.status, `${method} ${path}: ${res.status} ${text}`);
 }
 return (await res.json()) as T;
 }

 /**
 * requestWithEtag is the variant of request() that also lifts the
 * HTTP `ETag` response header onto the parsed JSON object as `etag`.
 * Used by the UGC read endpoints (per yaad-mcp) so the agent
 * gets the concurrency token without inspecting raw HTTP headers.
 * Same throw-on-non-2xx semantics as request().
 */
 private async requestWithEtag<T extends object>(
 method: "GET" | "POST",
 path: string,
 body?: unknown,
 ): Promise<T & { etag?: string }> {
 const headers: Record<string, string> = { Accept: "application/json" };
 if (body !== undefined) {
 headers["Content-Type"] = "application/json";
 }
 if (this.authToken) {
 headers["Authorization"] = `Bearer ${this.authToken}`;
 }
 const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
 method,
 headers,
 body: body === undefined ? undefined : JSON.stringify(body),
 });
 if (!res.ok) {
 const text = await res.text();
 throw new YaadIndexError(res.status, `${method} ${path}: ${res.status} ${text}`);
 }
 const parsed = (await res.json()) as T;
 const etag = res.headers.get("ETag");
 if (etag) {
 return { ...parsed, etag };
 }
 return parsed as T & { etag?: string };
 }
}

// userContentReadPath builds a `?limit=&cursor=` query string for the
// UGC read endpoints, omitting either parameter when unset so the
// server falls back to its defaults.
function userContentReadPath(
 base: string,
 opts: { limit?: number; cursor?: string },
): string {
 const params = new URLSearchParams();
 if (opts.limit !== undefined) {
 params.set("limit", String(opts.limit));
 }
 if (opts.cursor !== undefined && opts.cursor !== "") {
 params.set("cursor", opts.cursor);
 }
 const q = params.toString();
 return q === "" ? base : `${base}?${q}`;
}

export class YaadIndexError extends Error {
 constructor(
 public readonly status: number,
 message: string,
 ) {
 super(message);
 this.name = "YaadIndexError";
 }
}
