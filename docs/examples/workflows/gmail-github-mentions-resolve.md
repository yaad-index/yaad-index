---
name: gmail-github-mentions-resolve
version: 1
status: active
---

# Auto-resolve github-mention tasks when the linked PR closes or merges (#386)

When a GitHub PR/issue reaches a terminal state, the line tracking it in the
`gmail-github-mentions` task no longer carries actionable signal — it's pure
noise in `task_list`. This workflow resolves that line automatically, so
closed/merged PRs stop producing standing tasks.

It's built entirely from existing primitives — no new daemon code:

- **Trigger.** yaad-github re-fetches recently-closed items on each sync (its
  `is:closed updated:>=N-days` window). On re-ingest the daemon emits one
  `entity.updated` per changed field, so a now-terminal PR fires
  `entity.updated` with `field_changed: data.state`. The event carries the
  **source** entity's kind — `github` (the ADR-0021 source-namespace kind,
  `github:<slug>`), *not* the `github-pr`/`github-issue` canonical edge target
  (those only get `entity.created`). So the trigger filters `canonical_kind:
  [github]`; the PR/issue state + url + number all live on that source entity's
  `data`.
- **Condition.** A merged PR is also `closed`, so `entity.data.state == "closed"`
  covers both close and merge.
- **Action.** The existing `task_resolve` action checks (or removes) a line in
  another workflow's task file, matched by a CEL-rendered `match_key`.

**Adapt to your task shape.** The `match_key` below renders the PR's
`owner/repo#N` reference and matches a task line whose content *begins* with it
(`resolveTaskLineInBody` does a prefix match). Point `subject` / `section` at
your real `gmail-github-mentions` task file + section, and shape `match_key` to
however your task lines lead. `mode: check` flips `- [ ]` → `- [x]`; use
`mode: remove` to drop the line instead.

```yaml
allowed_plugins:
  - yaad-github

trigger:
  type: entity_updated
  match:
    field_changed: data.state
    # The source entity's kind (ADR-0021 source namespace), NOT the
    # github-pr / github-issue canonical edge target. entity.updated on
    # state fires for the source node (github:<slug>); the canonical
    # target only fires entity.created.
    canonical_kind: [github]

condition: 'entity.data.state == "closed"'

# The workflow's own per-fire subject (used for its dedup identity). This
# workflow only resolves another workflow's task, so the value is incidental.
subject: '{{ entity.id }}'

actions:
  - task_resolve:
      workflow: gmail-github-mentions
      subject: '"pending"'
      section: Open mentions
      match_key: 'regex_capture(entity.data.url, "github\\.com/([^/]+/[^/]+)/", 1) + "#" + string(entity.data.number)'
      mode: check
```
