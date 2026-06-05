# ADR-0030: Workflow `archive_when` — post-run predicate for source-entity auto-archive

## Status

Accepted 2026-06-05 (proposed 2026-05-31).

## Depends on

- [ADR-0008](./0008-vault-as-source-of-truth.md) — vault-as-source-of-truth. The archive move routes through the vault writer; DB toggle follows. Unchanged.
- [ADR-0018](./0018-archive-replaces-delete.md) — archive replaces delete. The two-step state machine (archive then delete) and the `_archive/<kind>/<slug>.md` placement convention already govern the on-disk + DB shape of archived entities. This ADR adds a new automated trigger that lands in the same archived state; no new archive primitive is introduced.
- [ADR-0024](./0024-workflows-and-tasks.md) — workflows-and-tasks. The post-run hook this ADR adds fires after the workflow's action set completes, alongside the existing task-spawn machinery.

## Context

Source-shape entities accumulate in the graph after the workflows that consumed them finish. A typical example: every fetched gmail becomes a `gmail:` source entity. The `gmail-catch-all` / `linkedin-digest-claim` / `gmail-github-mentions` workflows fill the gaps on it and write canonical edges (`is_about` → `person:`, `is_about` → `topic:`, …). After that, the source row has no further function — its job was to feed the workflows that derived the canonical state. But it stays permanently in `vault/gmail/` and crowds out the canonical entities the operator actually cares about (`list_entities`, search results, graph views).

The same shape repeats for `github:` events, `bgg:` source rows, and any future fetched-and-processed entity kind. Manual cleanup is impractical and contradicts ADR-0018's "archive replaces delete" intent — operators shouldn't have to track which source rows have outlived their usefulness.

The existing archive surface (`POST /v1/entities/{id}/archive`) is invokable per-entity but requires the caller to *know* when a source row is done. The workflow that produced the canonical state is the right authority for that decision — it knows when its predicate (gaps resolved? canonical edges written? actionability decided?) flips from "needs processing" to "done".

The cleanest place to express that is on the workflow itself: declare a predicate that runs *after* the workflow's action set completes, and if it's true, archive the source row that the workflow ran against.

## Decision

### 1. New workflow-config field: `archive_when`

Workflows opt into post-run archive by declaring an `archive_when` predicate at workflow-config level:

```yaml
name: gmail-catch-all
trigger:
  type: entity_updated
  match: kind == "gmail"
actions:
  - …
archive_when:
  all_gaps_resolved: true
```

A workflow without `archive_when` behaves unchanged — no archive evaluation runs, no archive move attempts. The default is opt-out; existing workflows are untouched.

### 2. Predicate vocabulary

`archive_when` accepts a small composable vocabulary:

```yaml
archive_when:
  all_gaps_resolved: true             # entity has no remaining unfilled gaps
  has_edges: [is_about, is_actionable_for]  # ALL listed outgoing edge types must exist
  field_equals:                       # specific data fields match the given values
    is_actionable: no
    state: closed
  any_of: [<predicate>, <predicate>]  # composability — true if any branch is true
  all_of: [<predicate>, <predicate>]  # composability — true only if all branches are true
```

Primitives:

- **`all_gaps_resolved: true`** — entity has no remaining unfilled gaps (`gap_state` shows every registered gap either filled or deferred). The most common predicate; covers the gmail-catch-all shape.
- **`has_edges: [<edge_type>, …]`** — true iff the entity has at least one outgoing edge of EACH listed type. Use when the workflow's canonical-edge writes are the authoritative "done" signal.
- **`field_equals: {<field>: <value>, …}`** — true iff each listed `data.<field>` equals the given value. Use for terminal-state markers (`state: closed`, `is_actionable: no`).
- **`any_of: [<predicate>, …]`** — composability OR; true if any nested predicate is true.
- **`all_of: [<predicate>, …]`** — composability AND; true only if all nested predicates are true. Equivalent to declaring multiple sibling primitives on the same map (which already AND together); `all_of` is the explicit form for nested compositions.

Multiple sibling primitives at the top level AND together implicitly. `all_gaps_resolved: true` + `field_equals: { is_actionable: no }` on the same map means both must hold.

### 3. Evaluation point: after the workflow's action set runs successfully

The predicate evaluates **once per workflow run, after the action set completes successfully**. Failed action sets do not trigger archive evaluation — a workflow that errored mid-action chain should leave the source row in place for the operator to inspect.

The evaluator reads the entity's state *as of the post-action commit*. Predicates that depend on edges written by the workflow's own actions (e.g. `has_edges: [is_about]` matching an edge the workflow just wrote) read the post-write state.

### 4. Archive flow: reuse the existing `/v1/entities/{id}/archive` path

When `archive_when` evaluates true, the workflow engine invokes the same code path the existing archive surface uses: `store.ArchiveEntity` + `vault.ArchiveWithCommit`. This means:

- Same atomic vault move (`vault/<kind>/<slug>.md` → `vault/_archive/<kind>/<slug>.md`) with auto-commit producing the git audit trail.
- Same DB toggle (`archived_at = NOW()`).
- Same query-side semantics (archived rows omit-by-default from `list_entities` + `search_local`; lookup-by-id still returns them).
- Same restore path (`POST /v1/entities/{id}/restore` works on workflow-archived entities just as on operator-archived ones).

No new lower-level archive primitive is introduced. ADR-0018's two-step state machine is preserved end-to-end — `archive_when` is one of three triggers (operator UI, agent /archive call, workflow predicate) that all land in the same archived state.

### 5. Failure mode: log-and-continue

If the archive move fails (vault write error, DB toggle race, etc.), the workflow's overall run status is **not** affected. The archive failure is logged at WARN; the workflow's actual outputs (gaps filled, edges written, tasks spawned) are the operator-facing contract and must not be invalidated by a vault-side housekeeping failure. Vault-DB drift introduced by a half-applied archive is recoverable via reindex.

Rationale: the `archive_when` predicate is advisory housekeeping — its failure to land must not invalidate the workflow's actual side-effects, since the operator-facing contract is the workflow's outputs (gaps filled, edges written, tasks spawned), not the archive itself.

### 6. What stays unchanged

- Workflows without `archive_when` behave exactly as today (no archive evaluation, no archive move).
- The existing `/v1/entities/{id}/archive` + `/restore` surfaces are unchanged.
- `_archive/<kind>/<slug>.md` placement convention from ADR-0018.
- `archived_at` semantics in the store (omit-by-default on list/search; visible on lookup-by-id).
- ADR-0024's workflow-engine task-spawn flow runs independently — `archive_when` evaluation happens after the action set, alongside task spawning, not gating it.

## Consequences

### Positive

- Workflows that own the "this source row is done" predicate can declare it once at config time instead of operators having to know-and-call.
- Common case (`all_gaps_resolved: true` on source-shape kinds) is a one-line addition.
- Reusing the existing archive flow means the archive state, DB visibility, audit log, and restore path are all automatic — no parallel machinery.
- ADR-0018's archive-replaces-delete intent extends naturally to workflow-driven cleanup.

### Negative

- Workflows that opt into `archive_when` couple their definition to the source-kind shape; a renamed `data.is_actionable` field requires a workflow update. Trade-off accepted — predicate vocabulary is small and explicit.
- Predicate evaluation cost runs on every workflow run; for workflows that don't opt in, the cost is the absence-check only. For opted-in workflows, the predicate read happens on the post-action entity state which is already in memory.
- `archive_when` adds a third archive-trigger source alongside operator UI + agent /archive. The audit log must distinguish them — covered by the existing per-call author field on the archive commit (workflow runs land with the workflow's name as the author).

### Migration

- One-time sweep for source-shape entities that already exist past their useful life is **out of scope** for this ADR — operators can run the workflows with `archive_when` newly added against the existing rows to catch up, but a dedicated "sweep everything older than X" surface is a separate issue.
- Existing workflow YAMLs without `archive_when` keep working.

## Out of scope

- Retroactive bulk-archive of entities that already exist past their useful life (one-time sweep / re-run workflows with the new `archive_when` to catch up). Separate issue.
- Graph-view filter UI for showing/hiding archived entities on demand. Separate issue.
- Per-kind-config visibility default (e.g. "kind X archived rows hidden in graph-view by default"). Separate issue.
- Per-section author-locking inside UGC (different scope entirely).

## Implementation cuts

Tracking #376. Implementation lands in three PRs:

1. **Cut 1 (this ADR)** — contract.
2. **Cut 2 — parser + predicate evaluator** — `internal/workflow/parser` schema struct for `ArchiveWhen` + new evaluator under `internal/workflow/decision` with testify table tests per primitive + `any_of` / `all_of` composition.
3. **Cut 3 — engine hook + end-to-end test (`closes #376`)** — post-action evaluation hook in `internal/workflow/engine`; on true, invokes the same code path as the existing `/v1/entities/{id}/archive` handler; end-to-end test where a workflow with `archive_when: all_gaps_resolved` archives its source row after the last gap fills.
