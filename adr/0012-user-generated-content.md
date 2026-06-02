# ADR-0012: User-generated content as a first-class entity source

**Status:** Accepted (2026-05-03; amended 2026-05-05 for section-level edit)

## Context

yaad-index's existing entity sources are external: plugins fetch from upstream (Wikipedia, BGG, etc.), structure the response, and yaad-index persists the result. The agent's role is fill-the-gaps after the plugin lands the source-shape entity.

There's a second pattern this doesn't cover: **content the user originates with the agent's help, not from any external source**. Examples by shape (not anchoring to any specific deployment):

- Standalone notes / observations / thoughts the user wants persisted as graph entities, not free-floating markdown.
- Project / design write-ups that should connect to other entities (people, ideas, tags) via the same edge mechanism plugin entities use.
- Long-form narrative content the user composes interactively with the agent.

Today these live in vault folders outside the entity-graph layer. Agents can't link them via edges; canonical-axis tools (search, kind-discovery) don't see them; notes-on-entity doesn't apply.

Routing this through a plugin (operator-configured subprocess that the agent invokes via Fetch) would work mechanically, but the lifecycle is wrong: plugins fetch from upstream, expire on TTL, refresh on cache miss. User-generated content has no upstream and no expiry; the agent doesn't refetch a memory.

A first-class in-process entity source pattern fits cleaner — no subprocess, no fetch-loop lifecycle.

## Decision

**Add a built-in entity source `user-content` that lets the agent originate entities directly, with the same edge / canonical-axis / notes machinery the plugin path uses.**

### Entity shape

- **ID:** `user-content:<slug>`. Slug derived from a human-readable title at creation time (per ADR-0008's slug rules).
- **Mandatory frontmatter fields:**
 - `kind: user-content`
 - `tags: [...]` — non-empty list, agent + user choose
 - Provenance with `source: user` (one entry per write; mirrors the plugin-path provenance shape from ADR-0009)
- **No `notations:` field** — UGC has no upstream URL forms to cache (the lookup-first cache does not apply here).
- **No expiry by default.** TTL semantics do not apply unless the user opts in for a specific entity.
- **`body`** holds the markdown body as the agent / user composed it. Named `body` (deliberately *not* `raw_content`, the plugin-emit path's name) to avoid a vault-writer collision: per ADR-0002, plugin `raw_content` is upstream-only and never persisted to the vault, while UGC body IS the persisted body. Sharing the name on the same `vault.Writer` would silently fold two contradictory contracts into one persistence path. The plugin-side rename is out of scope — that contract is settled at the API boundary as `clean_content` already.

### Creation flow

1. User asks for a new entity (verbal or structured request to the agent).
2. Agent and user iterate on the content interactively.
3. Agent submits the full document via a new endpoint (`POST /v1/user-content` — strawman; final shape per implementation PR).
4. yaad-index persists to vault + DB in one shot. State is `complete` immediately — **no `needs_fill` step on creation** (the agent submitted the body it composed; there's no plugin-emitted gap set the agent then fills). Gaps remain mechanically possible if a future workflow surfaces them on the entity (e.g., a canonical-axis stub that gets created from this UGC and then gets filled later); the v1 just doesn't use them on the create path.

The agent is the body-producer for this entity source: it composes the body, fills the structured fields, persists. No subprocess invocation, no `--init` capability handshake — `user-content` is a privileged in-process source with the contract "agent submits, yaad-index persists, no Fetch loop." This is NOT a "fake plugin": plugins are external subprocess binaries on a fetch-loop lifecycle (TTL, cache, refresh); this surface is the absence-of-plugin path.

### Update semantics (v1) — section-level edit

UGC v1 supports **section-level edit and pagination** as the editable granularity. Whole-body PUT was rejected as the v1 shape: a 5000-word UGC with a one-paragraph fix means resending all 5000 words on every edit, and concurrency edit-storms corrupt every time. Paragraph-level was rejected too: stable per-paragraph IDs require content-hash anchoring, which is more complex than v1 needs. Section-level is the natural balance — markdown headings change less often than paragraph order, are markdown-native, and address what operators actually reference ("the notes section about X").

**Containment model.** Every markdown ATX heading (`#`..`######`) is one addressable section in a flat list. A section's body extends from the line after its heading until the next heading of same-or-shallower depth, meaning DEEPER nested headings (and their content) are TEXTUALLY INCLUDED in the parent's body. Editing `# Top` rewrites the whole sub-tree below it; editing the leaf `### Foo` rewrites just its leaf content. The granularity choice IS the section choice — no "recursive" flag, no surgical-vs-destructive split. Pre-heading body is the implicit "section 0"; a body with no headings collapses to one section.

**Endpoints :**

```
POST /v1/user-content — create new UGC entity
GET /v1/user-content/{id} — read whole entity (with paginated sections)
GET /v1/user-content/{id}/sections — list sections (paginated, cursor-based)
GET /v1/user-content/{id}/sections/{sec} — read one section's body
PUT /v1/user-content/{id}/sections/{sec} — replace one section's body
DELETE /v1/user-content/{id} — delete entity
```

`{sec}` addressing: heading-text-slug for unique-heading sections OR positional index (`0`, `1`, …). Server canonicalizes either form. Positional is the disambiguating fallback when two headings slugify identically (rare for UGC, but possible).

**Auth.** All endpoints require Bearer JWT. Create stamps the JWT subject as `author` and the operator from the pair-claim. Edit / delete is restricted to callers whose JWT operator pair-claim equals the entity's stored operator — any agent under that operator may mutate, regardless of which agent originally wrote the entity. The `author` field stays as provenance (who wrote what) but is not part of the permission check. Cross-operator edit returns 403 `operator_mismatch`. (Amended #377: the prior rule conflated author-as-identity with author-as-edit-grant; the conflation blocked legitimate multi-agent flows under a shared operator and the explicit author check is redundant — operator-equality already prevents cross-operator tampering.)

**Concurrency.** PUT requires `If-Match: <etag>` to prevent lost-update on simultaneous section edits; the etag scheme (per-section body hash vs. whole-entity content-hash) is settled by the write-endpoint implementation PR. 412 on mismatch.

**Version history.** Auto-commit produces a per-edit git commit, and that IS the version history for v1. No separate revisions table.

**Out of scope for v1:**
- Paragraph or block-level edit (separate ADR if pursued).
- Section reordering (separate operation).
- Multi-section bulk edit (single section per PUT).
- Section rename without body rewrite (delete-and-create or full-body re-PUT through the parent section).

**Hand-edit fallback.** Operators editing UGC by hand-modifying the vault file directly remain supported (per ADR-0008's hand-edit-then-reindex flow). Reindex re-derives the DB and section list from the new vault state.

Multi-write provenance under `source: user` accumulates as:
- The original create write (one row).
- Each section-level edit (one row per PUT).
- Each hand-edit-and-reindex (one row, captured by reindex).
- Each note append (a separate `notes:` write, not a fresh provenance row).

### Edges

UGC entities can declare arbitrary edges in their frontmatter, same shape as plugin-emitted edges:

```yaml
edges:
 - type: idea
 to: <target> # agent decides per case (existing entity, auto-create, or free-text label)
 - type: design
 to: <target>
```

**Vault holds all edges** (every type the agent emits) — same universal pattern as plugin entities .

**DB filters per `canonical_edge_types` operator config:**
- Edge type in config → edge row written to the `edges` table; queryable via `?with_edges=` and the search surface.
- Edge type NOT in config → metadata stays in vault frontmatter; no DB row. Reindex picks the edge up later if/when the operator adds the type to config.

This is the same operator-gating story plugin entities already follow ; UGC inherits it without invention.

### Edge target flexibility

The edge `to:` field admits three shapes; the agent picks per case based on what makes sense for the content:

1. **Reference an existing entity** by id — `to: person:susanna-clarke` when the agent recognizes the canonical entity exists.
2. **Auto-create a stub** — agent emits a canonical stub on the same write path (the vault-canonical model), edge points at it.
3. **Free-text label** — `to: "<text>"` when the relationship is a description rather than a pointer to a queryable entity.

Operator config gating applies regardless of which shape — DB rows only for in-config edge types.

### Canonical-kind edges (idea / design / memory / etc.)

The user-facing edge types — `idea`, `design`, `memory`, etc. — are simply edge types the operator registers in `canonical_edge_types`. There's nothing UGC-specific about them; any plugin can emit edges of those types too. `user-content` entities just happen to be the most common emitter of `idea` / `design` / `memory` edges.

### Notes

UGC entities support notes per ADR-0010 — same body-section single-column-table format, append-only.

### Storage

- Vault path: `<vault>/user-content/<slug>.md`. Top-level folder mirroring the kind prefix (matches the existing `<vault>/<kind>/<slug>.md` convention from ADR-0008).
- File body holds `body` + the standard `## Edges` / `## Notes` rendered sections.

## Consequences

**Enables:**
- The agent and user can originate entities cleanly without faking a plugin or routing through external-fetch lifecycle.
- UGC participates in the existing graph: search, edges (via `canonical_edge_types` config), notes, single-hop GET body.
- A future "render UGC linked from a plugin entity" view comes for free — the edge layer is uniform.

**Costs:**
- New `POST /v1/user-content` endpoint surface to design + maintain.
- Reindex needs to handle `user-content/` folder alongside per-plugin folders (mostly free if the per-kind walk is folder-agnostic).
- Provenance schema needs a `user` source value tested against the existing `Source TEXT NOT NULL` constraint; just data, no schema change.

**Trade-offs we're accepting:**
- No backwards-compat path for any existing not-graph-tracked vault content. New surface; existing files outside the entity-graph layer remain outside until ingested via the new endpoint.
- The "agent decides" target-shape flexibility leaves room for inconsistency. Mitigation: operator can register edge types they expect to be queryable, leaving free-text shapes only for genuinely ad-hoc relationships.

## Action items if approved

1. Implement `POST /v1/user-content` endpoint in yaad-index.
2. Vault writer handles `user-content:<slug>` ids: compute folder path, write file with the standard frontmatter + body shape.
3. Reindexer parses `user-content/*.md` files alongside plugin folders.
4. Provenance store accepts `source: user` rows.
5. yaad-mcp gains a `create_user_content(title, body, tags, edges)` tool — the agent's surface.
6. Update `docs/plugin-flow.md` (or split to `docs/entity-source-flow.md` if the doc grows beyond plugins) with the UGC pattern.

## Open follow-ups

- Slug derivation rules for UGC titles (ASCII-fold, dedupe-suffix on collision). Punt to the implementation PR.
- Reindex behavior on hand-edit of UGC body / frontmatter — preserve user edits or re-render. Same open question as for notes.
- Search inclusion of UGC `body` — deferred. UGC will participate in `search_local` once that surface gets text-body indexing.
