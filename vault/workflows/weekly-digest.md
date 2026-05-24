---
name: weekly-digest
version: 1
status: enabled
---

Manual-trigger workflow per ADR-0027 §2a. Walks each of this
week's seven day-anchors via `days_in_week(this_week())`, collects
all `occurred_on` neighbors across the week via `.flatten()`, and
files them under a single weekly task section.

Exercises the period helpers (cut 2) + the `ext.Lists()` `.flatten()`
extension (cut 3 wiring). Triggered via
`yaad-index workflow trigger weekly-digest`.

```yaml
trigger:
  type: manual

actions:
  - task_append:
      section: this-week
      content: '{{ days_in_week(this_week()).map(d, graph.in_neighbors(d, "occurred_on").items).flatten().map(n, "- [[" + n.id + "]]").join("\n") }}'
      if_already_present: replace
```
