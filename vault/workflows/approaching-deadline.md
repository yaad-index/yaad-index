---
name: approaching-deadline
version: 1
status: enabled
---

Fires when any `due_on` edge is created. Files a task line with
the relative day-count computed via `days_between(today(), edge.to)`
from ADR-0027 cut 2. Positive count = days until the deadline;
zero = today; negative = past-due.

```yaml
trigger:
  type: edge_created
  match:
    edge_type: due_on

actions:
  - task_append:
      section: deadlines
      content: '- [[{{ edge.from }}]] due in {{ days_between(today(), edge.to) }} days'
```
