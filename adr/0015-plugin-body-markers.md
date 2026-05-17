# ADR-0015 — Plugin body ownership: marker-pair contract for plugin-emitted markdown body content

**Status:** Proposed (2026-05-06)
**Date:** 2026-05-06
**Depends on:** [ADR-0008](./0008-vault-as-source-of-truth.md), [ADR-0012](./0012-user-generated-content.md), [ADR-0014](./0014-plugin-attachment-contract.md)
**Tracked in:** yaad-index/(the trigger; the body-rendering work that surfaced the gap)

## Context

Plugins emit FetchResult JSON; the daemon writes that into a vault entity file (`<kind>/<local-id>.md`). The file has two surfaces:

1. **Frontmatter** — structured YAML the daemon manages. Plugin-emitted, but ADR-0012 establishes preservation rules: Summary / Tags / Notes / Edges are merged-in-place across re-ingest (operator additions survive).
2. **Body** — the markdown content under the frontmatter. Today the daemon REPLACES the body wholesale from plugin output on every re-ingest.

This becomes a problem the moment a plugin starts emitting non-trivial body content (e.g.,: render boardgame entity body with title heading + image embed + description text). An operator who appends `## Notes` with their own observations gets that wiped on the next re-ingest. ADR-0012's user-content surface (`/v1/user-content/`) is a separate entity track; it doesn't cover hand-written prose inside a plugin-emitted entity's `.md`.

The issue is general: any plugin emitting body content (yaad-bgg, future yaad-paper, yaad-letterboxd, etc.) collides with operator hand-edits the same way.

## Decision

**The daemon wraps plugin-emitted body content in a generalized marker-pair (`<!-- yaad:plugin start -->` / `<!-- yaad:plugin end -->`) when writing entity files. On re-ingest, the daemon detects the existing marker pair, replaces ONLY the content between markers with the plugin's new emission, and preserves everything outside verbatim. Plugins emit body content as plain markdown; they don't see the marker shape. Operators who append content above, below, or alongside the marked region keep their additions across every re-ingest.**

### 1. Marker shape

```markdown
<!-- yaad:plugin start -->
[plugin-managed body content goes here]
<!-- yaad:plugin end -->
```

- HTML-comment shape so it renders cleanly in Obsidian + standard markdown viewers (invisible to readers).
- Generic `yaad:plugin` rather than per-plugin (`yaad:bgg`, `yaad:wikipedia`) so the daemon implementation is plugin-agnostic. One implementation, all plugins.
- Single block per entity for v1; multi-block (multiple plugins emitting body for the same entity) is out of scope.

### 2. Plugin contract

Plugins emit body content as plain markdown. They do NOT wrap their content in markers; they don't need to know the markers exist. The daemon places the markers when writing to vault.

This is intentional:

- Plugin authors don't have to learn the marker shape, the escape rules, or remember to wrap.
- Existing plugins (yaad-wikipedia, future) gain preservation behavior automatically when the daemon merge lands; no plugin-side change required.
- Marker shape is a daemon implementation detail and stays evolvable without breaking plugins.

The constraint that follows: plugins MUST NOT emit the literal substrings `<!-- yaad:plugin start -->` or `<!-- yaad:plugin end -->` inside their body content. The daemon enforces this at write time (see §4 below).

### 3. Daemon merge contract

The daemon owns the markers end-to-end: placing them on first write, detecting them on re-ingest, splicing the new plugin content between them.

**First write (no prior markers in vault):**

1. Plugin emits body content as plain markdown.
2. Daemon writes the entity file with the body wrapped: `<!-- yaad:plugin start -->\n<plugin-content>\n<!-- yaad:plugin end -->`.

**Re-ingest (prior markers in vault):**

1. **Reads** the existing `.md` body (everything below the closing `---` of frontmatter).
2. **Locates** the marker pair `<!-- yaad:plugin start -->` ... `<!-- yaad:plugin end -->`. Finds the FIRST occurrence of each (single-block contract).
3. **Splits** existing body into three regions:
 - `before`: everything from start-of-body up to (and excluding) the start marker.
 - `between`: the previously-managed plugin region (replaced on re-ingest); spans from the start marker through the end marker, inclusive of both markers.
 - `after`: everything immediately after the end marker through end-of-body, EXCLUSIVE of the end marker itself.
4. **Constructs** new body as: `before` + `<!-- yaad:plugin start -->\n` + plugin-emitted-content + `\n<!-- yaad:plugin end -->` + `after`.
5. **Writes** the merged body back.

`before` and `after` together carry every byte the operator added outside the marked region; `between` is fully replaced. The exclusive end-marker semantics in `after` ensures the marker isn't duplicated on re-ingest.

### 4. Edge cases

- **First-write with existing un-marked body** (operator hand-wrote content into a plugin-emitted entity's body before the plugin started emitting body content): existing body becomes `before` (preserved verbatim). Plugin region is added below it as a clean wrap. Convention: plugin region appears at the END of existing body, not the start, so the operator's hand-written content stays at the top.
- **Malformed markers** (start without end, or end without start): daemon logs WARN, treats body as un-marked, replaces wholesale. Should never happen since the daemon owns marker placement; if it does, indicates daemon bug or operator hand-edit corruption. Falling back to wholesale-replace is a defensible safe-default that prefers data-correctness over preservation in a known-broken state.
- **Multiple marker pairs** (daemon bug): daemon picks the first pair, treats anything after the first end-marker as `after` (preserved). Logs WARN. Same shape as malformed: shouldn't happen, fall back conservatively.
- **Marker inside operator-added section**: edge case. Operator who hand-types `<!-- yaad:plugin start -->` somewhere in `before` or `after` would confuse the parser on the next re-ingest. Documented as "don't do that" — the marker shape is reserved-syntax for the daemon-plugin contract.
- **Plugin emits the literal marker substring in its body**: the daemon scans the plugin-emitted content at write time for the literal substrings `<!-- yaad:plugin start -->` and `<!-- yaad:plugin end -->`. On detection: log WARN with the plugin name + entity id, and either (a) reject the write (preferred for v1, fail-fast surfaces the plugin bug) or (b) escape the marker substring (e.g., zero-width-space injection between angle brackets) before writing. The choice between (a) and (b) is open in v1; lean (a) — fail-fast tells the plugin author they have a bug rather than silently mangling content. Implementation PR can decide.
- **Marker inside code fence (in operator-added content)**: the daemon does NOT context-aware-parse markdown to skip code fences in `before` or `after`. The reserved-syntax rule applies regardless of context: an operator who types the marker substrings inside a code fence in their `## Notes` section will confuse the next re-ingest's parser. Same "don't do that" treatment.

### 5. Vault-readability

The HTML-comment shape is invisible in rendered Obsidian / GitHub / standard markdown views. Operators see:

```markdown
# Brass: Birmingham

![thumbnail](brass-birmingham-2018.thumb.jpg)

Brass: Birmingham is an economic strategy game...

## My Notes

I played this with Eli last weekend, the second round we both went heavy on cotton...
```

The markers are present in the file but the reader doesn't see them. Editing in Obsidian works naturally — the operator types under the description, not aware of the marker's position; on re-ingest, their `## My Notes` survives because it's outside the marker region.

## Alternatives considered

### Alternative A: per-plugin markers (`<!-- yaad:bgg start -->`)

**Rejected.** Locks the daemon into per-plugin parsing logic indefinitely. Each new plugin would either need its own daemon-side support or all plugins would have to converge on the generic marker anyway. Surfaced + countered in the dispatch thread; the generic marker is strictly more extensible.

### Alternative B: section-aware merge

**Rejected.** Daemon parses body into top-level `## <heading>` sections; replaces plugin's known-name sections; preserves operator-added sections. Section-name collisions (operator titles a section with the same name as the plugin's) become silent data loss. Markdown heading parsing is fuzzy (setext vs ATX, indented headings under lists). Marker-pair gives explicit, deterministic boundaries.

### Alternative C: accept clobber, push notes to `/v1/user-content`

**Rejected.** Operator surprise: hand-edits to a plugin-emitted entity's body get silently wiped on re-ingest. The user-content API is a separate-entities surface, not a notes-on-this-entity surface. Asking operators to file notes elsewhere is a UX failure for the natural "I want to add my own commentary alongside the plugin's data" use case.

### Alternative D: extend ADR-0012's user-content surface to entity bodies

**Rejected for v1.** Conceptually clean — treat the body as composed of "plugin section" + "user-content sections" via the same `/v1/user-content/sections/` API. But it requires plugins to emit their body content via a special field that bypasses the body-write path, plus an operator-facing API to layer hand-written sections, plus a merge step on read. Substantially more daemon work than marker-pair, and the user-content API is targeted at structured-section editing, not freeform append. Marker-pair is the simpler contract that solves this case; ADR-0012 stays as it is for the structured-section use cases it was designed for.

## Consequences

### Daemon work

- Implement marker placement on first write (wrap plugin-emitted body content in the marker pair when no markers exist in the prior file).
- Implement marker-pair detection + split + splice on re-ingest (in the body-write path of `buildVaultEntity` or a new pre-write merge step).
- Implement plugin-emitted-marker-substring detection at write time (per §4); fail-fast on detect for v1.
- Test matrix: first-write-empty / first-write-existing-unmarked-body / re-ingest-clean-markers / malformed start-only / malformed end-only / multiple pairs / marker inside operator-added section / plugin emits literal marker substring (rejected) / unicode in plugin region.

### Plugin SDK work

None required. Plugins emit body content as plain markdown via the existing FetchResult shape. The marker logic lives entirely on the daemon side.

(If a plugin author wants their own SDK helper to construct typed body content with title heading + image embed + description, that's plugin-internal convenience; not part of this ADR.)

### Plugin author surface

- **yaad-bgg a prior PR (the trigger)**: emits body content as plain markdown. No marker awareness needed. The daemon wraps on write. Becomes a small PR (just emit the title H1 + image embed + description prose).
- **yaad-wikipedia (existing plugin emitting body)**: gains preservation behavior automatically once the daemon merge lands. No plugin-side change required.

### Operator surface

- New mental model: "below the plugin's stuff, my edits survive." The marker shape is technical (HTML comments) but invisible in rendered views. Documentation in the entity-format guide should explain the shape so operators editing in raw-text mode know what they're seeing.

### Migration

yaad-index is pre-tagged-release. Existing entities can be purged and re-ingested without preservation concerns; migration is not a load-bearing constraint at this stage. Once the project tags v1, this section will need to be filled in with the duplicate-content + workaround details.

## Open questions

Punted to follow-ups, not blockers for v1:

1. **Multi-region** — a future plugin emitting two distinct managed regions (e.g., a "live data" block + a "static reference" block). Marker-pair-with-id (`<!-- yaad:plugin id=live start -->`) handles this; not needed v1.
2. **Multi-plugin same entity** — two plugins both emit body content for the same canonical entity. Punt; the canonical-vs-source-shape architecture (ADR-0008) doesn't allow two plugins to "own" the same entity today.
3. **Operator-side opt-out** — an operator who wants the marker region wholesale-replaced (no preservation, classic clobber). Not needed v1; if they want clobber they can delete + re-ingest.
