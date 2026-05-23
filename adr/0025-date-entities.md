# ADR-0025: Date entities

**Status:** ACCEPTED
**Date:** 2026-05-16 (drafted); 2026-05-23 (accepted with Q2–Q6 resolutions below)

## Context

yaad-index has no first-class concept of dates. When operators or plugins refer to a calendar moment — a task deadline, an event date, the day an email arrived, when a meeting is scheduled — the reference is an arbitrary string in some entity's frontmatter. Two consequences:

1. **No way to aggregate by date.** "Show me everything happening on 2026-05-18" requires every entity to be scanned and date-strings parsed in a consistent format. There's no single node to walk to.
2. **No anchor for daily/weekly/monthly digests.** A "daily summary" workflow has nothing to attach edges to as it accumulates "today's deliveries / today's PRs / today's meetings."

The proposal: **dates become first-class entities.** A reference to a date in any entity's frontmatter becomes a canonical-ID reference (e.g., `deadline: day:2026-11-11`), the date entity is auto-created on first reference, and edges connect time-bound entities to their date(s). Workflows (per ADR-0024) and plugins can attach to date entities the same way they attach to any other entity.

This is a generic infrastructure concern, not a workflow-specific one — workflows BENEFIT from it (daily-digest becomes "query the day's edges" rather than "scan all entities by date string"), but the entity-kind concern stands on its own.

## Decision

### Entity kinds

Add date entity kinds, default-enabled at the daemon level (no plugin gates them on).

**Granularity — v1.x: day only.** First cut ships **`day:<YYYY-MM-DD>`** (e.g., `day:2026-11-11`). Week / month / year are deferred to a later layer once day-aggregation patterns are established.

When week/month/year arrive later, they'll be separate kinds connected to days via some hierarchical edge (`belongs_to`, `contains`, or whatever shape lands at that point). Operators who need week-level digests in the interim can build them from day edges in workflow CEL (multi-day aggregation via repeated `graph.get(day:YYYY-MM-DD)` calls, or a future `graph.find` predicate over a date range).

**Timezone.** The canonical ID `day:<YYYY-MM-DD>` names that day **in the daemon's configured timezone**. Resolution chain:

1. Operator config: `timezone:` knob (IANA name, e.g. `Europe/Berlin`, `UTC`, `America/Los_Angeles`) under the top-level config block.
2. Host system TZ as fallback when the operator config doesn't set the knob (Go's `time.Local`).

The canonical-ID string is plain `day:YYYY-MM-DD` — no zone embedded in the ID itself. The TZ-implicit form keeps IDs short and ergonomic for hand-edited references; the daemon resolves what "today" / "deadline" means against the configured zone at every read/write. Operators running the daemon across multiple TZs in the same vault should pick one — the canonical ID is per-deployment.

A `day:2026-11-11` reference in entity A and the same reference in entity B both resolve to the same node — they're textual matches on the canonical ID. The daemon doesn't store the originating zone with the reference; the operator's config zone IS the zone.

### Auto-creation: reference-driven

Day entities are created when something references them. Two complementary detection paths run on every ingest, fill, workflow action, and operator edit:

**Daemon baseline — shape-scan.** The daemon walks every entity's frontmatter on write and emits a `references_day` canonical edge from the entity to each `day:YYYY-MM-DD`-shaped canonical-ID value it finds. This runs for every plugin without per-plugin configuration. It catches operator hand-edits, generic plugin output, and ad-hoc references that the plugin author never explicitly typed.

**Plugin override — `date_fields`.** Plugins MAY declare richer typing in their `--init` capabilities via a `date_fields` block:

```json
{
  "name": "calendar",
  "date_fields": {
    "start": "occurred_on",
    "due": "due_on",
    "subject_day": "is_about_day"
  }
}
```

The keys are frontmatter field names; the values are the canonical edge type to emit instead of the baseline `references_day`. When a plugin declares a field in `date_fields`, that field's day reference produces the declared semantic edge type; the baseline `references_day` is NOT emitted for that field (to avoid double-edges).

A field NOT listed in `date_fields` still flows through the daemon shape-scan and gets the baseline `references_day` edge. Plugin authors only opt in for fields where the richer typing matters; the long tail keeps working without ceremony.

### Auto-creation: reference creation timing

**Eager / any-write-time.** The daemon ensures the target day entity exists on every write path that introduces a day reference:

- **Ingest** (`POST /v1/ingest`) — when a plugin emits frontmatter containing day references.
- **Fill** (`POST /v1/entities/{id}/fill`) — when an agent-fill response adds or updates a field carrying a day reference.
- **Workflow actions** — `add_canonical_edge` targeting a day entity, `set_property` writing a day-shaped field.
- **Operator vault edits** — reindex picks up new day references on the next walk and ensures the target day entities exist.

Upsert is idempotent: if `day:2026-11-11` already exists, the daemon reuses the existing entity; if not, it creates one. No dangling references at write commit — the edge target always resolves.

The eager rule applies symmetrically across all four write paths. The previous DRAFT considered lazy-on-query creation; rejected because dangling references break the graph-walk contract (`graph.get(day:2026-11-11)` should not 404 just because nothing has materialized the entity yet).

### `ingested_on` auto-tag — deferred

The DRAFT proposed an `ingested_on` edge automatically stamped on every newly-ingested entity, pointing at the day the ingest happened. This is **not shipping in v1.x**.

Rationale: the always-on stamp is the most opinionated possible default — every entity gets the edge whether anyone uses it or not, and the temporal-aggregation use case is better served by an explicit operator opt-in. Operators who want "show me everything ingested today" wire a workflow:

```yaml
match:
  event: entity.created
actions:
  - add_canonical_edge:
      source: "{{event.entity_id}}"
      edge_type: "ingested_on"
      target: "day:{{today()}}"
```

The `ingested_on` edge type remains in the canonical vocabulary (see "Edge types" below) so when the workflow above lands an edge, it carries the same semantic name an always-on stamp would have used. Future ADR may revisit the auto-stamp if the operator pattern proves universal enough to bake in.

### Edge types

The daemon defines a **canonical baseline set** of edge types for time-bound relationships. Plugins MAY use these for portability, or define their own custom edge types in `date_fields` for richer domain-specific vocabulary. Workflow authors get the canonical vocab for cross-plugin queries (e.g., "all entities `due_on` next Friday" works regardless of which plugin emitted them).

**Canonical baseline:**

- `due_on` — task / deadline entity → day entity (the day it's due)
- `occurred_on` — event / meeting / shipment → day entity (when it happened / will happen)
- `is_about_day` — newsletter / digest / journal entry → day entity (the day it describes)
- `references_day` — fallback baseline emitted by the daemon shape-scan for any `day:`-shaped reference whose field isn't declared in a plugin's `date_fields`. Generic / semantically-unspecific.
- `ingested_on` — entity → day entity. Reserved for the operator-wired workflow above; the daemon itself never emits it in v1.x.

**Plugin extensibility:** plugins are NOT restricted to the canonical set in their `date_fields` mappings. A calendar plugin can emit `attended_on`; a project-management plugin can emit `closed_on` or `merged_on`. The canonical set is a portability hint for cross-plugin workflows, not a constraint. Workflow authors mixing custom and canonical edge types pay the cost in workflow CEL (they have to know what each plugin emits).

### Date entity content

A day entity is mostly an anchor — its value comes from its inbound edges. The body is optional:

- **Body MAY hold operator notes** (journal content) — the operator can hand-write reflections, notes, or daily-recap text into `vault/day/2026-11-11.md` body the same way they edit any other entity's body.
- **Body MAY stay empty** — for days whose value is purely structural (every day with any inbound edge gets an auto-created entity; most of them are empty anchors).

Days carrying meaningful journal content are marked with `is_journal: true` in the day entity's frontmatter. Tools (workflows, queries, future UI) that want "show me my journal entries" filter on `is_journal: true`; tools that want "show me every day with any inbound edge" don't filter.

**Migration of pre-existing daily-note conventions.** Operators with `daily/YYYY-MM-DD.md` files in their vault (a common Obsidian / Logseq pattern) can map them to day entities:

- Move the file to `day/YYYY-MM-DD.md` (vault path matches canonical ID slug).
- Add frontmatter: `kind: day` + `id: day:YYYY-MM-DD` + `is_journal: true`.
- Body content survives unchanged.

The migration is **operator-driven**; the daemon does NOT auto-migrate existing `daily/` directories. Rationale: not every operator's `daily/` content is journal-shaped, and the operator should pick which days are journal entries vs which are abandoned scratchpads. A one-shot operator migration script (out of v1.x scope) may make this easier.

### Default-enabled

Date entities are part of the daemon's core kind set. Operators don't enable them via plugin config — they're always available. This is unusual (most entity kinds come from plugins) and intentional: dates are universal, not domain-specific.

## Out of scope (initial)

- **Time-of-day entities** (hour, minute). Out of v1.x; revisit if a use case surfaces. Most operator workflows are day-level.
- **Date arithmetic in CEL** (e.g., `day:2026-11-11 - day:2026-11-04 == 7`). Out unless workflows demand it.
- **Localized week-numbering schemes** (US vs ISO). Use ISO week (`<YYYY>-W<WW>`); operator-config knob for the schedule-of-week is post-v1.x.
- **Auto-tagging existing entities** with `ingested_on` edges at migration time. Per the workflow-driven decision above, `ingested_on` is never daemon-emitted in v1.x; backfill is a separate one-shot operator script if wanted.
- **Auto-migration of `daily/YYYY-MM-DD.md` legacy files** to day entities. Operator-driven per the Date-entity-content section.

## Workflow integration (downstream of ADR-0024)

Once date entities exist, ADR-0024 workflows benefit naturally:

- A **daily-digest workflow** triggered manually (or by external cron via `yaad-index workflow trigger daily-digest`) targets today's day entity, walks its incoming edges, formats a task or report.
- A **deadline-attached-task workflow** subscribes to `entity.edge_added` for `due_on` edges; when a deadline edge is created, the workflow surfaces a task referencing the deadline-bearing entity. (Note: this fires when the edge is added, not when the date approaches. "Workflow fires when a date is N days away" requires date arithmetic in CEL, which is explicitly out of v1.x scope. Approaching-deadline behavior would either need that v1.x extension or an external scheduled trigger.)
- A **shipping-day workflow** (the operator's worked example) attaches new shipping-related edges to the day entity as they arrive.
- An **`ingested_on` workflow** (per the deferred-auto-tag decision above) reproduces the always-on stamp for operators who want temporal aggregation of new entities, on an opt-in basis.

No ADR-0024 changes required to support date entities — they're just another entity kind to the workflow engine. This ADR adds the entity kinds + auto-creation + canonical edges; ADR-0024 already covers how workflows act on them.

## Consequences

**Positive:**
- Aggregation by date becomes a graph walk, not a scan.
- Daily / weekly digests have a natural anchor.
- Operator can hand-write daily notes against the day entity (with `is_journal: true` for tool-side filtering).
- Workflows that care about time become composable with the rest of the entity model.
- Daemon shape-scan covers every plugin transparently; plugins only opt in for richer typing.

**Negative:**
- New entity kind increases the surface of the kind catalog (one `day` kind in v1.x; week/month/year add to it later).
- Auto-creation adds an ingest-time / fill-time / workflow-action-time side effect that needs to be idempotent and cheap.
- Operators with pre-existing daily-note conventions need an operator-driven migration path.
- Daemon shape-scan walks every frontmatter on every write; cost is bounded (frontmatter is small) but non-zero.

**Migration:**
- v1.x reference-creation is forward-only across all write paths. Backfill of existing entities is a separate one-shot if wanted.
- Existing `daily/YYYY-MM-DD.md` files don't auto-convert; operator moves them to `day/YYYY-MM-DD.md` + frontmatter per the Date-entity-content section.
- **Plain-date-string fields.** Existing entities with frontmatter like `deadline: 2026-11-11` (raw string, not `day:2026-11-11` canonical reference) won't auto-convert — the daemon shape-scan looks for the `day:` prefix, not bare date strings. A migration script could rewrite them; out of scope here. Workflows / queries that want to reach day entities from plain-string fields will need either the migration or a forward-compat shim that accepts both forms.

## References

- ADR-0002: Edge model.
- ADR-0008: Vault-as-truth.
- ADR-0017: Daemon-owned canonical slugs.
- ADR-0024: Workflows and tasks (the downstream beneficiary).
