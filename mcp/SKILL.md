# yaad-mcp — using yaad-index from an MCP-wired agent

> ⚠️ **DESIGN IN FLUX — THE CONTRACT MAY CHANGE BETWEEN SESSIONS.**
>
> yaad-index's plugin / cache / API surface is iterating and is **NOT stable**. Tool names, response shapes, gap composition, edge expansion semantics, and the underlying entity wire format may change without notice or migration path until a `stable` flag is set on a future yaad-index release. Don't cache assumptions across sessions: re-read the response shape from the tool's actual output, not from this doc, when the two disagree. Re-read this skill when you hit unexpected behavior — it may have moved.

You're an agent with the **yaad-mcp** tools wired in. This skill teaches the model and the tools so you can ingest URLs, navigate the graph, and fill gaps cleanly.

Read this before calling any tool. The pattern is small enough that one read carries you.

## Connecting

Two connection paths to the same 33-tool surface — pick whichever fits your agent runtime:

- **Direct (preferred):** the daemon exposes its full tool surface as a Streamable-HTTP MCP server at `<base-url>/mcp` (same host + port that serves `/v1/...`, e.g. `http://localhost:7433/mcp`). Auth: the same Bearer JWT that protects every REST route — issue with `yaad-index issue-token --operator <op> --agent <name>`, send as `Authorization: Bearer <token>`. The full daemon mux handles the call in-process; no wrapper to run.
- **Legacy stdio wrapper:** the bundled `mcp/` Node process (TypeScript) speaks stdio MCP and forwards every tool to the daemon's REST surface. Still bundled, still works — use it when your agent runtime can't speak Streamable HTTP, or when you're already wired against it. Both paths surface identical tool semantics (same names, same arguments, same response shapes); the direct path eliminates the wrapper hop.

**Tool inventory is live.** Both paths advertise the same 33 tools via the MCP `tools/list` call. The catalog below is the per-tool reference; `tools/list` is the authoritative live source on a running daemon.

## What yaad-mcp is

An MCP surface over yaad-index — a knowledge index that turns URLs into structured entities, plus the workflow engine that reacts to graph changes. Thirty-three tools, all active: `ingest`, `get_entity`, `get_entity_with_context`, `edges`, `get_entities_batch`, `fill`, `set_operator_fill`, `defer_gap`, `add_note`, `list_entities`, `search_local`, `search_upstream`, `structure`, `cv_status`, `reindex`, `kinds`, `plugins`, `needs_fill`, `archive_entity`, `restore_entity`, `delete_entity`, plus the user-content (UGC) read trio `get_user_content`, `list_user_content_sections`, `get_user_content_section` and write trio `create_user_content`, `edit_user_content_section`, `delete_user_content`, plus the workflow surface `workflow_list`, `workflow_discover`, `workflow_trigger` and task surface `task_list`, `task_load`, `task_resolve` (per ADR-0024 §"Agent surface"). Outbox channels (Discord / email / etc.) will land via a separate yaad-outbox surface when that repo ships; they're not part of this MCP today.

## Mental model — the graph yaad-index builds

Two shapes of node, connected by edges:

- **Source-shape entities** are what plugins emit. ID looks like `<source_namespace>:<slug>` where `<source_namespace>` is declared in the plugin's capabilities — for example `wikipedia-article:tehran` (yaad-wikipedia) or `bgg:caverna` (yaad-bgg). They carry structured plugin-specific fields (`data`), provenance, and outgoing edges. The raw article body is delivered separately on the `ingest` response (`clean_content`), not on the entity itself.
- **Canonical-shape entities** are **edge-target labels**. ID looks like `<kind>:<slug>` (e.g. `person:martin-wallace`, `city:tehran`, `boardgame:caverna`). At ingest the daemon **materializes a thin entity row** (`Kind` + `ID`, no `Data`, no vault file) so the edges table's foreign-key holds and the label appears in `list_entities` / `search_local`. The vault file at `<root>/ct/<kind>/<slug>.md` is created lazily on the first operator-fill. Once an operator-fill lands, the row carries `Data` and shows up everywhere a fully-populated entity does.
- **Edges** connect entities with typed relationships. Stored on the **source** entity's frontmatter as outgoing references (`is_about`, `is_a`, `designed_by`, `artist_by`, `published_by`, `authored_by`, …). The universal plugin-emitted edges are `is_about` (canonical-axis) and `is_a` (system-reserved source-type label, e.g. every `bgg:<slug>` carries `is_a → source-type:bgg-record`). Cross-canonical edges (e.g. `boardgame:caverna` → `person:uwe-rosenberg` via `designed_by`) come from plugin emissions or agent fills depending on the gap shape.

Both shapes share the same fill mechanism (`gaps:` + `fill(id, fields)`), so an agent doesn't care which shape it's looking at — just whether there are gaps to close. Fills against an unmaterialized canonical-label target trigger the auto-materialize path (DB row + vault file on first fill); the agent doesn't need to differentiate.

## Kind-driven discovery — the load-bearing pattern

**The canonical-shape labels are the discovery surface.** When you want to find what's in the graph, query by kind:

```
list_entities(kind: "person")
```

Every person yaad-index has seen — across every plugin that emitted a `person` canonical stub — comes back, including thin auto-materialized rows that don't yet have a vault file. The result shape is `{results, total, limit, offset}` where each `result` is `{id, kind, snippet, score}` — call `get_entity(id)` on any id to load full state. Thin rows return `kind` + `id` with `data` empty; once an operator-fill lands the row carries the operator-supplied fields. To find the source-shape entities that point AT a canonical label, query the relevant source-shape kinds (`list_entities(kind: "wikipedia-article")`, `list_entities(kind: "bgg")`, etc.), then `get_entity` each id and walk its `edges[]` for `is_about` references back to the canonical id you're tracking. Most flows don't need that reverse hop — once you have the canonical entity, its own `data` + filled gaps usually carry what you need; reach for source-shape only when you need provenance or the raw article body (which rides on the `ingest` response, not on `get_entity`).

Source-shape entities answer "where did the data come from"; canonical-shape entities answer "what does the graph know about this?". You almost always start with the kind question and walk to the data, not the other way around.

**Operator-typical canonical_kinds.** Operators declare what canonical kinds they want indexed in their `canonical_kinds:` config block. The ones plugins typically emit and that operator configs typically enable are: `person`, `city`, `country`, `book`, `boardgame`. Reach for `structure()` to read the live operator-configured allowlist on a running yaad-index — that's authoritative; this list is operator-typical guidance. The system-reserved kind `source-type` (used as the target of every source-shape entity's `is_a` edge — e.g. `source-type:bgg-record`, `source-type:wikipedia-article-record`) bypasses the operator's `canonical_kinds:` gate and is filtered out of operator-facing list / search surfaces.

## Tools

### `ingest(url)`

Tells yaad-index to fetch a URL into the graph. Returns one of four states:

- **`complete`** — entity already in the graph and fresh; nothing to do.
- **`needs_fill`** — entity created (or refreshed); gaps are open. Returns `entity` + `gaps: {field-name → AI prompt}` + `clean_content` (the article body). **This is where fill work happens.**
- **`disambiguation`** — URL/shorthand resolved to multiple candidates. Returns `options: {<id> → {label, summary?}}`. Pick one and re-call ingest with the `<plugin>: <id>` shorthand input form (e.g. `wikipedia: Tehran`).
- **`queued`** — fetch is async; check back later via `get_entity(estimated_entity_id)`.

Always honor whichever state comes back. Don't auto-pick a disambiguation option — surface the options to your operator (or your own reasoning) before re-calling ingest.

**Disambiguation rendering.** When `state: disambiguation`, render `options` to your operator as a numbered list — `1.`, `2.`, `3.`, …, one per line, label first and the optional `summary` on a subsequent indented line. The operator picks by number; you re-call `ingest` with the chosen option's `<plugin>: <id>` shorthand. Don't auto-pick.

### `get_entity(id)`

Fetch the entity by id. Returns `{id, kind, data, provenance, edges}` — yaad-mcp passes `?with_edges=*` (the daemon's wildcard sentinel) so every outgoing edge type expands inline. Plugin-emitted edges (`is_about`, `is_a`, `designed_by`, `artist_by`, `published_by`, `authored_by`, etc.) and agent-filled cross-canonical edges all come back on the response's `edges[]` in one call. Reach for `edges(entity_id)` for inbound queries (`direction=in`) or filtered queries; reach for `get_entity_with_context(id, depth, edge_types)` for multi-hop walks where each neighbor's surrounding state matters. To reach a canonical label from a source-shape entity, walk any `is_about` edge in the response and `get_entity(<edge.to>)` to load the canonical state.

`raw_content` and `gaps` are NOT on the get_entity response — they ride on the ingest response (`clean_content` and `gaps`). Read those off ingest, then call `get_entity` later when you only need the structured state.

### `get_entity_with_context(id, depth?, edge_types?, max_results?)`

Fetch an entity plus its surrounding context — linked entities reachable within `depth` outbound edge-hops — in one call. The server BFS-walks, deduplicates cycles, and returns the canonical shape `{root, neighbors: [{edge, entity, depth}], truncated}`.

- `depth` defaults to 1 (entity + direct neighbors). `0` returns just the entity (same shape as `get_entity` plus the `neighbors: [], truncated: false` envelope). The server caps `depth` at 3; passing 4+ is rejected at the tool boundary with `invalid_argument`.
- `edge_types` is an optional list (e.g. `["is_about", "references"]`). When set, only edges of the named types are walked AND only those edges appear on the response neighbors. Omit to walk all edge types.
- `max_results` defaults to 200 (server-side); cap is 1000. When the unbounded result would exceed `max_results`, the response is truncated to the prefix that fit and `truncated: true` is set so you can decide whether to re-call with a narrower `edge_types` filter or accept the partial.

Each neighbor's `edge` carries the full `{type, from, to}` triple — at depth ≥ 2 the source `from` is one of the previously visited neighbors, not the root. Each entity's `depth` field names the BFS level at which it was first reached.

**When to use this vs `get_entity`.** Reach for `get_entity_with_context` when you'd otherwise be looping `get_entity` to walk multiple hops (PR + linked Jira ticket + linked Confluence doc + canonical process stub assembled in one shot). When you only need a single entity's structured state, `get_entity` is the leaner call.

### `edges(entity_id, edge_types?, direction?)`

Single-hop edge query — the flat one-hop surface for "who designed this game" / "what cites this person" without the BFS-traversal cost of `get_entity_with_context`. Wraps `GET /v1/edges?entity_id=X[&edge_types=...][&direction=...]`. Returns `{ok, edges: [{type, from_id, to_id, metadata?}], next_cursor}`.

- `direction` defaults to **out** (entity_id is `from_id` — backward-compat with the per-entity `?with_edges=` semantic). `direction=in` returns inbound edges (entity_id is `to_id`); `direction=both` returns inbound + outbound combined.
- `edge_types` is an optional allowlist; absent / empty → no type filter (every type returns).
- `next_cursor` is reserved for forward-compat and always null today (single-hop counts per entity are bounded; cursor traversal is a future enhancement).

**When to use this vs `get_entity_with_context`.** Use `edges` for direct one-hop queries — the flat shape (no nested neighbors, no BFS depth, no canonical_vocabulary payload) keeps the response small. Use `get_entity_with_context` when you need a multi-hop walk where each neighbor's surrounding state matters (clean_content, gaps, etc.). `edges` is also the right tool for inbound queries — `get_entity_with_context` walks outbound only.

### `get_entities_batch(ids, with_edges?)`

Fetch multiple entities in one round-trip. Wraps `POST /v1/entities/batch`. Returns `{ok, entities, missing}` verbatim — `entities` carries the resolved entities (same per-entity wire shape as `get_entity`), `missing` is the array of ids the daemon has no row for.

- The daemon caps batch size at 100 ids per call. Calls exceeding that bubble as a `too_many_ids` error so the agent splits + retries.
- Placeholder entities created by an in-flight ingest land in `entities` with sparse `data` — `missing` is reserved for ids the daemon has never seen at all.
- `with_edges` is plumbed through to the daemon's `with_edges` request field for forward-compat, but the v1 endpoint does NOT yet expand inline edges on the batch surface — `entities[i].edges` returns empty regardless. Use `get_entity` (which expands all edge types via the `*` wildcard) or `get_entity_with_context` when you need edge expansion.
- Empty / non-string-array `ids` rejected at the tool boundary (no API call).

**When to use.** Reach for `get_entities_batch` when you've already computed a list of ids you want to fetch — typically by walking edges from a previous `get_entity` / `get_entity_with_context` call — and want to avoid the N round-trip cost of calling `get_entity` in a loop. The split between `entities` + `missing` lets you handle partial-resolution cases (e.g. the operator deleted some targets between your edge-walk and the batch-fetch) without per-id error handling.

### `structure()`

Introspect the operator-configured yaad-index. Returns `{ok, version, kinds, edge_types, plugins}` verbatim from `GET /v1/structure`.

- `version` is a deterministic config-hash (truncated SHA-256, 16 hex chars) over the canonical JSON of `(kinds, sorted edge_types, sorted-by-name plugins, schema-discrim sentinel)`. Same config produces the same hash; reorder-stable. Operator rebuild / config change / plugin add/remove/upgrade bumps it. Agents that cache structure responses key on this string and re-fetch when it changes.
- `kinds` is `{<kind>: {is_canonical, gaps, instruction?}}`. `is_canonical` is locked to true in v1; `gaps` is `{<field>: <AI-prompt>}`; `instruction` is the operator's per-kind override (omitted when unset).
- `edge_types` is the sorted list of operator-enabled canonical edge types.
- `plugins` is the sorted-by-name list of loaded plugins, each with `{name, version, url_patterns, supports_search, emits_kinds, emits_edges}`. The metadata is sourced from each plugin's `--init` capabilities cached at server startup; the structure call itself doesn't re-spawn plugins.

**When to use this vs other tools.** Reach for `structure()` when the agent needs a snapshot of "what's the operator-configured yaad-index actually willing to accept" — discovering the canonical kinds before a fill, knowing which edge types you can declare on a comment, or checking which plugins claim a URL family. The response shape is small and the call is cheap; in v1 there is no client-side caching layer in yaad-mcp, so an agent that wants to cache should do so itself keyed by `version`.

### `cv_status()`

Check canonical-vocabulary drift on the operator-configured yaad-index. Returns `{ok, config_hash, drift, last_reindex_at, reindex_hint}` verbatim from `GET /v1/cv-status`.

- `config_hash` is a deterministic SHA over the canonical-vocabulary subset of the config — same width as `structure().version` (16 hex chars) but covers a smaller surface (canonical_kinds + canonical_edge_types only). Same config produces the same hash; reorder-stable on edge_types. Agents that cache results key on this string.
- `drift.kinds_emitted_not_enabled[]` — `{plugin, kind, would_materialize_count}` rows. The count is the cumulative number of canonical entity stubs the plugin emitted that the orchestrator's config-filter dropped at ingest time (because the kind isn't in `canonical_kinds:`). Persisted in DB so it survives daemon restart.
- `drift.edge_types_emitted_not_enabled[]` — `{plugin, edge_type, would_materialize_count}` rows; same axis for canonical edge types.
- `drift.kinds_enabled_not_emitted` / `drift.edge_types_enabled_not_emitted` — stubbed empty arrays in the v1 endpoint ("operator enabled X but no plugin emits it" is a different signal that lands later).
- `last_reindex_at` — RFC-3339 UTC timestamp when reindex last ran; `null` when never reindexed.
- `reindex_hint` — static guidance string. Operators read drift counts → enable kinds in config → call `POST /v1/reindex` (via the daemon, not via this MCP) to materialize the stubs.

**When to use.** Reach for `cv_status()` when investigating "why isn't this canonical entity showing up?" or polling for config-vs-emission drift. Pairs with `structure()` (which says "what's configured / what's loaded") — drift is the gap between the two surfaces. yaad-mcp adds no diff helpers, no client-side caching, no filtering — agents call `cv_status()` twice and diff if they want to detect changes.

### `reindex(mode?)`

Trigger the daemon to walk the markdown vault and rebuild the derived index (entities + edges from frontmatter). Returns the daemon's `reindex.Summary` verbatim from `POST /v1/reindex`: `{mode, scanned, skipped, parsed, entities_created, entities_updated, entities_deleted, edge_rows_written, errors?, started_at, finished_at, duration_ms}`.

- `mode` defaults to `"incremental"` — re-parses only files whose mtime / hash changed since the last walk. Pass `"full"` for a hard rebuild that re-parses every file (use sparingly: full walks scale with vault size).
- The call **blocks** until the daemon's walk completes. yaad-mcp adds no client-side polling, no progress reporting — the agent waits for the summary.
- `errors` is a non-fatal-error list: parse / store errors that didn't abort the walk. Empty / absent when the walk completed cleanly. Surface to the operator if non-empty; the index is still consistent past those files.
- Returns a 404-shaped error when `vault.path` isn't configured operator-side (no vault → no reindex).

**When to use.** Reach for `reindex()` after a batch ingest / delete / out-of-band vault edit when you want the derived index in sync without operator intervention. Don't reach for it on every ingest — the daemon already updates the index incrementally on each `ingest()` call. Reindex is the catch-up surface for vault changes the daemon didn't see (manual edits, git pulls of vault-files, post-config-change rematerializations).

### `kinds(name?)`

Plugin-emitted source-kinds registry. Wraps `GET /v1/kinds`. Returns `{ok, entity_kinds, edge_kinds}` — the union of every registered plugin's declared entity-kinds and edge-kinds. Each entry: `{name, description, source_plugins}` for entity-kinds; `{name, description, from_kind, to_kind, source_plugins}` for edge-kinds. `source_plugins` is sorted alphabetically for deterministic output.

Distinct from the other introspection tools:

- `structure()` returns the **canonical-shape** registry — operator config + per-kind merged gaps + instructions.
- `cv_status()` returns the **gating-shape** drift counters — emissions the operator's config dropped.
- `kinds()` is the **plugin-author-perspective** surface — "what does each plugin declare it can produce."

The `name?` param filters both arrays client-side to entries whose `name` matches. The daemon endpoint itself doesn't accept a `name=` filter; the tool fetches the full registry and narrows on the response. Empty / non-matching name yields `{ok: true, entity_kinds: [], edge_kinds: []}` (the `ok` flag from the upstream is preserved).

**When to use.** Reach for `kinds()` when you need to know "which plugin declares it emits this kind" or "what edge-types does the daemon know about across all plugins" — without needing to know operator-config layout (`structure()`) or current-state drift (`cv_status()`). Useful when bootstrapping an integration or surfacing a list of available source-shape kinds to a downstream consumer.

### `plugins()`

Per-plugin capability discovery. Wraps `GET /v1/plugins`. Returns `{ok, plugins: [{name, version, url_patterns, commands, entity_kinds, edge_kinds, source_namespace}, ...]}` — the per-plugin view of each registered plugin's `--init` Capabilities. Inverse shape of `kinds()`:

- `kinds()` aggregates **kind → plugins** (deduped by kind name, with `source_plugins` cross-references).
- `plugins()` enumerates **plugin → kinds + URL patterns + commands + namespace** (one entry per registered plugin, in dispatch order).

Plugins are listed in **registry order** (matching the first-match-wins precedence at `/v1/ingest`); within each plugin, `entity_kinds` + `edge_kinds` sort alphabetically. Empty list fields (e.g. `url_patterns` on a poll-driven plugin like yaad-gmail; `commands` on URL-shape-only plugins like yaad-wikipedia / yaad-bgg) marshal as `[]`, not `null`.

**Call at session start** to load a live view of what plugins are loaded + what each one accepts. This replaces what pre-#13 SKILL.md would have carried as per-plugin sections — see the "Bundled plugins" stubs below for an offline-readable fallback when the daemon isn't reachable.

**When to use.** Reach for `plugins()` to discover what URL shorthands the current daemon accepts (e.g. is `yaad-bgg` loaded? does this daemon have `yaad-gmail` for `gmail: !fetch`?), what commands each plugin advertises per ADR-0022, or what `source_namespace` each emits entities under. Use `kinds()` instead when the question is "which plugin emits this kind."

### `needs_fill(limit?, cursor?)`

Pull-based batch gap-call queue. Returns `{ok, entities, next_cursor?}` verbatim from `GET /v1/needs-fill`. Each `entities[i]` carries the full per-entity gap-call payload — `{id, kind, gaps, gap_metadata?, clean_content, clean_content_truncated, instruction?, canonical_vocabulary?}` — same shape as the cache-hit ingest envelope, so a single decoder reads both.

**`gap_metadata`.** Per-gap structured metadata that doesn't fit the plain `<field>: <prompt-string>` shape of `gaps`. Today it carries one field — `kinds` — populated only on `canonical_type` gaps:

```
gap_metadata: {
 designers: { kinds: ["person"] },
 inspired_by: { kinds: ["*"] },
}
```

`kinds` surfaces the operator-declared `Kinds:` allowlist on the gap spec — agents render it at fill-prompt time so the LLM knows the valid canonical kinds for that gap. The wildcard `["*"]` accepts any kind in the operator's `canonical_kinds:` registry. Absent on plain scalar gaps. Decoders should treat `gap_metadata` as opt-in: legacy entries omit it entirely.

- `limit` is server-clamped (default 50, cap 200, lenient on bad values; tool-side rejects non-integer up front).
- `cursor` is opaque base64(last-seen-id). Pass back the previous response's `next_cursor` to fetch the next page. When the queue is exhausted, `next_cursor` is **absent** from the response (not `null`); the agent stops paginating when the field is missing.

**No auto-pagination, no client-side filtering.** The agent decides whether to keep paginating, when to stop, and whether to act on each entry (typically by calling `fill(id, fields)`). yaad-mcp surfaces the queue verbatim — no aggregation across pages, no kind / instruction-text filtering, no caching.

**When to use this vs `get_entity`.** Reach for `needs_fill()` when you want to *find* work (which entities still need a gap-call) — driving a batch-fill loop, scheduling fills, or surfacing queue state. Reach for `get_entity` when you already know the id and want to read its current state. The two surfaces are complementary: `needs_fill()` produces ids; `fill()` acts on them.

### `fill(id, fields)`

Fill open gaps. `fields` is `{gap-name → value}`. Every key in `fields` MUST be a current open gap on the entity — passing a key that isn't a gap fails the whole call (no partial success). Returns the updated entity + the remaining gap names (empty array when you've closed every open gap on this entity).

The fill protocol:

1. Read the entity (`get_entity`) or use the `entity` returned by ingest.
2. For each gap in `gaps:` — read the prompt, derive the value from `clean_content` (or external knowledge if the prompt allows it), shape it per the prompt.
3. Call `fill(id, {gap1: value1, gap2: value2, …})` with the values.
4. If `gaps` in the response is non-empty, more work to do — usually because the AI couldn't derive every gap on the first pass. Iterate.

**`canonical_type` gap shape.** When the gap's spec type is `canonical_type`, the fill value is a list of `{name, kind}` objects:

```
fill("boardgame:caverna", {
 designers: [
 { name: "Uwe Rosenberg", kind: "person" }
 ]
})
```

The daemon NFC-normalizes + slugifies `name` and creates a canonical-label edge from the entity to `<kind>:<slug>`. Empty list `[]` is a valid fill (transitions the gap to filled with no edges; useful when the article confirms there are no entries). The **agent path REJECTS pre-formed `<kind>:<slug>` strings** — agents must use the object form so the daemon owns slug derivation. (The operator-fill path accepts pre-formed strings — see `set_operator_fill`.) The canonical-kind on each entry must be in the operator's `canonical_kinds:` registry OR the gap's `Kinds:` allowlist (whichever is narrower) — invalid kinds reject the call. Read the per-gap allowlist off `needs_fill`'s `gap_metadata.<field>.kinds` to render it at fill-prompt time.

### `add_note(entity_id, text, author?)`

Append a note to an existing entity. Server stamps the date (UTC), the agent identity (`author`, from your JWT subject), and the human resource owner (`operator`, from your JWT pair-claim). Returns `{ok, note, entity}` where `note` is the just-appended entry and `entity` is the merged-note-included fresh copy of the entity (so you don't need a follow-up `get_entity`).

- **`author` is optional and recommended-omitted.** When you leave it out, the server fills it from your JWT subject. When you set it, it MUST equal your JWT subject — passing someone else's name returns the upstream `{ok: false, error: "author_mismatch"}` envelope verbatim. The server enforces this cryptographically; there's no way to claim to be another agent.
- **Errors pass through structured.** Unlike most tools, `add_note` does NOT throw on auth errors. A 4xx from yaad-index returns `{ok: false, error, message}` directly so you can branch on `error === "author_mismatch"` / `"missing_authorization"` / etc. Successful calls return `{ok: true, note, entity}`. Read `ok` first.
- **Append-only in v1.** No edit, no delete, no threading — yaad-index v1 doesn't expose those surfaces. Re-posting the same text adds a new entry.

**When to use.** Reach for `add_note` when you want to leave human-readable context on an entity that survives reindex (vault-frontmatter-mirrored). Notes aren't gap fills — gap fills carry structured derivable data with a defined shape; notes are free-form prose for "I noticed X" / "this looks wrong" / "operator should verify". Don't use notes to encode structured data the gap mechanism could carry.

### `get_user_content(id, limit?, cursor?)`

Fetch a user-content (UGC) entity. Returns `{ok, id, kind, data, tags, provenance, sections: {entries, next_cursor?}, etag?}` — the entity envelope plus the first page of parsed sections embedded. `etag` is lifted from the HTTP `ETag` response header and rides on the response object so you don't have to peek at headers; pass it back as the `etag` argument of `edit_user_content_section` for If-Match concurrency.

### `list_user_content_sections(id, limit?, cursor?)`

Paginated section list for a UGC entity. Returns `{ok, entries, next_cursor?, etag?}`. Use this when the embedded section list on `get_user_content` doesn't fit in one page, or when you only want the section list without re-fetching entity metadata. Default page size is 20; max 100 (server-clamped).

### `get_user_content_section(id, sec)`

Fetch one section by address. `sec` accepts heading-text-slug (e.g. `books-i-loved`) OR positional index (`0`, `1`, …). Server canonicalizes either form. Returns `{ok, id, section: {index, depth, heading, heading_slug, body, byte_offset}, etag?}`.

**Section addressing — containment model.** Every markdown ATX heading (`#`..`######`) is one addressable section in a flat list. A section's `body` extends from after its heading line until the next heading of same-or-shallower depth — meaning DEEPER nested headings (and their content) are TEXTUALLY INCLUDED in the parent's body. Editing `# Top` rewrites the whole subtree below it; editing the leaf `### Foo` rewrites just its leaf content. The granularity choice IS the section choice: pick the section depth that matches the scope of your edit. Pre-heading body is the implicit "section 0" (depth=0, no heading). A body with no headings collapses to one section that spans the whole body.

**Disambiguation rendering.** When you list sections to your operator, render them as a numbered list — `1.`, `2.`, `3.`, …, one per line. The agent picks the section to address by either heading slug (when the heading is unique) or positional index (the canonical fallback when two headings slugify identically — server returns 404 on the slug in that case).

### `create_user_content(title, tags, body, data?)`

Create a new UGC entity. Server slugifies `title` → `id = user-content:<slug>`, stamps `author` from the JWT subject and `operator` from the pair-claim. Returns the full entity envelope (same shape as `get_user_content`) plus an `etag` lifted from the HTTP ETag header — the agent can chain edits without an extra GET. 409 conflict on slug collision; the agent picks a new title (auto-suffix is a deferred follow-up).

**`data` field — frontmatter-edge derivation.** Optional map of frontmatter fields. When the operator's `user_content_frontmatter_edges:` config declares mappings, fields named in those mappings produce canonical-label edges from the new UGC entity to `<kind>:<slug>` labels. Each declared field's value is one of:

- `{name: "...", kind: "..."}` — single object (daemon slugifies `name`).
- `[{name, kind}, ...]` — list of objects (one edge per entry).
- `"<kind>:<slug>"` — pre-formed canonical-label string (UGC is operator-authored, so the pre-formed shape is accepted — same waiver as `set_operator_fill`'s `canonical_type`).
- `["<kind>:<slug>", ...]` — list of pre-formed strings.

Other fields land verbatim under the entity's `Data` map without edge derivation. Idempotent re-create isn't a thing (409 conflict path); a future re-edit endpoint (surfaced as a separate MCP tool when the wrapper lands) re-applies the same derivation cleanly so the edge graph tracks frontmatter edits exactly.

### `edit_user_content_section(id, sec, body, etag)`

Replace one section's body. **The `etag` parameter is REQUIRED** — read it from a prior `get_user_content` / `list_user_content_sections` / `get_user_content_section` call and pass it back here as the If-Match concurrency token.

**Etag flow.** The agent's read-then-write loop:

1. Read: `get_user_content_section(id, sec)` returns `{section, etag}`.
2. Compose the new section body (per the containment model — including any nested headings the agent wants to keep).
3. Write: `edit_user_content_section(id, sec, newBody, etag)` with the etag from step 1.
4. On 200 success the response carries the new etag for the next edit; on 412 stale-etag the response carries `current_etag` so the agent can re-GET + retry without an extra round-trip.

**Error envelopes (passthrough — branch on `ok === false`):**

- `error: "precondition_failed"` (412, stale etag) — `current_etag` rides on the envelope. Refetch + retry.
- `error: "precondition_required"` (428, missing etag) — caller forgot to pass the etag arg.
- `error: "author_mismatch"` (403) — JWT claim doesn't match the entity's author/operator. Operator-on-behalf is allowed when the JWT operator equals the entity's stored operator.

5xx still throws (transient infrastructure failures).

### `set_operator_fill(id, fields)`

Operator-fill endpoint. POSTs to `/v1/entities/{id}/operator-fill` with per-field operations. **Operator-only** — the JWT MUST have `Subject == Operator`; agent-on-behalf tokens reject with 403 `agent_not_allowed`.

Per-field value shapes:
- **scalar** (number / boolean / string / ...) → set the field, stamp `gap_state.source=operator + filled_at`.
- **`null`** → clear: remove the field from `data` and drop the `gap_state` entry. The field re-appears as an open gap that the operator can re-fill or defer.
- **`{defer: true}`** → mark the field deferred. **Must be currently unfilled**; defer-on-filled returns **409 `deferred_requires_unfilled`**. Recovery is two-step: clear with `null` first, then defer.
- **`{defer: false}`** → un-defer (drop the deferred flag).

Per-field validation runs against the resolved canonical-kind config: type mismatch (`"9"` for an int field), out-of-range, max_length exceeded, enum value not in `values:` all reject with 400. fill_strategy=agent fields can't be operator-set (400 `agent_only_field`).

**`canonical_type` gap on operator-fill.** Operator-fill accepts both shapes — object form `[{name, kind}, ...]` AND pre-formed canonical-label strings `["<kind>:<slug>", ...]`. The pre-formed shape lets operators reference labels they already know the slug for without round-tripping through the slugifier. Both shapes go through the same edge-replacement path: prior edges of that type are cleared, then a fresh edge created for each new entry. `null` clears the gap (drops gap_state + edges); empty list `[]` transitions to filled with no edges.

**Auto-materialize-on-first-fill.** Operator-fill against a canonical-label edge target with no entity row OR no vault file auto-creates both: thin row promoted to populated, vault file created at `<root>/ct/<kind>/<slug>.md` with the operator-supplied `Data` rendered as frontmatter. The fill behaves identically from the operator's perspective — there's no separate "create then fill" path. Subsequent re-fills update the existing row + vault file in place.

Vault-then-DB write ordering. Auto-commit prefix `operator-fill: <id> [field1, field2, ...] by <operator>`.

### `defer_gap(id, field)`

Convenience wrapper for `set_operator_fill(id, {<field>: {defer: true}})`. Mark a single gap deferred so it stops surfacing on `/v1/needs-fill` for both audiences. Operator un-defers via `set_operator_fill(id, {<field>: {defer: false}})`.

Same constraint as the underlying endpoint: the field MUST currently be **unfilled**. Defer on a filled field returns 409 `deferred_requires_unfilled`; recover by calling `set_operator_fill(id, {<field>: null})` first to clear, then call this tool.

Same operator-only auth gate as `set_operator_fill`.

### `archive_entity(id)`

Archive an entity. The vault file moves from `<kind>/<slug>.md` to `_archive/<kind>/<slug>.md` (an `archive: <id>` git commit), and the DB `archived_at` timestamp is set. Edges are RETAINED in the DB so audit / restore flows can still traverse them; consumers see `archived: true` on endpoint objects via edge expansion. Idempotent (re-archiving preserves the original timestamp). Returns `{ok, id, archived: true}`.

This is the **prerequisite for `delete_entity`** — the daemon's DELETE state-machine refuses active entities with 409.

### `restore_entity(id)`

Inverse of `archive_entity`. The vault file moves back from `_archive/<kind>/<slug>.md` to the active layout (`restore: <id>` git commit), and the DB `archived_at` is cleared. The entity reappears in default-filtered list / search results. Returns `{ok, id, archived: false}`.

### `delete_user_content(id)`

Hard-destroy a UGC entity. **Two-step state-machine: `archive_entity` first, then `delete_user_content`.** On an active entity the call returns `{ok: false, error: "must archive before delete", message: "POST /v1/entities/<id>/archive first; ..."}` — surfaced verbatim, not thrown, so the agent branches on `error`. On an archived UGC entity the call removes the `_archive/...` vault file and cascade-drops the store row; returns `{ok, id, deleted: true}`.

403 author_mismatch (cross-author destroy; operator-on-behalf still allowed) still throws — and runs *before* the archive-first gate so an intruder can't probe other authors' archive state. 404 on already-gone is a real failure (NOT silently ok).

### `delete_entity(id)`

Hard-destroy a generic yaad-index entity (any kind: `boardgame:`, `wikipedia:`, `person:`, etc. — NOT `user-content:`, that's `delete_user_content`). Same archive-first state-machine as `delete_user_content`: **call `archive_entity` first**, then this tool to permanently remove the archived entity. The two explicit calls *are* the safety property; there is no `?confirm=permanent` shortcut.

On an active entity the daemon returns the structured `{ok: false, error: "must archive before delete", ...}` envelope — surfaced verbatim so the agent can branch on `error`. On an archived entity the call removes the `_archive/...` vault file (with a `destroy: <id> [<kind>] by <agent>` git commit) and cascade-drops the DB row + inbound/outbound edges + provenance. Returns `{ok, id, deleted: true}`. **Destructive and irreversible** at this point.

Other non-2xx (401, 404 already-gone, 503 vault not configured) still throw YaadIndexError. Unlike `delete_user_content`, there's no 403 author_mismatch path on the destroy itself: the surface is agent-callable-by-anyone (the WARN audit log + commit is the post-hoc accountability surface).

### `list_entities(kind)`

List entities of a given kind — the kind-driven discovery surface above. The `kind` parameter is required: yaad-index's `/v1/search` endpoint mandates either a kind filter or a query string, and the MCP surface only exposes kind-only listing. Returns `{results, total, limit, offset}` where each result is `{id, kind, snippet, score}` — call `get_entity(id)` on any id to load full state.

### `search_local(query, kind?, limit?)`

Full-text search across the **local** yaad-index — entities yaad-index has already ingested. `query` is required; `kind` filters to a specific kind; `limit` defaults to 20. Returns the same `{results, total, limit, offset}` shape as `list_entities` — each result is `{id, kind, snippet, score}`, call `get_entity(id)` to load full state.

**Local vs upstream.** `search_local` searches what's already in yaad-index — it's the "find an entity by keyword across what we know" tool. It does NOT reach Wikipedia / external sources. To FETCH new content from upstream, use `ingest(url)` (which runs the plugin against the upstream API). For the discovery-shaped step BEFORE `ingest` — "show me candidates from upstream sources matching this string so I can pick which to ingest" — use `search_upstream` (below).

### `search_upstream(query, plugins?, limit?, per_plugin_timeout_seconds?)`

Plugin-federated search across **upstream sources** — fans the query out to every plugin that opted in via `Capabilities.SupportsSearch=true` (today: yaad-wikipedia). Returns `{results, per_plugin_status, query, limit, per_plugin_timeout_seconds}` where `results` is the merged candidate list `[{plugin, id, label, summary}]` in plugin-declaration order and `per_plugin_status` carries per-plugin outcome (`ok`, `candidates`, `duration_ms`, `error_message`).

**Cost-shape contrast.** `search_local` is DB-only — sub-millisecond, no network. `search_upstream` makes per-plugin upstream API calls in parallel with a default 5s per-plugin timeout (bounded `[1, 30]` via `per_plugin_timeout_seconds`); it's the "go ask Wikipedia/BGG/... what they know" surface. Prefer `search_local` first; reach for `search_upstream` only when the local index doesn't have what you need and you're about to decide which URL to `ingest`.

**Partial-results semantic.** A single plugin's failure or timeout does NOT fail the call — the per-plugin error message lands in `per_plugin_status` and other plugins' results still surface. The federated call returns 200 even when every plugin errored, with empty `results` + populated per-plugin error block.

**Explicit plugin allowlist.** `plugins` empty / omitted → federate to every opted-in plugin. Explicit list selects exactly the named plugins. Naming a plugin that isn't registered → 400; naming a plugin whose `SupportsSearch=false` → 422 (the daemon won't silently downgrade an explicit request).

### `workflow_list()`

List every workflow pattern currently registered with the running yaad-index. Returns `{ok, workflows: [{name, version, status, trigger_type, dedup_policy}]}` verbatim from `GET /v1/workflows`. Sorted by name. The discovery surface for workflows — call this before `workflow_trigger` or `workflow_discover` to learn what exists.

### `workflow_discover(entity_id)`

Find every workflow whose condition predicate evaluates true for the given entity. Returns `{ok, entity_id, workflows: [<name>, ...]}` from `GET /v1/workflows/discover?entity=<id>`. Walks every registered workflow + evaluates each condition against the resolved entity. Best-effort surface — condition eval errors are treated as non-matching (this is operator inspection, not a fire commitment). Unknown entity → 404.

### `workflow_trigger(name, input?)`

Manually fire a registered workflow. Returns the recorded Decision envelope `{ok, workflow, entity_id, subject, fired, missing_refs?, err?, at}` from `POST /v1/workflows/trigger`. `input` shapes (per ADR-0024 §"workflow.trigger(input) input semantics"):

- **Empty** — target-less manual fire (only valid for `trigger.type=manual` workflows).
- **Canonical entity id** (`<kind>:<slug>`) — direct attach to a known entity.
- **URL** — routes through the daemon's ingest-or-lookup pipeline before attach; the trigger call itself fails synchronously on routing errors (no plugin handles, disambiguation required, malformed).

Unknown workflow → 404. Empty input on an event-driven workflow → 422. The returned `fired` is false when the workflow's condition predicate rejected — Decision is still recorded.

### `task_list(errored?)`

List workflow-produced tasks (markdown files under `vault/tasks/`). Returns `{ok, tasks: [{id, workflow, subject?, errored?, dedup_key?, created_at}]}` from `GET /v1/tasks`. Sorted by id. Active tasks only — resolved + auto-archived tasks live under `tasks/_archive/` and aren't included. Optional `errored: true` filter returns only err-tasks per ADR-0024 §"Runtime errors"; `errored: false` returns only normal tasks; omitted returns both.

### `task_load(id)`

Load one workflow-produced task by id. Returns `{ok, task: {id, workflow, subject?, errored?, dedup_key?, created_at, body}}` from `GET /v1/tasks/{id}`. `body` is the markdown content after the frontmatter, verbatim — includes section headers + content lines + any `## Missing references` annotations (per ADR-0024 §"Missing-reference handling"). 404 when the id doesn't resolve.

### `task_resolve(id)`

Mark a workflow-produced task done. Stamps `resolved_at: <now>` on the task's frontmatter; auto-archives (moves to `tasks/_archive/<id>.md`) when the originating workflow has `auto_archive_on_done: true` (the default). Err-tasks always auto-archive regardless of the workflow opt-out per ADR-0024 §"Runtime errors". Returns `{ok, id, errored, auto_archived, resolved_at}` from `POST /v1/tasks/{id}/resolve`. Idempotent: re-resolving an active task preserves the original timestamp; re-resolving an already-archived task is a no-op success.

## Bundled plugins (abbreviated reference)

The authoritative live view is `plugins()` against the running daemon — call it at session start to learn what's actually loaded. The stubs below are an **offline-readable fallback** for the no-daemon case (e.g. when authoring a tool that will run against a future daemon). They cover what each plugin in the canonical docker image accepts; an operator's actual deployment may load a different subset or third-party plugins.

- **yaad-wikipedia** — URL-shape. Accepts canonical Wikipedia URLs (`https://<lang>.wikipedia.org/wiki/<title>`) and the shorthand `wikipedia: <title>` (e.g. `wikipedia: Tehran`). Emits source-shape entities under namespace `wikipedia` plus `is_about` edges to canonical entities (person, place, etc.) when the Wikidata Q-id resolves. No commands.
- **yaad-bgg** — URL-shape. Accepts canonical BoardGameGeek URLs (`https://boardgamegeek.com/boardgame/<id>` and `boardgame/<id>` form) and the shorthand `bgg: <name-or-id>` (e.g. `bgg: 224517` or `bgg: brass birmingham`). Emits source-shape entities under namespace `bgg` plus `designed_by` / `artist_by` / `published_by` edges to `person` canonical entities. Requires `BGG_API_KEY` env on the daemon. No commands.
- **yaad-gmail** — **Command-shape, no URL form.** Declares `commands: ["fetch"]`; invoke via `gmail: !fetch` (or `yaad-index command gmail fetch` from the CLI with an operator-only-claim token per ADR-0022 §6). One invocation runs one IMAP poll cycle and emits NDJSON envelopes per un-ingested message. Requires `YAAD_GMAIL_ACCOUNT` + `YAAD_GMAIL_APP_PASSWORD` env on the daemon. Emits source-shape entities under namespace `gmail` plus `from` / `to` / `cc` / `bcc` edges to `email-address` canonical entities and `tagged_as` edges to `label` canonical entities.

When the stubs above conflict with `plugins()` output, **prefer `plugins()`** — it's live, the stubs are point-in-time.

## Conventions

- **Idempotency.** Ingesting the same URL twice is safe — yaad-index dedups by entity id. Ingesting a URL that's already in `complete` state is a no-op (returns the existing entity unchanged). Use this freely.
- **Don't mass-ingest in a loop.** yaad-index runs upstream API calls per ingest; rate limits live on the plugin's side. Small batches (≤5 at a time) avoid surprises.
- **Read clean_content fully before fill.** The fill quality is bounded by how much of the article body the agent reads. Don't summarize the body before deriving the gap values — the gap prompt expects values derived from the article, not from your summary of the article.
- **Trust the gaps map's prompts.** The plugin author wrote them to bound what kind of value the gap expects (string, integer, list, format). Honor the format the prompt names.

## When to use which tool

| You want to… | Tool |
|--------------|------|
| Add a new URL to the graph | `ingest(url)` |
| Pick from disambiguation options | `ingest("<plugin>: <id>")` again |
| Read a known entity's full state | `get_entity(id)` |
| Stitch entity + linked context (cross-source assembly) | `get_entity_with_context(id, depth)` |
| Single-hop edges (who designed / what cites — no BFS) | `edges(entity_id, edge_types?, direction?)` |
| Snapshot operator config (kinds + edge_types + plugins + version) | `structure()` |
| Check canonical-vocabulary drift (config-vs-emissions) | `cv_status()` |
| Find entities with open gaps (queue browse) | `needs_fill(limit?, cursor?)` |
| Close open gaps with derived values (agent-fill path) | `fill(id, fields)` |
| Operator writes gap values (rating, owned, want, played, ...) | `set_operator_fill(id, fields)` |
| Operator marks a single unfilled gap as ignored | `defer_gap(id, field)` |
| Append a note to an entity (free-form prose, not gap data) | `add_note(entity_id, text)` |
| Read a user-content (UGC) entity + first page of sections | `get_user_content(id)` |
| Paginate sections on a UGC entity | `list_user_content_sections(id, limit?, cursor?)` |
| Read one section of a UGC entity (capture the etag for editing) | `get_user_content_section(id, sec)` |
| Create a new UGC entity (server stamps author + operator) | `create_user_content(title, tags, body)` |
| Replace one section of a UGC entity (If-Match concurrency required) | `edit_user_content_section(id, sec, body, etag)` |
| Move an entity out of active queries (reversible) | `archive_entity(id)` |
| Bring an archived entity back into active queries | `restore_entity(id)` |
| Hard-destroy an archived UGC entity (after `archive_entity`) | `delete_user_content(id)` |
| Hard-destroy any archived non-UGC entity (after `archive_entity`; DESTRUCTIVE; no undo) | `delete_entity(id)` |
| See everything of a kind (discovery) | `list_entities(kind)` |
| Find entities by keyword across the local index | `search_local(query, kind?)` |
| Find candidates on upstream sources before deciding what to ingest | `search_upstream(query, plugins?)` |
| List registered workflow patterns | `workflow_list()` |
| Find workflows that match an entity (would fire on it) | `workflow_discover(entity_id)` |
| Manually fire a workflow (entity id, URL, or empty target) | `workflow_trigger(name, input?)` |
| List active workflow-produced tasks (operator queue) | `task_list(errored?)` |
| Read one task's body + frontmatter | `task_load(id)` |
| Mark a task done + auto-archive it | `task_resolve(id)` |

## Health check

Before running a flow, sanity-check yaad-index is alive:

```
ingest("https://en.wikipedia.org/wiki/Test")
```

A `complete` or `needs_fill` response means the platform is up. Connection errors mean check the daemon is running + reachable:

- **Direct path:** confirm `<base-url>/mcp` is reachable + the Bearer token is valid (curl `<base-url>/v1/health` returns 200; that same daemon serves `/mcp`).
- **Legacy stdio wrapper:** check `YAAD_INDEX_URL` is set + the Docker pilot is running.

A 401 from the MCP layer means the JWT is missing / malformed / expired — re-issue via `yaad-index issue-token` and reconfigure the agent.
