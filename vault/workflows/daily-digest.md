---
name: daily-digest
version: 1
status: active
---

Manual-trigger workflow per ADR-0027. Walks today's `due_on` and
`occurred_on` neighbors and files a daily-digest task section.
Triggered via `yaad-index workflow trigger daily-digest`.

```yaml
trigger:
  type: manual

actions:
  - task_append:
      section: today
      content: '{{ graph.in_neighbors(today(), "due_on").items.map(n, "- [[" + n.id + "]] (due)").join("\n") }}{{ graph.in_neighbors(today(), "occurred_on").items.map(n, "\n- [[" + n.id + "]] (occurred)").join("") }}'
      if_already_present: replace
```
