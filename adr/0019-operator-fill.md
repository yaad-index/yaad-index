# ADR-0019: Operator fills gaps the agent can't — extending gaps to dual-source

## Status

SUPERSEDED by [ADR-0029](./0029-unified-fill-surface.md) (2026-05-30).

The endpoint split (`/v1/fill` vs `/v1/operator-fill`) + caller-identity gate this ADR introduced collapse into a single `/v1/fill` endpoint per ADR-0029. The still-load-bearing model — `fill_strategy` on gaps, `gap_state` per-field state machine, the `defer` operation, auto-materialize, per-field-op shape — carries forward unchanged; the ADR-0029 framing is the canonical reference.

Proposed 2026-05-08.

## Context

Today's gap mechanism (per ADR-0008 / ADR-0013 / ADR-0016):

1. Plugin emits an entity with a list of `gaps` (fields the canonical
 shape declares missing).
2. Agent calls `POST /v1/entities/{id}/fill` with values derived from
 `clean_content` supplied by the daemon.
3. Vault + DB updated.
4. Repeat on next ingest if gaps reappear.

The mechanism assumes every gap is fillable from `clean_content`.
Some are not. The 2026-05-08 concerns meeting surfaced the missing
shape: **fields the operator must fill from their own knowledge**,
because no external source carries the truth.

Examples per the operator-database reframe:

- `rating` (1-10): operator's rating of this boardgame. Not on BGG,
 not on Wikipedia, not in any `clean_content`.
- `owned` (bool): whether the operator owns this. Same.
- `want` (bool): whether the operator wants it. Same.
- `played` (bool): whether the operator has played it. Simpler than
 count; not all players track plays and care about numbers.
- `knows_how_to_play` (bool): whether the operator knows the rules.
 Distinct from `played` (you can know-how-to-play without having
 played in 10 years; you can have played without knowing rules
 cleanly if someone else taught the round).

The naive approach — introduce a separate `user_fill_fields`
parallel to gaps (initial draft of this ADR, closed a prior PR) — was
rejected. Reason: it doubles the schema surface for what is
essentially the same shape (a missing field that needs to be
filled), and forces operators to learn two parallel-but-distinct
vocabularies. Cleaner: **gaps are gaps**; the agent fills what it
can from `clean_content`, and the rest surfaces to the operator.

## Decision

Extend the existing gap mechanism to be **dual-source**:

1. **Agent attempts first.** On `POST /v1/entities/{id}/fill`, the
 agent fills whatever fields it can derive from
 `clean_content`. Existing flow unchanged.
2. **Operator fills the rest.** Fields the agent leaves unfilled
 surface via the existing `/v1/needs-fill` endpoint, which now
 serves both audiences (agent picks up gaps it might be able to
 fill, operator picks up gaps the agent left behind).
3. **Operator-fill endpoint.** A parallel
 `POST /v1/entities/{id}/operator-fill` takes operator-supplied
 field values. Same per-field validation as the agent path; same
 vault-then-DB ordering.
4. **Defer state.** Operator can mark a gap as "ignore for now" via
 the same operator-fill endpoint. Deferred gaps stop surfacing to
 the operator (they don't surface to the agent either) until the
 operator un-defers. Re-ingest does NOT auto-clear the defer
 state — it's an explicit operator decision.

### Gap state machine

A gap on an entity is in one of four states:

- **unfilled** — no value, no defer. Surfaces to both agent and
 operator. (Default state on entity creation.)
- **agent-filled** — value present, written by the agent. Same
 shape as today.
- **operator-filled** — value present, written by the operator.
 New state.
- **deferred** — explicitly ignored-for-now. Surfaces to neither.

Transitions:

- unfilled → agent-filled: `POST /v1/entities/{id}/fill` (existing).
- unfilled → operator-filled: `POST /v1/entities/{id}/operator-fill`.
- unfilled → deferred: `POST /v1/entities/{id}/operator-fill` with
 `defer: true` for the field.
- agent-filled → operator-filled: operator overrides via
 operator-fill (agent value replaced).
- operator-filled → unfilled: operator clears via operator-fill
 with explicit `null`.
- deferred → unfilled: operator un-defers via operator-fill with
 `defer: false`.

**Filled → deferred is NOT a direct transition.** Defer is a state
for unfilled gaps the operator wants to stop seeing; a filled gap
isn't being prompted-for already, so deferring it is meaningless.
To "defer a filled gap," the operator must explicitly clear the
value first (operator-fill with `null`) and then defer. Two-step.
The state machine rejects single-step transitions from agent-filled
or operator-filled directly to deferred — the operator-fill
endpoint returns a `400 deferred_requires_unfilled` error if both
a value and `defer: true` are sent for the same field.

The store tracks the source of each filled value (agent or
operator) so the override semantics are explicit and auditable.

### Per-gap fill-strategy hint (optional)

Each gap declaration may carry a `fill_strategy`:

- `agent` — agent-only. Operator-fill on this field is rejected
 (rare; mostly for fields that are by-definition external like
 `wikipedia_summary`).
- `operator` — operator-only. Agent skips this field even if it
 could derive a value. Useful for opinion-shaped fields like
 `rating` where any agent attempt is hallucination.
- `both` (default) — agent attempts first, operator fills the rest.

Strategy lives in the canonical-kind config alongside the gap's
prompt and instruction. Plugin Capabilities can declare a strategy
hint per gap; operator config can override per ADR-0016's 4-layer.

```yaml
canonical_kinds:
 boardgame:
 gaps:
 summary:
 prompt: "Short summary of gameplay."
 fill_strategy: agent
 rating:
 prompt: "How do you rate this on a 1-10 scale?"
 type: int
 range: [1, 10]
 fill_strategy: operator
 owned:
 prompt: "Do you own this?"
 type: bool
 fill_strategy: operator
 want:
 prompt: "Do you want this?"
 type: bool
 fill_strategy: operator
 played:
 prompt: "Have you played this?"
 type: bool
 fill_strategy: operator
 knows_how_to_play:
 prompt: "Do you know how to play this?"
 type: bool
 fill_strategy: operator
```

Today's gaps without explicit `fill_strategy` default to `both`,
preserving current behavior.

### Field types on gaps

Gaps today are free-form (the agent fills with whatever shape
makes sense). To support operator-fill cleanly, gaps gain optional
typing:

- `int` — optional `range: [min, max]`.
- `bool` — true / false.
- `string` — single-line text. Optional `max_length`.
- `text` — multi-line free text. Default for today's prose-shape
 gaps (summary, description).
- `enum` — `values: [...]` list.

Untyped gaps default to `text` (matches current prose behavior).
Operator-fill validates against the type when present; agent-fill
similarly validated when the agent supplies a typed value.

### Storage

Gaps with values continue to live under entity `data` as today.
The new state metadata (deferred flag, write-source) lives in a
parallel `gap_state:` block on the entity frontmatter:

```yaml
---
id: boardgame:brass-birmingham
kind: boardgame
data:
 title: "Brass: Birmingham"
 summary: "..." # filled by agent, written under data
 rating: 9 # filled by operator, also written under data
 owned: true # filled by operator
gap_state:
 rating: { source: operator, filled_at: "2026-05-08T16:30:00Z" }
 owned: { source: operator, filled_at: "2026-05-08T16:30:00Z" }
 played: { deferred: true, deferred_at: "2026-05-08T16:30:00Z" }
 summary: { source: agent, filled_at: "2026-05-07T12:00:00Z" }
---
```

`gap_state` is omitted for fields with no metadata (untouched
defaults). The entity stays human-readable; the state metadata is
small.

DB mirror: parallel `gap_state` JSON column on `entities` (or a
dedicated `gap_state` table — implementation choice deferred to
companion design).

### Plugin Capabilities extension

Plugin Capabilities `gaps` declaration today is a string-keyed
prompt map. Extend to support optional metadata:

```json
{
 "gaps": {
 "summary": {
 "prompt": "Short summary of gameplay.",
 "fill_strategy": "agent"
 },
 "rating": {
 "prompt": "How do you rate this on a 1-10 scale?",
 "type": "int",
 "range": [1, 10],
 "fill_strategy": "operator"
 }
 }
}
```

The legacy string-shape (`{"summary": "Short summary..."}`)
remains supported with `type: text` and `fill_strategy: both`
defaults — pre-v1 BC isn't a goal, but the simpler shape stays
usable for plugins that don't need typed gaps.

The bgg plugin gains five operator-strategy gaps for boardgame:
rating, owned, want, played, knows_how_to_play.

### Endpoint surface

- `POST /v1/entities/{id}/fill` — agent path, unchanged.
- `POST /v1/entities/{id}/operator-fill` — new. Body shape:
 ```json
 {
 "rating": 9,
 "owned": true,
 "played": { "defer": true }
 }
 ```
 Per-field: scalar value = set + write-source `operator`; object
 with `defer: true` = mark deferred; explicit `null` = clear.
 Auth: operator-only.

- `GET /v1/needs-fill` — existing, behavior amended:
 - Returns gaps in `unfilled` state (current).
 - Each gap entry now includes `fill_strategy` + `type`.
 - Excludes deferred gaps (operator decided "not now").
 - Excludes `agent`-only gaps from operator-side queries (when
 auth is operator) and `operator`-only gaps from agent-side
 queries (when auth is agent). The agent shouldn't waste
 cycles attempting `rating`; the operator shouldn't be
 prompted for `wikipedia_summary`.

### Read path

Filled gaps are part of `data` and surface as today. The new
metadata (`gap_state`) is exposed:

- On `GET /v1/entities/{id}` — `gap_state` subobject (omitempty
 when no fields have metadata).
- Same on edge expansion / list_entities.

## Consequences

### Positive

- **Single mental model.** Gaps are gaps. Agent fills what it
 can; operator fills the rest. No parallel `user_fill` schema.
- **Predicate-searchable for free.** Filled gaps live in `data`
 alongside agent-filled fields; the future search-with-predicate
 layer treats them uniformly. `kind=boardgame AND data.rating
 > 5` works without special-casing.
- **Defer state.** Operators don't get prompted for the same
 unfillable field every reload; the explicit "ignore for now"
 signal is captured.
- **Per-gap strategy.** Operator-only fields (rating) skip the
 agent attempt entirely — no hallucination risk. Agent-only
 fields (wikipedia_summary) skip the operator surface — no
 noise.
- **Closes the cold-reviewer's ("would you miss it = no").** Operator-fill
 on rating/owned/want/played/knows_how_to_play is the missing layer that
 makes yaad-index a personal database.

### Negative

- **Existing endpoint amended.** `/v1/needs-fill` now serves two
 audiences with auth-aware filtering. Behavior change is small
 but real; agent callers that were getting back operator-only
 gaps (today, none — operator-only didn't exist) won't notice;
 new operator callers see the new shape.
- **Gap declaration grows.** Plugin Capabilities + canonical-kind
 config gain typing + strategy fields. Documentation tax.
- **Two write paths.** Agent and operator fill via separate
 endpoints. The split keeps auth + audit clean but is more
 surface than a single merged endpoint.

### Neutral

- **Pre-v1, no migration.** Existing entities have no
 `gap_state` block; behavior identical to today (agent-only,
 no defer). Operator opts in by configuring strategy + filling.
- **`gap_state` is overhead but small.** Per-field metadata only
 for fields the operator has touched (filled or deferred);
 untouched defaults stay silent.

## Implementation

Filed as small reviewable PRs (per `feedback_dispatch_small_steps`):

1. **Schema migration** — `gap_state` JSON column or table.
2. **Config schema** — typed gaps + `fill_strategy` per gap in
 canonical-kind config; parse-time validation.
3. **Plugin Capabilities extension** — typed-gap shape; legacy
 string-shape stays supported as default.
4. **Built-in defaults** — boardgame gains five operator-strategy
 gaps (rating, owned, want, played, knows_how_to_play). Other
 kinds unchanged in this step.
5. **Operator-fill endpoint** — `POST /v1/entities/{id}/operator-fill`.
6. **Defer state on operator-fill + needs-fill filtering.**
7. **MCP tools** — `set_operator_fill(entity_id, fields)` and
 `defer_gap(entity_id, field)`.
8. **BGG plugin update** — declare `fill_strategy: operator` on
 the five boardgame fields.

Search-with-predicate-on-gaps and search-UX are separate tracks
per the concerns-meeting build sequence; they land after this ADR
is approved and the operator-fill loop is live.

## Out of scope

- Per-field permissions beyond agent/operator (e.g., role-scoped
 gaps for multi-operator deployments).
- Bulk operator-fill (`POST /v1/operator-fill/bulk`). Lands when
 batch import is wanted.
- Auto-undefer on re-ingest. Today: defer is sticky until
 operator un-defers explicitly.
- Cross-entity validation (e.g., `played + knows_how_to_play >= count(plays
 table where game = id)`). User-fill values are operator's
 truth; cross-table consistency is a future feature.
- Search predicates on gap state (`deferred = true`, `source =
 operator`, etc.). Lands with the search work.
