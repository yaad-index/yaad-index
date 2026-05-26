# ADR-0008 — Vault as source of truth (DB as derived index)

**Status:** Accepted (2026-05-01)
**Date:** 2026-05-01
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md), [ADR-0002](./0002-api-surface.md), [ADR-0005](./0005-plugin-lifecycle.md), [ADR-0006](./0006-plugin-discovery-config-allowlist.md)
**Supersedes:** the storage-axis implication of ADR-0001 (DB-as-primary was never explicit but was the working assumption). Amends ADR-0002 in three places: snippet semantics, fill-token mechanism, new note endpoint.

## Context

ADR-0001 chose AI-first + remote-API as the structural premise. Storage was named obliquely — the README states "markdown vaults remain authoritative for human-edited content; the index is the agent-facing view." That framing was aspirational. The v1 implementation has no vault concept anywhere: no `vault.path` config, no markdown writer, no scanner, no `reindex` command, no frontmatter parser. Plugin output lands in SQLite and stays there.

This is the wrong shape for a personal-knowledge tool. Operator content — notes, references, journals — lives in an Obsidian vault: markdown, wikilinks, files. A yaad-index that holds parallel state in a SQLite blob makes the vault a second-class citizen and creates a sync problem on every read and every edit.

The shift this ADR commits to: **the vault is the source of truth for entity state.** The SQLite DB becomes a derived index — fast lookups, FTS, edge traversal — regenerable by scanning the vault. Nothing in the DB is unique state; if the file is deleted, `yaad-index reindex --vault <path>` rebuilds it.

This also revises the agent-fill loop ADR-0002 introduced. Currently `fill_token` is described as a short-lived in-memory handle; with vault-as-truth, the entity exists in the vault from the moment the plugin returns, and the entity ID itself is the durable callback. Agents can fill hours or days later without the server holding any in-flight state.

## Decision

**The vault (markdown files) is the source of truth for entity state. The SQLite DB is a derived index regenerable from the vault.** Every state mutation writes the vault first, then derives the DB from the vault. Reindex is a first-class operation.

The agent-fill loop is revised so that the entity exists from the moment yaad-index receives plugin output, the entity ID is the durable callback handle (no separate token), and the gap-fill response can land arbitrarily late.

### Flow

1. Agent calls `POST /v1/ingest` with a URL or shorthand input.
2. yaad-index dispatches to the matching plugin (subprocess, per ADR-0005). Plugin returns structured data, `clean_content`, named gaps, and any deterministically-derivable edges.
3. **yaad-index writes the partial entity to the vault as a markdown file** with frontmatter for entity ID, kind, plugin-derived data, provenance, plugin-emitted edges, and a body containing `clean_content` (verbatim) and an empty `## Notes` section. **Vault writes are atomic** — the writer serializes to a temp file in the same directory and renames into place; a crash mid-serialization leaves the previous version (or no file) intact, never a half-written frontmatter that would poison reindex.
4. yaad-index updates the DB with the partial entity (initial state, `summary` empty, agent-fillable gaps recorded).
5. yaad-index responds to the agent with the appropriate `state` (`complete` if no gaps, `needs_fill` otherwise), the entity, the `clean_content`, the gap set, and the entity ID as the callback handle.
6. The agent runs its own LLM on the `clean_content` to fill the gaps.
7. The agent calls `POST /v1/entities/{id}/fill` with the filled fields. This may be seconds, hours, or days after step 5; no server-side state expires the call.
8. yaad-index validates submitted gap field names against the entity's current gap set in the vault frontmatter.
9. **yaad-index updates the vault file first** — rewrites frontmatter with filled values, populates the body with summary + tags + edges in their canonical sections.
10. yaad-index re-derives the DB rows for the affected entity from the new vault state.

Loss of the DB is recoverable: `yaad-index reindex --vault <path>` walks `*.md` files and rebuilds every row. Loss of the vault is not recoverable from yaad-index alone — the vault is the user's responsibility (Syncthing, Git, backups).

### The three always-available gaps

Every entity carries at minimum these three gap fields. Plugins fill what they can deterministically; the agent fills the rest.

- **`summary`** — short AI-generated summary surfaced as the snippet field on `GET /v1/search`. Frontmatter; DB-indexed. **Replaces the current substring-strip snippet.** The current implementation extracts a window from the entity's data fields at query time; the redesign treats summary as a property the agent wrote into the entity, not a query-time transformation. Plugins MAY supply a starter summary if the source provides one (Wikipedia's lead paragraph, BGG's short description); the agent refines or replaces it during fill.
- **`tags`** — topic tags. Frontmatter list. DB-indexed. Plugins emit source-derivable tags (Wikipedia categories); agent infers additional tags during fill from `clean_content`.
- **`edges`** — typed relationships to other entities. Plugin emits the deterministically-derivable edges (Wikipedia infobox links, BGG designer references); agent fills the rest from `clean_content` during the LLM pass. Edges are also expressible as wikilinks in the markdown body — the human-natural authoring shape — and reindex parses both frontmatter `edges:` lists and body `[[entity:id]]` wikilinks into the same edge rows.

Plugins MAY declare additional kind-specific gaps (e.g. `complexity_assessment` for boardgames per the BGG plugin's `--init` capabilities). The three above are the floor every entity carries.

### Notes as first-class

Users author notes on entities. Decision: **notes are a first-class field on the entity model, not a separate plugin.**

- Plugins are fetchers of external content; notes are user-authored, no fetch. A note plugin would conflate two unrelated jobs. Every entity should carry notes regardless of which plugin produced it.
- Shape: `notes: [{date, text, author?}]`. Append-only; full edit/delete is out of scope for v1.
- Storage: notes live at the end of the markdown body in a `## Notes` section, one entry per dated block. Frontmatter mirrors the array for fast access; the body is the authoring surface.
- DB-indexed (FTS) — notes often carry the most personal-knowledge signal.
- Authoring path 1: `POST /v1/entities/{id}/notes` (new endpoint, takes `{text, author?}`, server stamps `date`).
- Authoring path 2: hand-edit the markdown directly. The vault is truth — a hand-edit of the `## Notes` section is just as valid as an API call. Reindex picks up either path.

### Cross-source identity

Each source-shape stays its own entity (Wikipedia article, BGG designer record, etc.) — different kinds, different data shapes. Cross-source identity is expressed as edges, not entity merging.

A canonical entity (e.g. `person:martin-wallace`) exists alongside the source entities (`wikipedia:martin-wallace`, `bgg-designer:martin-wallace`). `is_about` or `same_as` edges connect each source entity to the canonical:

```
(wikipedia:martin-wallace) -[is_about]-> (person:martin-wallace)
(bgg-designer:martin-wallace) -[is_about]-> (person:martin-wallace)
```

"Everything we know about Martin Wallace across sources" becomes a single-hop traversal from the canonical person entity. The source-shapes stay round-trippable; the human-level identity stays clean.

Who creates the canonical entity is layered:

- Plugin emits a stub canonical entity if it can derive the canonical id from source structure (Wikipedia infobox `Type: Person` → emit stub `person:martin-wallace` alongside the article entity).
- Agent creates the canonical during fill if the plugin couldn't ("this article mentions he designed Brass" — agent emits `(person:martin-wallace) -[designed]-> (boardgame:brass-birmingham)`, creating the person stub if it doesn't exist).
- the operator creates manually by writing the markdown file.

### Vault layout

Folder per kind, file per entity, slug-based filenames matching the entity ID's local part:

```
vault/
 wikipedia-articles/
 martin-wallace.md
 tehran.md
 bgg-designers/
 martin-wallace.md
 bgg-boardgames/
 brass-birmingham.md
 people/
 martin-wallace.md
 cities/
 tehran.md
```

Single tree, one file per entity, no nested namespacing in v1. Entities of canonical kinds (defined by the operator's `canonical_kinds:` config, see below) live in the same shape: `people/`, `cities/`, `countries/`, etc.

Wikilinks in the markdown body are the human-natural way to express edges (`[[people/martin-wallace]]`). Reindex parses them. Frontmatter `edges:` lists are the plugin/agent output shape; reindex parses those too. Both shapes coexist, deduplicated on write.

### Canonical kinds (operator config, not a plugin)

Canonical kinds and edge types are **operator-defined in `config.yaml`**, not registered by a plugin. There is no "core plugin." The structure of what an operator's yaad-index knows about is the operator's choice; plugins discover and emit, but the operator's config decides what materializes as nodes and edges.

**Why config, not plugin:** the canonical kinds are static data — names of types — not behavior. Wrapping them in plugin machinery (whether subprocess or in-process) buys nothing and makes "what kinds exist on this deployment?" a function of which plugins are loaded rather than a function of operator intent. Operator intent is the right axis. the operator's framing on this: *"if someone who doesn't care about board games, once talked about some game they played, it will not be a node, since they don't have it in the structure."* The structure belongs to the operator; plugins serve it, not the other way around.

**Config schema (extension to ADR-0006's `plugins:` key):**

```yaml
canonical_kinds:
 - person
 - city
 - country
 - book

canonical_edge_types:
 - is_about
 - same_as
 - lives_in
 - born_in
 - authored

plugins:
 - name: wikipedia
 path: /home/operator/.local/bin/yaad-wikipedia
 - name: bgg
 path: /home/operator/code/yaad-bgg/yaad-bgg
```

**Resolution at ingest:**

1. Plugin emits a source-shape entity (e.g. `wikipedia:martin-wallace` of kind `wikipedia-article`). Source-shape kinds are plugin-owned and always live; they do not require entry in `canonical_kinds`.
2. Plugin emits canonical-shape stubs and edges (`person:martin-wallace`, `is_about` edge). yaad-index validates each canonical kind/edge type against the operator's `canonical_kinds` / `canonical_edge_types` config:
 - **Enabled** (in config) → entity / edge materializes.
 - **Not enabled** (absent from config) → silently dropped at ingest. The source-shape entity still lands; the canonical stub and the cross-source edge do not.
3. Agent fill of canonical edges follows the same rule. An agent inferring `(person:martin-wallace) -[designed]-> (boardgame:brass-birmingham)` produces no `boardgame:brass-birmingham` node if the operator hasn't enabled `boardgame` as a canonical kind.

**Defaults:** an empty or missing `canonical_kinds:` is treated as "no canonical kinds enabled" — only source-shape entities materialize. Operators opt-in explicitly to each canonical kind they care about. There is no built-in default list; the system ships zero-canonical and the operator names what they want.

**Adding a kind later:** edit `config.yaml`, restart the server (or POST `/v1/reindex`), and re-ingest from sources where the now-enabled canonical kind would have materialized. Past ingests don't backfill canonicals automatically — but the source data is in the vault, so reindex + a future "backfill canonicals" admin operation can catch up. (Backfill semantics are explicitly out of scope for v1; the operator's flow today is: enable kinds at deployment time, accept that pre-config canonical entities don't appear retroactively.)

**Source-shape kinds** (`wikipedia-article`, `bgg-designer`, etc.) are NOT in `canonical_kinds` and don't need to be — they're owned by the emitting plugin and always present. The config gates only the canonical / cross-source layer.

**Per-kind `resolver_plugin:` (per #276).** Each `canonical_kinds:` entry MAY name a `resolver_plugin:` — the plugin authoritative for entities of that kind. When set, `canonical_type` gap fills (agent-fill via `POST /v1/entities/{id}/fill` and operator-fill via `POST /v1/entities/{id}/operator-fill`) targeting that kind require the canonical id to already exist in the store; the agent should have ingested through the named plugin first. Fills against an unresolved target return 422 `unresolved_target` with a suggested-action hint naming the resolver. Kinds without `resolver_plugin:` set fall through to the existing auto-materialize path — agents / operators add new entries freely. Plugin-emit edge paths are unaffected (the plugin IS the resolver when it emits its own canonical-edge targets).

Operator-fill can bypass the gate per-call with `?allow_unresolved=true` — useful for legitimately registering homebrew / custom entities that aren't in the resolver plugin's index. The bypass is stamped into the commit message (`... (allow_unresolved)`) so the vault history shows the override was intentional. Agent-fill has no bypass: the whole point of `resolver_plugin:` is to block phantom-entity creation from agent typos.

```yaml
canonical_kinds:
  boardgame:
    resolver_plugin: bgg
  github-pr:
    resolver_plugin: github
  person: {}            # no resolver — free creation OK
  book: {}              # no resolver yet
```

### Callback ID = entity ID

ADR-0002's `fill_token` (described as `ft_abc123` with a short expiry) becomes the entity ID itself. No in-memory token registry; nothing to lose on server restart; agent can fill at any later time.

Validation: gap field names submitted to `POST /v1/entities/{id}/fill` are checked against the entity's *current* gap set in the vault frontmatter.

**v1 fill-conflict semantics: 409 Conflict.** If any submitted field name is not in the entity's current gap set (because it was filled by another agent, by the operator's hand-edit, by reindex picking up an external source, or because the field was never a gap to begin with), the fill is rejected with `409 Conflict`. The response body lists the rejected field names so the agent can decide whether to re-fetch the current entity and overwrite via a future endpoint, or accept that its fill was redundant. The 409 applies per-call, not per-field — submitting four fields where one is already filled fails the whole call; the agent decides the recovery shape. Merge semantics for specific field types (e.g. `tags` as a union) are a deliberate follow-up; the v1 contract is unambiguous so agent implementations don't diverge on recovery behavior.

`fill_token` and `fill_token_expires_at` fields in the `needs_fill` response are removed.

### Entity ID scheme

Entity IDs are `<kind>:<slug>` for canonical entities (`person:martin-wallace`, `city:tehran`) and `<plugin>:<slug>` for source-shape entities (`wikipedia:martin-wallace`, `bgg-designer:martin-wallace`, `bgg-boardgame:brass-birmingham`). Slug derivation is **deterministic** and **plugin-owned**: the plugin emits the canonical slug from the source identity (URL, upstream stable ID, page title), and yaad-index never re-derives or mutates it.

Slug rules:

- Lowercase ASCII; non-ASCII letters transliterated by the plugin if natural (or kept as-is — plugin's call, must be deterministic).
- Hyphen-separated tokens; no spaces.
- URL-path-normalized when the source identity is a URL (Wikipedia: page slug stripped of query params and fragments; BGG: numeric ID OR canonical-name slug from the API response).
- Plugin re-emits the same slug for the same source identity across calls — that's the durable-callback contract.

**Re-ingest semantics:** if `POST /v1/ingest` is called for a URL whose plugin produces a slug already in the vault, the existing entity is **updated in place**. Provenance gets a new entry; data fields are refreshed; gap set is recomputed (filled fields stay filled, new gaps added). The entity ID does not change. Agents holding a previous-call ID can continue to fill against it.

Plugins that derive slugs from data (rare, since most sources have stable upstream IDs) MUST document the derivation in their `--init` output. The Wikipedia plugin's case (`wikipedia:<en-page-slug>`) is canonical: page-rename on Wikipedia produces a different slug, which yaad-index treats as a different entity (the old one is orphaned, surviving as historical state until a future "merge" endpoint or hand-edit consolidates it). This trade-off favors stability over follow-renames; agents preferring follow-renames can implement that as a separate concern.

### DB role

Pure derived index. Holds:

- Entity rows (id, kind, frontmatter fields, summary).
- Edge rows (from, to, type, metadata).
- FTS index over data fields, summary, tags, notes, body.
- Reindex bookkeeping (per-file mtime, content hash, last_indexed_at).

Schema can change freely; reindex regenerates. No migration scripts; no schema versioning beyond what reindex needs to detect "rebuild required."

### New endpoint and CLI surface

- `POST /v1/entities/{id}/notes` — append a note.
- `POST /v1/reindex` — trigger a full reindex from the vault. v1 has no auth layer (per ADR-0001's network-topology trust model: localhost-only or trusted-network-only deployment); the endpoint is reachable to anyone who can reach the server. A future auth ADR may gate this and other state-mutating endpoints; until then, the trust model is the network boundary.
- CLI: `yaad-index reindex` — full or incremental rebuild from the vault.
- Config: new top-level key `vault.path` (required). Missing or unreadable → fail-fast at server start.

### Frontmatter schema (v1)

The markdown frontmatter for an entity carries:

| Field | Type | Source |
|---|---|---|
| `id` | string | plugin-derived, immutable |
| `kind` | string | plugin-emitted (source-shape) or operator-config-allowed (canonical-shape) |
| `plugin` | string | which plugin produced this entity (`wikipedia`, `bgg`); empty for canonical-shape entities materialized from a plugin's canonical-stub emission, since the canonical layer is operator-defined |
| `data` | map | kind-specific fields (e.g. for `wikipedia-article`: `title`, `lang`, `url`, `extract`) |
| `provenance` | list | append-only fetch records: `{source, fetched_at, ok, error?, error_message?}` |
| `summary` | string | agent-filled gap; empty until fill |
| `tags` | list of strings | plugin-emitted + agent-filled |
| `edges` | list of `{type, to, metadata?}` | plugin-emitted + agent-filled (also expressible as body wikilinks) |
| `notes` | list of `{date, text, author?, operator?}` | append-only; mirrored to body `## Notes` section. Per yaad-index a prior PR the body heading row reads `<date> — <author> @ <operator>` when both are set; legacy `<date> — <author>` rows still parse and round-trip with empty `operator` |
| `gaps` | list of strings | currently-unfilled gap field names; consumed by the fill endpoint's validation |

The body of the markdown file holds the `clean_content` (verbatim from the plugin) followed by `## Edges` and `## Notes` sections that mirror the frontmatter for human authoring. Reindex parses both representations; on write, the canonical source is the frontmatter, and the body sections are regenerated.

Implementation PRs MAY add fields (e.g. `last_indexed_at` for reindex bookkeeping, kind-specific data subfields) but MUST NOT remove or rename the fields above without a follow-up ADR.

### File watcher (optional, deferred)

Hand-edits during a server run cause DB drift until the next reindex. Mitigations:

- **No watcher (v1 default):** accept staleness. Periodic full reindex (cron or `make reindex`) is the operator's responsibility.
- **Watcher (v1+):** fsnotify-based file watcher detects writes; debounced reindex of affected files.

Watcher complexity is bounded but real. Pushed to a follow-up ADR if/when the staleness window becomes an issue.

## Alternatives considered

- **DB primary, vault as periodic export.** Keep the existing DB-first model; write markdown to the vault on a cadence as a one-way export. Rejected because the operator authors in Obsidian — bidirectional sync from a primary DB is fragile, and a one-way export defeats the personal-knowledge-first framing. The vault has to be the place edits happen.
- **Plugin writes to vault directly.** Plugins emit markdown files instead of structured JSON. Rejected because it couples every plugin to the filesystem (breaking the subprocess-and-stdio shape from ADR-0005), turns plugin authors into vault-schema authors (drift across plugins), and conflates fetching with persistence. yaad-index keeps the orchestration role; plugins stay thin.
- **Token registry kept.** Keep ADR-0002's `fill_token` as a separate handle. Rejected — durable callback via entity ID is structurally simpler, removes in-memory state, removes expiry edge cases, and lets agents pick fills back up after restarts or long delays.
- **Note plugin.** Treat notes as a plugin like any other source. Rejected because plugins fetch external content; notes are user-authored. Conflating breaks the plugin contract; user-data should not flow through the same protocol as fetch-data.
- **Entity merging across sources.** Single canonical entity rows with multi-source provenance, instead of separate source entities + canonical. Rejected because it collapses kind-shape — a Wikipedia article and a BGG designer record have different fields, different lifecycle, different cleaning logic; merging them into one row makes round-trip back to source representation lossy. Edges-between-source-and-canonical preserves both axes.

## Consequences

### Positive

- The vault is the human-facing artifact. Obsidian works directly on yaad-index entities; no sync layer.
- DB loss is non-fatal — reindex restores from the vault.
- DB schema can evolve freely; reindex regenerates rows under the new shape.
- Hand-edits in the vault are first-class. A user editing the `## Notes` section of `vault/people/alice.md` produces the same result as an API call.
- Agent fills can land arbitrarily late. The session-coupling of `fill_token` goes away.
- Cross-source identity via edges keeps source-shape intact. Future plugins (IMDb, Goodreads, etc.) plug in alongside without renegotiating the canonical kinds.

### Negative / costs

- Vault writes are now load-bearing in the ingest hot path. Slower than DB-only — markdown serialization plus a file write per ingest. Acceptable for personal-scale; revisit if a bulk ingest path needs batching.
- Hand-edits during a server run cause DB drift until next reindex. Mitigated by an optional file watcher; without one, operators run periodic reindex.
- Implementation is substantial. Vault writer + parser + reindex + ingest-writes-vault + fill-endpoint-vault-first + snippet→summary + canonical-kinds config schema + notes endpoint + AGENTS.md update = eight PRs of work (see Action items). Sequenced, not concurrent (per the one-task-per-worker rule).
- Existing v1 behavior changes. Pre-alpha state allows the break — no production deployments to migrate. Test data already ingested in the existing DB is discardable.

### Migration

There is no production yaad-index. The existing DB schema can be tossed; the existing API surface gets the documented amendments. No backward-compatibility shims; the redesign is the first real v1.

## Open questions (resolved during implementation)

- ~~**Frontmatter schema**~~ — codified in the Decision (see "Frontmatter schema (v1)" subsection). Implementation PRs may add fields, not remove.
- **Vault layout depth** — flat `<kind>/<slug>.md` or deeper (e.g. `<kind>/<first-letter>/<slug>.md` for very large directories). Default flat; revisit when a directory has >10k entries.
- **Wikilinks vs frontmatter edges precedence** — when both express the same edge, dedupe on write. Resolved in the markdown-parser PR.
- **File watcher** — deferred. v1 is reindex-only; watcher is a follow-up ADR.
- **Multi-vault** — not in v1. ADR-0001's "one vault per server instance" stays.
- **Notes edit/delete** — append-only in v1. Edit-by-note-id is a follow-up if/when it becomes a real need.
- **Vault edits during a server run** — acceptable staleness in v1. Operator runs reindex on a cadence; future watcher closes the gap.
- **Conflict resolution on concurrent fills** — locked in the Decision (Callback ID = entity ID subsection): v1 returns 409 Conflict with the rejected field names. Merge semantics for specific field types (e.g. `tags` as a union) are a deliberate follow-up.

## Action items if approved

Sequenced PRs from the implementer after ADR merges. Each is a separate dispatch under the one-task-per-worker rule.

1. **Vault writer + reader.** Markdown serialization (entity → markdown), parser (markdown → entity), frontmatter schema. New `internal/vault/` package. Tests over round-trip fidelity.
2. **Reindex command.** `yaad-index reindex` walks the vault, parses every file, regenerates DB rows. Incremental + full modes. Config key `vault.path`.
3. **Ingest writes vault.** `POST /v1/ingest` writes markdown to the vault first, then DB. Provenance entries in frontmatter.
4. **Fill endpoint vault-first + entity ID callback.** `POST /v1/entities/{id}/fill` updates vault first, then DB. Remove `fill_token` and `fill_token_expires_at` from response shapes.
5. **Snippet → summary.** Replace the substring-strip snippet implementation with reading `summary` from the entity. Search results carry the agent-filled summary directly.
6. **Canonical-kinds config schema + validation.** Extend the config loader (per ADR-0006) with `canonical_kinds:` and `canonical_edge_types:` keys. Wire the ingest pipeline + fill endpoint to validate plugin-emitted and agent-emitted canonical entities/edges against the operator's enabled set, dropping entities/edges for kinds not in config.
7. **Notes endpoint and parsing.** `POST /v1/entities/{id}/notes`, body-section parsing, frontmatter mirror, FTS indexing.
8. **AGENTS.md update.** Architecture overview reflects vault-as-truth; configuration section documents `vault.path`.

ADR-0008 itself is one PR (this document only); the eight implementation PRs follow.
