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

**The `context` stanza.** Workflows can declare named bindings that get evaluated before the `condition`. Each entry has a `name` and a `via` CEL expression; the engine evaluates the `via` expression once and binds the result to `name` in the expression context for `condition` and action templates. Used when the same sub-expression appears in multiple places (DRY) or when the operator wants to give a named handle to a graph-walked entity for readability. `via` failure (e.g., `graph.get` not-found, or the expression itself raises) follows the same missing-reference path as condition-eval: a note attached to the resulting task. `context` is optional; workflows that don't need named pre-bindings omit the stanza entirely.

**Worked example — minimal complete workflow file.** A boardgame-news workflow that fires on the relevant edge type, pulls the prior edition (if the operator has stored its ID on the 2nd edition's frontmatter), decides via CEL, and appends to a recurring task:

````markdown
---
name: boardgame-news
version: 1
status: active
---

# Boardgame news → review queue

Surfaces news articles about boardgames I own or care about (rating > 7
on this game OR on the prior edition stored as `previous_edition_id` in
frontmatter).

```yaml
trigger:
  type: edge_created
  match:
    edge_type: is_about
    target_kind: boardgame

subject: '{{ entity.slug }}'

context:
  # Optional named binding. If the 2nd-edition entity has a previous_edition_id
  # field, fetch the prior edition entity once and bind it as `prior` for the
  # condition + action templates. has() guards the optional field; the engine's
  # graph.get() not-found semantics surface as a missing-reference note if the
  # ID is set but doesn't resolve.
  - name: prior
    via: 'has(entity.previous_edition_id) ? graph.get(entity.previous_edition_id) : null'

condition: 'entity.rating > 7 || (prior != null && prior.rating > 7) || entity.owned == true'

dedup:
  key: 'workflow + entity.id'
  policy: update

actions:
  - task_append:
      section: candidates
      content: '{{ entity.name }} ({{ entity.year }}) — surfaced via {{ edge.from_title }}'
      if_already_present: skip
```
````

Frontmatter holds metadata (name, version, status) only. Body's prose explains what the workflow is doing in operator-readable terms. The YAML code-fence holds the structured rules the engine parses. Operator can re-read this file in six months and remember why it exists; the engine can re-parse it on reload without ambiguity.

The example deliberately uses **a single related entity** (one prior edition stored by ID), not a collection. Collection-shaped `context` bindings (`graph.get(...).editions` returning a list of IDs auto-resolved to entities) is the natural next pattern but the resolution semantics for ID-list → entity-list aren't settled in v1; defer to post-v1 alongside `graph.find`. v1 operators who need a multi-related-entity predicate model it with multiple named `context` entries (`prior`, `prior_prior`, etc.) — clunky but explicit.

**Worked example — lifecycle mirror pair.** Archive a GitHub PR or issue when upstream reports it closed; restore it when upstream reopens. Two workflow files, one per direction, both subscribed to `entity.updated` with a `field_changed: data.state` filter and a `canonical_kind` filter narrowing to the github source kinds. Mirror pair; idempotent.

````markdown
---
name: github-archive-on-close
version: 1
status: active
---

# Archive closed GitHub items

Wraps ADR-0018 archive lifecycle around the github plugin's emitted
`data.state` transitions. The plugin re-fetches its open + recently-
closed set on each `!fetch` sweep; when an item that was previously
`open` returns as `closed`, `entity.updated` fires with a
`data.state` delta and this workflow archives.

```yaml
trigger:
  type: entity_updated
  match:
    field_changed: data.state
    canonical_kind: [github-pr, github-issue]

condition: 'entity.data.state == "closed"'

dedup:
  key: 'workflow + entity.id'
  policy: skip

actions:
  - archive_entity:
      target: '{{ entity.id }}'
      reason: 'github-state-closed'
```
````

The restore direction is the mirror — same trigger + match shape, `condition: 'entity.data.state == "open"'`, action calls `restore_entity` with `reason: 'github-state-reopened'`. Each direction is a separate workflow file so the operator can disable one without the other; both are idempotent so re-firing on the same state value is a no-op.

The `dedup` uses `skip` rather than `update` because `archive_entity` / `restore_entity` are idempotent at the engine layer and don't need a per-fire task surface — the audit trail lives in the entity's archive metadata, not in a recurring task. A future enhancement could collapse the two files into a single workflow with a CEL conditional choosing between actions, but that requires action-level conditional expressions (out of v1).

### Task

A **task** is a workflow's instance — a first-class entity created when a workflow's output produces one.

- Each task is its own entity with its own ID, frontmatter, and edges.
- Entity id shape: `task:<workflow-slug>-<subject-slug>` (or `task:<workflow-slug>-err` for err tasks). On-disk vault path is `<vault>/tasks/<slug>.md`; the canonical kind is the singular `task` while the operator-facing directory keeps the plural `tasks/` convention.
- Edges: a `triggered_by` edge from the task to the source entity whose firing produced it (the entity the workflow loaded — PR, issue, email, etc.). Spec also calls for a workflow → task edge; that target requires a `workflow` entity kind which v1.x doesn't yet ship, so the spawning-workflow attribution lives in the task frontmatter's `workflow:` field and in the `via:` breadcrumb list instead.
- Properties: priority (operator-modifiable), snooze (see below), notes (operator notes, agent-readable on load).
- Close: either operator-marks-done (default) or condition-based per workflow pattern (e.g., PR-merged → auto-close).
- Auto-archive on done (per ADR-0018 archive lifecycle). Workflow can opt out per-pattern via `auto_archive_on_done: false` for cases where the operator wants the audit trail of completed tasks to stick around.

The `task` kind + `triggered_by` edge type are daemon-managed (always available, like `day` and the day-anchored edges per ADR-0025); operators don't enable them in `canonical_kinds:` / `canonical_edge_types:` config. Vault is authoritative per ADR-0008: store rows materialize on the **first-create** of a task file, so going forward every new task spawn is a first-class entity. Pre-#268 task files (already on disk before this revision lands) won't have store rows backfilled — they remain readable via `/v1/tasks` (filesystem-walk) but `/v1/entities/task:<slug>` returns 404 for the historical set. Operators wanting the historical set as entities recreate the affected tasks; there is no automatic migration pass.

#### Snooze semantics

Operator can snooze a task with an until-time (snooze until tomorrow, until next Monday, until a specific date, etc.). Snoozed tasks are **hidden from normal `task.list`** until the snooze expires. An operator who wants to see them explicitly fetches with an include-snoozed flag.

A snoozed task is **still live** — it continues to accept updates. If new material surfaces while the task is snoozed (a re-trigger that the workflow's de-dup rule routes to the existing task), the task is updated with the new state, but the task stays hidden until the snooze expires. The operator chose to defer the surface, not to disconnect the task from incoming data.

Snooze is operator-controlled, not workflow-controlled — the workflow doesn't get to auto-snooze its own tasks. (A future operator-config knob could permit per-workflow default-snooze for patterns where deferred-by-default makes sense; out of v1.)

Tasks outlive the workflow that spawned them — if a workflow pattern is deleted, its tasks orphan (listable + closable, no re-trigger). This is intentional: the operator's commitment to a task ("I'll review this PR") is independent of the workflow rule that surfaced it.

### Trigger types (v1)

1. **Event-driven** — workflow subscribes to internal events emitted by the daemon's event bus. First-class in v1, starting with `edge_created` and growing to the full CRUD set by v1-final.
2. **Manual** — invoker calls `workflow.trigger(name, input)` (via MCP / HTTP API) or via the equivalent CLI shape `yaad-index workflow trigger <name> [input]`. Input shapes: entity ID, URL, or empty (workflow runs without an entity target). The engine disambiguates ID vs URL via the existing rule documented in the **`workflow.trigger(input)` input semantics** section below — URL pattern triggers ingest-or-lookup; otherwise the input is parsed as a canonical entity ID. Empty input is reserved for workflows that don't take a target (e.g., a daily-summary workflow that aggregates across the index). External host cron uses the CLI form for time-based patterns — e.g., a host cron entry firing `yaad-index workflow trigger morning-brief` daily covers the morning-brief / weekly-summary cases without the daemon owning a scheduler.
3. **Time-based (internal scheduling)** — **deferred post-v1.** Originally folded into v1; reverted in this revision. External host cron + the manual-trigger CLI cover the immediate need without requiring the daemon to grow scheduling primitives. A future v1.x can fold internal scheduling back in if external cron proves clunky.

### Internal event bus (v1 core)

The daemon emits internal events that workflows can subscribe to:

- `entity.created` — new entity added by any plugin (fresh-ingest only; not re-fetch of an already-known entity).
- `entity.edge_added` — new edge attached to an entity. Fires whenever an edge is added regardless of origin: fresh-ingest, cache-hit re-fetch surfacing a new connection, operator-side manual edge add, **and fill-gap operations that produce edges** (e.g., an `agent` strategy fills a `series_id?` gap with a value that resolves to an entity, creating a `belongs_to_series` edge; the edge fires `entity.edge_added` separately from the `fill.completed` event the same operation produces). Workflows subscribed to `edge_added` see all of these uniformly — the trigger semantics are "an edge exists now that didn't a moment ago," not "an ingest cycle ran." A node can be ingested without any edges and later acquire them via fill-gap; both cases reach the same `edge_added` subscribers.
- `entity.updated` — a single field inside `structured.data` on a known entity changed via re-fetch. Fires when a plugin's ingest re-fetches a known entity and the new value differs from the previously-stored value. Each event represents **one changed field** and carries `field`, `old`, and `new` — singular. **A re-fetch that changes N fields emits N separate `entity.updated` events**, not one event with a delta list. Mirrors `entity.edge_added`'s per-edge granularity: one event per logical change, so the engine can fan out subscribers via simple per-field matching instead of asking each subscriber to navigate a delta list in its CEL predicate. Does NOT fire on first ingest (that's `entity.created`); does NOT fire on edge-only changes (that's `entity.edge_added`); does NOT fire on gap-fills (that's `fill.completed`). Used by workflows that react to lifecycle transitions stored in entity data — e.g., a `github-pr`'s `state` flipping `"open"` → `"closed"`, or a boardgame's `owned` flag flipping `false` → `true`.

  **Event ordering within a single re-fetch.** The engine emits the N events sequentially as part of the same ingest transaction, in declaration order of the changed fields (the order plugins list them in the wire envelope's `structured.data`). Subscribers should NOT depend on the order — independent workflows watching different fields fire independently. A workflow that genuinely needs to coordinate across multiple field changes from the same re-fetch is out of v1 (the engine doesn't expose a "re-fetch transaction" boundary to workflows). v1 workflows are single-field reactive.
- `fill.completed` — a gap-fill landed on an entity. Fires on every fill, including workflow-injected gap-fills evaluating during re-fetch. Carries a `source` tag identifying who initiated the fill (`agent`, `operator`, or `workflow:<name>` for workflow-injected fills). Note: when a fill produces an edge, both events fire — `fill.completed` for the gap closure and `entity.edge_added` for each emerged edge. Workflows can subscribe to whichever is the more natural match for the rule they're expressing.

**`entity.updated` vs `fill.completed`.** A gap-fill modifies `structured.data` too, but it fires `fill.completed` exclusively — it does NOT additionally fire `entity.updated`. The two events are semantic peers: `fill.completed` covers gap-closure mutations (agent / operator / workflow filled a declared gap), `entity.updated` covers plugin-driven re-fetch mutations (the upstream system reports a different value than the index previously held). A workflow watching for a field change from any source subscribes to both events with matching field-level filters; a workflow watching specifically for upstream-reported transitions (the GitHub state-flip case) subscribes to `entity.updated` alone. This split keeps gap-fill provenance distinct from upstream-truth provenance, mirroring the [ADR-0008](./0008-vault-as-source-of-truth.md) source-of-truth boundary.

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
- **`task_resolve`** (per #266) — flip or remove a single line in ANOTHER workflow's task file. Cross-workflow line-resolve surface for the recurring shape where one workflow records a *pending* state (via `task_append`) and a second workflow *resolves* it when the underlying entity transitions. Shape: `{workflow, subject, section, match_key, mode}`. Modes: `check` flips `- [ ]` → `- [x]` on the first match-key-prefix-matched line; `remove` strips the line. Missing target file → no-op (originating workflow may not have fired). No-match within an existing file → no-op. Idempotent end-state-driver semantics: a workflow firing N times against an already-checked line leaves the line in place. The operator-side `task_resolve` MCP tool that resolves an entire task by id is a separate path; this action operates at line granularity within a section.
- **`add_note`** — attach a note to an existing entity (the entity that triggered the workflow, or one reached via graph). Reuses the existing entity-note primitive (the `add_note` MCP tool). Used when the workflow wants to enrich an entity with workflow-observed context rather than spawning a task.
- **`plugin_dispatch`** — fire a plugin command (e.g., `bgg.fetch`) from inside the workflow. Used for the "look-something-up-then-decide" shape: a workflow asks the index to go ingest a related entity that isn't yet known, gets the result back, and proceeds with that entity in context.
- **`add_gap`** — inject a gap onto an entity from the workflow's action stage. Used for the "ask the operator/agent a question via the gap-fill pipeline" shape: a workflow that doesn't have a deterministic predicate (because the operator's interest isn't stored anywhere) injects a gap (e.g., `is_interesting_to_me?`) onto the relevant entity; the gap surfaces in `needs-fill` for the operator to answer; a subscribing workflow (could be the same, could be a sibling) fires on the `fill.completed` event and decides on the answer. Bridges decision-needing-human-input cases into the workflow framework without requiring an LLM-decision step in v1.

  **Constraints on `add_gap`:**
  - **Permanent on the entity** per ADR-0008 (vault-as-truth) — same rule as trigger-time gap injection. Once added, the gap persists in frontmatter and survives re-fetches; the fill is durable.
  - **Constrained to the workflow's declared gap vocabulary.** Each workflow declares ONE unified `addable_gaps` set in the YAML body (e.g. `addable_gaps: [is_interesting_to_me, owned_status]`) that covers BOTH paths — trigger-time injection AND action-stage `add_gap`. Calls outside that declared set are rejected at expression-evaluation time as a workflow-author error. Single source of truth keeps the workflow's gap-side-effect surface visible at file-read time rather than scattered across CEL expressions or split between two declarations.
  - **Re-fire semantics distinct from self-loop.** The existing internal-event-bus `source` tag breaks **direct self-loops** — workflow X's own fill (where X is also a filler) doesn't re-fire X. `add_gap` is different: workflow X adds the gap, but the **operator or agent** fills it later. The resulting `fill.completed` event carries `source: operator` or `source: agent`, NOT `workflow:X`. A workflow X that adds a gap AND subscribes to `fill.completed` for that gap WILL re-fire when the operator/agent answers — that's the intended round-trip ("workflow asks question; human answers; workflow decides on the answer"). The source-tag self-loop detection doesn't apply here. The engine-backstop re-evaluation counter (per-`(workflow, entity)` counter, fixed bound within a short window) still applies as a runaway-detection backstop.

**Decision rule between `task_append`, `add_note`, `add_gap`, `archive_entity`, and `restore_entity`:**
- If the information needs operator queuing (i.e., something to act on later) or accumulates across multiple workflow fires into a recurring surface → `task_append`.
- If it's entity-local annotation (context observed against an entity, no required operator action) → `add_note`.
- If the workflow needs a human-shaped answer to proceed (the predicate can't decide alone) → `add_gap`, with a sibling workflow subscribing to `fill.completed` for the decision step.
- If a lifecycle transition observed on the entity (typically a `data.*` field change reported by re-fetch) means the entity should leave the active set → `archive_entity`. Inverse direction → `restore_entity`.

A single workflow can use multiple — e.g., a PR-review workflow can `add_note` on the PR entity with the review-state delta AND `task_append` the operator's "review needed" line to the running task.

**`archive_entity` + `restore_entity` action primitives.** Mirror-pair actions that wrap [ADR-0018](./0018-archive-replaces-delete.md)'s archive lifecycle and surface it as a workflow output. The pair is the load-bearing mechanism for "plugin emits truth, workflow owns lifecycle" — plugins emit a `structured.data` field describing upstream state (e.g., a GitHub PR's `state: "closed"`) and a workflow subscribed to `entity.updated` with a `field_changed: data.state` filter calls `archive_entity` when the new value indicates the entity should leave the active set, `restore_entity` when the reverse transition occurs.

Both are **idempotent**: archiving an already-archived entity is a no-op (the engine resolves the target via the entity's canonical ID, checks current archive state, and skips if no change is needed); restoring an already-active entity is likewise a no-op. This matters because the same `entity.updated` event can re-fire if a downstream re-fetch surfaces the same field value on multiple sweeps — the workflow author shouldn't have to guard against that.

**Target.** Both actions take a single `target:` field — a CEL-templated entity ID. The most common pattern is `target: '{{ entity.id }}'` (act on the triggering entity), but graph-walked targets are valid (`target: '{{ graph.get(edge.to).id }}'`). The engine resolves the template at evaluation time and calls the existing `archive_entity` / `restore_entity` daemon primitives (per ADR-0018 §"Archive lifecycle"). A target that doesn't resolve follows the missing-reference path (note on the resulting task, no err-task) — same handling as `graph.get` not-found.

**Optional `reason:` field.** Both actions accept an optional `reason:` string for provenance — written into the archive/restore audit record so the operator can answer "why did this archive happen" by reading the entity's history. Convention: a stable token identifying the workflow + transition (e.g., `'github-state-closed'`, `'github-state-reopened'`), not a free-form description. Free-form lives in the worked example's documentation prose, not the audit field.

**Self-loop interaction.** `archive_entity` and `restore_entity` mutate entity state but they do NOT fire `entity.updated` (the archive flag is metadata, not a `structured.data` field). The pair therefore cannot self-trigger a workflow that subscribes to `entity.updated` on `data.state`. A workflow that needs to chain archive → some-other-state-mutation uses workflow-to-workflow chaining (out of v1).

**`plugin_dispatch` execution semantics.** The call is **synchronous from the workflow's point of view**: the workflow blocks on the plugin result up to a configurable timeout (v1 default: 30s). The plugin can do its work async internally (long-poll, queue, etc.), but the workflow does not see "async result later" — it sees the result inline or a timeout. The "async" framing in earlier drafts was misleading; replacing it. On timeout: the workflow's err-task pattern fires (one err task per workflow, error appended); the workflow continues firing on future events but the current evaluation aborts. On plugin error (typed failure surfaced through the unified plugin response envelope per ADR-0023): same path — err task, abort current evaluation.

Out of v1 (deferred — listed below): edge-creation as a workflow action, silent-log shape, emit-notification, `graph.find` lookups, internal time-based trigger.

**Why the smaller set:** the earlier draft listed four output types (create task / mutate entity / add edge / silent log). The v1-reconciled set drops `add edge` and `silent log` as separate primitives — `add edge` becomes a follow-up via `plugin_dispatch` (the plugin emits the edge during ingest) or a manual operator add; `silent log` collapses into `add_note` (record what the workflow saw, no task surface). `plugin_dispatch` is genuinely new — the earlier draft had no explicit way for a workflow to ask the index to go fetch something. `archive_entity` + `restore_entity` are also v1 — added in the 2026-05-21 amendment to back the workflow-driven lifecycle pattern (plugin emits truth, workflow owns archive state).

**Concurrent writes.** Two workflows — or any two writers (workflow output, UGC mutation, note addition, edge addition, plugin emit, operator manual write) — may touch the same on-disk artifact at the same time. v1 protects via a daemon-internal **per-artifact write-lock manager** (`internal/writelocks` per yaad-index #23) with a **block-on-conflict** policy: an Acquire on an artifact already held by another writer returns a typed conflict error immediately, surfacing as a 409 envelope naming the active holder. No queuing, no merging, no last-writer-wins; the rejected caller retries.

Two write classes deliberately skip the lock as additive-append shapes that don't conflict at the storage layer:

- **Notes** (`POST /v1/entities/{id}/notes`) — append-only entries in vault frontmatter's notes table.
- **Edges** (`POST /v1/edges`) — append-only rows in the store + frontmatter.

Every other mutation surface (ingest, fill, operator-fill, archive/restore, delete, UGC section / frontmatter / create / delete) acquires the per-entity lock — section-scoped where applicable (UGC section writers key on `<id>#<idx>` so different sections of the same UGC file proceed concurrently). Cross-host distributed locking is out of scope; the manager is in-process only.

Workflow-to-workflow chaining is **out of v1** — ergonomic but adds engine complexity.

### Decision logic is agent-free in v1

The workflow's decision evaluates deterministically without an LLM call at trigger time. **The expression language is CEL** (Common Expression Language, Google's purpose-built predicate language with a mature Go implementation in `cel-go`). Chosen because:

- Purpose-built for predicate expressions (compared to a general scripting language).
- Safe-by-default: no I/O, no side effects, deterministic evaluation, fast.
- Mature Go impl already used by Kubernetes, Envoy, Cloud Audit — well-trodden.
- Smaller surface than embedding a full scripting language (Starlark, Lua, etc.); harder for operators to abuse with code-where-rules-belong.

Settles the "specific pick deferred; not inventing custom" open question from the original draft.

**Predicate shape.** Workflow decisions can pull related entities into the expression context (via the optional `context` stanza, defined in the Workflow Files section). The triggering entity is always in scope as `entity`; named bindings populated from `graph.get(...)` give the predicate access to specific related entities by ID. The decision predicate then evaluates over the triggering entity, the optional bindings, and any literals. Worked example: a boardgame-news workflow firing on the 2nd-edition can bind the 1st-edition via `context.prior = graph.get(entity.previous_edition_id)` and evaluate `entity.rating > 7 || prior.rating > 7`, surfacing even when the immediate target has no rating but a related one does. Collection-shaped predicates (`editions.exists(e, e.rating > 7)` over a list of related entities) are out of v1 — resolution semantics for ID-list → entity-list aren't settled; deferred alongside `graph.find`.

The expression context provides:

- `entity` — the **triggering entity**, the `this`-like reference for the current workflow fire. Fully resolved (fetched-if-missing). Workflows are generic predicates; `entity` becomes specific at trigger time. The same workflow firing on N different entities sees N different values of `entity` — that's the dynamism. Predicates that key off the triggering entity write `entity.rating > 7`, not a hardcoded ID.
- `edge` — the triggering edge. Fields available in expressions: `edge.type` (canonical edge type), `edge.from` (source entity canonical ID), `edge.to` (target entity canonical ID), `edge.from_title` and `edge.to_title` (display-readable titles of the endpoints, resolved from the linked entities), `edge.timestamp` (when the edge was created). The titles are populated by the engine at expression-evaluation time via a single graph lookup per endpoint; not free, but bounded. **`edge` is nil/absent for manually-triggered workflows** (the `workflow.trigger(name, input)` path has no triggering edge). CEL predicates that reference `edge.*` on a workflow that supports manual triggers must guard, e.g., `has(edge) && edge.type == 'is_about'`. A future iteration may let the manual-trigger CLI optionally pass an edge ID to populate the slot, but v1 does not.
- `trigger` — the **trigger context**, the per-firing description of *what caused this fire*. Fields: `trigger.source` (the fully-resolved entity whose action initiated the event — e.g. the source emitting the `is_about` edge that materialized this canonical), `trigger.event` (the bus event type: `entity_created` / `entity_updated` / `edge_added` / `fill_completed` / `manual`), `trigger.timestamp` (when the originating event occurred), and `trigger.cause` (sub-event detail — the changed field name for `entity_updated`, the edge type for `edge_added`, the gap name for `fill_completed`; empty otherwise). Lets workflows write `condition: 'trigger.event == "entity_updated"'` to fire only on updates, or `via: 'trigger.source'` in a context binding to deterministically reach the firing source even when multiple sources resolve to the same canonical entity (e.g. both yaad-gmail and yaad-github emit `is_about → github-pr:X`; reading `trigger.source` picks the one that *caused this firing*, not an arbitrary in-neighbor). When the bus event predates the trigger-context surface (legacy publisher with no `caused_by_entity_id`), the engine falls back to the triggering entity itself so `trigger.source == entity`. For self-triggered events (source-plugin re-ingesting its own truth) the same self-cause shape applies by construction.
- `graph.get(id)` — fetch a **related** entity by its canonical ID (per ADR-0017: `<canonical-kind>:<slug>`). For pulling something other than the triggering entity. The id is typically an operator-stored field on the triggering entity's frontmatter (e.g., `entity.previous_edition_id`) or a graph-walked target (`edge.to`). This is the only graph lookup in v1.

  **Not-found behavior.** `graph.get(id)` for an absent entity does NOT silently return false, and does NOT fire the err-task pattern. Instead it follows the **missing-reference handling** pattern (see below): the workflow proceeds, the resulting task gets a note attached explaining the unresolved reference, and the task surfaces with that note. The operator decides whether to manually add the missing edge / ingest the missing entity, at which point the workflow re-evaluates with the now-complete context.

  This means there are three distinct failure modes the operator should keep clear:

  1. **Missing reference during context-load** (regex finds no key, frontmatter field absent before evaluation starts) — note on task, task surfaces with note, no err-task.
  2. **Missing reference during CEL condition evaluation** (`graph.get(id)` for an entity not in the graph) — same missing-reference path: note on task, task surfaces, no err-task. Not silent-false.
  3. **Systemic failure** (plugin timeout, malformed payload, store IO error) — err-task pattern (one per workflow, accumulating).

  Operators who want to guard explicitly in CEL — e.g., to make condition=false a real path the workflow author chose — use `has()`: `has(entity.previous_edition_id) && graph.get(entity.previous_edition_id).rating > 7`. The `has()` short-circuit lets them distinguish "the field is absent" (don't surface) from "the field is set but resolves to nothing" (surface as missing-ref).

**Concrete example — what `entity` vs `graph.get` look like in a predicate:**

```cel
# triggering entity IS a boardgame; check its own rating
entity.rating > 7

# triggering entity is a news article ABOUT a boardgame;
# the boardgame is on the edge target
graph.get(edge.to).rating > 7

# triggering entity is a 2nd-edition boardgame with an
# operator-stored field pointing at the 1st edition
graph.get(entity.previous_edition_id).rating > 7
```

The same workflow file might use `entity.X` and `graph.get(...)` together — `entity` for the trigger context, `graph.get` for any related entity the predicate needs.

**`graph.find` is out of v1.** The original revision draft mentioned `graph.find({predicate})` for "find me all entities matching X." The predicate shape (CEL nested? schema map? typed filter?) is non-trivial to settle and the in-flight v1 workflows don't need it (each works with a known related-entity ID stored in the triggering entity's frontmatter, retrievable via `graph.get`). Re-introduce post-v1 with a settled predicate spec.

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

**Action-level match semantics (v1):** exact-byte match of the assembled line against existing lines in the target section. No trimming, no normalization, no case-folding. Rationale: predictable + cheap; if the operator wants fuzzy match they construct it via CEL in the line template. For template-expanded lines with embedded timestamps or IDs (e.g., `"<timestamp> review pinged"`), exact-match will never fire dedup — that's the intended behavior, and the operator who wants idempotent appends should template the line without time-varying tokens (or set `if_already_present: append-anyway` to explicitly opt into duplicates).

**`if_already_present` scope:** `replace` rewrites the **matching line only**, not the entire section. The section's other lines stay in place. `append-anyway` writes a new copy regardless. `skip` is the default no-op.

**Workflow-level `update` policy semantics:** when `update` is the policy and a duplicate-key event fires, the workflow runs against the existing task — re-evaluating its action steps with the new event context, with the action-level dedup determining what actually lands. Concretely: the workflow's `task_append` steps re-run; lines that already exist in the target section get the configured `if_already_present` treatment (skip by default); new lines append. The task's frontmatter is updated with the latest event context (e.g., last-seen-at timestamp, latest priority signal). `update` does not modify task properties not touched by the workflow (operator-set priority, snooze state, notes).

## Out of v1 (explicit)

- **Webhook ingress** — no HTTP-in server today; large add.
- **Workflow-to-workflow chaining** — ergonomic but adds engine complexity.
- **LLM-involved decisions** — last v1 step at earliest, not first.
- **External direct plugins** (github / jira / calendar direct) — gmail-via-notifications covers the first-tier workflows. Direct integration is v2.
- **Emit-notification output type** — deferred unless concrete need surfaces in v1.
- **Push notifications on fill-gaps needing answer** — operator polls `/v1/needs-fill` in v1.
- **Internal time-based / cron trigger** — deferred to post-v1. External host cron + the manual-trigger CLI cover the immediate need; daemon doesn't grow a scheduler.
- **`add_edge` as a workflow action** — workflows don't directly create edges in v1. Edge creation surfaces via `plugin_dispatch` (the plugin emits edges during ingest) or via operator manual `POST /v1/edges`.
- **`silent_log` as a distinct output shape** — collapses into `add_note` (record what the workflow observed against the entity without spawning a task surface). If a true silent-no-side-effect log is needed (audit only), a workflow that does nothing on its happy path achieves that via the err-task pattern on failures only.

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
- **Action vocabulary: `task_append`, `add_note`, `plugin_dispatch`** — three primitives, not four. `add_edge` and `silent_log` move to Out-of-v1. `plugin_dispatch` is new — it's how a workflow asks the index to go fetch related context mid-evaluation.
- **File format: `.md` extension with YAML in a body code-fence.** Frontmatter holds metadata only (name, version, status). Earlier draft put rules in frontmatter; YAML code-fence is richer and lets operator-readable prose live alongside structured rules.
- **Trigger types: event-driven + manual-via-CLI in v1.** Internal time-based / cron trigger deferred post-v1. External host cron + the `yaad-index workflow trigger <name>` CLI cover the morning-brief / weekly-summary cases without the daemon growing a scheduler.
- **Dedup: two layers explicit.** Per-pattern at workflow level (key + policy `update`/`skip`/`replace`) prevents N tasks for the same situation; action-level inside `task_append` (skip-if-line-exists) prevents N duplicate lines inside the same task. Stack, don't conflict.
- **Workflow location: vault-default + daemon-reserved.** Operator-authored vault workflows are the only thing today; daemon-side is reserved as a forward-compatible path for future system-shipped workflows.

Status remains PROPOSED pending operator hard-gate review of this revision.

### 2026-05-16 — pre-review fold-in

Pre-reviewer flagged 9 specification gaps before the hard-gate read. All accepted or deferred:

- `graph.find({predicate})` predicate shape was undefined — **dropped from v1**, ship `graph.get(id)` only. Re-introduce post-v1 with a settled predicate spec.
- **Collection-shaped predicates also deferred.** ID-list → entity-list auto-resolution semantics aren't settled in v1; workflows that want multi-related-entity predicates use multiple named `context` entries instead. Re-introduce alongside `graph.find`.
- `context` stanza spec added — what it binds, when it evaluates, what `via` failure means (missing-reference path).
- `edge.from_title` / `edge.to_title` added to the edge shape spec as display-readable convenience fields populated by the engine.
- `graph.get` not-found behavior clarified — follows missing-reference path (note on task), not silent-false and not err-task.

### 2026-05-21 — entity_updated + archive_entity / restore_entity

Adds the workflow-engine shapes the github plugin's closed-item lifecycle (per ADR-0026 §6's 2026-05-21 amendment) consumes. Operator framing that drove the amendment: **"plugin emits truth, state lives in workflows + operator. plugin never participates in state management."** Archive lifecycle accordingly moves out of plugin code and becomes a workflow-engine concern.

- **New bus event `entity.updated`.** Fires when a re-fetch surfaces a `structured.data` field delta on an already-known entity. **One event per changed field** (singular `field`/`old`/`new`), not one event per re-fetch with a delta list — mirrors `entity.edge_added`'s per-edge granularity so subscribers fan out via simple per-field matching. Distinct from `entity.created` (first ingest), `entity.edge_added` (edge-only changes), and `fill.completed` (gap closures). The `entity.updated` vs `fill.completed` split preserves ADR-0008's source-of-truth boundary: gap-fills are author-tagged closures, `entity.updated` is upstream-reported truth.
- **New `match.field_changed` shape.** Trigger-side filter naming which field's delta the workflow cares about (e.g., `data.state`). Combines with the existing `canonical_kind` match (already supported by `edge_created` triggers, extended here to `entity_updated`).
- **New `archive_entity` + `restore_entity` actions.** Mirror pair wrapping ADR-0018's archive lifecycle. Both take a CEL-templated `target:` (usually `{{ entity.id }}`) and an optional `reason:` provenance string. Idempotent at the engine layer — re-firing on the same archive state is a no-op. Do NOT fire `entity.updated` themselves (archive flag is metadata, not `data.*`), so cannot self-trigger.
- **Worked example** added for the GitHub archive-on-close / restore-on-open mirror pair as two workflow files. Demonstrates the `entity_updated` trigger + `field_changed: data.state` + `canonical_kind: [github-pr, github-issue]` match + `dedup.policy: skip` (idempotent actions need no per-fire task surface).

Status remains PROPOSED pending operator hard-gate review of this revision.

### 2026-05-25 — gmail address fields + daemon-managed canonical vocabulary expansion (#272)

Extends the daemon-managed canonical-kind / edge-type set (the precedent set by `day` in ADR-0025 and `task` in #268) to cover the gmail-emitted vocabulary so workflow authors targeting gmail sources don't have to enable a long list in `canonical_kinds:` / `canonical_edge_types:` config.

- **Three new daemon-managed kinds**: `email` (per-message anchor — the `is_about` target of a gmail source), `email-address` (per-address entity — the `from`/`to`/`cc`/`bcc` target), `label` (per-Gmail-label entity — the `tagged_as` target). All slug shapes preserved from the existing gmail-side definitions — no rename, no operator-facing breaking change.
- **Five new daemon-managed edge types**: `from` / `to` / `cc` / `bcc` (gmail source → `email-address`) and `tagged_as` (gmail source → `label`). `is_about` stays plugin-declared (it's cross-plugin — bgg, wikipedia, gmail all emit it; daemon-managing it requires coordinated removal from each plugin's `canonical_edge_types_emitted` list and is left as a follow-up).
- **Lazy materialization on POST /v1/edges** generalized to thin-label kinds: the day-target ensure landed in #268 was specific to `day:YYYY-MM-DD`. The same path now lazy-ensures any thin-label daemon-managed target (`day:` / `email:` / `email-address:` / `label:`) via `canonical.EnsureLabelRow` so manual edge creation against those targets holds the FK without the caller pre-creating the entity. `task:` is deliberately excluded — task rows index `<vault>/tasks/*.md` files per §Task and only materialize on first-create (no automatic backfill); a manual edge to an unknown `task:<slug>` correctly returns 422 missing_entity rather than landing a phantom store row with no backing vault file.
- **Structured `data` fields**: the gmail plugin now surfaces `from` (string), `to` (`[]string`, possibly empty), `cc` (`[]string`, possibly empty), and `bcc` (`[]string`, omitted when empty since BCC is sent-folder-only) on the source entity's `data` map. Workflow CEL predicates can read `entity.data.from == "noreply@example.com"` directly without regexing `clean_content`.

Status remains PROPOSED pending operator hard-gate review of this revision.

### 2026-05-25 — task entity promotion (#268)

Aligns the implementation with the §Task spec text. Pre-#268 tasks lived as markdown files under `<vault>/tasks/` without a backing entity row; `/v1/entities/task:<slug>` 404'd, `set_property` couldn't target a task id, and graph walks couldn't reach tasks.

- **New daemon-managed kind**: `task` (joins `day` per ADR-0025 in `DaemonEntityKinds()` — operator doesn't enable in `canonical_kinds:` config).
- **New daemon-managed edge type**: `triggered_by` from task → triggering source entity. Joins the day-anchored vocabulary in `DaemonEdgeTypes()`.
- **Path routing**: `vault.KindDir(kind="task")` maps to the `tasks/` directory so the canonical `<root>/<kind>/<slug>.md` resolver finds the on-disk file at the operator-facing `<root>/tasks/<slug>.md` location. Symmetric across reader / writer / archive / destroy sites.
- **Spawn-side materialization**: `FileTaskWriter.AppendTaskSection` first-create and `FileErrTaskWriter.AppendErrTask` first-create both upsert the `task:<slug>` row + (for normal tasks with a triggering entityID) emit a `triggered_by` edge. Idempotent on subsequent appends; the row stays put so operator / workflow set_property mutations survive.
- **No automatic migration**: pre-#268 task files (already on disk before this revision lands) stay file-only — the store row + edge surface fills in for newly-spawned tasks only. The historical set remains readable via `/v1/tasks`; operators wanting the full entity surface on a pre-existing task recreate it.
- **Workflow surface**: `set_property` targeting a task id now works through the existing `VaultPropertyWriter` (kind-aware vault path resolution). The unchanged `/v1/tasks` API + MCP tools (`task_list` / `task_load` / `task_resolve`) remain operational — the store row is the index over the file, not a replacement.

Day-side (#268 day fold): the day-anchored materialization paths already in the codebase (ingest `EmitDayRefs`, fill `EmitDayRefs`, workflow `set_property` `EmitDayRefs`, `add_canonical_edge` `EnsureLabelRow`, reindex `EmitDayRefs`, canonical_type ops `ensureCanonicalLabelRow`) cover their respective edge-write sites. The one gap closed in this revision: `POST /v1/edges` with a `day:YYYY-MM-DD` target now lazy-ensures the day entity row before `CreateEdge` so manual edge-creation matches the lazy-on-write pattern. The `isRegisteredEdgeKind` validator also folds in `DaemonEdgeTypes()` so operators can `POST /v1/edges` with `references_day` / `triggered_by` / etc. without registering a plugin that advertises them.

Status remains PROPOSED pending operator hard-gate review of this revision.

### 2026-05-25 — `trigger.*` CEL surface (#264)

Adds the per-firing trigger context to the CEL expression environment so workflows can read what caused the firing — `trigger.source`, `trigger.event`, `trigger.timestamp`, `trigger.cause`. Driven by the multi-source disambiguation case in #264: when both yaad-gmail and yaad-github emit `is_about → github-pr:X`, a workflow firing on `canonical_kind: [github-pr]` reading the source via `graph.in_neighbors(entity.id, "is_about").items[0]` picks an arbitrary in-neighbor — there's no way to know which source initiated *this firing*. The previous workaround was author-fragile and order-dependent.

- **Wire shape.** `entity.created` / `entity.updated` / `entity.edge_added` envelopes gain a `caused_by_entity_id` field naming the entity whose action drove the event. Publishers stamp the cause on emit:
  - Source-plugin self-ingest → `caused_by = id` (self).
  - Canonical thin-row materialized from another entity's `is_about` edge → `caused_by = e.From` (the source emitting the edge).
  - Manual `POST /v1/edges` → `caused_by = e.From` (the edge-tail-is-cause convention).
  - Workflow `add_canonical_edge` action → `caused_by = sourceID` (the entity the workflow operated on).
  - Operator/agent fill landing a canonical-type value → `caused_by = sourceID` (the entity being filled).
- **Engine binding.** At fire-time the engine resolves `caused_by_entity_id` via the same entity resolver as `entity`, then exposes the resolved map at `trigger.source`. `trigger.event` and `trigger.timestamp` come straight from the envelope. `trigger.cause` carries the sub-event detail when available (field name for `entity_updated`, edge type for `edge_added`, gap name for `fill_completed`).
- **Legacy publishers.** Events arriving with an empty `caused_by_entity_id` fall back to the triggering entity itself, so `trigger.source == entity` for any publisher not yet migrated. Workflows referencing `trigger.*` stay functional; the multi-source disambiguation case is the one that requires explicit cause-stamping to work correctly.
- **Manual triggers.** `workflow.trigger(input)` synthesizes a manual-shape trigger context: `trigger.event == "manual"`, `trigger.source` resolves to the target entity (self-cause). Workflows that need to branch on event-driven vs manual firings condition on `trigger.event`.
- **Reserved name.** `trigger` joins `entity` and `edge` as a reserved CEL variable — declaring a context binding named `trigger` is rejected at workflow-load time.

Status remains PROPOSED pending operator hard-gate review of this revision.

### 2026-05-16 — third-round pre-review fold-in + add_gap

- `edge.target` → `edge.to` in graph.get prose + worked CEL example (consistency with the edge shape spec).
- Pre-existing workflow-chaining out-of-v1 line cleaned of meeting-reference framing (operator hard-gate requirement before PROPOSED → ACCEPTED).
- **`add_gap` action primitive added** (operator-requested). Bridges decision-needing-human-input cases into the workflow framework without an LLM step. Constraints: permanent per ADR-0008, scoped to a single unified `addable_gaps` declaration covering BOTH trigger-time injection and action-stage adds. The intended add_gap-loop (workflow asks → human fills → workflow decides on the answer) is design, not a self-loop; the source-tag self-loop detection doesn't cover it (fills are sourced from operator/agent, not the workflow), and the engine-backstop re-evaluation counter is the right protection layer.
- Decision rule between `task_append`/`add_note`/`add_gap` extended to cover all three primitives.
- `plugin_dispatch` async/sync semantics were contradictory — clarified as **synchronous bounded-await** (default timeout 30s; err-task on timeout/error).
- `task_append` skip-if-line-exists match semantics — specified as **exact-byte match, no normalization**; dynamic content needs explicit `if_already_present: append-anyway`.
- `if_already_present: replace` scope — clarified as **matching line only**, not the entire section.
- `edge` context for manual triggers — specified as **nil/absent for manual**, CEL must guard with `has(edge)`.
- `update` dedup policy semantics — concretely specified: workflow re-runs against existing task; action-level dedup handles per-line behavior; task frontmatter updates with latest event context; operator-set properties stay untouched.
- `add_note` vs `task_append` decision rule — added one-sentence heuristic.
- Manual trigger input disambiguation — cross-referenced to the existing `workflow.trigger(input) input semantics` section already in the ADR.
- YAML code-fence schema — added minimal worked example (boardgame-news workflow) to the workflow location section.

### 2026-05-28 — amendment: worked-example `workflow` var is aspirational

Three pre-implementation passages in this ADR (the `dedup.key` lines at `:77` + `:119`, and the §"Dedup default" prose at `:334`) reference a `workflow` CEL variable as if the engine bound the workflow's name into the expression environment — e.g. `key: 'workflow + entity.id'`, "the default key is `workflow + entity_id`". The implementation never wired this binding; the runtime CEL env exposes `entity`, `edge`, `trigger`, and each `context[].name` binding only (per `internal/workflow/decision/decision.go`'s `buildEnv`). A workflow-author writing `key: 'workflow + entity.id'` rejects at workflow-load time with `undeclared reference to 'workflow'`.

The intent in the ADR's worked-example phrasing was illustrative — naming the per-workflow-scoping concept rather than pinning a specific CEL identifier. Implementations consistent with the framing: compose a per-workflow prefix as a literal string in the key (`key: '"boardgame-news:" + entity.id'`) when cross-workflow uniqueness matters. The single-workflow case can use `entity.id` directly — `dedup` is already scoped per-workflow at the engine layer; the key only needs to disambiguate within a single workflow's fires.

The ADR text is preserved verbatim above for historical accuracy. For live behavior + the recommended dedup-key pattern, see [`docs/workflows.md`](../docs/workflows.md) §3.1 (variables list) and §6 (dedup-key composition).
