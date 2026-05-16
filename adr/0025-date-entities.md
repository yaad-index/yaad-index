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

When week/month/year arrive later, they'll be separate kinds connected to days via some hierarchical edge (`belongs_to`, `contains`, or whatever shape lands at that point). Operators who need week-level digests in the interim can build them from day edges in workflow CEL (multi-day aggregation via repeated `graph.get(day:YYYY-MM-DD)` calls, or a future `graph.find` predicate over a date range).

Settled: **day-first, others later**.

**Open question 2:** timezone handling. The canonical ID embeds a calendar-date string — but in whose timezone? Options:

- Daemon-configured default (operator picks one zone at deployment; canonical ID is the day in that zone)
- Per-entity (each date carries its TZ context as a separate frontmatter field)
- Fixed UTC at the canonical-ID layer with display-time conversion

**This question is entangled with the canonical ID format itself.** The `day:<YYYY-MM-DD>` examples throughout this DRAFT assume the question is settled; they are illustrative-only until Q2 lands. The format may shift (e.g., `day:2026-11-11Z` for UTC-tagged, `day:2026-11-11@Europe/Berlin` for TZ-embedded, or `day:2026-11-11` with the TZ implicit per daemon config). Don't anchor downstream design on the exact form shown here until Q2 settles.

### Auto-creation: two distinct mechanisms

Two different paths create day entities. They're separate concerns and should be treated as such:

#### Mechanism A — reference-driven creation

When an operator-emitted entity has a frontmatter field that references a day (e.g., `deadline: day:2026-11-11`), the day entity is created if it doesn't already exist. This is reference-following: the daemon (or plugin) sees a `day:` canonical-ID reference and ensures the target exists.

**Open question 3a:** where does the reference detection live?
- Daemon-side validator on ingest (every plugin gets it for free; one canonical implementation; daemon scans canonical-ID-shape strings)
- Plugin-side (each plugin declares which of its frontmatter fields are date references; verbose but explicit)
- Hybrid (daemon scans by shape; plugins can be explicit for richer typing or to opt fields out)

**Open question 4:** when does reference-driven creation happen?
- Ingest time (cheapest; creates the day node as soon as anything references it)
- First query (lazy; creates only when something asks for the day node)
- Both / configurable

#### Mechanism B — daemon-side auto-tag (`ingested_on`)

Independently of reference-following, the daemon may set an `ingested_on` edge on every newly-ingested entity, pointing at the day the ingest happened. This is daemon-internal and doesn't require any frontmatter reference from the plugin — every entity gets a date stamp automatically.

**Open question 3b:** ship `ingested_on` auto-tagging in v1.x, or defer? It's the simplest case (always-on, no per-plugin config) but it's also the most opinionated (every entity gets the edge whether anyone uses it or not).

These two mechanisms answer "when does a day entity get created?" but for different reasons. Q3a + Q4 are about the reference path; Q3b is about the auto-tag path. Don't conflate them in the implementation.

### Edge types

Time-bound entities connect to dates via canonical edges. Examples:

- `due_on` — task / deadline entity → day entity (the day it's due)
- `occurred_on` — event / meeting / shipment → day (when it happened/will happen)
- `is_about_day` — newsletter / digest / journal entry → day (the day it describes)
- `ingested_on` — any entity → day (the day yaad-index first received it; potentially set automatically by the daemon)

**Open question 5:** canonical edge type vocabulary. Do we define an explicit closed set (`due_on`, `occurred_on`, `is_about_day`, `ingested_on`) or let plugins emit any edge name targeting a date entity?

### Date entity content

A date entity is mostly an anchor — its value comes from its inbound edges. Beyond that, the content shape is still open:

**Open question 6:** day-as-journal — what (if anything) lives in the day entity's body? Options:
- **Body holds the daily note** (operator's hand-written reflection for that day; day entity is both anchor + journal)
- **Body stays empty / metadata-only** (separate journal entities link to the day via edge; day entity is anchor-only)
- **Hybrid** (body may hold operator notes but isn't required; journal-as-edge is also valid)

Both anchor-only and body-as-journal have downstream consequences for migration of pre-existing daily-note conventions; deferring picks until Q6 lands.

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
- **Plain-date-string fields.** Existing entities with frontmatter like `deadline: 2026-11-11` (raw string, not `day:2026-11-11` canonical reference) won't auto-convert. A migration script could rewrite them; out of scope here. Workflows / queries that want to reach day entities from plain-string fields will need either the migration or a forward-compat shim that accepts both forms.

## Open questions (consolidated)

1. ~~Granularity model — separate kinds with hierarchical edges, or one `date:` kind, or day-only-first?~~ **Settled 2026-05-16: day-first, week/month/year deferred to later layer.**
2. Timezone — daemon-default, per-entity, or fixed UTC? (Entangled with canonical ID format; examples in this DRAFT are illustrative until this settles.)
3a. Reference-driven creation detection — daemon-side, plugin-side, or hybrid?
3b. `ingested_on` auto-tag in v1.x, or defer?
4. Reference-driven creation timing — ingest-time, lazy-on-query, or both?
5. Edge type vocabulary — closed set or plugin-extensible?
6. Day-as-journal — body holds the daily note, anchor-only, or hybrid?

## References

- ADR-0002: Edge model.
- ADR-0008: Vault-as-truth.
- ADR-0017: Daemon-owned canonical slugs.
- ADR-0024: Workflows and tasks (the downstream beneficiary).

## Status: DRAFT

This ADR is a placeholder skeleton opened to capture the design space. Refinement happens while or after ADR-0024 lands. **Do not implement against this file until status moves out of DRAFT** — open questions above need concrete answers first, and the refinement may shift the proposed shape.
