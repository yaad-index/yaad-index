# ADR-0021: Daemon owns slug derivation; canonical entities are edge-target labels

## Status

Proposed 2026-05-09.

## Supersedes

- **ADR-0017** (canonical-id clean-slug, plugin-owned). The clean-slug
 rule is preserved; ownership of slug derivation moves from plugin
 to daemon.

## Context

ADR-0017 established that canonical entity IDs use a plugin-agnostic
clean slug, and assigned slug derivation to plugins. Two implications
of that ADR have surfaced as load-bearing problems:

1. **Canonical entities have been treated as nodes** (vault files,
 entities-table rows). The current implementation has plugins emit
 directly into canonical-shape namespaces (yaad-bgg writes
 `boardgame:brass-birmingham` as a node alongside its source
 record). This conflates source data (the plugin's own
 representation of an artifact) with canonical labels (the
 stable cross-source identity).

2. **Plugin-owned slug derivation breaks once agents fill canonical
 references.** ADR-0019 introduced agent-fill on gaps; an agent
 filling a canonical-typed gap would need to produce a slug.
 Agents are nondeterministic, so an agent producing slugs breaks
 the deterministic-slug contract. The natural fallback is
 "agent provides a name, plugin canonicalizes the slug" — but
 that requires every fill to round-trip through a plugin call,
 which is expensive and forces agent-as-plugin extension to
 carry plugin-specific slugification logic.

The cleanest resolution: **slug derivation moves to the daemon**.
Plugins (and agents acting as plugin-extensions during fill) emit
descriptive names + kinds only. The daemon applies the clean-slug
rule deterministically.

This ADR also resolves the canonical-entity-as-node ambiguity:
canonical entities are **edge-target labels**, not materialized
nodes. The vault-file layer holds source-shape nodes only.

## Decision

### 1. Slug derivation lives in the daemon

The clean-slug algorithm (ADR-0017's rule: lowercase, hyphenate,
strip plugin-specific disambig like Wikipedia parens or BGG
year-suffix) moves out of plugin code into a single daemon-side
deterministic function. Plugins do not produce slugs.

### 2. Plugin output shape: source kind, descriptive name, edge refs

Plugin emission carries:

- **Source node payload.** Every node a plugin emits is a
 `kind: source`. Source-type information (this is a Wikipedia
 article, this is a BGG record, this is an email) lives as an
 EDGE on the source node, not as a per-plugin entity kind.
- **All cross-entity references are edges**, flat in a single
 `edges` block, keyed by edge type. Edge target is `{name, kind}`
 for one-to-one or a list of those for one-to-many. v1
 simplification: a plugin emits exactly one source per call.
 Multi-source emission can come later.

Full example for the yaad-wikipedia article on Martin Wallace:

```json
{
 "kind": "source",
 "name": "Martin Wallace (game designer)",
 "edges": {
 "is_a": {"name": "wikipedia-article", "kind": "source-type"},
 "is_about": {"name": "Martin Wallace", "kind": "person"},
 "designed": [
 {"name": "Brass Birmingham", "kind": "boardgame"},
 {"name": "Age of Steam", "kind": "boardgame"},
 {"name": "A Few Acres of Snow", "kind": "boardgame"}
 ]
 }
}
```

`is_a` describes what KIND of source this node is (a Wikipedia
article). `is_about` is the canonical-link to the person the
article describes (Martin Wallace). `designed` is the multi-target
edge from the person-subject to each game he designed (inverse
direction of yaad-bgg's `designed_by` which goes from
boardgame → person; the article is about the person, so edges
emit person-as-source-of-the-relationship). The daemon slugifies
the source node's name for the vault path + entity ID, and
resolves each edge target's `{name, kind}` into a canonical-label
edge.

No slugs anywhere in plugin output.

**`source-type` is a system-reserved canonical kind.** Operators
do not declare `source-type` in their `canonical_kinds:` config —
yaad-index reserves it for the `is_a` edge that distinguishes
source nodes (each plugin emits one source-type label like
`wikipedia-article`, `bgg-record`, `email-message`). Source-type
labels are well-known to the system; treating them as a special
canonical kind keeps the universal `kind: source` simplification
clean.

**Plugins declare a `source_namespace` in their capabilities.**
The daemon uses the declared namespace (NOT the plugin binary
name) as the vault file-path prefix. Examples:
- `yaad-bgg` declares `source_namespace: bgg` → files land in
 `bgg/<slug>.md`.
- `yaad-wikipedia` declares `source_namespace: wikipedia-article`
 → files land in `wikipedia-article/<slug>.md`.

The namespace MAY match the source-type label by convention, but
they are separate concepts: source-type identifies what KIND of
source this is (an entity-graph label), while source_namespace
controls vault-file organization (an operational path prefix).

### 3. Canonical entities are edge-target labels, optionally materialized

A canonical reference like `boardgame:brass-birmingham` is a
**label** — by default it lives in the edge graph as a target
identifier, not in the vault as a file. Multiple source nodes (a
BGG record, a Wikipedia article) can have edges pointing at the
same canonical label without that label being materialized.

**Operator-promoted canonical metadata.** Operators MAY attach
metadata to a canonical label by creating a markdown file at:

```
{ROOT}/ct/<canonical-kind>/<slug>.md
```

Example: `{ROOT}/ct/person/martin-wallace.md`. Markdown is the
fixed extension — the body supports the existing note system
(per ADR-0008), and YAML frontmatter holds operator-attached
metadata (notes, hand-curated description, operator-fill values
that aren't tied to any source).

**Not 1-to-1 with canonical labels.** Files exist only when the
operator wants to attach metadata. Most canonical labels in the
edge graph have NO corresponding file — they're pure pointers.
The file appears when the operator decides "this canonical label
deserves notes / notes / metadata."

**System never auto-materializes canonical metadata files at
edge creation time.** Edges to a canonical label do NOT trigger
file creation; the label stays a pure pointer.

**Auto-materialize when there is honest content to attach.**
The materialization principle is content-driven: when an action
produces substantive content that needs a home on the canonical
label's vault file, the daemon auto-creates the file at
`{ROOT}/ct/<kind>/<slug>.md` and proceeds with the action against
the freshly-materialized file. "Honest content" is the test —
not the token claim shape, not the action class as such.
Trigger sets today:

- **Operator-fill on an operator-strategy gap** (e.g. `rating`,
 `owned`, `want`, `played`, `knows_how_to_play` per yaad-bgg's
 declared boardgame gaps) — the fill values land in frontmatter
 `data` + `gap_state`. **Operator-fill is also the
 deliberate-create path**: when neither the entity row nor the
 vault file exists, operator-fill auto-creates both. This is the
 intentional path for "operator manually invents canonical
 metadata."
- **Dataview-paragraph-append from canonical_type fill** — when
 a canonical_type fill carries the optional `data: {...}` map
 per entry, the daemon appends one dataview paragraph per entry
 to the target canonical label's body. The structured per-event
 content (role / salary / source / etc.) is substantive — it
 carries its own structured fields and its own dedup key — so
 the vault file auto-materializes on first paragraph append
 regardless of token claim shape. This is not a "casual"
 action; it's structured data with provenance.
- **Note authored on the canonical label** — the note
 lands in the body's `## Notes` section (per ADR-0008's
 note layout). Notes **do NOT create entities from
 nothing**: when the DB row is absent, note requests return
 404. The vault file gets materialized only when the thin DB
 row already exists (typically from an ingest-time
 materialization, phase B). The asymmetry vs
 operator-fill exists because **for notes specifically**, an
 uninhibited create-from-nothing path would accumulate dangling
 entries on canonical labels that nobody meaningfully promoted.

**Note-specific token gate.** For notes only (not for the other
triggers above): notes authored by the operator (whether typed
directly or relayed through an agent acting on the operator's
behalf via a pair-claim JWT) count as operator action and
trigger the carve-out; agent-only tokens (no operator claim) do
NOT trigger materialization through the note path. The gate
exists because a casually-typed note is a low-substance action
that can accumulate noise; the dataview-paragraph trigger above
is higher-substance and does not need the same gate.

Future operator-attached metadata writes (e.g. operator-set tags,
operator-set aliases — separate issues if/when they're built)
follow the same pattern: the trigger set widens, the
materialization condition (honest content to attach) stays.

The file appears because there's now actual content (a fill
value, a note, an operator-attached note) to put in it; this
is honest materialization triggered by the first piece of
attached data, not speculative materialization on edge presence.

### 4. Source-only vault layout for plugin emissions

The vault holds source nodes from plugins. File layout uses the
plugin's namespace:

- `bgg/brass-birmingham-2018.md` — yaad-bgg source node
 (yaad-bgg's declared `source_namespace: bgg`), daemon-slugified
 name (year-disambig retained at source layer).
- `wikipedia-article/martin-wallace-game-designer.md` —
 yaad-wikipedia source node (yaad-wikipedia's declared
 `source_namespace: wikipedia-article`), daemon-slugified name
 (parens-disambig retained at source layer).

(The vault file-path prefix is the plugin's declared
`source_namespace` from §2 above, NOT the plugin binary name.
The ENTITY's `kind` is `source` regardless. The path
organization is operational; the entity model has one source
kind.)

Each source node carries edges with `{name, kind}` refs that the
daemon resolves into canonical-label edges:
`(bgg:brass-birmingham-2018) -[is_about]-> boardgame:brass-birmingham`.

The canonical metadata layer (`{ROOT}/ct/<kind>/...`) is operator-
authored only — system writes never land there.

### 5. Determinism preserved at the daemon

The clean-slug algorithm is plugin-agnostic, deterministic, and
versioned. Reindex re-runs it from current vault state.

**Cross-plugin convergence is best-effort, not guaranteed.** Two
plugins referencing the same conceptual entity produce the same
canonical label IF their emitted names normalize identically. For
example, two plugins both emitting `{name: "Martin Wallace", kind:
person}` produce `person:martin-wallace` from each.

If plugins emit name variants (`Brass: Birmingham` vs `Brass
Birmingham`, `Martin Wallace` vs `Martin Wallace (game designer)`),
they slug to different labels. Cross-plugin deduplication then
relies on the existing **alias-synthesis layer (per ADR-0011)**:
each canonical entity carries an `aliases` set; the daemon merges
canonical entities post-slugification when alias overlap surfaces
(handled by reindex / explicit merge endpoints).

Plugins are encouraged but not required to emit normalized names;
the daemon-side alias-overlap merge is the safety net.

Agents filling canonical-typed gaps follow the same contract:
provide `{name, kind}`, daemon canonicalizes. Agent's
nondeterminism cannot break the slug-contract because the agent
never produces the slug.

## Consequences

### Positive

- **Single source of truth for slug rules.** Daemon code is the
 one place to fix bugs, evolve the algorithm, version transitions.
- **Agent-as-plugin-extension works.** Agents fill `{name, kind}`
 identically to plugins; daemon handles the rest.
- **Canonical labels are cheap.** No vault file required to
 reference a canonical entity; thousands of edge labels can exist
 without filesystem pressure.
- **Edge-graph queries work cleanly.** Queries like "all
 source-shape entities with `designed_by` edge to
 `person:martin-wallace`" don't depend on whether the canonical
 node has been materialized.

### Pre-v1 ripple (not negative — expected work)

- Existing canonical-shape vault files (e.g., `boardgame/brass-birmingham.md`
 created by yaad-bgg's direct-to-canonical emission) re-canonicalize on
 next reindex into the new shape: `bgg/brass-birmingham-2018.md` source
 node + edge to canonical label `boardgame:brass-birmingham`. No
 metadata file at `ct/boardgame/brass-birmingham.md` unless the operator
 later chooses to attach metadata.
- yaad-bgg and yaad-wikipedia rewrites needed. Both carry slugification
 + per-plugin source-kind logic that moves to the new model
 (daemon slugifies, source nodes are `kind: source` with edges).
 Concrete work named in "Implementation" below.

Per the pre-v1 rule (no migration, no BC mode until after v1), the
ripple is the expected cost, not a downside. Reindex handles
transition.

### Neutral

- ADR-0017's clean-slug rule is preserved as the algorithm; only
 ownership shifts. Existing slug semantics (lowercase, hyphenate,
 strip year-suffix, strip parens-disambig) carry forward.
- Aliases + notations continue per ADR-0011.

## Implementation

Concrete v1 work this ADR triggers:

1. **Daemon: clean-slug function** — single deterministic
 slugification utility, exported across the daemon's emission
 pipeline. Tests for each canonical-name-shape (Wikipedia parens,
 BGG year, mixed-script, special chars).
2. **yaad-bgg rewrite** — emit source-shape nodes
 (`bgg/<slug-with-year>.md`) with descriptive names + edge refs
 as `{name, kind}`. Drop direct-to-canonical emission. Drop
 plugin-side slugification.
3. **yaad-wikipedia adjustment** — emit source-shape nodes
 (`wikipedia-article/<slug-with-parens-stripped>.md`) with
 descriptive names + edge refs as `{name, kind}`. Drop
 plugin-side slugification.
4. **Reindex update** — when daemon re-reads existing vault files
 on reindex, apply the clean-slug rule to migrate canonical-shape
 files to the new shape. Default behavior on existing
 canonical-shape files (e.g., `boardgame/brass-birmingham.md`):
 **kept in place** (no operator action needed; reindex doesn't
 delete state by default). Operator may opt into deletion of
 pre-existing canonical-shape files via an explicit reindex flag
 like `--prune-legacy-canonicals` (specific name TBD at
 implementation time). The migration emits source-shape nodes
 alongside the kept legacy files, and the legacy files become
 de facto operator-promoted canonical metadata until the operator
 chooses to remove them.
5. **Edge-resolution layer** — daemon receives plugin-emitted
 `{name, kind}` refs and produces canonical-label edges. Same
 logic that yaad-index already does for edge creation, just
 shifted to consume the new plugin shape.
6. **ADR-0017 marked superseded** — strikethrough or "Status:
 Superseded by ADR-0021" header update.

## Out of scope

- Backwards-compat for vault files created under the old model.
 Pre-v1; reindex handles migration.
- Multi-edition disambig in canonical labels (Age of Steam 2002 vs
 2018). Already out of scope per ADR-0017; carried forward.
- Salience / weighting of canonical labels. Separate architectural ADR.
- The workflow concept itself (forthcoming as a separate ADR);
 this ADR provides the slug-ownership foundation that workflows
 rely on.

## Action items if approved

1. Implement the daemon clean-slug utility (single function, tested).
2. Open issues for yaad-bgg + yaad-wikipedia rewrites with the new
 emission contract.
3. Update ADR-0017 status header to "Superseded by ADR-0021."
4. Update ADR-0019 references to plugin-owned slug.
5. Update ADR-0020 (search-with-gap-predicates) to reflect that
 canonical-typed gaps produce edge labels via daemon, not via
 plugin call.
6. Re-spec canonical_type fill with the new
 contract: agent fills `{name, kind}`, daemon slugifies, no
 plugin call. The earlier
 implementation assumed plugin-canonicalize-at-fill-time and is
 now stale; the re-spec produces a fresh implementation.
