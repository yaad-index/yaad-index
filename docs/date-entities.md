# Date entities

Day entities (canonical kind `day`, slug shape `day:<YYYY-MM-DD>`) are first-class anchors per [ADR-0025](../adr/0025-date-entities.md). Any entity that references a day in its frontmatter — a task with a deadline, an event with a date, a message with a timestamp — produces a canonical edge to a `day` entity the daemon creates on demand. Workflows aggregate by date via graph walks instead of scanning every entity's frontmatter for date strings.

This doc covers the operator + agent-facing surface: what gets created when, what edge types land, how to mark a day as a journal entry, and two worked examples.

## What's a day entity

A `day` entity is a daemon-managed canonical kind. Operators don't enable it via `canonical_kinds:` — it's always available. The slug is the calendar date in the daemon's configured timezone:

```yaml
# vault/day/2026-11-11.md
---
id: day:2026-11-11
kind: day
plugin: yaad-index
---
```

The body is optional. Day entities are mostly anchors — the value comes from inbound edges from time-bound entities. See [§ Journal entries](#journal-entries) for the case where the operator wants to write reflections directly into the day's body.

The `<YYYY-MM-DD>` portion is interpreted in the daemon's configured timezone — see the `timezone:` knob in [`docs/configs.md`](./configs.md). Empty / unset config falls back to the host's `time.Local` (distinct from the broader display fallback to UTC — day resolution gets the operator's wall clock).

## When day entities get created

Day entities are created lazily on first reference. The daemon's shape-scan walks every entity's frontmatter on every write path:

- **Ingest** — a plugin emits an entity with `data: { deadline: "day:2026-11-11" }` in its frontmatter; the daemon ensures `day:2026-11-11` exists + emits an edge.
- **Fill** — an agent (or operator) fill response writes a `day:`-shaped value into a field; same scan, same emit.
- **Workflow `set_property`** — a workflow action writes a day-shaped value; same.
- **Operator vault edits** — reindex picks up new day references on the next walk.

Upsert is idempotent. Re-emitting the same `(source, edge_type, day)` tuple is a no-op at the SQL layer.

The eager-create rule means `graph.get(day:2026-11-11)` always resolves once anything has referenced that day. There's no "lazy materialize on query" mode — dangling references would break the graph-walk contract.

## Canonical edge vocabulary

Five edge types are reserved at the daemon level per ADR-0025 § Edge types:

| Edge type | Source side | Target | Use |
|---|---|---|---|
| `due_on` | task / deadline entity | `day:<...>` | "this task is due on this day" |
| `occurred_on` | event / meeting / shipment | `day:<...>` | "this happened or will happen on this day" |
| `is_about_day` | newsletter / digest / journal entry | `day:<...>` | "this content describes this day" |
| `references_day` | any entity (daemon-emitted fallback) | `day:<...>` | "this entity refers to this day for some unspecified reason" |
| `ingested_on` | any entity | `day:<...>` | "this entity was first received on this day" — reserved for an operator-wired workflow; the daemon itself never emits it in v1.x. |

`references_day` is the **baseline**. When a plugin doesn't declare a per-field semantic edge type (see [§ Plugin `date_fields`](#plugin-date_fields)), every `day:`-shaped reference produces a `references_day` edge.

Plugins MAY use any of the canonical names OR define their own — see [`docs/plugin-flow.md`](./plugin-flow.md) § 3 for the `date_fields` Capabilities block. The canonical set is a portability hint for cross-plugin workflows ("all entities `due_on` next Friday" works regardless of which plugin emitted them); plugins can emit custom names for domain-specific vocabulary.

## Plugin `date_fields`

Plugins declare per-field semantic edge overrides in their `--init` Capabilities:

```json
{
  "name": "calendar",
  "date_fields": {
    "start_date": "occurred_on",
    "due_date": "due_on",
    "subject_day": "is_about_day"
  }
}
```

When a `data.<field>` value matches the `day:YYYY-MM-DD` shape AND the field name is declared in `date_fields`, the daemon emits the declared edge type INSTEAD OF the baseline `references_day`. No double-edge — declared field → declared only; undeclared field → baseline only.

Fields not in `date_fields` still flow through the shape-scan and get the baseline `references_day`. Plugin authors only opt in for fields where the richer typing matters.

## Journal entries

A `day` entity's body MAY hold operator notes — a daily reflection, recap, or hand-written observation. The body is optional; days carrying meaningful journal content are marked with `is_journal: true` in `data:`:

```yaml
---
id: day:2026-11-11
kind: day
plugin: yaad-index
data:
  is_journal: true
---

Started the week with a strong run. Brass: Birmingham hit the table —
five players, 90 minutes, won by industrialist on the canal phase.
```

Tools that want "show me my journal entries" filter on the flag; tools that want every day entity (or every day with any inbound edge) don't filter:

```bash
# Every day entity (anchors + journals)
curl "http://localhost:7433/v1/search?kind=day" -H "Authorization: Bearer $TOKEN"

# Only operator-marked journal entries
curl "http://localhost:7433/v1/search?kind=day&is_journal=true" -H "Authorization: Bearer $TOKEN"
```

The same filter rides on the MCP `list_entities` tool via an optional `is_journal: true` argument — agents can scope their day-related queries to the journal subset without paging through anchor-only entries.

The flag is operator-set. The daemon doesn't auto-detect journal content; the operator marks days they care about. Empty body + `is_journal: false` (or absent) is the default — most days are anchors only.

## Worked example: a plugin promotes day references into typed edges

A plugin emits entities whose frontmatter carries day-shaped values. By declaring `date_fields` in its `--init` Capabilities, the plugin tells the daemon which fields earn a typed canonical edge type instead of the baseline.

Plugin `--init` output:

```json
{
  "name": "calendar",
  "version": "0.1.0",
  "url_patterns": ["^calendar:.+"],
  "entity_kinds": [{"name": "meeting"}],
  "edge_kinds": [],
  "canonical_kinds_emitted": [],
  "canonical_edge_types_emitted": ["occurred_on", "due_on"],
  "source_namespace": "calendar",
  "date_fields": {
    "scheduled_for": "occurred_on",
    "rsvp_by": "due_on"
  }
}
```

When the plugin emits an entity per fetch:

```json
{
  "structured": {
    "kind": "source",
    "name": "Weekly team sync 2026-11-11",
    "data": {
      "scheduled_for": "day:2026-11-11",
      "rsvp_by": "day:2026-11-10",
      "topic": "release planning"
    }
  }
}
```

The daemon's shape-scan walks the frontmatter on commit and finds two `day:`-shaped values:
- `scheduled_for` is declared in `date_fields` → emit `occurred_on` edge to `day:2026-11-11` (NOT `references_day`).
- `rsvp_by` is declared in `date_fields` → emit `due_on` edge to `day:2026-11-10`.
- `topic` is not day-shaped → skipped.

Both day entities are auto-materialized as thin canonical rows (no vault file unless the operator hand-edits one) so the edge FK is satisfied. Subsequent queries / workflows / `get_entity_with_context` walks see the typed edges directly.

If the plugin removed the `date_fields` block, both fields would still flow through the shape-scan and produce `references_day` edges — the canonical semantic is just less specific. Plugins opt in for richer typing only when downstream consumers benefit from it.

## Worked example: agent-side "what's anchored on a given day" query

The day-entity model is graph-walk-shaped — once an edge exists from `X` to `day:2026-11-11`, an agent / operator can ask "what's on November 11?" via `/v1/entities/{id}/context`:

```bash
TOKEN="$(cat op.jwt)"
DAY="day:$(date +%Y-%m-%d)"

# Every entity with an edge to today's day, walking one hop.
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:7433/v1/entities/$DAY/context?depth=1"

# Just the entities tagged with the canonical `due_on` edge.
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:7433/v1/entities/$DAY/context?depth=1&edge_types=due_on"

# Same shape, accessible from an agent via MCP — get_entity_with_context
# returns the root day + neighbors keyed by edge type.
```

The MCP `get_entity_with_context` tool wraps the same endpoint; agents call it with `id: day:<YYYY-MM-DD>` and walk the result's `neighbors` array.

## Worked example: daily-digest workflow (post-ADR-0027)

ADR-0027 added the CEL temporal + graph-walk primitives that make daily-digest a first-class workflow shape. The example below lives at `vault/workflows/daily-digest.md`:

```yaml
trigger:
  type: manual
actions:
  - task_append:
      section: today
      content: '{{ graph.in_neighbors(today(), "due_on").items.map(n, "- [[" + n.id + "]] (due)").join("\n") }}{{ graph.in_neighbors(today(), "occurred_on").items.map(n, "\n- [[" + n.id + "]] (occurred)").join("") }}'
      if_already_present: replace
```

Triggered via `yaad-index workflow trigger daily-digest`. The workflow targets `today()` (in the daemon's TZ), walks each canonical edge type relevant to "today's anchors," concatenates the lines into a task section.

Weekly fan-out follows the same shape via the §2a period helpers:

```yaml
- task_append:
    section: this-week
    content: '{{ days_in_week(this_week()).map(d, graph.in_neighbors(d, "occurred_on").items).flatten().map(n, "- [[" + n.id + "]]").join("\n") }}'
```

See `vault/workflows/weekly-digest.md` for the full file.

## Worked example: ingested-on workflow

The `ingested_on` edge type is reserved in the canonical vocabulary; the daemon never emits it automatically per ADR-0025 § "ingested_on auto-tag — deferred". Operators who want "show me everything ingested today" wire an `entity_created` workflow that emits the edge explicitly. Lives at `vault/workflows/ingested-on.md`:

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

Fires on every new entity. `add_canonical_edge`'s `target.name` accepts the CEL expression `today()` which evaluates to `day:YYYY-MM-DD`; the action runner's kind-prefix strip (ADR-0027 cut 1) removes the leading `day:` before slugifying so the target id resolves to the canonical `day:2026-11-11` shape. The runner also ensures the target day entity exists.

To opt out, remove the workflow file — the existing edges remain (harmless alongside other inbound day-edges) but stop growing. The four shipped example workflows live alongside this doc as copy-paste-adapt starting points; the agent-side query shapes (curl / MCP `get_entity_with_context`) above remain valid for ad-hoc "what's on this day?" inspection outside a fired workflow.

## References

- [ADR-0025](../adr/0025-date-entities.md) — Date entities (design + decisions)
- [`docs/configs.md`](./configs.md) § `timezone:` — TZ resolution chain
- [`docs/plugin-flow.md`](./plugin-flow.md) § 3 — `date_fields` Capabilities block
- [`docs/workflows.md`](./workflows.md) — workflow event / action vocabulary
- [`mcp/SKILL.md`](../mcp/SKILL.md) — the `list_entities` MCP tool surface (incl. `is_journal` filter)
