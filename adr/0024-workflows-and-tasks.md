# ADR-0024: Workflows and Tasks

**Status:** PROPOSED
**Date:** 2026-05-12
**Revised:** 2026-05-16 (v1 design slice reconciled — see Revisions log at end)

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

Workflows are markdown files at `workflows/<name>.md` (vault-side default; daemon-managed; plural to match `tasks/`). **Body holds the rules as a YAML code-fence; frontmatter is metadata only** (name, version, status, etc.). The structured workflow definition lives inside a fenced ```yaml``` block in the body, with operator-readable prose around it. Rationale: frontmatter is limited (flat key-value); a YAML code-fence in the body lets the structured rules be richer + editor-friendly while the surrounding prose stays operator-readable as documentation.

**Workflow definitions are operator-authored, not yaad-index-shipped.** yaad-index ships the engine — parser, trigger detector, fill-gap integration, output dispatch. Operators (with agent help) write the actual workflow files in the vault.

**Daemon-side location is reserved** for any future system-shipped workflows (e.g., a workflow that yaad-index needs to bootstrap its own behavior). v1 has none; vault-only is effective today. The schema supports the daemon-side path so it can grow into it without a migration.

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

1. **Event-driven** — workflow subscribes to internal events emitted by the daemon's event bus. First-class in v1, starting with `edge_created` and growing to the full CRUD set by v1-final.
2. **Manual** — invoker calls `workflow.trigger(name, input)` (via MCP / HTTP API) or via the equivalent CLI shape `yaad-index workflow trigger <name> [input]`. Input shapes are entity ID, URL, or empty (workflow runs without an entity target). External host cron uses the CLI form for time-based patterns — e.g., a host cron entry firing `yaad-index workflow trigger morning-brief` daily covers the morning-brief / weekly-summary cases without the daemon owning a scheduler.
3. **Time-based (internal scheduling)** — **deferred post-v1.** Originally folded into v1; reverted in this revision. External host cron + the manual-trigger CLI cover the immediate need without requiring the daemon to grow scheduling primitives. A future v1.x can fold internal scheduling back in if external cron proves clunky.

### Internal event bus (v1 core)

The daemon emits internal events that workflows can subscribe to:

- `entity.created` — new entity added by any plugin (fresh-ingest only; not re-fetch of an already-known entity).
- `entity.edge_added` — new edge attached to an entity. Fires on every ingest that produces a new connection — including cache-hit re-fetches of a known entity that surface new edges, and operator-side manual edge adds.
- `fill.completed` — a gap-fill landed on an entity. Fires on every fill, including workflow-injected gap-fills evaluating during re-fetch. Carries a `source` tag identifying who initiated the fill (`agent`, `operator`, or `workflow:<name>` for workflow-injected fills).

This is the load-bearing piece. Without an internal event bus, workflows can only react to "new external thing came in via ingest" and the fill-gap integration described below collapses. With it, workflows become reactive to the index itself, not just external input — which is what differentiates a workflow from a glorified gmail rule.

**Cache-hit re-fetch semantics.** Re-fetch of a known entity does NOT re-fire `entity.created` (the entity already exists). Workflows that need to react to changes from re-fetch subscribe to `entity.edge_added` instead — a re-fetch surfacing a new connection (a news article gaining a topic-link, a PR gaining a new reviewer) fires `entity.edge_added` and the workflow re-evaluates from there. A workflow watching a topic entity for incoming `is_about` edges sees every new article tagged to that topic without needing a sibling "fetched" event.

**Self-loop detection.** Because workflows can both inject gaps AND subscribe to `fill.completed`, a naive engine would re-trigger the same workflow when its own injected fill lands. The `source` tag on `fill.completed` is what the engine uses to skip self-triggered re-evaluation — a workflow named `X` does not re-fire on a `fill.completed` event whose `source` is `workflow:X`. Cross-workflow chains are still out of v1 (see "Out of v1"), but the source tag also lays the groundwork for that future iteration to detect and break loops at the engine layer rather than relying on per-workflow author discipline.

The source tag breaks **direct** self-loops only. An **indirect** loop is still possible: workflow X injects gap G; agent fills G (`source: agent`); X subscribes to `fill.completed` on the entity, re-fires; X injects G again (or a different gap that the agent also fills); loop. v1 addresses this with two layered defenses:

1. **Workflow author discipline (primary).** Decision conditions must be monotone — once a workflow has produced its output for an entity, the decision should not re-fire on the same state. In practice this means the workflow's decision checks for the output's presence (a task already exists for this entity + key) before injecting again; the per-pattern dedup key + `update`/`skip` policies are the standard mechanism.
2. **Engine backstop.** Engine maintains a per-`(workflow, entity)` re-evaluation counter. If a workflow re-fires on the same entity more than a fixed bound within a short window (initial bound: 10 re-evaluations within 60 seconds), the engine suppresses further fires for that pair and surfaces an err task on the workflow with details. Backstop, not policy.

**Re-evaluation timing.** A workflow re-evaluates on every event matching its trigger condition. If a workflow subscribes to `fill.completed` for a gap that the agent strategy never extracts, the workflow does not fire — there is no event. If the gap stays unfilled across multiple ingests, the workflow stays dormant; surfacing-on-incomplete-context is handled by the missing-reference path below, not by firing on absence.

### Fill-gap injection (third filler-source)

ADR-0019 defined two fill-gap sources: agent-strategy (LLM fills from clean_content) and operator-strategy (operator fills from their own knowledge). Workflows become a **third source**: a workflow can ADD gaps to an entity at trigger time, and those gaps flow through the existing fill-gap pipeline.

Two worked-example shapes:

- **Newsletter-shape workflow** triggers on email-ingest, injects gaps like `is_newsletter?` (bool) and `subjects?` (canonical-typed list). Fill-gap pipeline answers them. Workflow's decision evaluates the filled data. Task created or not.
- **Deterministic-classifier-shape workflow** (e.g. Amazon receipt) triggers, knows what it is by sender domain, injects no gaps, extracts data inline, creates a task (or silent-logs) directly.

Workflow-injected gap fills are **permanent** on the entity per ADR-0008 (vault-as-truth). Future re-fetches reuse the stored fill rather than re-classifying. Side benefit: gradual personal-database enrichment as workflows fire over time.

### Output surface — action vocabulary

A workflow's output is one of three v1 action primitives:

- **`task_append`** — append content to a section of `tasks/<workflow>-<subject>.md`. Find-or-create semantics: the canonical task path is deterministic (workflow name + operator-defined subject template), so the same workflow firing repeatedly on the same subject lands in the same task file. The action appends to a named section in the task body; sections accumulate over time as the workflow re-fires. This is the default for workflows that surface recurring situations (PR-review, newsletter-of-interest, classified-receipt).
- **`add_comment`** — attach a comment to an existing entity (the entity that triggered the workflow, or one reached via graph). Reuses the existing entity-comment primitive (the `add_comment` MCP tool). Used when the workflow wants to enrich an entity with workflow-observed context rather than spawning a task.
- **`plugin_dispatch`** — fire a plugin command (e.g., `bgg.fetch`) from inside the workflow. Used for the "look-something-up-then-decide" shape: a workflow can trigger an ingest of a related entity that isn't yet in the index, wait for it (async), and proceed with the result attached to context.

Out of v1 (deferred — listed below): edge-creation as a workflow action, silent-log shape, emit-notification.

**Why the smaller set:** the earlier draft listed four output types (create task / mutate entity / add edge / silent log). The v1-reconciled set drops `add edge` and `silent log` as separate primitives — `add edge` becomes a follow-up via `plugin_dispatch` (the plugin emits the edge during ingest) or a manual operator add; `silent log` collapses into `add_comment` (record what the workflow saw, no task surface). `plugin_dispatch` is genuinely new — the earlier draft had no explicit way for a workflow to ask the index to go fetch something.

**Concurrent writes.** Two workflows — or any two writers (workflow output, UGC mutation, comment addition, edge addition, plugin emit, operator manual write) — may touch the same on-disk artifact at the same time. v1 protects via a daemon-internal **per-artifact write-lock manager** (`internal/writelocks` per yaad-index #23) with a **block-on-conflict** policy: an Acquire on an artifact already held by another writer returns a typed conflict error immediately, surfacing as a 409 envelope naming the active holder. No queuing, no merging, no last-writer-wins; the rejected caller retries.

Two write classes deliberately skip the lock as additive-append shapes that don't conflict at the storage layer:

- **Comments** (`POST /v1/entities/{id}/comments`) — append-only entries in vault frontmatter's comments table.
- **Edges** (`POST /v1/edges`) — append-only rows in the store + frontmatter.

Every other mutation surface (ingest, fill, operator-fill, archive/restore, delete, UGC section / frontmatter / create / delete) acquires the per-entity lock — section-scoped where applicable (UGC section writers key on `<id>#<idx>` so different sections of the same UGC file proceed concurrently). Cross-host distributed locking is out of scope; the manager is in-process only.

Workflow-to-workflow chaining is **out of v1** (decision deferred from the May 9 trio brainstorm; ergonomic but adds engine complexity).

### Decision logic is agent-free in v1

The workflow's decision evaluates deterministically without an LLM call at trigger time. **The expression language is CEL** (Common Expression Language, Google's purpose-built predicate language with a mature Go implementation in `cel-go`). Chosen because:

- Purpose-built for predicate expressions (compared to a general scripting language).
- Safe-by-default: no I/O, no side effects, deterministic evaluation, fast.
- Mature Go impl already used by Kubernetes, Envoy, Cloud Audit — well-trodden.
- Smaller surface than embedding a full scripting language (Starlark, Lua, etc.); harder for operators to abuse with code-where-rules-belong.

Settles the "specific pick deferred; not inventing custom" open question from the original draft.

**Predicate shape — cluster-aware.** Workflow decisions operate over a **cluster** of related entities, not only the single triggering entity. The operator declares the cluster (via a graph query — e.g., "all editions of this series", "all PRs against this repo touched today") and the predicate evaluates across the set. CEL's collection operators (`.exists(...)`, `.all(...)`, `.filter(...)`) carry this natively. Worked example: a boardgame-news workflow firing on the 2nd-edition of a game can pull the 1st-edition via graph, evaluate `editions.exists(e, e.rating > 7)`, and surface even when the immediate target has no rating.

The expression context provides:

- `entity` — the triggering entity (resolved, fetched-if-missing).
- `edge` — the triggering edge (its from/to/type/timestamp).
- `graph` — a lookup function (`graph.get(id)`, `graph.find({predicate})`) for pulling related entities.

If a decision genuinely needs context-shape understanding beyond what CEL + the cluster query can express, default to **always create the task** and let the operator (or agent at task-load time) decide. Don't push subtle decisions into workflow YAML — that's pseudo-intelligence and the maintenance debt is high.

LLM-involved decision evaluation is a **later v1 step**, not v2 — but not in the first slice.

### Workflow declares its plugin scope

Each workflow declares which plugins it operates on (`allowed_plugins: [yaad-bgg]`, `allowed_plugins: [yaad-gmail, yaad-wikipedia]`). Analogous to `import` at the top of a code file: the workflow only sees those plugins' surface. This naturally constrains things like `canonical_type` gaps (a `canonical_type(*)` gap inside a workflow with `allowed_plugins: [yaad-bgg]` is implicitly limited to kinds yaad-bgg emits).

**Load-time validation.** When the daemon loads a workflow file, it validates `allowed_plugins` against the live plugin registry. If a declared plugin isn't loaded, the workflow file is rejected on load with a clear error in the daemon log. Operator fixes the workflow (drops the plugin or loads it) and the daemon picks it up on next reload.

**Runtime errors — the err-task pattern.** A workflow can still fail after load: a plugin breaks mid-fetch, an upstream API returns malformed data, a fill-gap times out. These don't crash the workflow; instead they surface via a per-workflow **err task**.

- One err task per workflow, ever. Not per-failure.
- First failure creates the err task. **Frontmatter shape: `kind: task` + `errored: true` field** — not a separate `kind: err-task`. The reasoning: err tasks remain queryable through the standard `task.list` paths and the operator's normal task surface; the `errored` boolean is a filter, not a separate entity kind. Avoids the kind-explosion problem and keeps err tasks first-class without a parallel taxonomy.
- Subsequent failures on the same workflow update the existing err task — appending the failure details (timestamp, source entity, error message) to the task body, not spawning new err tasks.
- The err task is operator-visible alongside normal tasks (with the failure marker). Operator can read it, mark it resolved (which closes the err task), at which point the next failure spawns a fresh err task.
- Err tasks don't block the workflow from continuing to fire — they're observability, not a stop-signal.
- **`auto_archive_on_done: false` does NOT apply to err tasks.** That flag governs normal-task lifecycle when the operator completes the work. Err tasks always auto-archive on operator-resolve regardless, because resolution means "I fixed the source of the failures," which is operationally distinct from "I completed the work the task surfaced." Keeping resolved err tasks around bloats the surface without payoff.

This means: one consolidated "this workflow has been having trouble" surface per workflow, instead of N error tasks the operator has to triage one at a time.

### Missing-reference handling

If a workflow's context-load step follows a reference that doesn't resolve (e.g., regex for a JIRA key in a PR body and finds none), the workflow:
- Adds a **note** to the resulting task explaining the gap.
- Surfaces the task with that note attached.
- Operator can manually add the edge later (POST /v1/edges per ADR-0002 + `canonical_edge_types`).

Non-blocking: the task still surfaces, the operator sees the incomplete-context state and decides how to handle.

**Re-trigger on edge add.** When the operator manually adds the missing edge later, `entity.edge_added` fires. Workflows subscribed to that event re-evaluate with the now-complete context. The workflow's de-dup key (next section) determines what happens to the existing missing-reference task: with `update` policy the task is updated with the resolved context (still the same task, now complete); with `replace` policy the missing-ref task closes and a fresh task spawns; with `skip` the missing-ref task stays as-is and the re-trigger no-ops. This is a self-healing pattern when the workflow opts into it via subscription + policy.

### Per-pattern de-duplication

When the same entity gets re-triggered (e.g., PR-foo gets 3 review-request emails over 2 days), the workflow should not spawn 3 separate tasks. De-dup is declared per workflow as a key that scopes "same situation."

The default key is `workflow + entity_id`: one task per workflow per source entity. A PR-review workflow keyed on `entity_id` produces one task for PR-123 — subsequent ingest events touching PR-123 update that existing task rather than creating new ones. Concrete example: an operator gets the initial PR-review-request email, then a ping-reminder a day later; the second email's workflow fire updates the existing task (with the latest PR state from the cache) instead of creating a duplicate.

For workflows whose "same situation" is time-windowed rather than entity-keyed (a daily morning-brief, a weekly summary), the key takes a different form. Time-based workflows don't always have a single source entity — a morning-brief reads across many entities, a weekly summary aggregates. The key drops the entity anchor and becomes `workflow + window`: `workflow + day` for daily cadences, `workflow + week` for weekly, etc. Multi-entity time-based workflows are explicitly excluded from entity-keyed de-dup; their key form is window-only. Entity-keyed time-windowed forms (e.g. `workflow + entity_id + day`) remain available for time-windowed-but-still-entity-anchored cases (a daily-digest for a specific PR's review activity).

**Two distinct dedup paths.** The err-task pattern above carries an *implicit* one-per-workflow-ever dedup (one err task per workflow, by mechanism). The per-pattern de-dup here is *functional* — the workflow declares its own key + policy. These are different mechanisms operating at different concerns: err-task dedup is observability scaffolding tied to engine-detected failures; per-pattern dedup is the workflow author's expression of "same situation." The engine should not conflate them. A workflow's `key` + `policy` settings apply to its **normal task** outputs only; err tasks are out of band.

Policy when a duplicate key fires:
- `update` (default) — modify the existing task with the new data; useful when the task surfaces a live entity that's getting refreshed.
- `skip` — no-op; useful when subsequent triggers are noise.
- `replace` — close the old task, create a new one; useful when each fire is a distinct moment to surface.

Workflows declare both the key and the policy. Implementation may extend the key vocabulary as patterns surface.

**Secondary action-level dedup inside `task_append`.** Per-pattern dedup at the workflow level prevents N tasks for the same situation. A second layer inside `task_append` prevents N duplicate lines inside the same task: by default `task_append` skips a write when the content line already exists in the target section. The operator can override per-step (`if_already_present: append-anyway` / `replace` / `skip`). The two layers cover different concerns and stack:

- **Workflow-level dedup (per-pattern key + policy)** decides whether a workflow fire reaches the action stage at all.
- **Action-level dedup (`task_append` skip-if-line-exists)** decides whether a specific line lands inside the task body.

In practice the workflow-level layer handles "should this become a fresh task or update an existing one"; the action-level layer handles "the workflow fired with `update` policy, the existing task is being updated — does this specific content line need to be appended or is it already there?" Belt and suspenders, not redundant.

## Out of v1 (explicit)

- **Webhook ingress** — no HTTP-in server today; large add.
- **Workflow-to-workflow chaining** — ergonomic but adds engine complexity.
- **LLM-involved decisions** — last v1 step at earliest, not first.
- **External direct plugins** (github / jira / calendar direct) — gmail-via-notifications covers the first-tier workflows. Direct integration is v2.
- **Emit-notification output type** — deferred unless concrete need surfaces in v1.
- **Push notifications on fill-gaps needing answer** — operator polls `/v1/needs-fill` in v1.
- **Internal time-based / cron trigger** — deferred to post-v1. External host cron + the manual-trigger CLI cover the immediate need; daemon doesn't grow a scheduler.
- **`add_edge` as a workflow action** — workflows don't directly create edges in v1. Edge creation surfaces via `plugin_dispatch` (the plugin emits edges during ingest) or via operator manual `POST /v1/edges`.
- **`silent_log` as a distinct output shape** — collapses into `add_comment` (record what the workflow observed against the entity without spawning a task surface). If a true silent-no-side-effect log is needed (audit only), a workflow that does nothing on its happy path achieves that via the err-task pattern on failures only.

## Agent surface

New tools exposed via the daemon HTTP API + MCP:

- `workflow.list` — registered workflow patterns.
- `workflow.discover(entity_id)` — workflows that match a given entity.
- `workflow.trigger(name, input)` — manual trigger with required input.
- `task.list` — light list of open tasks with one-line descriptions.
- `task.load(id)` — uses the standard entity+edges fetch (`GET /v1/entities/{id}?with_edges=*`); no special endpoint needed. The workflow's pre-load step ensures the bundle is attached as edges by the time the agent reads the task.
- `task.resolve(id)` — mark done.

### `workflow.trigger(input)` input semantics

`input` accepts two shapes, dispatched by string form:

- **Entity ID** (matches the daemon's canonical ID format) — caller has already resolved the target entity; the engine attaches the workflow to it directly.
- **URL** (matches a URL pattern) — the engine routes through ingest-or-lookup before attaching: if the URL is already a known entity, that entity is the target; if not, the URL is ingested through the normal plugin pipeline and the workflow attaches to the resulting entity.

Both shapes are first-class; the engine branches on the input's syntactic shape. Callers can pre-resolve for performance (skip ingest-or-lookup) or pass a URL for convenience (let the engine handle resolution).

**Failure paths.** Two distinct failure modes for URL inputs:

- **URL doesn't resolve to a plugin / no plugin accepts it / URL is malformed** — the trigger call itself fails. `workflow.trigger` returns a typed error to the caller before any workflow run starts; no entity is attached, no err task is created, the workflow's behavior is unchanged. The caller sees a synchronous error.
- **URL routes to a plugin but the plugin fails during ingest** (network timeout, upstream API error, unparseable payload) — the trigger call succeeds (it dispatched to a known plugin), and the failure surfaces through the existing err-task pattern: one err task per workflow, updated on subsequent failures, resolvable by the operator. This is the same pathway as workflow-runtime failures already documented above.

Entity-ID inputs only have one failure mode (entity does not exist) — synchronous error to the caller, no err task.

### `workflow.discover(entity_id)` performance note

The discovery walks every registered workflow and evaluates each trigger condition against the entity. Cost is `O(W × T)` where W is the registered workflow count and T is the per-condition evaluation cost. For v1, W is small (single-digit or low-double-digit workflows per operator) so this is fine; if W grows past a few dozen and discovery becomes the bottleneck, a future iteration can index trigger conditions for sublinear lookup. Not in v1.

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

## Revisions

### 2026-05-16 — v1 design slice reconciled

The original PROPOSED draft (2026-05-12) was richer than necessary for a first v1 cut and had several open-question placeholders. This revision tightens the v1 slice and settles those questions. Changes:

- **Expression language: CEL.** Settles the "specific pick deferred; not inventing custom" open question. `cel-go` is the impl. Predicates operate over a **cluster** of related entities queried via `graph.get` / `graph.find`, with collection operators (`exists`, `all`, `filter`) carrying the multi-entity decision shape.
- **Action vocabulary: `task_append`, `add_comment`, `plugin_dispatch`** — three primitives, not four. `add_edge` and `silent_log` move to Out-of-v1. `plugin_dispatch` is new — it's how a workflow asks the index to go fetch related context mid-evaluation.
- **File format: `.md` extension with YAML in a body code-fence.** Frontmatter holds metadata only (name, version, status). Earlier draft put rules in frontmatter; YAML code-fence is richer and lets operator-readable prose live alongside structured rules.
- **Trigger types: event-driven + manual-via-CLI in v1.** Internal time-based / cron trigger deferred post-v1. External host cron + the `yaad-index workflow trigger <name>` CLI cover the morning-brief / weekly-summary cases without the daemon growing a scheduler.
- **Dedup: two layers explicit.** Per-pattern at workflow level (key + policy `update`/`skip`/`replace`) prevents N tasks for the same situation; action-level inside `task_append` (skip-if-line-exists) prevents N duplicate lines inside the same task. Stack, don't conflict.
- **Workflow location: vault-default + daemon-reserved.** Operator-authored vault workflows are the only thing today; daemon-side is reserved as a forward-compatible path for future system-shipped workflows.

Status remains PROPOSED pending operator hard-gate review of this revision.
