# ADR-0024: Workflows and Tasks

**Status:** PROPOSED
**Date:** 2026-05-12

## Context

yaad-index is currently an entity-and-edge store. Ingest, fill-gap, search, and edges are first-class; everything operators DO with that data — "this PR is waiting on me," "this newsletter is for a boardgame I might want," "this Amazon receipt should be filed under tax" — happens outside the daemon, in ad-hoc agent prompts or wrapper scripts.

Operators have informal flows running through agents:
- Classify a newsletter, decide if it's actionable, log it.
- Watch for PR review requests, surface them as work-to-do.
- Triage incoming receipts into category buckets.

These flows share a shape: **a trigger condition, a context-load step, an optional decision, an output**. Today the shape is implicit and re-invented every time an operator describes it to an agent. The cost is real: every flow is hand-assembled context. The pain isn't fetching individual entities — it's assembling the bundle that makes the decision possible.

This ADR proposes a first-class concept in yaad-index for declaring these flows once and letting the daemon run them.

## Decision

Introduce two new concepts:

### Workflow

A **workflow** is a declared pattern with:

- A **trigger condition** — what fires the workflow.
- An optional **gap-injection list** — gaps the workflow adds to the triggering entity at fire time (these flow through the existing fill-gap pipeline per ADR-0019).
- A **decision** — deterministic evaluation against entity data (filled-or-otherwise), agent-free by default.
- An **output** — what the workflow produces if the decision evaluates true.

Workflows are markdown files at `workflow/<name>.md` (vault-side, daemon-managed). Frontmatter holds the rules; body is operator-readable documentation. **Workflow definitions are operator-authored, not yaad-index-shipped.** yaad-index ships the engine — parser, trigger detector, fill-gap integration, output dispatch. Operators (with agent help) write the actual workflow files.

### Task

A **task** is a workflow's instance — a first-class entity created when a workflow's output produces one.

- Each task is its own entity with its own ID, frontmatter, and edges.
- Edges to: the spawning workflow, the source entities the workflow loaded (PR, jira, project, email, etc.).
- Properties: priority (operator-modifiable), snooze (see below), comments (operator notes, agent-readable on load).
- Close: either operator-marks-done (default) or condition-based per workflow pattern (e.g., PR-merged → auto-close).
- Auto-archive on done (per ADR-0018 archive lifecycle). Workflow can opt out per-pattern via `auto_archive_on_done: false` for cases where the operator wants the audit trail of completed tasks to stick around.

#### Snooze semantics

Operator can snooze a task with an until-time (snooze until tomorrow, until next Monday, until a specific date, etc.). Snoozed tasks are **hidden from normal `task.list`** until the snooze expires. An operator who wants to see them explicitly fetches with an include-snoozed flag.

A snoozed task is **still live** — it continues to accept updates. If new material surfaces while the task is snoozed (a re-trigger that the workflow's de-dup rule routes to the existing task), the task is updated with the new state, but the task stays hidden until the snooze expires. The operator chose to defer the surface, not to disconnect the task from incoming data.

Snooze is operator-controlled, not workflow-controlled — the workflow doesn't get to auto-snooze its own tasks. (A future operator-config knob could permit per-workflow default-snooze for patterns where deferred-by-default makes sense; out of v1.)

Tasks outlive the workflow that spawned them — if a workflow pattern is deleted, its tasks orphan (listable + closable, no re-trigger). This is intentional: the operator's commitment to a task ("I'll review this PR") is independent of the workflow rule that surfaced it.

### Trigger types (v1)

1. **Event-driven** — workflow subscribes to internal events emitted by the daemon's event bus.
2. **Manual** — agent invokes `workflow.trigger(name, input)`. Input required (e.g., a URL or entity ID).
3. **Time-based** — workflow declares a schedule (cron-style or named cadence like "daily-morning"). Folded into v1 because existing operator-side patterns (morning brief, weekly summary) are already workflows in disguise; formalizing now costs less than retrofitting later.

### Internal event bus (v1 core)

The daemon emits internal events that workflows can subscribe to:

- `entity.created` — new entity added by any plugin (fresh-ingest only; not re-fetch of an already-known entity).
- `entity.edge_added` — new edge attached to an entity. Fires on every ingest that produces a new connection — including cache-hit re-fetches of a known entity that surface new edges, and operator-side manual edge adds.
- `fill.completed` — a gap-fill landed on an entity. Fires on every fill, including workflow-injected gap-fills evaluating during re-fetch.

This is the load-bearing piece. Without an internal event bus, workflows can only react to "new external thing came in via ingest" and the fill-gap integration described below collapses. With it, workflows become reactive to the index itself, not just external input — which is what differentiates a workflow from a glorified gmail rule.

**Cache-hit re-fetch semantics.** Re-fetch of a known entity does NOT re-fire `entity.created` (the entity already exists). Workflows that need to react to changes from re-fetch subscribe to `entity.edge_added` instead — a re-fetch surfacing a new connection (a news article gaining a topic-link, a PR gaining a new reviewer) fires `entity.edge_added` and the workflow re-evaluates from there. A workflow watching a topic entity for incoming `is_about` edges sees every new article tagged to that topic without needing a sibling "fetched" event.

### Fill-gap injection (third filler-source)

ADR-0019 defined two fill-gap sources: agent-strategy (LLM fills from clean_content) and operator-strategy (operator fills from their own knowledge). Workflows become a **third source**: a workflow can ADD gaps to an entity at trigger time, and those gaps flow through the existing fill-gap pipeline.

Two worked-example shapes:

- **Newsletter-shape workflow** triggers on email-ingest, injects gaps like `is_newsletter?` (bool) and `subjects?` (canonical-typed list). Fill-gap pipeline answers them. Workflow's decision evaluates the filled data. Task created or not.
- **Deterministic-classifier-shape workflow** (e.g. Amazon receipt) triggers, knows what it is by sender domain, injects no gaps, extracts data inline, creates a task (or silent-logs) directly.

Workflow-injected gap fills are **permanent** on the entity per ADR-0008 (vault-as-truth). Future re-fetches reuse the stored fill rather than re-classifying. Side benefit: gradual personal-database enrichment as workflows fire over time.

### Output surface

A workflow's output is one of:

- **Create task entity** — the default; spawns a `tasks/<name>-<task-id>.md`.
- **Mutate existing entity** — e.g., update priority, add a tag, set a property.
- **Add edge** — attach a new edge between existing entities.
- **Silent log** — process inline, no task created (Amazon-receipt-shape: the workflow knows what it is, files it, done).
- **Emit notification** — out-of-v1 unless concrete need surfaces.

**Concurrent writes.** Two workflows — or any two writers (workflow output, UGC mutation, comment addition, edge addition, plugin emit, operator manual write) — may touch the same on-disk artifact at the same time. v1 protects via a daemon-internal **per-artifact write-lock manager** (`internal/writelocks` per yaad-index #23) with a **block-on-conflict** policy: an Acquire on an artifact already held by another writer returns a typed conflict error immediately, surfacing as a 409 envelope naming the active holder. No queuing, no merging, no last-writer-wins; the rejected caller retries.

Two write classes deliberately skip the lock as additive-append shapes that don't conflict at the storage layer:

- **Comments** (`POST /v1/entities/{id}/comments`) — append-only entries in vault frontmatter's comments table.
- **Edges** (`POST /v1/edges`) — append-only rows in the store + frontmatter.

Every other mutation surface (ingest, fill, operator-fill, archive/restore, delete, UGC section / frontmatter / create / delete) acquires the per-entity lock — section-scoped where applicable (UGC section writers key on `<id>#<idx>` so different sections of the same UGC file proceed concurrently). Cross-host distributed locking is out of scope; the manager is in-process only.

Workflow-to-workflow chaining is **out of v1** (decision deferred from the May 9 trio brainstorm; ergonomic but adds engine complexity).

### Decision logic is agent-free in v1

The workflow's decision evaluates deterministically without an LLM call at trigger time. Use a known simple DSL (specific pick deferred; not inventing custom). If a decision genuinely needs context-shape understanding beyond a DSL can express, default to **always create the task** and let the operator (or agent at task-load time) decide. Don't push subtle decisions into workflow YAML — that's pseudo-intelligence and the maintenance debt is high.

LLM-involved decision evaluation is a **later v1 step**, not v2 — but not in the first slice.

### Workflow declares its plugin scope

Each workflow declares which plugins it operates on (`allowed_plugins: [yaad-bgg]`, `allowed_plugins: [yaad-gmail, yaad-wikipedia]`). Analogous to `import` at the top of a code file: the workflow only sees those plugins' surface. This naturally constrains things like `canonical_type` gaps (a `canonical_type(*)` gap inside a workflow with `allowed_plugins: [yaad-bgg]` is implicitly limited to kinds yaad-bgg emits).

**Load-time validation.** When the daemon loads a workflow file, it validates `allowed_plugins` against the live plugin registry. If a declared plugin isn't loaded, the workflow file is rejected on load with a clear error in the daemon log. Operator fixes the workflow (drops the plugin or loads it) and the daemon picks it up on next reload.

**Runtime errors — the err-task pattern.** A workflow can still fail after load: a plugin breaks mid-fetch, an upstream API returns malformed data, a fill-gap times out. These don't crash the workflow; instead they surface via a per-workflow **err task**.

- One err task per workflow, ever. Not per-failure.
- First failure creates the err task (kind `task`, with an `errored=true` marker).
- Subsequent failures on the same workflow update the existing err task — appending the failure details (timestamp, source entity, error message) to the task body, not spawning new err tasks.
- The err task is operator-visible alongside normal tasks (with the failure marker). Operator can read it, mark it resolved (which closes the err task), at which point the next failure spawns a fresh err task.
- Err tasks don't block the workflow from continuing to fire — they're observability, not a stop-signal.

This means: one consolidated "this workflow has been having trouble" surface per workflow, instead of N error tasks the operator has to triage one at a time.

### Missing-reference handling

If a workflow's context-load step follows a reference that doesn't resolve (e.g., regex for a JIRA key in a PR body and finds none), the workflow:
- Adds a **note** to the resulting task explaining the gap.
- Surfaces the task with that note attached.
- Operator can manually add the edge later (POST /v1/edges per ADR-0002 + `canonical_edge_types`).

Non-blocking: the task still surfaces, the operator sees the incomplete-context state and decides how to handle.

### Per-pattern de-duplication

When the same entity gets re-triggered (e.g., PR-foo gets 3 review-request emails over 2 days), the workflow should not spawn 3 separate tasks. De-dup is declared per workflow as a key that scopes "same situation."

The default key is `workflow + entity_id`: one task per workflow per source entity. A PR-review workflow keyed on `entity_id` produces one task for PR-123 — subsequent ingest events touching PR-123 update that existing task rather than creating new ones. Concrete example: an operator gets the initial PR-review-request email, then a ping-reminder a day later; the second email's workflow fire updates the existing task (with the latest PR state from the cache) instead of creating a duplicate.

For workflows whose "same situation" is time-windowed rather than entity-keyed (a daily morning-brief, a weekly summary), the key extends to include the window: `workflow + entity_id + day` or `workflow + week`.

Policy when a duplicate key fires:
- `update` (default) — modify the existing task with the new data; useful when the task surfaces a live entity that's getting refreshed.
- `skip` — no-op; useful when subsequent triggers are noise.
- `replace` — close the old task, create a new one; useful when each fire is a distinct moment to surface.

Workflows declare both the key and the policy. Implementation may extend the key vocabulary as patterns surface.

## Out of v1 (explicit)

- **Webhook ingress** — no HTTP-in server today; large add.
- **Workflow-to-workflow chaining** — ergonomic but adds engine complexity.
- **LLM-involved decisions** — last v1 step at earliest, not first.
- **External direct plugins** (github / jira / calendar direct) — gmail-via-notifications covers the first-tier workflows. Direct integration is v2.
- **Emit-notification output type** — listed in the output surface above; deferred unless concrete need surfaces in v1.
- **Push notifications on fill-gaps needing answer** — operator polls `/v1/needs-fill` in v1.

## Agent surface

New tools exposed via the daemon HTTP API + MCP:

- `workflow.list` — registered workflow patterns.
- `workflow.discover(entity_id)` — workflows that match a given entity.
- `workflow.trigger(name, input)` — manual trigger with required input.
- `task.list` — light list of open tasks with one-line descriptions.
- `task.load(id)` — uses the standard entity+edges fetch (`GET /v1/entities/{id}?with_edges=*`); no special endpoint needed. The workflow's pre-load step ensures the bundle is attached as edges by the time the agent reads the task.
- `task.resolve(id)` — mark done.

The pain point this ADR addresses (context-bundle assembly) is resolved by combining the workflow's pre-load step with the standard entity-with-edges fetch. The agent sees a task; the task has edges to the loaded context; the bundle is already there.

## Consequences

**Positive:**
- Operators stop hand-assembling context every time a flow runs. The workflow definition captures the shape once.
- Fill-gap pipeline gets a third source-shape; existing infrastructure does the work.
- Internal event bus opens a clean extension point for future engine features.
- Workflow definitions are markdown files in the vault — operator-editable, agent-readable, version-controlled.

**Negative:**
- Internal event bus is new daemon-side infrastructure with its own surface for bugs.
- Workflow YAML schema introduces a configuration surface that will evolve; early decisions on DSL choice constrain later flexibility.
- Tasks-as-entities adds an entity kind; existing ADR-0018 archive lifecycle and ADR-0008 vault-write disciplines apply.
- Operators can author workflows that conflict, loop, or over-trigger; v1 has no static validation beyond "is the YAML parseable."

**Migration:**
- No data migration required. Existing entities are unchanged.
- Existing implicit operator flows (e.g., Amazon receipt classification, GitHub notification triage) can be formalized as workflows incrementally — first three workflows to ship are Amazon, GitHub, Wolt, drawn from the highest-frequency existing patterns.
- Vikunja-style external task managers can be deprecated once `task.list` + `task.resolve` cover the operator's daily surface.

## References

- ADR-0002: Edge model.
- ADR-0008: Vault-as-truth.
- ADR-0017: Daemon-owned canonical slugs.
- ADR-0018: Archive lifecycle.
- ADR-0019: Operator-fill + gap types.
- ADR-0020: Search with gap predicates (this ADR's query backend).
- ADR-0023: Unified plugin response protocol (envelope shape that workflows subscribe to).
