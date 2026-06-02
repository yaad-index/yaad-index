# ADR-0011: Vault file `aliases:` synthesized from entity titles

**Status:** Accepted (2026-05-03). Initial title-synthesized aliases shipped; generalization (multi-valued, typed-prefix, plugin-emittable, search-indexed) landed per the §"Generalization (#3)" amendment below.

## Context

Vault files materialized by yaad-index follow ADR-0008's slug-based filename convention: `<plugin-or-kind>:<slug>.md`. The slug is stable across upstream renames (Wikipedia retitles articles regularly; the entity id stays put), and the plugin namespace prefix prevents collisions across plugins (`wikipedia:caverna.md` vs `bgg:caverna.md`).

That's good for plugin authors and for git — every entity has a deterministic, stable on-disk identity.

It's bad for **vault navigation**. Obsidian (and most markdown-aware editors) resolve wikilinks like `[[Martin Wallace]]` against the **filename** of the target. With slug-based filenames, the natural human-readable wikilink doesn't resolve:

- File on disk: `wikipedia:martin-wallace-designer.md`
- Wikilink the agent or human would write: `[[Martin Wallace (designer)]]`
- Result in Obsidian: broken link.

The author has to know the slug to make a wikilink work — a bad user experience for both human authors and the AI agents we want using the vault as a graph.

Surfaced during end-to-end testing when Obsidian wasn't resolving cross-entity references.

## Decision

**yaad-index synthesizes an `aliases:` frontmatter field on every vault file at write time, populated from the entity's human-readable title field.**

For each entity yaad-index materializes:

- **Source-shape entities** (id `<plugin>:<slug>`): `aliases` includes `data.title` if present and non-empty and not equal to the slug.
- **Canonical-shape entities** (id `<kind>:<slug>`): `aliases` includes `data.name` if present and non-empty and not equal to the slug.
- If the alias would equal the slug exactly, omit it (no signal).
- If neither field is present, omit `aliases:` entirely (don't write an empty list).

The frontmatter shape becomes:

```yaml
---
id: wikipedia:martin-wallace-designer
kind: wikipedia-article
plugin: wikipedia
data:
 title: Martin Wallace (designer)
 lang: en
 url: https://en.wikipedia.org/wiki/Martin_Wallace_(designer)
aliases:
 - "Martin Wallace (designer)"
provenance: [...]
---
```

In Obsidian, `[[Martin Wallace (designer)]]` now resolves to this file via the alias. The slug-based filename still wins for git, for stability, and for plugin namespacing — aliases are purely a **navigation overlay**.

## Where the change lives

`internal/vault/format.go`'s frontmatter writer is the right place. Plugins emit `data.title` / `data.name` per their existing contracts (yaad-wikipedia already sets these); yaad-index reads those fields when building the YAML frontmatter and synthesizes `aliases:` from them.

Plugin authors don't need to change anything. Plugins remain opaque-text producers per ADR-0008.

## Consequences

### Positive

- Obsidian wikilinks via human-readable names work without filename gymnastics.
- Filenames stay slug-based: stable across upstream renames, plugin-namespaced, no cross-plugin collisions.
- Title changes (Wikipedia renames an article) re-synthesize aliases on next reindex per ADR-0008's vault-canonical write path — automatic.
- AI agents reading the vault can author wikilinks naturally without knowing slugs.

### Negative / open questions

1. **Alias collisions across entities.** Both `wikipedia:martin-wallace-designer.md` (source) and `person:martin-wallace-designer.md` (canonical, if it lands) carry the alias "Martin Wallace (designer)". Obsidian's link disambiguation picks one — typically the lexically-first match in the alias resolution path. The wrong target may surface for `[[Martin Wallace (designer)]]`.

 **v1 acceptance:** acceptable. Slug-based ids are the canonical reference for any precise lookup; aliases are a UX nicety. When the user clicks a wikilink and lands on the source-shape entity instead of the canonical one (or vice versa), they can navigate from there.

 **Future refinement (out of scope here):** a "primary" entity strategy where canonical-shape entities take precedence — yaad-index could write source-shape aliases as `<kind>: <title>` (e.g. "wikipedia: Martin Wallace (designer)") to make them deterministically distinct from canonical-shape aliases. Defer until the collision becomes a real friction point.

2. **No multi-alias support in v1.** Wikipedia articles often have redirects (e.g. "MWal" → "Martin Wallace (designer)"). Those are additional natural names that could become aliases. v1 emits exactly one alias (the title); multi-alias extension is a follow-up if it becomes useful.

3. **Reindex semantics.** Per ADR-0008, the vault file is canonical. When yaad-index re-derives the file (reindex from the entity row), the alias re-synthesizes from `data.title`. If the user has hand-edited the `aliases:` block to add a custom alias, that edit is **lost** on the next reindex.

 **v1 acceptance:** consistent with ADR-0008's frontmatter-derived-from-DB model. The `aliases:` field is a write-only output of yaad-index, not an input. Documented in this ADR + the frontmatter-format docs.

 **Future refinement:** a separate `user_aliases:` field that yaad-index never touches, OR detect and preserve unknown aliases on reindex. Defer.

4. **One more frontmatter field.** Tiny cost; YAML parsers handle it transparently.

## Alternatives considered

### A. Rename files to titles (no aliases)

`wikipedia:martin-wallace-designer.md` becomes `Martin Wallace (designer).md`. Wikilinks work directly.

**Rejected because:**

- Cross-plugin collisions: `wikipedia:caverna.md` and `bgg:caverna.md` both want `Caverna.md`. The plugin-namespacing in the filename is load-bearing.
- Title instability: when Wikipedia renames an article, the file gets renamed, breaking any historical references that used the old filename. Slug-based filenames anchor to the entity id; aliases handle title changes for free.
- Filesystem-unfriendly characters: titles can contain `/`, `:`, `?`, etc., which require sanitization. Slugs are pre-sanitized.

### B. Symlinks / shadow files

Create a sibling `Martin Wallace (designer).md` that contains only a wikilink to the slug-based file.

**Rejected because:**

- Two files for one entity violates the canonical-source model.
- Maintenance burden when titles change.
- Git diff noise on every title rename.

### C. Folder structure with title-named files inside

`wikipedia/Martin Wallace (designer).md` instead of `wikipedia:martin-wallace-designer.md`.

**Rejected because:**

- Doesn't help — Obsidian still uses filename for resolution, regardless of folder.
- Reorganizes the on-disk layout for no navigation benefit.

### D. No aliases (status quo)

Accept that wikilinks need the slug.

**Rejected because:**

- Hostile to humans navigating the vault.
- Hostile to AI agents writing natural-language wikilinks.
- Defeats one of Obsidian's main features (wikilink graph navigation).

## Action items if approved

- Implementation lands as a follow-up issue/PR against `internal/vault/format.go`'s frontmatter writer.
- Documentation note in the frontmatter-format section: `aliases:` is a yaad-index synthesized field; user edits are not preserved on reindex.

## Generalization (#3)

The v1 single-element `aliases:` list is now a multi-valued navigation overlay backed by a dedicated `entity_aliases` index table. Three orthogonal layers compose:

1. **Title-synthesis (v1, unchanged).** `internal/vault/format.go` still synthesizes one entry from `data.title` / `data.name` per the rules in §Decision above.
2. **Plugin emission.** Plugins return a `FetchResult.Aliases []string` slice; the daemon merges plugin entries with the synthesized one (synthesized first, dedup case-sensitive, see `mergeAliases`). Plugins that don't set the field stay equivalent to v1.
3. **Search index.** A new SQLite table `entity_aliases (alias PRIMARY KEY, entity_id FK, alias_kind)` mirrors `entity_notations` on the outbound-label axis. `store.Search` ORs an EXISTS subquery against this table into its WHERE so a query that substring-matches an alias returns the owning entity even when neither id nor data carries the term.

### Bare vs typed prefix

An alias entry that matches `<prefix>: <label>` AND whose `<prefix>` is in the operator's `canonical_edge_types:` registry lands with `alias_kind = 'typed'`. Everything else — plain strings, prefixed entries whose prefix is not a registered edge type — lands as `alias_kind = 'bare'`. The classifier is `store.TypedAliasPrefix` and is shared between the ingest path (`buildAliasEntries`) and the reindex re-derive path (`vaultAliasesToStore`).

Examples (with `canonical_edge_types: [author, isbn]`):

| Alias | Classification |
|---|---|
| `Martin Wallace (designer)` | bare (no prefix) |
| `author: Susanna Clarke` | typed (`author` registered) |
| `isbn: 9781635575637` | typed (`isbn` registered) |
| `unknown: prefix` | bare (`unknown` not registered) |
| `nested:key: value` | bare (colon in prefix is rejected by the shape gate) |

The split is recorded for future readers (filters, callers that want only typed entries); v1.x consumers can ignore it.

### Primary key on `alias`

`entity_aliases.alias` is the table's primary key — an alias points at exactly one entity at write time. The cross-entity collision question raised in §"Negative / open questions" #1 stays deferred; when two entities both want the same alias, the second write moves it to the second entity (via `INSERT … ON CONFLICT DO UPDATE`). v1.x acceptance: same trade-off `entity_notations` already accepted; revisit if user-friction surfaces.

### Reindex contract

The vault file remains canonical per ADR-0008. On reindex, `entity_aliases` rows are reconciled to the vault frontmatter `aliases:` list via the same DELETE + INSERT shape `ReplaceNotations` uses for `entity_notations` and `ReplaceProvenance` uses for provenance. Orphan rows the vault no longer carries are dropped. Operator hand-edits to the `aliases:` block are preserved (they become the new vault truth on the next reindex pass).

### Ingest mirrors what Marshal writes

The DB `entity_aliases` rows MUST match the alias list the vault writer puts on disk on the same pass — not just the raw plugin slice. Concretely, the ingest path passes the plugin-emitted aliases through `vault.MergedAliasesFor` (same merge `vault.Marshal` performs internally — title-synthesized alias first, plugin entries after, dedup) before calling `store.ReplaceAliases`. Without this, a plugin emitting zero aliases would write `aliases: [<title>]` to the vault frontmatter but clear the DB index, and the title alias would only surface in `/v1/search` after the next reindex.

### Classifier symmetry across ingest + reindex

`alias_kind` derivation reads the same enabled-edge-type set on both paths — `enabledEdgeTypes` = `cfg.CanonicalEdgeTypes ∪ collectPluginEmittedEdgeTypes(registry)`. Plugin-auto-activated prefixes (declared by a plugin's `--init` output without an operator-side `canonical_edge_types:` entry) classify as `typed` on first ingest, not bare-then-typed-after-reindex.

### What didn't change

- Title-synthesized aliases are still written by `vault.Writer` exactly as v1 did — the test cohort for §Decision behaviors is unchanged.
- The Obsidian wikilink-resolution shape is unchanged. Aliases the vault writes still resolve in editors that read the YAML `aliases:` property.
- Plugins that don't emit `Aliases` keep their v1 behavior.
- The `data.title` / `data.name` selection rule in §Decision is unchanged.

## References

- [ADR-0008](./0008-vault-as-source-of-truth.md) — vault as source of truth; yaad-index re-derives frontmatter from entity state on every write.
- [ADR-0009](./0009-provenance-reconciliation.md) — re-derive pattern (DELETE + INSERT in one transaction); `ReplaceAliases` mirrors this shape.
- [ADR-0010](./0010-row-level-idempotency-for-derived-tables.md) — table-as-cache discipline `entity_aliases` inherits.
- Obsidian aliases documentation: aliases are a recognized property in Obsidian's link-resolution algorithm. https://help.obsidian.md/Linking+notes+and+files/Aliases
