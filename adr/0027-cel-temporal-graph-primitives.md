# ADR-0027: CEL temporal + graph-walk primitives

**Status:** ACCEPTED
**Date:** 2026-05-23 (drafted); 2026-05-23 (accepted with Q1–Q7 resolutions below)

## Context

The workflow CEL surface today is narrow: `entity.*` + `edge.*` + `Bindings.*` for input, and `graph.get(id)` as the only graph-traversal primitive. ADR-0024 established this surface; ADR-0025 added day entities + canonical edge vocab but explicitly deferred the temporal and walking primitives needed to express daily-digest / deadline / ingested-on workflows.

The deferred items, called out in ADR-0025 § "Workflow integration" and PR-228's docs:

1. **No `today()` helper.** Workflows can't target "the current day" without operator-side date interpolation. The CEL evaluator has no concept of "now."
2. **No date arithmetic.** `day:X + 7` / `day:X - day:Y` are not expressible — needed for "N days from now," "approaching deadline," weekly digests.
3. **No in-edge / out-edge walker.** `graph.get(id)` returns the entity by id, but can't answer "what entities point at this day with edge type Y?" Daily-digest's core query shape is impossible.

PR-228's worked examples for daily-digest + ingested_on were rewritten to a curl/shell-side shape precisely because the CEL surface couldn't express them. This ADR closes that gap.

## Decision

Three categories of primitive land on the workflow CEL evaluator:

### 1. Date helpers — function form, canonical-ID return

```cel
today()        // → "day:2026-11-11"
yesterday()    // → "day:2026-11-10"
tomorrow()     // → "day:2026-11-12"
```

**Return shape:** canonical-ID string (`day:YYYY-MM-DD`) so the result composes directly with `graph.get(today())`, `add_canonical_edge: target.name: today()` (see "Action runner kind-prefix strip" below), action-template interpolation, and every other surface that takes an entity id. No translation layer between the temporal primitive and the storage convention.

**Action runner kind-prefix strip (required for the surface to compose).** The `add_canonical_edge` runner today computes `targetID = targetKind + ':' + slug.Slug(targetName)`. If `target.name` is passed `today()`'s canonical-ID return `day:2026-11-11`, `slug.Slug` mangles the colon and the result is `day:day-2026-11-11`. To preserve the "today() = id, use it anywhere" mental model, the action runner MUST strip a leading `<targetKind>:` prefix from `targetName` before slugifying:

```go
// vault_writers.go (around line 841)
prefix := targetKind + ":"
name := strings.TrimPrefix(targetName, prefix)
targetSlug := slug.Slug(name)
targetID := targetKind + ":" + targetSlug
```

Operators may write either form for `target.name`:
- `name: today()` → evaluates to `day:2026-11-11`, prefix stripped, slug = `2026-11-11`, id = `day:2026-11-11`. ✓
- `name: 'today()'.substring(4)` or bare-string literal `'2026-11-11'` → no prefix, slug = `2026-11-11`, id = `day:2026-11-11`. ✓
- `name: 'My Daily Note'` (operator-named day with custom slug) → no prefix, slug = `my-daily-note`, id = `day:my-daily-note`. ✓ (existing behavior preserved.)

This is the only action-runner change required by this ADR. The behavior is conservative — strip only when the prefix exactly matches the declared `target.kind`; any other text is left alone for slugification.

**Resolution:** the evaluator binds `clock.DayLocation()` (per ADR-0025's TZ chain — operator config → host `time.Local`) at engine construction. Engine construction happens at daemon start AND on operator-config reload — an operator who edits `timezone:` and reloads picks up the new zone on the next reload event, not only at restart. Tests stub the clock via the existing `internal/clock` test helpers.

**Evaluation cadence per fire.** `today()` / `yesterday()` / `tomorrow()` are evaluated **once per workflow fire**, cached in the fire's binding scope. Multiple `today()` callsites within one fire's actions all see the same value — important for the rare midnight-crossing case where a single fire might otherwise see day-N in one action and day-N+1 in the next. The cache is fire-scoped, not engine-scoped, so back-to-back fires across a midnight boundary correctly see different days.

### 2. Date arithmetic — function form

```cel
add_days(today(), 7)            // → "day:2026-11-18"
days_between(day_X, day_Y)      // → int (Y - X in days, signed)
```

**Function form, not operator overloading.** `today()` keeps composing as a regular string; `add_days` takes (string + int) → string; `days_between` takes (string, string) → int. No custom CEL type, no overloaded `+` / `-` / `<` semantics on day-tagged strings. The cel-go evaluator gets two new function definitions and zero new types.

`days_between(a, b)` is signed — positive when `b` is in the future relative to `a`, negative when in the past. This lets `days_between(today(), edge.to) > 0` read directly as "deadline is in the future."

Comparison via `days_between(a, b) > 0` is the substitute for an operator-form `a < b`. Verbosity is mild; implementation cost is roughly 1/5 of the custom-type alternative for ~95% of the ergonomic value.

### 3. Graph walking — overloaded function form, single-hop only

```cel
graph.in_edges(id)                          // → list<Edge>     — all edges pointing AT id
graph.in_edges(id, "due_on")                // → list<Edge>     — filtered by edge_type (SQL-side)
graph.out_edges(id)                         // → list<Edge>
graph.out_edges(id, "occurred_on")          // → list<Edge>

graph.in_neighbors(id)                      // → list<Entity>   — convenience: source entities
graph.in_neighbors(id, "due_on")            // → list<Entity>   — overloaded + filtered
graph.out_neighbors(id)                     // → list<Entity>
graph.out_neighbors(id, "is_about")         // → list<Entity>
```

**Overloaded form** — each function has two arities, no separate `_by_type` variant. When the `edge_type` arg is given, the daemon pushes the filter into the SQL query; without it, the full set is returned and CEL `.filter()` can still narrow further for ad-hoc shapes.

**Edge shape** (returned by `in_edges` / `out_edges`):

```
{ from: string, to: string, type: string, metadata: map<string, dyn> }
```

Mirrors the Go `store.Edge` struct's `Metadata map[string]any` field, lowercased per CEL convention. The `metadata` dict carries the same fields the ingest layer wrote. (The separate edge-bound trigger binding from `edge_created` workflows surfaces `{type, from, to, from_title, to_title, timestamp}` — that's the trigger-event payload, structurally distinct from the `Edge` value `in_edges` / `out_edges` return; they intentionally don't share a schema because the trigger binding has access to denormalized lookups the graph-walk results don't.)

**Neighbor convenience helpers** (`in_neighbors` / `out_neighbors`) return a list of Entity directly — single SQL JOIN at the daemon (edge query + entity fetch in one round trip), much faster than CEL-side `graph.in_edges(...).map(e, graph.get(e.from))` (one query + N entity fetches). Callers who need edge metadata (`data` dict, type, timestamps) still use the `_edges` form.

**Single-hop only.** No `graph.walk(id, depth=N)` primitive in this cut. None of the deferred workflow examples (daily-digest, ingested-on, deadline-approaching) need multi-hop, and multi-hop CEL queries risk pathological depth-N traversals. Multi-hop walking lives at the API endpoint `/v1/entities/{id}/context?depth=N` for ad-hoc operator / agent use; a future ADR can add a bounded CEL primitive if a real multi-hop workflow surfaces.

**List size cap + return-shape wrapping.** Each `in_edges` / `out_edges` / `in_neighbors` / `out_neighbors` call caps at a default of 1000 entries. A CEL `list<T>` can't carry sidecar flags, so the function returns a struct:

```
graph.in_edges(id, ...) → { items: list<Edge>, truncated: bool, total: int }
graph.in_neighbors(id, ...) → { items: list<Entity>, truncated: bool, total: int }
```

Callers iterate via `result.items` (the list) and check `result.truncated` / `result.total` for overflow handling. Trade-off accepted: every callsite types `.items` once for access; in exchange the truncation flag is checkable in CEL without a side-channel binding. The map / filter / join idioms still work — `result.items.map(...)` / `result.items.filter(...)`.

**Truncation handling is operator's responsibility.** On overflow, the engine does NOT auto-fail the action — the workflow continues with the first 1000 entries plus a `truncated: true` marker. Workflows that need exhaustive results MUST check the flag and paginate via the API (no CEL pagination primitive in this cut).

The cap is configurable via an operator-config knob (`workflow.graph_walk_cap` or similar). Default 1000 is sized for the day-anchor use case — operators can raise for dense-graph deployments.

### 4. Template interpolation — no loop construct

Action templates continue to do single-expression CEL substitution per ADR-0024 + `docs/workflows.md` (e.g. `content: '{{ entity.name }} ({{ entity.year }})'`). The new primitives surface naturally because they're CEL functions — `{{ today() }}` and `{{ graph.in_neighbors(today(), "due_on") }}` work in action-template `text` fields with zero template-engine changes.

**No `{{ for ... }}{{ end }}` construct.** List-shaped results render via CEL-native `.map().join()`:

```yaml
- task_append:
    section: today
    content: '{{ graph.in_neighbors(today(), "due_on").items.map(n, "- [[" + n.id + "]]").join("\n") }}'
```

Adding a template-loop construct opens a design space (nested loops, else branches, break/continue) that the daily-digest readability gain doesn't justify. Stay CEL-only.

**`.join()` receiver requires cel-go's `ext.Strings()` extension.** The string-extension library is not loaded by default in cel-go. Cut 3 (the graph-walking cut, where `.join()` first becomes useful) wires the evaluator with:

```go
env, err := cel.NewEnv(
    // ... existing options ...
    ext.Strings(),
)
```

Without `ext.Strings()`, `.join(separator)` returns a compile-time error. The other list methods we rely on (`.map`, `.filter`, `.exists`, `.all`) are CEL standard and don't need an extension.

## Worked examples (now runnable)

### Daily-digest workflow

```yaml
# vault/workflows/daily-digest.md
---
name: daily-digest
---

```yaml
trigger:
  type: manual
actions:
  - task_append:
      section: today
      content: '{{ graph.in_neighbors(today(), "due_on").items.map(n, "- [[" + n.id + "]] (due)").join("\n") }}{{ graph.in_neighbors(today(), "occurred_on").items.map(n, "\n- [[" + n.id + "]] (occurred)").join("") }}'
      if_already_present: replace
```
```

Triggered via `yaad-index workflow trigger daily-digest`. The workflow targets `today()` (in the daemon's TZ), walks each canonical edge type relevant to "today's anchors," concatenates lines into a task-section body.

### Ingested-on workflow

```yaml
# vault/workflows/ingested-on.md
---
name: ingested-on
---

```yaml
trigger:
  type: entity_created
actions:
  - add_canonical_edge:
      source: 'entity.id'
      edge_type: 'ingested_on'
      target:
        kind: 'day'
        name: 'today()'
```
```

Fires on every new entity. `add_canonical_edge`'s `target.name` field accepts the CEL expression `today()` which evaluates to `day:YYYY-MM-DD`; the action runner's kind-prefix strip (§1 above) removes the leading `day:` before slugifying so `target.id` resolves to the operator-expected `day:2026-11-11` shape. The runner also ensures the target day entity exists (per PR-226's ensure-target work in ADR-0025 cut 2).

### Approaching-deadline workflow

```yaml
# vault/workflows/approaching-deadline.md
---
name: approaching-deadline
---

```yaml
trigger:
  type: edge_created
  match:
    edge_type: due_on
actions:
  - task_append:
      section: deadlines
      content: '{{ edge.from }} due in {{ days_between(today(), edge.to) }} days'
```
```

Fires when any `due_on` edge is created. The action computes the relative day-count and appends to the operator's deadline-tracking task.

## Out of scope (initial)

- **Multi-hop CEL walks.** Future ADR if a real use case surfaces.
- **Operator-form arithmetic** (`day:X + 7` / `day:X < day:Y`). Function form ships; custom CEL type is deferred unless ergonomic friction proves material.
- **Recurrence semantics** ("every Tuesday," "monthly on the 15th"). Out of v1.x — separate ADR.
- **Time-of-day CEL** (`now()` returning hour/minute/second). Day-level only per ADR-0025's day-only scope.
- **Date locale** (week-numbering schemes, first-day-of-week, fiscal years). Out of v1.x.

## Implementation surface

This ADR ships in 4 cuts (mirroring ADR-0025's cadence):

1. **Cut 1** — date helpers (`today` / `yesterday` / `tomorrow`) + the CEL evaluator binding + the **action runner kind-prefix strip** (the one-line fix in `vault_writers.go` so `target.name: today()` resolves cleanly). Smallest surface in function count; lands the clock plumbing AND the runner contract that the rest of the ADR depends on.
2. **Cut 2** — date arithmetic (`add_days`, `days_between`). Builds on cut 1's binding.
3. **Cut 3** — graph-walk primitives (`graph.in_edges`, `graph.out_edges`, `graph.in_neighbors`, `graph.out_neighbors`) + the per-call cap + truncation-via-struct shape (`{items, truncated, total}`) + `ext.Strings()` evaluator wiring. Largest cut; SQL-side filter wiring + new evaluator types + JOIN-based neighbor variants.
4. **Cut 4** — docs walkthrough updates (extend `docs/workflows.md` § CEL environment with the new functions; extend `docs/date-entities.md` worked examples; ship the three concrete `vault/workflows/*.md` example files).

## Consequences

**Positive:**
- Daily-digest, ingested-on, deadline workflows become first-class workflow-shape (no host-cron / external-script crutches).
- ADR-0025's docs become fully runnable; the deferred worked examples ship as concrete `vault/workflows/` files in cut 4.
- Future temporal ADRs (week/month/year aggregation if those land, recurring schedules) build on this primitive layer instead of re-litigating the surface.

**Negative:**
- CEL evaluator gains new bindings (date helpers) and new function-overload definitions (graph walking). Evaluator-side test surface grows nontrivially.
- Per-call list cap is one more knob operators may need to tune for dense-graph deployments — surfaced via config + truncation flag, not silent.
- Graph-walk primitives need careful SQL-side filtering to avoid N+1 fetches on the neighbor variants. Implementation cost concentrated in cut 3.

## References

- ADR-0024 — Workflows and tasks (the CEL evaluator surface this ADR extends).
- ADR-0025 — Date entities (the `clock.DayLocation()` resolution chain; the deferred workflow examples this ADR unblocks; PR-228's docs that defer to this ADR).
