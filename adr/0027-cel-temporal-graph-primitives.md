# ADR-0027: CEL temporal + graph-walk primitives

**Status:** DRAFT
**Date:** 2026-05-23

## Context

The workflow CEL surface today is narrow: `entity.*` + `edge.*` + `Bindings.*` for input, and `graph.get(id)` as the only graph-traversal primitive. ADR-0024 established this surface; ADR-0025 added day entities + canonical edge vocab but explicitly deferred the temporal and walking primitives needed to express daily-digest / deadline / ingested-on workflows.

The deferred items, called out in ADR-0025 § "Workflow integration" and PR-228's docs:

1. **No `today()` helper.** Workflows can't target "the current day" without operator-side date interpolation. The CEL evaluator has no concept of "now."
2. **No date arithmetic.** `day:X + 7` / `day:X - day:Y` are not expressible — needed for "N days from now," "approaching deadline," weekly digests.
3. **No in-edge / out-edge walker.** `graph.get(id)` returns the entity by id, but can't answer "what entities point at this day with edge type Y?" Daily-digest's core query shape is impossible.

PR-228's worked examples for daily-digest + ingested_on were rewritten to a curl/shell-side shape precisely because the CEL surface couldn't express them. This ADR closes that gap.

## Proposed primitives

### 1. Date helpers

```cel
today()        // → day:2026-11-11    (canonical-ID string for "today" in the daemon's configured TZ)
yesterday()    // → day:2026-11-10
tomorrow()     // → day:2026-11-12
```

Return shape: canonical-ID string (`day:YYYY-MM-DD`) so the result composes directly with `graph.get(today())`, `add_canonical_edge: target: today()`, etc. without operator-side prefixing.

Resolution: `clock.DayLocation()` per ADR-0025's TZ chain (operator config → host `time.Local`). The evaluator binds the clock at engine construction; tests stub it.

### 2. Date arithmetic

Two options for the surface — picked one-at-a-time during the design pass.

**Option A — function form:**
```cel
add_days(today(), 7)            // → day:2026-11-18
days_between(day:X, day:Y)      // → int (Y - X in days)
```

**Option B — operator form:**
```cel
today() + 7                     // → day:2026-11-18  (needs custom CEL type)
day:X - day:Y                   // → int
day:X < day:Y                   // → bool
```

Option A is friendlier to CEL's evaluator (existing function-call machinery, no custom type). Option B is more ergonomic for inline use.

### 3. Graph walking

```cel
graph.in_edges(id)                          // → list<Edge>     — all edges pointing AT id
graph.in_edges(id, "due_on")                // → list<Edge>     — filtered by edge_type
graph.out_edges(id)                         // → list<Edge>     — edges FROM id
graph.out_edges(id, "occurred_on")          // → list<Edge>     — filtered

graph.in_neighbors(id)                      // → list<Entity>   — convenience: just the source entities
graph.out_neighbors(id, "is_about")         // → list<Entity>
```

`Edge` shape: `{from: string, to: string, type: string, data: map<string, dyn>}` — same shape the API surfaces.

Single-hop only at the CEL layer. Multi-hop walks remain at `/v1/entities/{id}/context?depth=N`. Rationale: CEL evaluation must be bounded and predictable; arbitrary-depth graph walks at evaluation time risk pathological queries.

## Worked examples once shipped

### Daily-digest workflow

```yaml
name: daily-digest
trigger:
  type: manual
actions:
  - task_append:
      section: today
      content: '{{ for n in graph.in_neighbors(today()) }}- [[{{ n.id }}]]{{ end }}'
      if_already_present: replace
```

### Ingested-on workflow

```yaml
name: ingested-on
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

### Approaching-deadline workflow

```yaml
name: approaching-deadline
trigger:
  type: edge_created
  match:
    edge_type: due_on
actions:
  - task_append:
      section: deadlines
      content: '{{ entity.id }} due in {{ days_between(today(), edge.to) }} days'
```

## Open questions (for design pass)

**Q1 — date arithmetic surface.** Option A (function form, `add_days` / `days_between`) or Option B (operator form, `+` / `-` / `<` on day types)?

**Q2 — `today()` return shape.** Canonical-ID string (`day:2026-11-11`) or naked date string (`2026-11-11`)? String composes with `graph.get(today())` and `target: today()`; naked date composes with anything taking a `YYYY-MM-DD` value.

**Q3 — graph-walk filter argument.** Required `edge_type` (separate `_filtered` variant) or optional positional arg as drafted (`graph.in_edges(id)` vs `graph.in_edges(id, "type")`)?

**Q4 — neighbor convenience helpers.** Include `in_neighbors` / `out_neighbors` (returns entities, skips edge metadata) or force callers to walk `graph.in_edges(...).map(e, graph.get(e.from))`?

**Q5 — bounded list size.** Should `graph.in_edges(id)` cap at some N (e.g. 1000) and surface a `truncated: true` flag, error out, or return everything? Day entities with N years of inbound edges could have unbounded results.

**Q6 — multi-hop at CEL.** Keep single-hop only (force /v1/entities/{id}/context for depth>1) or add a bounded `graph.walk(id, depth=2)` helper? Multi-hop in CEL means more powerful queries but also more pathological-query risk.

**Q7 — workflow template interpolation.** Action templates use `{{ entity.X }}` substitution per ADR-0024 § "Workflow." Does `{{ today() }}` and `{{ for n in graph.in_neighbors(today()) }}` work in the action-template surface, or only in CEL-evaluated decision/target fields? (The looping `{{ for }}` construct in particular has no prior precedent in the template engine — may need its own design.)

## Consequences

**Positive:**
- Daily-digest, ingested-on, deadline workflows become first-class workflow-shape (no host-cron / external-script crutches).
- ADR-0025's docs become fully runnable; the deferred worked examples ship as concrete vault/workflows files.
- Future temporal ADRs (week/month/year aggregation, recurring schedules) build on this primitive layer.

**Negative:**
- CEL evaluator gains new bindings + custom types (especially if Q1 Option B). Evaluator-side complexity nontrivial.
- Action template engine may grow loop construct (Q7) — first time templates have control flow.
- Graph-walk primitives risk pathological queries on dense edge sets (mitigated by Q5).

## References

- ADR-0024 — Workflows and tasks (CEL evaluator surface).
- ADR-0025 — Date entities (the `clock.DayLocation()` resolution chain; the deferred workflow examples this ADR unblocks).
