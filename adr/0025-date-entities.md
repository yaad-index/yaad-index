# ADR-0025: Date entities

**Status:** DRAFT
**Date:** 2026-05-16

> This is a placeholder skeleton. Many sections are stubs with open questions called out. The ADR is opened now to capture the idea and let the design refine while or after ADR-0024 (workflows) lands. **Do not implement against this file until status moves out of DRAFT.**

## Context

yaad-index currently has no first-class concept of dates. When operators or plugins refer to a calendar moment — a task deadline, an event date, the day an email arrived, when a meeting is scheduled — the reference is an arbitrary string in some entity's frontmatter. Two consequences:

1. **No way to aggregate by date.** "Show me everything happening on 2026-05-18" requires every entity to be scanned and date-strings parsed in a consistent format. There's no single node to walk to.
2. **No anchor for daily/weekly/monthly digests.** A "daily summary" workflow has nothing to attach edges to as it accumulates "today's deliveries / today's PRs / today's meetings."

The proposal: **dates become first-class entities.** A reference to a date in any entity's frontmatter becomes a canonical-ID reference (e.g., `deadline: day:2026-11-11`), the date entity is auto-created on first reference, and edges connect time-bound entities to their date(s). Workflows (per ADR-0024) and plugins can attach to date entities the same way they attach to any other entity.

This is a generic infrastructure concern, not a workflow-specific one — workflows BENEFIT from it (daily-digest becomes "query the day's edges" rather than "scan all entities by date string"), but the entity-kind concern stands on its own.

## Decision

*(skeleton — sections below name the open questions; concrete decisions arrive as the ADR refines.)*

### Entity kinds

Add date entity kinds, default-enabled at the daemon level (no plugin gates them on).

**Granularity — v1.x: day only.** First cut ships **`day:<YYYY-MM-DD>`** (e.g., `day:2026-11-11`). Week / month / year are deferred to a later layer once day-aggregation patterns are established.

When week/month/year arrive later, they'll be separate kinds connected to days via `belongs_to` edges (day→week→month→year). Operators who need week-level digests in the interim can build them from day edges in workflow CEL (multi-day aggregation via repeated `graph.get(day:YYYY-MM-DD)` calls, or a future `graph.find` predicate over a date range).

Settled: **day-first, others later**.

**Open question 2:** timezone handling. `day:2026-11-11` in whose timezone? Daemon-configured default (operator picks one zone at deployment), per-entity (each date carries its TZ context), or fixed UTC at the canonical-ID layer with display-time conversion?

### Auto-creation on first reference

The daemon detects date references in inbound entities (frontmatter fields matching a date-canonical-ID shape) and creates the referenced date entities if they don't already exist.

**Open question 3:** where does detection live?
- Daemon-side validator on ingest (every plugin gets it for free; one canonical implementation)
- Plugin-side (each plugin declares its date fields; verbose but explicit)
- Hybrid (daemon scans canonical-ID-shape strings; plugins can be explicit about date-fields for richer typing)

**Open question 4:** does auto-creation happen at:
- Ingest time (cheapest; creates the day node as soon as anything references it)
- First query (lazy; creates only when something asks for the day node)
- Both / configurable

### Edge types

Time-bound entities connect to dates via canonical edges. Examples:

- `due_on` — task / deadline entity → day entity (the day it's due)
- `occurred_on` — event / meeting / shipment → day (when it happened/will happen)
- `is_about_day` — newsletter / digest / journal entry → day (the day it describes)
- `ingested_on` — any entity → day (the day yaad-index first received it; potentially set automatically by the daemon)

**Open question 5:** canonical edge type vocabulary. Do we define an explicit closed set (`due_on`, `occurred_on`, `is_about_day`, `ingested_on`) or let plugins emit any edge name targeting a date entity?

### Date entity content

A date entity is mostly an anchor — its value comes from its inbound edges. The entity itself probably has minimal frontmatter (the date, the kind) and optional body for operator notes / journal entries on that day.

**Open question 6:** day-as-journal — should a daily note (hand-written reflection for a given day) be the day entity's body, or a separate journal entity that links to the day via edge? Operator vaults that already keep daily-note files will need a migration path either way; the path differs depending on which model.

### Default-enabled

Date entities are part of the daemon's core kind set. Operators don't enable them via plugin config — they're always available. This is unusual (most entity kinds come from plugins) and intentional: dates are universal, not domain-specific.

## Out of scope (initial)

- **Time-of-day entities** (hour, minute). Out of v1.x; revisit if a use case surfaces. Most operator workflows are day-level.
- **Date arithmetic in CEL** (e.g., `day:2026-11-11 - day:2026-11-04 == 7`). Out unless workflows demand it.
- **Localized week-numbering schemes** (US vs ISO). Use ISO week (`<YYYY>-W<WW>`); operator-config knob for the schedule-of-week is post-v1.x.
- **Auto-tagging existing entities** with `ingested_on` edges at migration time. v1.x sets it forward-only on new ingests; backfill is a separate one-shot operator script if wanted.

## Workflow integration (downstream of ADR-0024)

Once date entities exist, ADR-0024 workflows benefit naturally:

- A **daily-digest workflow** triggered manually (or by external cron via `yaad-index workflow trigger daily-digest`) targets today's day entity, walks its incoming edges, formats a task or report.
- A **deadline-watcher workflow** could subscribe to `entity.edge_added` for `due_on` edges and surface a task when a deadline lands within a near window.
- A **shipping-day workflow** (the operator's worked example) attaches new shipping-related edges to the day entity as they arrive.

No ADR-0024 changes required to support date entities — they're just another entity kind to the workflow engine. This ADR adds the entity kinds + auto-creation + canonical edges; ADR-0024 already covers how workflows act on them.

## Consequences

**Positive:**
- Aggregation by date becomes a graph walk, not a scan.
- Daily / weekly digests have a natural anchor.
- Operator can hand-write daily notes against the day entity (depending on Open Q 6).
- Workflows that care about time become composable with the rest of the entity model.

**Negative:**
- New entity kinds increase the surface of the kind catalog (4 kinds if separate granularity, 1 if unified).
- Auto-creation adds an ingest-time side effect that needs to be idempotent and cheap.
- Operators with pre-existing daily-note conventions may need a migration path depending on Open Q 6's resolution.

**Migration:**
- v1.x ingest-time creation is forward-only. Backfill is a separate one-shot if wanted.
- Migration path for pre-existing daily-note conventions depends on Open Q 6 (day-as-journal body vs separate-journal-entity model); out of scope for the v1.x cut.

## Open questions (consolidated)

1. ~~Granularity model — separate kinds with `belongs_to` edges, or one `date:` kind, or day-only-first?~~ **Settled 2026-05-16: day-first, week/month/year deferred to later layer.**
2. Timezone — operator-default, per-entity, or fixed UTC?
3. Auto-creation detection — daemon-side, plugin-side, or hybrid?
4. Auto-creation timing — ingest-time, lazy-on-query, or both?
5. Edge type vocabulary — closed set or plugin-extensible?
6. Day-as-journal — body holds the daily note, or separate journal entity with edge?

## References

- ADR-0002: Edge model.
- ADR-0008: Vault-as-truth.
- ADR-0017: Daemon-owned canonical slugs.
- ADR-0024: Workflows and tasks (the downstream beneficiary).

## Status: DRAFT

This ADR is a placeholder skeleton opened to capture the design space. Refinement happens while or after ADR-0024 lands. **Do not implement against this file until status moves out of DRAFT** — open questions above need concrete answers first, and the refinement may shift the proposed shape.
