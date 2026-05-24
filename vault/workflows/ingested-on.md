---
name: ingested-on
version: 1
status: enabled
---

Stamps every newly-created entity with an `ingested_on` edge to
today's `day:YYYY-MM-DD` entity. The canonical replacement for
the deferred always-on daemon stamp per ADR-0025 §
"ingested_on auto-tag — deferred" + ADR-0027 cut 1.

The action runner's kind-prefix strip (ADR-0027 cut 1) removes the
leading `day:` from `target.name: today()` before slugifying so
the target id resolves to the canonical form (not the doubled-
prefix `day:day-2026-11-11`).

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
