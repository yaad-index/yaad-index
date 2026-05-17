# ADR-0002 — API surface

**Status:** Accepted (2026-04-27). Amended by ADR-0008 in three places (snippet semantics, fill-token mechanism, new note endpoint). See ADR-0008's "Supersedes" line for details. The obsolete `fill_token` + equal-set fill semantics in this ADR's body have been struck through in-place with cross-links to ADR-0008 (Per the prior design, Option A: visible deprecation, audit trail preserved via git history + strike-through markers).
**Date:** 2026-04-27
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md)

## Context

ADR-0001 committed to AI-first + remote-API as the structural premise: every client (agents, the CLI, future tooling) reaches yaad-index through HTTP. That decision named the surface but left the actual shapes open.

This ADR fixes the v1 API. Subsequent ADRs (storage, schema evolution, collectors) can now reference concrete endpoints rather than re-deriving them.

## Decision

**Seven endpoints in v1**, all under `/v1/`. Everything else lands as future-ADR additions.

**Note on examples.** This ADR uses BoardGameGeek (BGG) as a worked example throughout (`boardgame:brass-birmingham`, `bgg:224517`, etc.) because it's concrete enough to specify against. BGG is *not* a core or built-in collector — it's the first reference plugin, illustrating how the contract works. The same shapes apply to any future plugin (gmail, web-pages, books, recipes, …); each plugin is equal first-class and a separate domain. The architecture's value is in supporting many such plugins, not in privileging any one.

### Entities and edges

The graph is built from two kinds of records:

- **Entities** (nodes) — concrete things the index knows about: a boardgame, a person, a book. Each has a `kind` (boardgame, person, …) and `data` (kind-specific fields). One entity = one identity. `person:tolkien` exists once whether he authored ten books or designed nothing — being a person is what he *is*, not what he does.
- **Edges** (typed relationships) — directed, typed connections between entities: `(boardgame:brass) -[designed_by]-> (person:martin-wallace)`, `(book:lotr) -[authored_by]-> (person:tolkien)`. The role lives on the edge, not on the entity. A person who plays multiple roles (Tolkien authoring books, Lacerda designing games) gets multiple edges, not multiple entity rows.

This decision (per the operator's review on a prior PR) settles the question "is `person/author` a sub-kind, or is `authored_by` an edge type?" in favor of edges. The latter scales: query patterns like "all books authored by Tolkien" or "all games designed by Lacerda's collaborators" become natural traversals; the entity model stays clean.

### `GET /v1/health`

Liveness probe. Returns 200 with a thin JSON body identifying the build:

```json
{ "ok": true, "version": "v0.0.0-..." }
```

- `ok` — always `true`. A non-200 / connection failure is the negative signal; the field exists for envelope consistency with every other 2xx response.
- `version` — best-effort module version from `runtime/debug.ReadBuildInfo()`. Omitted (`omitempty`) when build info isn't available (e.g. some `go test` paths).

The handler does not touch the store, the plugin registry, or any subsystem — the implicit signal is "the HTTP layer accepted a connection and ran a handler." Operators wanting a deeper readiness probe (store integrity, plugin reachability, etc.) should add a separate `/v1/readiness` endpoint in a later ADR; conflating liveness with readiness leads to monitor false-negatives in degraded modes.

### `GET /v1/entities/{id}`

Fetch a single entity by canonical ID. For multi-hop context stitching across linked entities, see [`GET /v1/entities/{id}/context`](#get-v1entitiesidcontext) below — `with_edges` returns inline edge refs but not the linked entities themselves.

**Request:**
- Path: `id` — canonical entity ID (slug-like, see ADR-0007 of v0 for shape)
- Query (optional): `with_edges` — controls inline edge expansion. Four equivalent shapes (per yaad-index):
 - `with_edges=designed_by,authored_by` — comma-separated type filter; only the named types are expanded.
 - `with_edges=*` (canonical) or `with_edges=all` — sentinel "expand all edge types"; either spelling works, `*` is the canonical form.
 - `with_edges` (key present, no value) or `with_edges=` (key present, explicit empty) — presence-based "expand all edge types"; equivalent to the sentinel.
 - Key absent entirely — no expansion (legacy default; `edges: []` on the response).
 - A sentinel mixed with concrete types in the same value (e.g. `with_edges=*,is_about`) collapses to "all types" — the broader semantic wins.

**Response 200:**
```json
{
 "id": "boardgame:brass-birmingham",
 "kind": "boardgame",
 "data": { ...kind-specific fields... },
 "provenance": [
 {
 "source": "bgg:14-2024-04-12",
 "fetched_at": "2024-04-12T15:03:11Z",
 "ok": true
 },
 {
 "source": "bgg:14-2024-04-13",
 "fetched_at": "2024-04-13T06:00:00Z",
 "ok": false,
 "error": "extractor_timeout",
 "error_message": "AI extraction did not complete within 60s"
 }
 ],
 "edges": [
 { "type": "designed_by", "to": "person:martin-wallace" }
 ]
}
```

**Provenance entries are the machine-readable status mechanism.** The latest entry (highest `fetched_at`) is the most recent ingestion attempt. `ok: true` means that attempt succeeded; `ok: false` with `error` populated means it failed. A client polling for the result of an async ingest reads the latest provenance entry: success → use `data`, failure → inspect `error`/`error_message` to decide whether to retry. This makes "sparse entity from a low-yield extraction" and "failed extraction" distinguishable, which prose-only documentation cannot.

**Response 404:** `{"ok":false,"error":"not_found","message":"no entity with id <id>"}`

### `POST /v1/ingest`

Submit a source for ingestion. The collector decides whether the source is structured (parsed deterministically) or unstructured (queued for AI extraction).

**Primary mode: long-poll.** The server holds the request open up to a configurable timeout (default `60s`, max `300s`) waiting for extraction to complete. If extraction finishes inside the window the server returns the full entity inline. If not, it returns a 202 so the client can fall back. This shape is chosen because it makes the simple-agent case a single call ("ingest URL, get entity") and only forces async-handling code when extraction genuinely takes longer than the agent is willing to wait. Per the operator (review on a prior PR): the question of "request → return-call-again, or block-and-return" was decided in favor of block-and-return as the agent-friendly default.

**Request body:**
```json
{
 "url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
 "hint": "boardgame",
 "force_refetch": false,
 "wait_seconds": 60
}
```

- `url` — required, the source URL.
- `hint` — optional, the expected `kind`. Collector validates and may override if it doesn't match the parsed shape.
- `force_refetch` — optional, default `false`. When `true`, bypass the cache and re-fetch from origin.
- `wait_seconds` — optional, default `60`, max `300`. How long the server should hold the request waiting for extraction. **`0` is async-only mode**: the server starts (or reuses) the ingest job and returns 202 immediately without waiting, even if a fast collector could have completed inline. Use when the caller wants to fire-and-forget.

**Universal `state` field on every 2xx response.** All 2xx responses below carry a `state` field whose value names the canonical terminal: `"complete"`, `"queued"`, `"needs_fill"`, `"disambiguation"`. The legacy `status` field is preserved with the same value for backwards compatibility — clients reading either field keep working. New code SHOULD read `state`. The state is yaad-index-side inference (see [ADR-0006](./0006-plugin-discovery-config-allowlist.md) §"Disambiguation responses"); plugins emit data, the server labels.

**Response 200 (completed inline):**
```json
{
 "ok": true,
 "state": "complete",
 "status": "complete",
 "entity": { ...full entity object, same shape as GET /v1/entities/{id}... }
}
```

Returned when the collector finished within `wait_seconds` (either synchronously or via async extraction that landed in time). Structured collectors and cache hits typically return 200 immediately.

**Response 200 (disambiguation — see [ADR-0006](./0006-plugin-discovery-config-allowlist.md)):**
```json
{
 "ok": true,
 "state": "disambiguation",
 "status": "disambiguation",
 "options": {
 "Go_(programming_language)": {"label": "Go (programming language)", "summary": "Open-source language by Google"},
 "Go_(game)": {"label": "Go (game)", "summary": "Ancient Chinese strategy board game"}
 }
}
```

Returned when the plugin produced multiple candidate entities for one input — Wikipedia's `Go` page being the canonical example (programming language, board game, verb, etc.). No `entity` field; the canonical id isn't resolved yet. The caller picks one option by its key (the plugin's canonical id) and re-invokes `POST /v1/ingest` with the plugin's shorthand input shape (`<plugin>: <id>`) to fetch that candidate. Single-option responses are valid (an "is this what you meant?" confirmation) and are NOT auto-resolved. ADR-0006's "Disambiguation responses" section documents the protocol invariants (stateless plugin, idempotent, caller-decides) and the contract requirement that any plugin emitting `Options` MUST support shorthand-by-id input.

**Response 202 (still extracting after wait expired):**
```json
{
 "ok": true,
 "state": "queued",
 "status": "queued",
 "estimated_entity_id": "boardgame:brass-birmingham"
}
```

**Response 202 (needs agent-side fill — see [ADR-0005](./0005-plugin-lifecycle.md)):**
```json
{
 "ok": true,
 "state": "needs_fill",
 "status": "needs_fill",
 "entity": {
 "id": "boardgame:brass-birmingham",
 "kind": "boardgame",
 "data": { ...fields the plugin filled deterministically... },
 "provenance": [{ "source": "bgg:224517-2026-04-28", "fetched_at": "...", "ok": true }]
 },
 "clean_content": "...plugin-cleaned focused source content for the agent's AI to extract gap fields from...",
 "clean_content_truncated": false,
 "gaps": {
 "summary": "one paragraph summary based on long_summary",
 "tags": "topic tags relevant to this entry",
 "complexity_assessment": "qualitative complexity ranking from the description"
 }
}
```

> **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md):** the original ADR-0002 shape included `fill_token` and `fill_token_expires_at` here. Both removed — the entity's own `id` is the durable callback handle (no expiry, no in-memory token registry). See [ADR-0008 §"Callback ID = entity ID"](./0008-vault-as-source-of-truth.md).

Returned when the plugin completed but the entity has unfilled fields the agent's AI is expected to provide. The agent runs AI on `clean_content`, fills the named `gaps`, POSTs to `POST /v1/entities/{id}/fill` (below). ~~`fill_token` is one-shot, time-limited, bound to the entity.~~ **Entity ID is the durable callback handle ([ADR-0008](./0008-vault-as-source-of-truth.md))** — agents fill at any later time using `entity.id`, no token registry, no expiry. ADR-0005 documents the full plugin/agent feedback loop.

**`gaps` is a `{field → description}` object.** Keys are the data field names the agent must fill (the `fill` endpoint validates submitted field names against the entity's current gap set, read fresh from the vault frontmatter on every call — per [ADR-0008](./0008-vault-as-source-of-truth.md); see §`POST /v1/entities/{id}/fill` below). Values are short human-readable descriptions guiding the AI on what each field should contain — descriptions are advisory and never feed back into validation; the keys are the contract.

**Entity ID is frozen at ingest.** The `entity.id` returned in a `needs_fill` response is the canonical ID; the fill endpoint cannot re-mint it. AI-filled fields that would feed the slug rule (e.g., `data.title`) do not change the ID — the deterministic plugin output produced the slug at ingest time. The `estimated_entity_id` field documented below applies only to the `queued` 202 shape; it does not appear in `needs_fill` responses (which already carry a full `entity.id`).

**`clean_content` is plugin-processed, not raw upstream.** The plugin is responsible for cleaning the upstream payload before passing it forward — for BGG, that means dropping the legacy XML wrapper, blank space, and useless tags, and serializing the actual game data (description paragraphs, alternative names, designer/publisher refs, etc.) into a focused JSON shape. The agent's AI receives the cleaned form, not the raw API body. This makes the field name accurate and keeps the size manageable: a typical cleaned game record is a few KB.

**Plugin specificity is what makes cleaning possible.** The BGG plugin knows BGG: where descriptions live, which tags are useful, which fields are noise. A generic-API plugin couldn't clean because it has no model of signal vs noise for any specific source. This is the architectural reason for one-collector-per-domain (see ADR-0005); the cleaning step is the load-bearing payoff.

**Plugin internal cache vs API surface.** Plugins MAY internally retain the raw upstream payload (rate-limit avoidance, retry, refetch, replay) — that is plugin-internal state, not part of the contract. Only `clean_content` crosses the API surface, only `clean_content` lands in the vault, and the agent AI only ever sees the cleaned form. Raw upstream never reaches the vault or the agent.

**`clean_content` size bound.** The server bounds the cleaned content to `SERVER_CLEAN_CONTENT_MAX` bytes (configurable; default 64 KiB — comfortably fits typical cleaned records with headroom for unusually rich entries; revisit if a collector legitimately needs more). Content larger than the bound is byte-truncated at the boundary, and the response carries `clean_content_truncated: true`. Agents may attempt fill on truncated content or fail with an extraction error indicating insufficient input. If a plugin's cleaned output exceeds the bound regularly, that is a signal to review the cleaning step (the plugin should produce focused output, not pass-through bulk).

`estimated_entity_id` semantics:

- **Present and reliable** when the canonical ID can be derived without the extracted data: cache hits being re-extracted, structured collectors where the slug rule operates on URL/source-internal-ID alone, and any case where the entity already exists at job start (e.g. a previous ingest minted the ID).
- **Omitted (field absent)** when the canonical slug is data-derived and the data isn't yet known — the typical unstructured / AI-extraction case. The server cannot honestly predict the slug because the slug rule reads `data.title` and the title hasn't been extracted yet.

Client decision tree from the 202 response:

1. **`estimated_entity_id` present** → polling by entity is reliable. Client may **poll**: `GET /v1/entities/{estimated_entity_id}` until the latest provenance entry shows the extraction landed (or failed).
2. **`estimated_entity_id` absent** → polling by entity is not reliable; **re-call `POST /v1/ingest`** with the same URL is the canonical fallback. Idempotency-on-URL means the server returns the existing in-flight job and waits up to another `wait_seconds`. This is also the simplest "just retry" pattern when the field is present.
3. **Wait and ignore** is always available: extraction continues server-side. Next time any agent path resolves the entity, it'll be there.

**Response 400:** validation error (URL malformed, scheme is not http/https, `wait_seconds` outside `[0, 300]`, request body not JSON).

**Response 422 `unsupported_url`:** request well-formed but no registered plugin's `url_patterns` (per [ADR-0006](./0006-plugin-discovery-config-allowlist.md)) matches the URL — the server cannot dispatch this URL with the currently-loaded plugin set. Distinct from 400: the request itself is fine; the server's plugin allowlist just doesn't cover this surface. Operators register a plugin (or re-check `--config`) to enable the URL family.

**Response 404 `not_found`:** a registered plugin claimed the URL but produced no result — empty FetchResult (no entity, no options, no gaps). Distinct from 422 `unsupported_url` (no plugin claimed the URL at all) and from 500 `internal_error` (plugin or store errored mid-flight). Per [ADR-0006](./0006-plugin-discovery-config-allowlist.md), an empty options array specifically does NOT mean "disambiguation with zero candidates"; the server treats "plugin returned nothing" identically regardless of which fields it left empty.

**Response 504:** the server's internal extraction deadline expired (separate from `wait_seconds`, which is the request-side wait). **Server invariant:** every ingest attempt that reaches the extractor commits a placeholder entity with a failure provenance entry, so a client receiving 504 can always inspect the entity (when `estimated_entity_id` is known) or re-call ingest (always works) to find the failure record. There is never a "504 with no record" case. Clients can retry per their own policy.

### `GET /v1/entities/{id}/context`

Stitch context: return an entity plus all entities reachable within N edge-hops, in one round-trip. Added per yaad-index the source issue to support cross-source context-assembly use cases (a PR-review agent fetching the PR + linked Jira ticket + linked Confluence doc + canonical process stub in one shot) without round-tripping per hop. See also [`GET /v1/entities/{id}`](#get-v1entitiesid) — that endpoint returns inline edge refs but not the linked entities themselves; this endpoint is the multi-hop variant.

**Request:**
- Path: `id` — canonical entity ID; the BFS root.
- Query: `depth` — required, integer 0–3. Server caps the depth at 3 (`depth=4+` returns `400 invalid_argument` with `field: "depth"`). No implicit default — caller MUST specify.
- Query (optional): `edge_types` — comma-separated edge type filter (e.g. `edge_types=is_about,references`). When set, only edges of the named types are walked AND only those edges appear in the response. Empty / absent → no filter.
- Query (optional): `max_results` — bounds the `neighbors` array. Default 200, cap 1000. `0`, negative, or above the cap returns `400 invalid_argument` with `field: "max_results"`.

**Response 200:**
```json
{
 "root": { "id": "wikipedia:susanna-clarke", "kind": "wikipedia", "data": {...}, "provenance": [...], "edges": [...] },
 "neighbors": [
 {
 "edge": {"type": "is_about", "from": "wikipedia:susanna-clarke", "to": "person:susanna-clarke"},
 "entity": { "id": "person:susanna-clarke", "kind": "person", "data": {...}, "provenance": [...], "edges": [...] },
 "depth": 1
 },
 {
 "edge": {"type": "authored", "from": "person:susanna-clarke", "to": "book:jonathan-strange-and-mr-norrell"},
 "entity": { "id": "book:jonathan-strange-and-mr-norrell", "kind": "book", "data": {...}, "provenance": [...], "edges": [...] },
 "depth": 2
 }
 ],
 "truncated": false
}
```

**Semantics:**

- **Cycle detection.** Each entity appears at most once across `root` + `neighbors` (by id). If the BFS would revisit an already-included entity, the back-edge is dropped — the entity is already in the result set under its earlier-depth visit. Forward-edges between two not-yet-visited entities at the same depth are also deduped (the first edge to introduce a neighbor wins; v1 doesn't surface multi-edge metadata between the same pair).
- **Depth bounding.** `depth=0` returns root + empty neighbors. `depth=N` returns all entities reachable within N edge-hops, with each neighbor's `depth` field naming the BFS level at which it was first reached.
- **Edge-type filter.** When `edge_types` is set, only edges of the named types are walked AND only those edges appear in `neighbors[].edge`. Unwalked edges are omitted entirely — the agent decides whether the omission means "no such edge" (filter excluded the type) or "no such relationship" (no edge of any type) by re-calling without the filter.
- **Pagination.** BFS order — depth-major (depth-1 entries before depth-2, etc.; arbitrary within a depth). When the unbounded result would exceed `max_results`, the response is truncated to the prefix that fit and `truncated: true` is set. Truncation can leave a partial frontier — the agent decides whether to re-call with `edge_types` narrowing or accept the partial.
- **Single batched query.** Implementation walks the graph as one recursive CTE OR N pre-batched point-lookups (one SQL query per depth level via `GetEdgesForMany` plus one `GetEntities` resolve per level). Not N independent round-trips.
- **Direction.** v1 walks outbound edges only (`from` → `to`). Reverse traversal is a future iteration (`direction=both` query param if needed); not in v1.

**Response 404 `not_found`:** path `id` doesn't resolve to any entity in the vault. Same envelope shape as other 404s.

**Response 400 `invalid_argument`:** `depth` missing / out of range, `max_results` non-positive or above cap, malformed query value. The envelope carries a `field` discriminator (`"depth"` / `"max_results"`) so agents can correlate the rejection with the offending parameter without re-deriving it from the message:

```json
{ "ok": false, "error": "invalid_argument", "message": "must be in [0, 3]", "field": "depth" }
```

### `POST /v1/entities/batch`

Fetch multiple entities in one call. Saves agents from N sequential `GET /v1/entities/{id}` calls when assembling context for a query.

**Request body:**
```json
{
 "ids": ["boardgame:brass-birmingham", "person:martin-wallace"],
 "with_edges": ["designed_by", "authored_by"]
}
```

- `ids` — required, array of canonical IDs (1–100 per call).
- `with_edges` — optional, edge types to expand on each entity (same semantics as the single-entity endpoint).

**Response 200:**
```json
{
 "ok": true,
 "entities": [
 { ...entity 1... },
 { ...entity 2... }
 ],
 "missing": ["person:nobody"]
}
```

`entities` contains every requested ID that exists; `missing` lists IDs the server has no record of. Unlike `GET /v1/entities/{id}`, the batch endpoint never returns 404 for individual misses — partial results are normal.

**In-flight entities are *not* missing.** If a client requests an ID for which an ingest is currently extracting (placeholder entity with a pending provenance state but no completed `data` yet), the entity is returned in `entities` with whatever placeholder fields the storage layer has — `data` may be `null` or sparse, the provenance array makes the in-flight state machine-readable. `missing` is reserved exclusively for IDs the server has never seen.

Rationale for inclusion in v1 (was deferred): per the operator's review, the "50 calls is cheap on LAN" argument is sketchy for typical agent workloads where pulling 5–20 related entities at once is the common case. Adding `batch` to v1 is small (one handler, no new schema) and removes an obvious agent-side foot-gun where a chained tool call balloons into N round-trips.

### `POST /v1/entities/{id}/fill`

Submit AI-extracted field values to complete an entity returned by a `needs_fill` ingest response.

The agent receives `{entity, clean_content, gaps}` from a `POST /v1/ingest` that resulted in `status: "needs_fill"`. The agent runs its AI on `clean_content` (with the partial `entity` as context, and the `gaps` as the target field set), then submits the AI's output here.

> **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md):** the original ADR-0002 tuple was `{entity, clean_content, gaps, fill_token}`. The `fill_token` member is removed — `entity.id` is the durable callback handle.

~~**Token vs path.** The `fill_token` is **authoritative** for entity binding — it is one-shot, time-limited, and bound to a specific entity at issue time. The path `{id}` is a defense-in-depth verification check: the server validates path equals the token-bound entity and rejects mismatches with `id_mismatch`, but the path itself carries no semantics the token does not already establish. Agents should not rely on path-derived behavior; the token defines the contract.~~

**Entity ID is authoritative ([ADR-0008](./0008-vault-as-source-of-truth.md)).** The path `{id}` IS the contract — there is no separate token. The server reads the entity's current `gaps:` set from the vault frontmatter and validates submitted field names against it. No expiry; agents may fill at any later time (seconds, hours, days).

**Path:**
- `id` — canonical entity ID, ~~must match the partial entity's id from the ingest response. Verified against the token's bound entity; mismatches return `id_mismatch`.~~ resolves directly against the vault. Unknown ID returns `404 not_found`.

**Request body:**
```json
{
 "fields": {
 "summary": "Heavy economic euro by Martin Wallace, set in industrial-revolution Birmingham...",
 "tags": ["heavy-euro", "economic", "engine-building"],
 "complexity_assessment": "..."
 }
}
```

> **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md):** the original request body included `"fill_token": "ft_abc123"`. Removed.

- ~~`fill_token` — required, the token from the `needs_fill` 202 response.~~ (Removed by [ADR-0008](./0008-vault-as-source-of-truth.md).)
- `fields` — required, an object whose keys must be a **subset of the entity's current `gaps:` set** (read fresh from the vault on every call). ~~must **equal the set of `gaps`** from the `needs_fill` response: every gap must be filled, no extras, no omissions. Partial fills are rejected with `incomplete_fill` (carries `missing: [...]`); unknown keys are rejected with `unknown_field`. Rationale: a partial fill would leave the entity in an undefined state — silently complete-looking from the storage layer, but with gaps no second pass can recover (the original `fill_token` is one-shot, and a follow-up `needs_fill` would re-issue every gap, including the ones already filled). An agent that cannot extract every gap must fail loud on its side (e.g., re-issue ingest with `force_refetch=true` after improving the source) rather than submit an incomplete fill.~~ **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md): partial fills are first-class.** Submit a subset; the remaining gaps stay open for a future call. Any submitted key not in the entity's current gap set (because it was already filled by another agent, by the operator's hand-edit, by reindex picking up an external source, or was never a gap to begin with) causes **per-call atomic** rejection with `409 fill_conflict` — no partial success, the call fails as a whole and the response carries `rejected: [...]`.

**Response 200:**
```json
{
 "ok": true,
 "entity": { ...complete entity, full shape as GET /v1/entities/{id}, with fields merged + a new provenance entry recording the agent fill... }
}
```

The agent that submitted the fill is recorded in the new provenance entry. Provenance shape for an agent fill:

```json
{ "source": "agent:bob", "filled_at": "2026-04-28T19:30:00Z", "ok": true }
```

The agent identity goes in `source` (no embedded date — parallel to plugin provenance which is `"source": "bgg:224517"` plus `"fetched_at": "..."`); the fill timestamp goes in `filled_at`, semantically parallel to the plugin's `fetched_at`. Future readers can tell which agent's AI produced which fields, and when. Future agents querying this entity get the merged version from cache — the AI work was paid once, served to many.

~~**Response 410 Gone:**~~
~~- Token expired (past TTL, default 5 min).~~
~~- Token already used (one-shot).~~

~~The recovery path is `POST /v1/ingest` with the original URL: idempotent ingest reuses the cached partial entity and issues a fresh `fill_token` without re-running the plugin. Adding `force_refetch=true` re-runs the plugin too, for cases where the partial itself is stale.~~

> **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md):** Response 410 is gone — there is no token, no expiry. An entity whose gaps were all filled out-of-band simply rejects every submitted key as a `409 fill_conflict`. If the agent wants a fresh re-fetch from upstream, `POST /v1/ingest` with `force_refetch=true` still works (independent of the fill loop).

**Response 409 `fill_conflict`** ([ADR-0008](./0008-vault-as-source-of-truth.md)) — one or more submitted field names are not in the entity's current gap set. Per-call atomic: the call fails as a whole with `rejected: [...]` listing the offending names. The agent decides recovery — re-fetch the current entity (`GET /v1/entities/{id}`) to see what's still open, or accept that the fill was redundant.

**Response 404 `not_found`:** the path `id` doesn't resolve to any entity in the vault. Distinct from `409 fill_conflict` (entity exists; gap-set mismatch).

~~**Response 400:**~~
~~- `incomplete_fill` — `fields` does not cover every gap from the `needs_fill` response. Response carries `missing: ["gap_a", "gap_b", ...]`.~~
~~- `unknown_field` — `fields` contains a key not in the original `gaps`.~~
~~- `id_mismatch` — path `id` doesn't match the entity bound to the token (verification-only check; the token is authoritative).~~
~~- `invalid_token` — token format malformed or never issued.~~

> **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md):** the four 400 codes above are obsolete. The current contract collapses them into one signal — `409 fill_conflict` for any submitted key not in the entity's current gap set, atomic per-call. There is no `incomplete_fill` (partial fills are first-class), no `unknown_field` (folded into `fill_conflict`), no `id_mismatch` (path is authoritative; unknown id → 404), no `invalid_token` (no token).

### `POST /v1/edges`

Create or upsert a typed relationship between two entities.

The ingest pipeline already writes edges that the collector can extract automatically (a boardgame ingest also writes its `designed_by` edges to person entities). `POST /v1/edges` is the explicit surface: agents adding relationships the auto-extraction didn't produce, the CLI back-filling cross-collector edges, or a collector that learned about an edge through a second pass.

**Request body:**
```json
{
 "type": "designed_by",
 "from": "boardgame:brass-birmingham",
 "to": "person:martin-wallace",
 "metadata": { "role": "lead designer" }
}
```

- `type` — required, the edge kind. Must be in the set returned by `GET /v1/kinds` (see `edge_kinds`).
- `from` — required, canonical ID of the source entity. Must exist.
- `to` — required, canonical ID of the target entity. Must exist.
- `metadata` — optional, free-form JSON for kind-specific edge attributes (e.g. `role`, `chapter`, `co_designer`).

**Response 200:**
```json
{
 "ok": true,
 "edge": {
 "type": "designed_by",
 "from": "boardgame:brass-birmingham",
 "to": "person:martin-wallace",
 "metadata": { "role": "lead designer" },
 "provenance": [
 {
 "source": "agent:bob-2026-04-28",
 "fetched_at": "2026-04-28T17:50:00Z",
 "ok": true
 }
 ]
 }
}
```

**Idempotency:** `(type, from, to)` is the edge identity. Re-posting the same triple updates the metadata and appends a provenance entry; it does not create a duplicate.

**Response 400:** validation error — `type` not in `edge_kinds`, `from`/`to` malformed.

**Response 422:** `missing_entity` — `from` or `to` references an entity id the server has no record of. The offending ID is carried in `message`. The request itself is well-formed; it can't be processed because of a referential-integrity gap (RFC 9110 §15.5.21 unprocessable content). The server does not auto-create endpoints; ingest the entities first.

**In-flight entities are valid edge endpoints.** If `from` or `to` references an entity that's currently extracting (placeholder row exists, `data` may be `null` or sparse), the edge is created normally — the placeholder is a real row, identity is stable, and the edge will simply expand against the populated entity once extraction completes. This matches the in-flight semantics in `POST /v1/entities/batch`.

**Provenance** mirrors the entity model: each edge keeps an array of `{source, fetched_at, ok, error?, error_message?}` entries so a client can tell which collector saw the relationship and when.

**Edge kinds are plugin-owned**, same as entity kinds. The bgg plugin owns `designed_by` (boardgame → person), `published_by` (boardgame → company); the books plugin owns `authored_by` (book → person), `illustrated_by` (book → person). Edge kinds are validated against the active-plugin enumeration at write time.

### `GET /v1/kinds`

Return the kinds supported by the running server, populated from active ingestion plugins. Returns both entity kinds and edge kinds in one call so a client can validate ingest input + edge writes against the same response.

**Query params (all optional):**
- `with_descriptions` — default `true`; if `false`, return only names.

**Response 200:**
```json
{
 "ok": true,
 "entity_kinds": [
 {
 "name": "boardgame",
 "description": "A boardgame as catalogued on BoardGameGeek.",
 "source_plugins": ["bgg"]
 },
 {
 "name": "book",
 "description": "A book referenced by an authored_by edge.",
 "source_plugins": ["books"]
 },
 {
 "name": "person",
 "description": "A designer, author, or contributor referenced by another entity.",
 "source_plugins": ["bgg", "books"]
 }
 ],
 "edge_kinds": [
 {
 "name": "designed_by",
 "description": "Relates a boardgame to its designer(s).",
 "from_kind": "boardgame",
 "to_kind": "person",
 "source_plugins": ["bgg"]
 },
 {
 "name": "authored_by",
 "description": "Relates a book to its author(s).",
 "from_kind": "book",
 "to_kind": "person",
 "source_plugins": ["books"]
 }
 ]
}
```

Each entity kind names its source plugins so a client can reason about availability (e.g. "is the bgg plugin loaded? then `boardgame` ingest will work"). Each edge kind additionally declares its `from_kind` and `to_kind` so clients can construct valid edges without trial-and-error.

The plugin-management surface itself (load/unload, plugin metadata, etc.) is **out of scope for this ADR** and is owned by a future ADR on plugin lifecycle. `GET /v1/kinds` is the read-only window into plugin state for callers that just need to know what's available.

**Orphan handling on plugin unload** — what happens to entities and edges of a kind whose owning plugin was unloaded — is a question for the plugin-lifecycle ADR, not this one. The placeholder answer is "data persists; the kind disappears from `GET /v1/kinds` until the plugin re-loads"; the plugin-lifecycle ADR will commit to that or an alternative.

This endpoint resolves Open Question 1 (kind enumeration owner) — kinds live with the plugins; the registry is the runtime composition of active plugins; this endpoint is the canonical query for both entity and edge kinds.

### `GET /v1/needs-fill`

Pull-based batch gap-call surface (per [ADR-0013](./0013-canonical-kind-owns-gap-contract.md) §6, yaad-index). Returns entities currently gap-callable — those whose `gap_call_done_at` is NULL (per ADR-0013 §4 lifecycle) AND whose vault frontmatter carries unfilled gaps. Each entry on the response is the same per-entity needs_fill payload the cache-hit `POST /v1/ingest` envelope emits.

Use cases: cron-driven batch fills, multi-agent coordinator dispatch. Direct AI failure recovery (single-entity case) does NOT need this endpoint — a failed fill leaves the flag unset, so a direct ingest still returns `needs_fill`. This endpoint is for batch operations.

**Request:**
- Query (optional): `limit` — bounds the `entities` array. Default 50, cap 200. Bad values (non-integer, ≤ 0) silently default to 50 — lenient. Values > 200 silently clamp to 200.
- Query (optional): `cursor` — opaque pagination token from a previous response's `next_cursor`. Internally a base64(last-seen-id); clients treat it as opaque. Absent on the first request. Malformed base64 returns `400 invalid_argument` with `field: "cursor"` (sketch — current implementation surfaces the error in the message).

**Response 200:**
```json
{
 "ok": true,
 "entities": [
 {
 "id": "wikipedia:tolkien",
 "kind": "wikipedia",
 "gaps": {"birth_place": "", "occupation": ""},
 "instruction": "<resolved per ADR-0013 §2>",
 "canonical_vocabulary": {...},
 "clean_content": "..."
 }
 ],
 "next_cursor": "opaque-base64-or-omitted"
}
```

**Per-entity payload shape parity.** The `instruction` resolution (per-kind override → global → omit per ADR-0013 §2), `canonical_vocabulary` field (registry verbatim or omitted), and `gaps` map (field-name → AI-prompt) match the cache-hit ingest envelope exactly. Both surfaces share the `buildNeedsFillEntry` helper so a refactor that drifts one shape immediately breaks the other's regression suite.

**Pagination semantics.** Order is `id ASC` — deterministic across calls. The cursor is set when the candidate-stream wasn't exhausted by the current page (the DB returned a full `limit`-row chunk); omitted when the stream is exhausted. The handler vault-reads each candidate to filter on `Gaps`-non-empty; entities with all gaps filled are dropped from the response but the cursor still advances past them, so subsequent pages don't re-consider the same id. Returned page size may be smaller than `limit` if vault filtering dropped some candidates; the client uses `next_cursor` to keep paginating until `null`.

**Out of scope.** Push-via-channels (webhook/SSE) — v2. Per-entity filters (e.g. by kind) — v2. Sort modes other than `id ASC` — premature. v1's id-only cursor is sufficient; future (created_at, id) compound is deferred.

### `GET /v1/cv-status`

Canonical-vocabulary drift surface (per [ADR-0013](./0013-canonical-kind-owns-gap-contract.md) §3, yaad-index). Surfaces the per-(plugin, kind / edge_type) drop counters the orchestrator increments at ingest time when a plugin emits a canonical stub or canonical edge type that the operator's `canonical_kinds:` / `canonical_edge_types:` config didn't enable. Today the only signal is startup WARN logs that scroll; this endpoint replaces "you need to remember to look" with "the counter is in the surface."

**Request:** no body, no query params.

**Response 200:**
```json
{
 "ok": true,
 "config_hash": "<truncated SHA over canonical_kinds + canonical_edge_types>",
 "drift": {
 "kinds_emitted_not_enabled": [
 { "plugin": "wikipedia", "kind": "person", "would_materialize_count": 3 }
 ],
 "kinds_enabled_not_emitted": [],
 "edge_types_emitted_not_enabled": [
 { "plugin": "wikipedia", "edge_type": "is_about", "would_materialize_count": 8 }
 ],
 "edge_types_enabled_not_emitted": []
 },
 "last_reindex_at": "2026-05-04T15:45:18Z",
 "reindex_hint": "POST /v1/reindex to materialize stubs after enabling kinds/edges in config"
}
```

**Field semantics:**
- `config_hash` — deterministic SHA over the canonical-vocabulary subset of the config (canonical_kinds + canonical_edge_types). Bumps on any change to the canonical config; stable across calls otherwise. Operator tooling polls + diffs to detect drift between snapshots. Distinct from `/v1/structure`'s `version` (which covers the full structural signature including plugins). Edge-types sorted before hashing — same contract as `/v1/structure` per yaad-index a prior PR.
- `drift.kinds_emitted_not_enabled[]` — per-(plugin, kind) counter rows. The `would_materialize_count` field is the cumulative count of canonical entity stubs the plugin emitted that the orchestrator's config-filter dropped at ingest time. Persisted in DB so it survives daemon restart; sourced from the `dropped_canonical_kinds` table (yaad-index a prior PR).
- `drift.edge_types_emitted_not_enabled[]` — same axis for canonical edge types.
- `drift.kinds_enabled_not_emitted` / `drift.edge_types_enabled_not_emitted` — stubbed empty arrays for v1 per ADR-0013 §3. "Operator enabled X but no plugin emits it" is a different signal that lands in a follow-up.
- `last_reindex_at` — `MAX(last_indexed_at)` across the `reindex_files` table; null when no reindex has ever run.
- `reindex_hint` — static guidance string. After enabling kinds in config, the operator calls `POST /v1/reindex` to materialize the stubs from vault state (force-refetch was rejected per ADR-0013 §3 as too aggressive).

**Out of scope** per ADR-0013 §3 + the dispatch:
- Auto-reindex on detected drift (force-refetch rejected; reindex stays operator-prompted).
- Push-via-channels for drift state changes (v2).
- The `kinds_enabled_not_emitted` heuristic (v2 — different signal).

### `GET /v1/structure`

Introspection endpoint (per [ADR-0013](./0013-canonical-kind-owns-gap-contract.md) §7, yaad-index). Returns the running instance's structural signature: enabled canonical kinds (with gaps + per-kind instructions), enabled canonical edge-types, and active plugin metadata. Operator tooling polls + diffs the `version` field to detect rebuild / config-change / plugin-add-remove-upgrade transitions.

**Request:** no body, no query params.

**Response 200:**
```json
{
 "ok": true,
 "version": "<deterministic-config-hash>",
 "kinds": {
 "person": {
 "is_canonical": true,
 "gaps": {"name": "Full name.", "summary": "..."},
 "instruction": "<verbatim from per-kind config; omitempty when absent>"
 },
 "boardgame": {...}
 },
 "edge_types": ["designed_by", "is_about", "..."],
 "plugins": [
 {
 "name": "yaad-wikipedia",
 "version": "1.2.3",
 "url_patterns": ["https://*.wikipedia.org/wiki/*"],
 "supports_search": true,
 "emits_kinds": ["wikipedia-article"],
 "emits_edges": ["is_about", "references"]
 }
 ]
}
```

**Field semantics:**
- `version` — SHA-256-truncated deterministic hash over `(canonical_kinds, edge_types sorted, plugins sorted-by-name)`. Bumps on rebuild, config change, or plugin add/remove/upgrade. Stable across calls when none of the above change. Operator-side decision when to act on a bump (the system does NOT auto-reindex per ADR-0013 §8).
- `kinds` — populated from `cfg.CanonicalKinds`. `is_canonical: true` is locked-in for v1; reserved for future "passthrough" kinds.
- `edge_types` — sorted list from `cfg.CanonicalEdgeTypes`.
- `plugins` — sorted-by-name list from the loaded plugin registry. Each entry comes from the plugin's `--init` capabilities, cached at startup.

**Out of scope** (per ADR-0013 §7 + the dispatch's explicit exclusions):
- `POST /v1/plugins/refresh` (refresh-on-demand) — owned by a future plugin-management ADR per ADRs 0005/0006.
- `GET /v1/structure/diff?since=<version>` (server-computed diff) — clients diff snapshots themselves in v1.
- Push-via-channels for structure changes — v2.
- Per-vault / multi-tenant CV — single-tenant in v1.

### `GET /v1/search`

Search entities by text and/or kind.

**Query params:**
- `q` — free-text search query. Optional when `kind` is supplied.
- `kind` — filter by entity kind (`boardgame`, `wikipedia-article`, `person`, …). Optional when `q` is supplied.
- `limit` — optional, default 20, max 100
- `offset` — optional, default 0

**At least one of `q` or `kind` is required.** Both empty → `400 invalid_argument: q or kind is required`. Listing every entity across every kind is `GET /v1/entities/batch` territory (when known ids are in hand); this endpoint is for filtered discovery.

**Three call shapes:**

1. **`q` set, `kind` empty** — text search across every kind. Existing behaviour.
2. **`q` set, `kind` set** — kind-filtered text search.
3. **`q` empty, `kind` set** — list every entity of that kind, paginated. Useful for "show me every Wikipedia article I've ingested" — agents and the CLI hit this for discovery.

**Response 200:**
```json
{
 "ok": true,
 "results": [
 {
 "id": "boardgame:brass-birmingham",
 "kind": "boardgame",
 "snippet": "Heavy economic strategy game set in industrial Birmingham…",
 "score": 0.92
 }
 ],
 "total": 47,
 "limit": 20,
 "offset": 0
}
```

**Snippet derivation.** Each result carries a `snippet` — a short prose preview of the entity, suitable for in-line display without a follow-up `GET /v1/entities/{id}`. The handler picks the snippet source per entity kind:

1. If the search backend (FTS5, etc.) populated a snippet on the hit, that wins.
2. Otherwise, the handler walks the entity's `data` for the first non-empty string field in the kind's `snippet_fields` (advertised by the plugin via `KindSpec.snippet_fields` in its `--init` capabilities).
3. Plugins that don't advertise `snippet_fields` fall back to a default chain: `description` → `summary` → `extract` → `content`.
4. The chosen field is truncated to `YAAD_INDEX_SNIPPET_MAX_CHARS` (default 200) at a rune boundary, with an ellipsis suffix if truncated.

If no candidate field is non-empty, `snippet` is the empty string — clients tolerate this. Entities with prose-y data populate naturally; structured-only entities (e.g. a `person` with just `name` and `dob`) leave snippet blank.

## Cross-cutting decisions

### Content negotiation

JSON only in v1. `Accept: application/json` is required (server returns `406 Not Acceptable` otherwise). MessagePack and other formats can land in a later ADR if needed; deferring keeps the implementation small.

### Versioning

URL-prefix versioning: `/v1/...`. Breaking changes ship under `/v2/...` with `/v1` running in parallel until clients migrate. No header-based or content-type-based versioning.

### Error envelope

All non-2xx responses return:

```json
{ "ok": false, "error": "<code>", "message": "<human-readable>" }
```

Error codes are machine-readable strings, not HTTP status numbers. Stable across versions; new codes can be added but existing ones never change meaning.

Initial code set:
- `not_found` (404)
- `invalid_argument` (400)
- `conflict` (409) — fill-conflict per [ADR-0008](./0008-vault-as-source-of-truth.md): any submitted `/fill` key not in the entity's current `gaps:` set. Per-call atomic; response carries `rejected: [...]`.
- `missing_entity` (422) — referenced entity not found; request well-formed but referentially unprocessable (see `POST /v1/edges`).
- `unsupported_url` (422) — `POST /v1/ingest` URL well-formed but no registered plugin's `url_patterns` claim it (per [ADR-0006](./0006-plugin-discovery-config-allowlist.md)). Server cannot dispatch with the currently-loaded plugin set; operator registers a plugin (or re-checks `--config`) to enable the URL family.
- `not_acceptable` (406)
- `unsupported_media_type` (415)
- `vault_required` (503) — state-mutating endpoints (`/fill`, `/notes`) reject when `vault.path` is not configured (per [ADR-0008](./0008-vault-as-source-of-truth.md) §"Vault layout"). Ingest stays DB-only as a backwards-compatible fallback.
- `internal_error` (500)
- `collector_unavailable` (502)
- `collector_timeout` (504)

> **Amended by [ADR-0008](./0008-vault-as-source-of-truth.md):** the original ADR-0002 fill section introduced `incomplete_fill` (400), `unknown_field` (400), `id_mismatch` (400), and `invalid_token` (400). All four removed when ADR-0008 collapsed the fill-validation contract into a single per-call atomic `409 fill_conflict`. See §`POST /v1/entities/{id}/fill` for the current shape.

### Authentication / authorization

**Pair-claim Bearer JWT (RS256) on protected routes.** Per yaad-index/. The original v1 stance ("None — network topology is the trust boundary") was relaxed when the deployment surface widened beyond the home LAN; auth lands as middleware so the endpoint shapes from this ADR do not change.

**Token model** : every token carries `sub` (the agent calling the API) AND `operator` (the human resource owner) — the OAuth resource-owner / client split. Audit trail is `(operator, agent)` for every action; a per-agent revocation does not rotate operator-side trust. RS256 signed; `iss` is `yaad-index`; `kid` defaults to `yaad-index-default` (rotation lands in a follow-up).

**Wire** :
- Clients send `Authorization: Bearer <token>` on every request to a protected route.
- The server verifies via `<keys_dir>/public.pem` (default `/etc/yaad-index/keys/`).
- On success, the parsed `auth.Claim` is attached to the request context; downstream handlers retrieve it via `api.ClaimFromContext(ctx)`.
- On failure, the canonical 401 envelope is emitted with one of the codes:
 - `missing_authorization` — header absent or not `Bearer <token>`
 - `token_expired` — `exp` claim in the past
 - `wrong_key` — signature does not verify against `public.pem`
 - `invalid_token` — anything else (malformed JWT, wrong issuer, …)

**Protected routes** (require a valid token):

- `GET /v1/kinds`
- `POST /v1/entities/batch` (and `GET /v1/entities/batch` 405 carve-out)
- `GET /v1/entities/{id}`
- `GET /v1/entities/{id}/context`
- `GET /v1/search`
- `POST /v1/ingest`
- `GET /v1/needs-fill`
- `POST /v1/entities/{id}/fill`
- `POST /v1/entities/{id}/notes`
- `POST /v1/edges`
- `POST /v1/reindex`

**Public routes** (intentionally unauthenticated — system metadata, no vault data):

- `GET /v1/health` — liveness probe; monitors must reach this without a token.
- `GET /v1/structure` — operator-visible vocabulary signature (canonical kinds + edge types). Vocabulary metadata, not entity content.
- `GET /v1/cv-status` — config-hash + drift state for the canonical-vocabulary registry. Same metadata-only justification.
- `GET /v1/jwks` — the public-key endpoint per [RFC 7517](https://datatracker.ietf.org/doc/html/rfc7517); MUST be reachable without a token so cooperating agents can verify peer tokens (the bootstrap is otherwise circular). Registration is contingent on a readable keypair on disk: dev-mode without keys leaves the route unregistered (404). Cache-Control: `public, max-age=3600`. Single-key v1; the JSON shape supports multi-key for forward-compat with rotation. Per yaad-index.

The split is enforced at routing time in `internal/api/api.go`. The default-protected rule: **any route exposing operator data (entities, edges, ingest) is protected; only health-check and vocabulary-introspection metadata stays public.**

**Dev-mode bypass** (`auth.required=false`): the operator opts out explicitly via the config file (`auth.required: false`) or `--auth-required=false` / `YAAD_INDEX_AUTH_REQUIRED=false`. The middleware then attaches a synthetic anonymous claim (`sub=anonymous`, `operator=none`) and skips verification. The server logs a startup warning so running disabled is never silent. **Default is `required=true`**; production deployments inherit the safe default without any operator action.

**Path precedence chain (locked):** CLI flag > env var > config file > built-in default — the same chain used for `keys_dir` and `default_ttl` . For `auth.required` the built-in default is `true`; for `keys_dir` it is `/etc/yaad-index/keys/`; for `default_ttl` it is `2160h` (90 days).

Note-author validation (the JWT's `sub` must match `POST /v1/entities/{id}/notes` body's `author`) lands in a prior PR. The full auth series (a prior a prior PR keypair + sign/verify + CLIs, a prior a prior PR middleware, a prior a prior PR note validation, a prior a prior PR /v1/jwks) closes out the auth surface area for v1.

### Rate limiting

Out of scope for v1. Single-tenant + small known-agent fleet means contention is unlikely in the near term. If/when it matters, lands as middleware.

### Idempotency

`POST /v1/ingest` is **idempotent on URL**. A second ingest of the same URL within a short window (TTL TBD in storage ADR) joins (or returns) the existing in-flight or completed entity, not a duplicate. `force_refetch=true` overrides this.

### Canonical-ID stability under source rename

When a source renames an entity (BGG retitles a game, a blog renames a post slug, etc.), the question is whether re-ingest of the (possibly redirected) URL creates a new entity or updates the existing one.

**Decision:** **canonical IDs are stable from first ingest. Source URL is the lookup key. Renames stay a collector concern.** The flow:

1. First `POST /v1/ingest` of a URL mints a canonical ID (slug derived from `data.title` at that moment, prefixed by kind).
2. The entity stores the source-internal ID (e.g. BGG game id `224517`) inside its provenance.
3. A later ingest of the same URL — including a redirect-target — looks up by source-internal ID first, falls back to URL match, finds the existing entity, and **updates `data` plus appends a new provenance entry**. The canonical ID does not change.
4. `data.title` reflects the latest known title; the canonical ID retains the original slug. Display layers can show the new title without breaking links.
5. Edges referencing the entity continue to resolve.

**Alternative considered:** derive the canonical ID from the source-internal stable ID (`boardgame:bgg-224517`) instead of a slug. More robust, no slug-drift concern. **Rejected** for v1 because it makes the canonical IDs non-human-readable in URLs and notes; defeats the readability that motivated slug-form IDs in v0 ADR-0007. The first-ingest-stable approach gets most of the robustness (renames don't fork entities) while keeping `boardgame:brass-birmingham` legible. Source-internal IDs are still stored in provenance for the lookup machinery.

**Implication for v0 ADR-0007:** survives, but the slug rule is now "slug minted at first ingest, never changed by re-ingest." Will surface in the v1 imported version of that ADR.

**Load-bearing assumption (named, not deferred):** this approach assumes **single-source-per-canonical-entity** — exactly one collector produces entities of a given kind, and a real-world thing reaches yaad-index through one source's URLs. If that assumption ever flips (two collectors that can independently produce entities for the same real-world object — e.g. both BGG and a hypothetical TableTopFinder collector emitting entities for the same boardgame), the ID strategy needs revisiting regardless of which option we picked here. Source-internal IDs alone don't solve cross-source dedup either; that's a different problem class (entity resolution / record linkage). Naming this so the trade-off is honest rather than just deferred to a future surprise.

### Long-running ingest jobs

The primary path is **long-poll** on `POST /v1/ingest` (see endpoint above): server holds the request open up to `wait_seconds` (default 60). For most ingestion calls the agent gets an inline entity and never sees the async machinery.

When extraction exceeds `wait_seconds`, the server returns `202`. The client picks a workaround based on whether `estimated_entity_id` is present in the 202 response (see the endpoint section above):

1. **Re-call `POST /v1/ingest`** with the same URL. Always available. Idempotency means the server joins the existing in-flight job and waits up to another `wait_seconds`. Simplest "just retry" pattern, and the canonical fallback when `estimated_entity_id` is absent (typical unstructured ingest).
2. **Poll `GET /v1/entities/{estimated_entity_id}`**. Available only when the field is present in the 202. When the entity exists, ingestion has run at least once. The latest provenance entry signals success or failure: `provenance[len-1].ok === true` means the most recent attempt succeeded; `ok === false` with `error`/`error_message` populated means it failed.
3. **Wait and ignore.** The next time the agent looks (any tool call that resolves the entity), it'll be there.

**Server invariant for polling:** every ingest attempt that reaches the extractor results in either a completed entity or a placeholder entity with a failure provenance entry. Clients polling a known `estimated_entity_id` will always find a record once the attempt finishes — never "still no record after timeout."

**Concurrency cap:** v1 assumes a single-host LAN deployment with a small known agent fleet — long-poll connections are not capped. If yaad-index ever runs behind a load balancer or proxy with idle-connection limits, this assumption needs revisiting.

A dedicated `/v1/ingest/{ingest_id}/status` endpoint is **deferred** to keep v1 small. The combination of long-poll-primary + idempotent re-call + polling-by-entity covers every flow without it. Note that `ingest_id` is no longer surfaced in v1 responses (the field was removed in this ADR's v3-to-v4 revision); a future status endpoint can mint a fresh handle if it lands.

## Consequences

### Positive

- **Five endpoints, all small.** Smallest viable working surface that doesn't force agent-side workarounds for the obvious cases. Storage layer stays opinion-free; collectors are server-internal modules.
- **Long-poll primary on ingest** means the simple-agent case is one call → entity. Async machinery only surfaces when it has to.
- **Idempotency on URL** prevents accidental duplicate fetches when an agent re-runs an ingest call, and is what makes the "just re-call ingest" workaround clean.
- **Batch endpoint** removes the N-round-trip pattern for clients pulling related entities together.
- **Kinds endpoint** lets clients reason about what's available without server-config inspection or out-of-band coordination.
- **No authn now means no authn UX to design.** When tokens land, they're middleware — the rest of the API is untouched.

### Negative / costs

- **Long-poll holds connections.** Up to `wait_seconds` per outstanding ingest. The server has to be configured for long-lived HTTP connections (no aggressive idle-timeout proxy in front). Not a meaningful cost on a LAN deployment; flagged for the day yaad-index runs behind a CDN.
- **No streaming responses.** Search results land all-at-once. If/when result sets grow large enough to matter, paged search via `offset`/`limit` covers it.
- **`force_refetch=true` is a small footgun.** A client can stampede the cache. Acceptable in v1 (small known fleet); future authz can scope this.
- **Plugin lifecycle is out of scope.** `GET /v1/kinds` is read-only on plugin state; how plugins get loaded, configured, version-pinned, etc. is a separate ADR. The dependency is named.

## Open questions

- ~~**Entity `kind` enumeration.**~~ **Resolved in this ADR** (see `GET /v1/kinds` endpoint above): kinds live with the active ingestion plugins; `GET /v1/kinds` is the canonical query; plugin lifecycle (load/unload/version-pin) is owned by a future plugin-management ADR.
- **Search backend.** SQLite FTS5 vs. external (typesense, meilisearch). Lean: FTS5 for v1 — keeps the storage layer single-binary, no extra service. **Concrete revisit triggers (any one of these tips us toward a swap):**
 1. p95 search latency exceeds 200ms on a vault of 10k+ entities
 2. Recall measurably below 0.85 on a hand-curated query set (≥ 50 queries)
 3. A required feature lands that FTS5 doesn't support natively: vector/semantic search, fuzzy/typo tolerance beyond trigram, faceted aggregation
 4. Storage size of the FTS5 index exceeds 5x the entity-data size (signal that it's outgrown the embedded-engine fit)
- ~~**Canonical-ID stability under source rename.**~~ **Resolved in this ADR** (see "Canonical-ID stability under source rename" above): IDs stable from first ingest, URL/source-internal-ID is the lookup key, renames stay a collector concern. Documented decision + alternative considered + implication for v0 ADR-0007.

## Action items if approved

1. Stub the seven endpoints with hardcoded sample responses:
 - `GET /v1/entities/{id}` → fixed `boardgame:brass-birmingham` entity
 - `POST /v1/entities/batch` → returns the same fixed entity for any matching id
 - `POST /v1/ingest` → recorded ingest job (returns 200 inline for cache hit, 202 with `status: "queued"` after `wait_seconds` for unstructured, or 202 with `status: "needs_fill"` when the plugin returned gaps)
 - `POST /v1/entities/{id}/fill` → accepts ~~`{fill_token, fields}`~~ `{fields}` (per [ADR-0008](./0008-vault-as-source-of-truth.md): no token), returns merged entity. ~~Stub behavior (deliberate, callers see real shapes from day 1): token validation accepts any non-empty `fill_token` (the real token store lands at action item 5); `fields` validation accepts any object whose keys are a subset of a hardcoded stub-gap list (real "must equal gaps" check follows once the fill-token store carries the originally-issued gap set per token).~~ ADR-0008 supersedes the stub flow: the production handler reads the entity's current `gaps:` from the vault frontmatter and validates submitted keys against it (atomic 409 `fill_conflict` on any out-of-set key).
 - `POST /v1/edges` → accepts triple, validates against stub `edge_kinds` (`designed_by`, `authored_by`), returns canonical edge shape
 - `GET /v1/search` → single hardcoded result
 - `GET /v1/kinds` → bootstrap entity kinds list (boardgame, person, book, …) **plus** the bootstrap `edge_kinds` list (`designed_by`, `authored_by`, …) as the seed enumeration
2. Define the JSON schemas for request/response bodies in `internal/schema/v1.go` (or equivalent), including the edge-triple shape, the dual-list `kinds` response, and the ~~fill-token-bearing~~ `needs_fill` response + fill-request shape (no `fill_token` per [ADR-0008](./0008-vault-as-source-of-truth.md); entity ID is the callback).
3. Wire the error envelope as a Go middleware/helper that all handlers use.
4. Wire long-poll on `POST /v1/ingest` with a configurable timeout, default 60s.
5. ~~Implement the fill-token store: in-memory, one-shot, 5-minute TTL, indexed by token. Each entry carries `{entity_id, gaps: [...]}` so the fill handler can enforce the "fields must equal gaps" invariant against the originally-issued gap set per token (not a global stub list). Token issued in any `needs_fill` response; consumed on `POST /v1/entities/{id}/fill` (or expired).~~ **Obsoleted by [ADR-0008](./0008-vault-as-source-of-truth.md)** — no fill-token store, no expiry. The fill handler reads the entity's current `gaps:` set from the vault frontmatter on every call and validates submitted keys against that. See [ADR-0008 §"Callback ID = entity ID"](./0008-vault-as-source-of-truth.md).
6. End-to-end test: `curl` all seven endpoints, assert response shapes match the ADR. Long-poll path tested by stubbing a 70s "extraction" and confirming the 202 fallback. Fill-loop tested by stubbing a plugin that returns gaps, confirming the 202 needs_fill, POSTing to fill with the complete gap set and confirming the 200 merged entity, **plus** asserting that ~~a partial fill returns `400 incomplete_fill` with the correct `missing` array, and an extra-key fill returns `400 unknown_field`~~ a fill carrying any key not in the entity's current gap set returns `409 fill_conflict` with the correct `rejected` array (per [ADR-0008](./0008-vault-as-source-of-truth.md); subset fills succeed). Edge round-trip tested by writing an edge then expanding it via `GET /v1/entities/{id}?with_edges=...`.
7. Bring the first reference plugin (BGG) online behind `POST /v1/ingest` once the stub flow is proven. BGG serves as the worked example for plugin authors — not a core collector. It emits both entities and edges (`designed_by`, `published_by`) per its plugin-owned edge-kind set; future plugins (gmail, web-pages, books, …) are independent and equal first-class.
