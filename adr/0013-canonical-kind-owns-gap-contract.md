# ADR-0013 — Canonical-kind owns gap contract; plugin emits edge-keyed data; index extracts edges

**Status:** Accepted (2026-06-05; proposed 2026-05-04)
**Date:** 2026-05-04
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md), [ADR-0002](./0002-api-surface.md), [ADR-0005](./0005-plugin-lifecycle.md), [ADR-0008](./0008-vault-as-source-of-truth.md)

## Context

Today the ingest `needs_fill` flow has four coupled gaps:

1. **Plugin-defined gap-set.** Each plugin owns the field names it considers "gaps" for a given canonical kind (e.g., `yaad-wikipedia`'s `kindGaps(kind)` for `person`). A second plugin emitting `person` would have its own `kindGaps`, possibly with different field names — inconsistency risk across plugins for the same canonical kind.
2. **No fill-source constraint surfaced.** The wire shape doesn't tell the agent *where* to source values. Each agent's behavior is its own prompt-engineering responsibility.
3. **No skip-if-absent contract.** When a gap field's value isn't in `clean_content`, agent behavior is unconstrained — some fabricate, some leave empty.
4. **AI extraction is selective by vocabulary.** If the operator's CV declares `boardgame` and an article is about Risk, the AI must know `boardgame` is in scope to extract a `boardgame:risk` companion. Without that vocabulary in the wire shape, the AI misses canonical companions entirely.

ADR-0008 (vault-as-source-of-truth, DB-as-derived-index) gives the storage axis. This ADR specifies: who owns the gap contract, how plugins emit data, how the index canonicalizes, and the gap-call lifecycle.

## Decision

**Canonical-kinds own the gap-set + AI-extraction vocabulary + fill-source rules. Plugins emit source-shape data with edge-keyed structured fields; the index extracts edges and auto-creates thin canonical entities. The gap-call is bounded to one per fetch-cycle, tracked in DB only.**

### 1. CV registry — canonical-kind owns the contract

Operator config declares per kind:

```yaml
canonical_kinds:
 person:
 gaps:
 name: "Full name."
 summary: "One-paragraph summary."
 tags: "Topic tags relevant to this entry."
 birth_date: "Birth date in YYYY-MM-DD if available."
 instruction: "Fill the gaps using only the supplied clean_content. If a value is not present in clean_content, omit that field from the fill payload — do not invent values, do not pull from training data or external sources."
 boardgame:
 gaps:
 name: "Game title."
 summary: "Short summary of gameplay."
 tags: "..."
 publication_year: "..."
 instruction: "..."
```

**Skip-if-absent is the implicit and only contract.** When a gap value isn't in `clean_content`, the AI omits the field from the fill payload. There is no per-kind override (`empty` or `error` flavors) — every kind follows the same rule, encoded in the canonical instruction text. Operators tune the instruction wording per kind if needed but cannot change the underlying skip semantic.

Plugins emitting kinds inherit gap-set from registry; they don't define their own.

### 2. Wire shape — `instruction` + `canonical_vocabulary` on needs_fill

```json
{
 "ok": true,
 "state": "needs_fill",
 "entity": { ... },
 "clean_content": "...",
 "gaps": { "birth_date": "<AI-prompt>", ... },
 "instruction": "<verbatim from config>",
 "canonical_vocabulary": {
 "person": {
 "gaps": {"name": "...", "summary": "...", "tags": "...", "birth_date": "..."},
 "instruction": "..."
 },
 "boardgame": { "...": "..." }
 }
}
```

**Server passes `instruction` text verbatim from config.** No string composition. Wire field is byte-identical to the per-kind config field (or global `fill_instruction:` if no per-kind override).

`canonical_vocabulary` carries the operator's full CV registry verbatim. AI uses judgment — extract canonical companions only when the content actually supports them. Skip-if-absent is implicit (per §1) and reinforced by the instruction text.

**Provenance / prompt-injection guardrail:** canonical_vocabulary is operator-config-only. Plugins do NOT control its contents.

### 3. Plugin emits edge-keyed source data; index extracts edges

**Plugin contract simplification:** plugins emit source-shape data with **edge-named keys** for relations, not separate canonical_entities/canonical_edges fields. Plugin's `--init` declares which canonical edge types it can emit AND **the target kind for each edge type** (so the index knows what kind of canonical to auto-create at the edge target):

```yaml
# excerpt from yaad-bgg --init capabilities
emits_edges:
 designed_by: { target_kind: person }
 published_by: { target_kind: organization }
 has_mechanic: { target_kind: boardgame_mechanic }
 has_category: { target_kind: boardgame_category }
```

The plugin's `emits_kinds` declaration (already in `--init`) covers source-entity kinds; `emits_edges` covers canonical-target kinds. Together they give the index full mapping context.

Example (BGG plugin for Brass: Birmingham):

```yaml
data:
 name: "Brass: Birmingham"
 year_published: 2018
 description: "..."
 
 # edge-named keys — index parses these as canonical edges:
 designed_by: [{id: 6, name: "Martin Wallace"}, {id: 32887, name: "Gavan Brown"}]
 published_by: [{id: 21765, name: "Roxley"}]
 has_mechanic: [{id: 2040, name: "Hand Management"}]
```

**The index extracts edges from this data.** For each edge-named key + array element, the index:

1. Constructs an edge: `{type, from, to, source_plugin, source_id, properties}`
 - `type` = the data-block key (e.g., `designed_by`)
 - `from` = source-entity id (e.g., `bgg:brass-birmingham`)
 - `to` = canonical slug (derived from element)
 - `source_plugin` + `source_id` carry the plugin namespace + element ID
 - `properties` carry per-source view (name, summary if present, tags if present)
2. Auto-creates a thin canonical-entity stub at `to` if it doesn't exist: `{slug, kind, title}`. **The thin canonical lives in DB only, never in vault frontmatter.** Vault is source-shape only (per ADR-0008's vault-as-source-of-truth); reindex re-derives thin canonicals from edge endpoints across all source entities. **Canonical entities are thin nodes for traversal, never carry duplicate content.**

**Edge names are bare** (`designed_by`, not `bgg.designed_by`). Operator-config declares the canonical edge types; plugins must emit only types in that declared set. Cross-plugin name collision = operator-config error, out of scope for v1.

**Slug derivation is plugin's responsibility.** Default: sluggify(name). Plugins handle their own disambiguation (e.g., yaad-wikipedia strips parenthetical disambiguation: `Martin Wallace (game designer)` → `martin-wallace`). Cross-plugin convergence relies on plugins agreeing on slug shape; cross-plugin slug-disagreement requires manual alias merging — out of scope for v1.

**Storage shape:** vault frontmatter stays minimal — source-shape data only, with structured edge-keyed fields. The index materializes edges (with denormalized per-source-view properties) into the DB for fast query. Vault is reconstructable; DB is the bloated-for-fast-access materialized view. Reindex re-derives edges from vault.

### 4. Gap-call lifecycle (key invariant)

The needs_fill response with full instruction + canonical_vocabulary payload is the **gap call**.

- **One gap-call per fetch-cycle.** First get on a fresh ingest (or first get after a refetch) returns `needs_fill` with the full gap-call payload. The AI fills (full or partial).
- **Subsequent gets return current state, no `needs_fill`.** Holds even if some gaps remain unfilled — no reason to re-attempt with same `clean_content`.
- **Refetch is the only way to re-trigger a gap-call** (cron-driven via TTL, or explicit `force_refetch=true`). Refetch is deterministic / tool-driven, never AI.

### 5. Gap-call-done flag is DB-only, NOT vault

- Vault stores authoritative content.
- DB derives "has unfilled gaps" by intersecting CV-registry gap-set ∩ vault `data` keys. Unfilled = absent or empty.
- DB tracks **"gap-call done on this fetch-cycle"** — set when the AI submits a successful fill via `POST /v1/entities/{id}/fill` (any 2xx response, full or partial). NOT set when the `needs_fill` payload is served. This timing matters: AI failures before fill-submission (network errors, rate-limits, content-filter blocks, tool crashes) leave the flag unset, so subsequent direct GETs on the entity return `needs_fill` again — direct retry works. `/v1/needs-fill` is for batch operations; direct retries don't need it.
- **Wipe DB → flag gone.** Reindex re-derives gap-callability from vault. Regen invariant per ADR-0008.

**Cost note: DB-wipe replays gap-calls against unchanged content.** Predictable but acceptable. **Implementors must NOT add cleverness like "skip gap-calls when content_hash matches a previous attempt"** — that re-introduces a persistent flag that defeats the regen invariant. If token cost becomes painful, the right escape hatch is operator-tunable, not implicit content-hash tracking.

### 6. `GET /v1/needs-fill` — pull-based batch gap-call surface

Returns entities with unfilled gaps + their gap-call payloads. `canonical_vocabulary` lives at the **response root** (one copy per response, not per entry — see #275 amendment in §Revisions) so multi-entity pages don't repeat the registry block N times:

```json
{
 "ok": true,
 "canonical_vocabulary": {...},
 "entities": [
 {
 "id": "wikipedia:tolkien",
 "kind": "wikipedia",
 "gaps": {"birth_place": "<AI-prompt>", "occupation": "<AI-prompt>"},
 "instruction": "...",
 "clean_content": "..."
 },
 ...
 ],
 "next_cursor": "opaque-string-or-null"
}
```

**Query params:**
- `limit` — optional, default 50, cap 200. Bounds `entities` array length.
- `cursor` — optional, opaque string from a previous response's `next_cursor`. Resumes pagination.
- `exclude` — optional comma-separated list of fields to strip from the response per #275. Supported names: `canonical_vocabulary` (drops the top-level registry block when the agent has already cached it from `/v1/structure` or `/v1/kinds`), `clean_content` (drops the per-entry body when the agent has cached it from `/v1/entities/<id>`). Unknown names are silently ignored.

**Use cases:**
- **Cron-driven batch fills.** Operator's tool polls + dispatches AI to clean up gaps on its own schedule.
- **Multi-agent coordination.** A coordinator queries the queue + dispatches to worker agents.

**Direct AI failure recovery (single-entity case)** does NOT require this endpoint — per §5, the gap-call-done flag is set on fill-submission, so a failed AI (no submission) leaves the entity gap-callable; a direct GET on the entity returns `needs_fill` again.

**v1 is pull-only.** v2 adds push-via-channels (webhook/SSE) as a later refinement. Pagination is part of v1; the entities listed can include large `clean_content` blobs.

### 7. `GET /v1/structure` — introspection endpoint

Returns the full structural signature of the running yaad-index instance:

```json
{
 "ok": true,
 "version": "<config-hash-signature>",
 "kinds": {
 "person": {"is_canonical": true, "gaps": {...}, "instruction": "..."},
 "boardgame": {...}
 },
 "edge_types": ["designed_by", "published_by", "is_about", "has_mechanic", ...],
 "plugins": [
 {
 "name": "yaad-wikipedia",
 "version": "1.2.3",
 "url_patterns": ["https://*.wikipedia.org/wiki/*"],
 "supports_search": true,
 "emits_kinds": ["wikipedia:*"],
 "emits_edges": ["is_about", "references"]
 },
 {
 "name": "yaad-bgg",
 "version": "0.5.0",
 "url_patterns": ["https://boardgamegeek.com/*"],
 "supports_search": true,
 "emits_kinds": ["bgg:*"],
 "emits_edges": ["designed_by", "published_by", "has_mechanic", "has_category"]
 }
 ]
}
```

**Plugin sections come from each plugin's `--init` capability call and are cached.** Cache invalidation: plugin add/upgrade or explicit `POST /v1/plugins/refresh` (per ADR-0005 / ADR-0006). yaad-index does NOT re-invoke plugins on every `/v1/structure` call.

`version` is a structural signature that bumps on: rebuild, config change, plugin add/remove/upgrade.

**Operator tooling polls `/v1/structure` to detect config changes** by comparing the `version` field across calls. When a version-bump is detected, the operator's tool surfaces "you may want to run reindex." This replaces the earlier-proposed `/v1/cv-status` (richer, single endpoint).

**Note: `/v1/structure` does not surface a pre-computed `changed_kinds` diff.** The operator's tooling must cache + diff snapshots to identify which kinds changed. This is a regression from `/v1/cv-status`'s `changed_kinds` field. Acceptable in v1 (single endpoint, simpler server-side; client-side diff is straightforward); a dedicated `GET /v1/structure/diff?since=<version>` endpoint can be added in v2 if operator-tooling complexity warrants.

**No config-version field on every API request/response in v1.** The pure-polling shape is sufficient — agents read `canonical_vocabulary` inline from each `needs_fill` response, no client-side cache to keep coherent. Push-based version-mismatch protocol is deferred to v2 if a concrete cache-coherence pain point appears.

### 8. CV-change detection + operator-prompted reindex

When the operator's CV registry changes:

- yaad-index detects the change at startup by comparing the current config-hash to the last-stored hash in DB.
- Surfaces the diff via:
 - A startup log line naming the changed kinds
 - The `version` field in `/v1/structure` (operator tooling polls + compares)
- The system does NOT auto-reindex. Force-refetching to recover canonical data was explicitly rejected (2026-05-04 design call) — too aggressive, ignores operator intent.
- After operator runs `POST /v1/reindex`, DB re-derives. Entities with unfilled gaps under the changed/new kind become gap-callable; next read on those triggers an AI gap call.

## Worked example: gap-set widened on existing kind

```
operator config: canonical_kinds: [person]
 person.gaps: {name, summary, tags, birth_date}
 person.instruction: "Fill the gaps using only clean_content..."

agent ingests https://en.wikipedia.org/wiki/J._R._R._Tolkien
plugin emits source(wikipedia:tolkien) with structured data block
first get → needs_fill {
 gaps: {birth_date: "<prompt>", ...},
 instruction: "<verbatim from config>",
 canonical_vocabulary: {person: {...}}
}
AI fills name + summary + tags + birth_date from clean_content
DB flags wikipedia:tolkien as "gap-call done for fetch-cycle 2026-05-04T08:00:00Z"
second get → entity with filled fields, NO needs_fill (fetch-cycle done)

… two weeks later, operator widens person.gaps to add `birth_place` + `occupation`
yaad-index restart → config-hash diff detected → /v1/structure version bumps
operator tooling notices the version change → surfaces "run reindex"
operator runs POST /v1/reindex
reindex re-derives DB. wikipedia:tolkien now has 2 unfilled gaps (birth_place + occupation absent in vault data).
DB clears wikipedia:tolkien's gap-call-done flag.
Next read on wikipedia:tolkien → needs_fill with {birth_place, occupation}.
AI fills from cached clean_content (no upstream re-fetch needed).
```

The example does NOT require any new capability beyond this ADR + existing source-shape gap-fill flow.

The new-kind case (operator adds a new canonical-kind, expects existing source entities to derive new canonical companions retroactively) is **out of scope for ADR-0013 alone** — it requires (vault-carries-canonical-emissions) plus a separate capability to re-extract canonical companions from cached `clean_content` during reindex (not specced today). Treat as future work.

## Consequences

### Positive
- **Single source of truth for gap contract per canonical kind** — operator config, not plugin code.
- **AI vocabulary awareness** — AI knows what kinds to extract during fill.
- **Plugin contract simplifies dramatically** — emit source-shape data with edge-keyed fields + declare init mappings; index does the canonicalization.
- **Server-side guidance on fill-source + skip-if-absent** — single instruction string carries the rule per kind. Skip-if-absent is the implicit and only contract; no per-kind override.
- **Bounded AI cost per fetch-cycle.**
- **Regen-from-vault** — wipe DB → reindex restores correct state. Fits ADR-0008.
- **Decoupled storage shapes** — vault stays minimal (source-shape only); DB bloats with denormalized per-source-view edges for fast query.
- **Pull-based retry** via `/v1/needs-fill` enables AI failure recovery without full refetch.
- **Single introspection surface** via `/v1/structure` — covers operator tooling, agent setup, plugin discovery, and CV-change detection in one endpoint.

### Negative
- **CV registry adds operator config surface area.** Mitigated by ship-defaults: yaad-index can ship a default CV with `person` / `book` / `boardgame` shapes that operators inherit and override.
- **Migration cost.** yaad-wikipedia's `kindGaps()` deprecates; transitional fallback during migration window.
- **Cross-source view distribution at query time.** When 100 sources mention the same canonical, querying that canonical returns 100 edges with overlapping per-source views. Server doesn't dedup (views are intentionally per-source). Agent does the synthesis. Real read-time cost; acceptable for graph-shape queries.
- **DB dedup at write.** Index must write ONE canonical-entity row per slug regardless of how many edges target it. Implementation challenge.
- **Operator must run reindex explicitly** when CV changes. UX cost; mitigated by `/v1/structure` version surface.
- **Polling for change detection is passive.** Operator tooling must poll. Acceptable for v1; push-via-channels in v2.

### Neutral
- **Plugins still emit canonical-companion data during ingest** (in the source-shape data block). What changes: who owns the edge structure (index extracts from edge-keyed data) and who owns the gap contract (CV registry).

## Implementation order

1. **ADR-0013** (this document) — captures the design.
2. **Smallest first PR** — `fill_instruction` config string + `instruction` field on needs_fill response. Per-kind override deferred.
3. **Follow-up PR** — full CV registry; plugins emit edge-keyed source data; index extracts edges + auto-creates thin canonicals; gap-call lifecycle; DB flag; `GET /v1/needs-fill`; `GET /v1/structure`.
4. **Final PR** — vault-carries-canonical-emissions semantics. Lands on top of (3); without (3)'s CV registry, AI doesn't know what to extract.

## Tests

- Smallest PR: unset config → no `instruction` field. Set config → `instruction` appears on `needs_fill` responses (fresh + cache-hit).
- Follow-up:
 - Per-kind override wins over global; missing per-kind falls through to global; missing both omits the field.
 - First get post-ingest returns full gap-call payload.
 - Second get returns entity without `needs_fill` (gap-call flag set).
 - `force_refetch=true` resets flag; next get returns `needs_fill` again.
 - DB wipe → reindex → entities with unfilled gaps become gap-callable.
 - Plugin emits `data.designed_by: [...]` → index extracts N edges + auto-creates N thin canonical stubs in DB only (NOT in vault frontmatter).
 - Multiple sources emit edges to same canonical slug → DB writes ONE canonical-entity row + N edges.
 - Wipe DB → reindex re-creates all thin canonicals correctly from edge endpoints across vault sources (regen invariant for thin canonicals).
 - Gap-call flag timing: AI receives `needs_fill` but errors before submitting fill → flag stays unset → next direct GET returns `needs_fill` again.
 - AI submits successful fill (full or partial) → flag set → subsequent GET returns entity without `needs_fill`.
 - `/v1/needs-fill?limit=N&cursor=<opaque>` returns paginated entity-list with cursor.
 - `/v1/needs-fill` returns entities with unfilled gaps; AI retries via `POST /v1/entities/{id}/fill`.
 - `/v1/structure` returns kinds + edge_types + plugins; `version` bumps on config change.
 - Config change → next `/v1/structure` call returns new `version`; reindex clears the diff.

## Out of scope

- Plugin contract change to drop `Gaps` / `KindGaps` fields entirely (deferred — backward-compat through migration window).
- Server-side validation that fill submissions only contain values present in `clean_content` (untrusted-data validation; agents stay responsible).
- Auto-reindex on config change (explicitly rejected — operator decides when).
- Cross-plugin slug-disagreement / same-name disambiguation across plugins (alias-tooling, v2).
- Push-via-channels for `/v1/structure` and `/v1/needs-fill` (deferred to v2 — pull-only in v1).
- Config-version field on every API request/response (premature infrastructure — agents already get fresh CV inline from each `needs_fill` response, no cache-coherence pain to solve).
- `/v1/structure/diff?since=<version>` — server-computed diff of structural changes between two `version` snapshots. v2 if operator-tooling complexity warrants; v1 has clients diff full snapshots themselves.
- Multi-language CV / per-vault CV / namespace-scoped CV (single-tenant config in v1).

## Revisions

### 2026-05-26 — needs-fill payload de-dup + `?exclude=` (#275)

`/v1/needs-fill` previously repeated the full `canonical_vocabulary` registry on every entry of its response. With 12+ canonical kinds and dozens of gaps each, even a `limit=10` page could overflow agent context windows. This revision moves the field to the response root (one copy per response) and adds an opt-out query param for callers that have cached the registry separately.

- **Top-level `canonical_vocabulary`.** `needsFillResponse` carries the registry once at response root; per-entity `canonical_vocabulary` removed from `needsFillEntry`. Page size drops from O(N × vocab) to O(N + vocab). The `/v1/ingest` cache-hit needs_fill response shape (`ingestNeedsFillResponse`) already carried the field at root (single-entity envelope); no structural change there.
- **`?exclude=field1,field2` query param.** Comma-separated list of fields to strip from the response. v1 supports two names: `canonical_vocabulary` (drops the top-level registry block) and `clean_content` (blanks the per-entry body). Default is empty (include everything) so existing callers see no behavior change beyond the dedup itself. Unknown names silently ignored. Applies symmetrically to `/v1/needs-fill` and `/v1/ingest` cache-hit responses.
- **Caching agents.** An agent that fetched the operator's registry from `/v1/structure` or `/v1/kinds` at session start passes `?exclude=canonical_vocabulary` on every needs-fill page. An agent that fetched a body via `/v1/entities/<id>` passes `?exclude=clean_content` on subsequent revisits.
- **MCP `needs_fill` tool** at `/mcp` adds the matching `exclude` parameter so MCP-client callers get the same opt-out surface.

Status remains PROPOSED pending review of this revision.
