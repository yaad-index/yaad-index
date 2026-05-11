# ADR-0017: Canonical entity IDs use plugin-agnostic clean slug

## Status

**Superseded by ADR-0021** (2026-05-09).

The clean-slug *algorithm* (lowercase, hyphenate, strip
plugin-specific disambig) is preserved by ADR-0021. What changed:

- **Slug derivation moves from plugin-owned to daemon-owned.**
 ADR-0017 placed slug derivation inside each plugin; ADR-0021
 centralises it in the daemon. Plugins (and agents acting as
 plugin-extensions during fill) emit `{name, kind}` only.
- **Canonical entities are edge-target labels, not nodes.**
 ADR-0017 implied canonical entities live as vault files
 alongside source-shape entities; ADR-0021 re-frames them as
 pure labels in the edge graph. Operator-promoted metadata
 files at `{ROOT}/ct/<kind>/<slug>.md` are the optional
 materialization path.

Read ADR-0021 for the current model. The historical context
below describes the v0/v1-transitional state and explains why
the duplicate-canonical-id problem motivated the clean-slug
rule in the first place.

Originally accepted 2026-05-07.

## Context

Plugin-emitted entities live at two distinct layers in the daemon's
data model:

1. **Source-shape entities** (the plugin's own representation): id is
 `<plugin-name>:<slug>`. The slug is the plugin's preferred
 human-readable form for that source artifact and may legitimately
 carry plugin-specific disambiguation (Wikipedia's `(disambig)`
 parens, BGG's edition-year suffix, user-content's free-form
 namespace).
2. **Canonical entities** (per ADR-0008 §canonical-shape stubs): id
 is `<canonical-kind>:<slug>`. The slug must be plugin-agnostic so
 that two plugins emitting the same conceptual entity converge on
 the same id.

Today's emission code in yaad-wikipedia and yaad-bgg leaks
plugin-specific disambig into canonical IDs. Concretely, the live
vault (post-2026-05-07 wipe + re-ingest) shows duplicates that
should have merged:

- `person:martin-wallace` (bgg-derived from a `designed_by` edge on
 `boardgame:brass-birmingham-2018`)
- `person:martin-wallace-game-designer` (wikipedia-derived from
 `is_about` edge on `wikipedia:martin-wallace-game-designer`)

Same person, two canonical ids, no cross-plugin merge. Same shape
for `boardgame:brass-birmingham-2018` (bgg, year-suffix) vs
`boardgame:brass-board-game` (wikipedia, parens-stripped slug),
`boardgame:age-of-steam-2002` vs `boardgame:age-of-steam-game`,
`boardgame:london-board-game`, etc.

The root cause: each plugin's slug-mint logic was carried forward
into canonical-emission. The convention to separate them was
implicit; this ADR makes it explicit.

## Decision

**Canonical entity IDs use the cleanest possible plugin-agnostic
slug.** Plugin-specific disambiguation (year, parens-suffix,
namespace-prefix-shaped tokens) stays at the source layer only.

### Plugin-emitted node IDs (source-shape)

Format: `<plugin-name>:<plugin-specific-slug>`

The `<plugin-specific-slug>` is the plugin's own surface for the
artifact. It MAY include disambig the plugin uses to round-trip back
to the source — e.g. Wikipedia's parens-disambig (`martin-wallace-game-designer`),
BGG's slug-year (`brass-birmingham-2018`).

### Canonical entity IDs

Format: `<canonical-kind>:<clean-slug>`

The `<clean-slug>` is the entity's title slugified WITHOUT the
plugin-specific disambig. Concretely:

- Strip Wikipedia parens-disambig:
 `Martin Wallace (game designer)` → `martin-wallace`
- Strip BGG year-suffix:
 `Brass: Birmingham (2018)` → `brass-birmingham`

### What this means by example

Across `boardgame:brass-birmingham-2018` (bgg-source) +
`wikipedia:brass-board-game` (wiki-source), both converge on the
canonical `boardgame:brass-birmingham`.

Across `bgg`-emitted `person:martin-wallace` (from a designed_by
edge) + `wikipedia:martin-wallace-game-designer` (source), both
converge on the canonical `person:martin-wallace`. The plugin-source
ID is free to use the longer form; canonical reference is the clean
slug.

### Source notations stay plugin-specific

The `notations:` array on canonical entities continues to carry
plugin-specific identifiers per ADR-0011 alias synthesis. So the
canonical `boardgame:brass-birmingham` carries notations like
`bgg: 224517` (bgg's numeric notation),
`https://boardgamegeek.com/boardgame/224517` (bgg URL),
`wikipedia: Brass: Birmingham` (wikipedia notation), etc.

This separation is load-bearing: **canonical IDs are the universal
surface; notations + aliases are the plugin-specific surface.**
Operators reference canonical IDs when querying or wiring user-content;
plugins reference notations when re-fetching their own source artifact.

## Consequences

### Positive

- **Cross-plugin entity merge becomes free.** Two plugins emitting
 the same canonical entity collapse to one row in the entities
 table.
- **Operator surface gets cleaner.** Querying by `person:martin-wallace`
 always returns the right thing, regardless of whether wikipedia
 or bgg ingested first.
- **User-content can reference canonical IDs.** the operator writing
 `[[person:martin-wallace]]` in a vault page resolves cleanly
 without knowing which plugin ingested the metadata.
- **Future plugins inherit the rule.** A new plugin emitting
 canonical kinds doesn't need to negotiate naming with existing
 plugins; the clean-slug rule is the only contract.

### Negative

- **Edition-collision in canonical space.** Multiple editions of the
 same titled boardgame (Age of Steam 2002 original vs 2018 reprint)
 collide on `boardgame:age-of-steam`. Disambig falls back to alias
 synthesis + per-edition notations. Operators wanting per-edition
 granularity reference via notations or hand-built canonical IDs
 with explicit suffix.

### Neutral

- The plugin-source-ID layer is unchanged. Wikipedia still uses
 parens for round-trip, bgg still uses slug-year for cache keys.

## Implementation

Concrete plugin work landed alongside this ADR:

- **/ a prior PR** — strip parens-disambig from
 canonical entity ID in wikipedia's emission path.
- **/ a prior PR** — drop year-suffix from canonical entity
 ID in bgg's emission path.

Future plugins MUST follow the same shape: slug for the canonical
entity uses the cleanest plugin-agnostic form; the source-layer
slug may carry whatever disambig the plugin needs for its own
namespace.

## Out of scope

- Backwards-compat for legacy-shaped canonical entities. Pre-1.0;
 re-ingest produces the new shape and that's the only path. BC
 handling lands after v1.0.
- Cross-plugin alias-merge for the canonical entities once they
 share an ID. ADR-0011's alias-synthesis layer already handles the
 merge; this ADR's clean-slug rule just gets the ids onto the same
 key.
- Per-edition disambig in canonical IDs (Age of Steam 2002 vs
 2018). Solved at the alias / notation layer, not by re-injecting
 year into canonical IDs.
