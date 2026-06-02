# ADR-0010 — Row-level idempotency for vault-derived DB tables

## Status

Accepted (2026-05-02).

## Context

[ADR-0009](0009-provenance-reconciliation.md) established that the `provenance` table is fully derived from the vault frontmatter — the vault list is canonical, reindex re-derives the DB rows via `store.ReplaceProvenance`. The Race / consistency section of that ADR named two outcomes for the AppendProvenance-during-ReplaceProvenance race:

1. **ReplaceProvenance wins** — duplicate-work blip (idempotent at the action level), no DB issue.
2. **AppendProvenance wins after Replace** — **duplicate row** in `provenance`, because the table has no UNIQUE constraint on `(target_entity_id, source, fetched_at)`. Self-heals on the next reindex; observable to API consumers in the meantime.

ADR-0009 acknowledged outcome 2 honestly and named "Optional schema tightening" as a follow-up — that's this ADR.

The broader pattern: any DB table that's **derived from the vault** is going to face the same race shape. Reindex's `Replace*` operation will collide with live API writes' `Append*` semantics whenever the live path runs concurrently with a re-derivation pass. "Self-heal on next reindex" is fine in steady-state operation but kicks the can:

- Reindex isn't free. Per-vault scan; full-mode wipes-and-rebuilds. The cadence varies (operator-driven `--full` may not run for hours; nightly cron at most once per day).
- API consumers reading during the window see stale or duplicate state. Aggregation queries (count, distinct, latest-by-source) get the wrong answer.
- The "wait for next reindex" semantic is a leaky abstraction — a class of bug that's invisible until someone hits it, then debuggable only by understanding the reindex schedule.

DB invariants belong at the storage layer, not deferred to a periodic batch process.

## Decision

**Vault-derived DB tables enforce row-level idempotency via DB-level UNIQUE constraints + `INSERT ... ON CONFLICT DO NOTHING` semantics on the live append path.**

Concrete principle:

- Any table whose row-set is fully derivable from vault state — `entities`, `edges`, `provenance`, future `notes`, future canonical-stub mirrors — gets a UNIQUE constraint on the natural key composing its rows.
- `Append*` store methods that write into those tables use `ON CONFLICT (...) DO NOTHING`: a duplicate insert is a silent no-op, not a constraint error.
- `Replace*` store methods (which delete-then-insert in one tx) are unaffected — they can never collide with themselves; only with concurrent `Append*` calls. The UNIQUE-on-Append makes that collision benign.

The first concrete instance of this principle: `provenance(target_entity_id, source, fetched_at)` UNIQUE. Issue tracks the implementation.

### Provenance-specific implementation

The `provenance` table has two operational shapes per row, distinguished by which timestamp is set:

- **Ingest-path rows** — `fetched_at` set, `filled_at` NULL. Plugin Fetch wrote this.
- **Fill-path rows** — `filled_at` set, `fetched_at` NULL. Agent fill wrote this.

A naive single UNIQUE on `(target_entity_id, source, fetched_at)` would NOT protect fill-path rows: SQLite treats each NULL as distinct in unique indexes, so fill-path entries (`fetched_at` = NULL) bypass the constraint. **Two partial indexes** handle the split cleanly:

1. **Migration** (next sequential migration number; 006 likely):
 ```sql
 -- Dedupe existing rows before adding constraints (GROUP BY treats NULL = NULL,
 -- so dedup catches both shapes correctly)
 DELETE FROM provenance WHERE rowid NOT IN (
 SELECT MIN(rowid) FROM provenance
 GROUP BY target_entity_id, source, fetched_at, filled_at
 );

 -- Ingest-path uniqueness
 CREATE UNIQUE INDEX idx_prov_unique_fetch
 ON provenance(target_entity_id, source, fetched_at)
 WHERE fetched_at IS NOT NULL;

 -- Fill-path uniqueness
 CREATE UNIQUE INDEX idx_prov_unique_fill
 ON provenance(target_entity_id, source, filled_at)
 WHERE filled_at IS NOT NULL;
 ```

2. **`AppendProvenance` update** in `internal/store/sqlite.go`:
 ```sql
 INSERT INTO provenance (...)
 VALUES (...)
 ON CONFLICT(target_entity_id, source, fetched_at) WHERE fetched_at IS NOT NULL DO NOTHING
 ON CONFLICT(target_entity_id, source, filled_at) WHERE filled_at IS NOT NULL DO NOTHING
 ```
 (SQLite supports multiple `ON CONFLICT` clauses; each names the matching index by predicate.)

3. **`ReplaceProvenance`** (from ADR-0009) is unaffected — `DELETE` first, then `INSERT` within the same tx. UNIQUE conflicts impossible inside the tx.

**Why two partial indexes (vs. alternatives):**

- **Single UNIQUE on `(target_entity_id, source, fetched_at)`** — half-protection. Rejected.
- **Expand to `(target_entity_id, source, fetched_at, filled_at)`** — same NULL-distinct issue if both ever both-NULL; doesn't fully close the gap.
- **`COALESCE(fetched_at, filled_at)` + CHECK at-least-one-set** — single index, more compact, but conflates the two semantics. Future readers querying "show me ingest-path provenance" lose a clean signal.
- **Sentinel non-null timestamp** — fragile, wrong abstraction. Rejected.

Two partial indexes are the most explicit + most-honest-to-the-schema option. The two operational shapes get their own constraint each; future code touching provenance reads the schema and sees the shape directly.

### Future tables follow the same shape

When a future PR adds a new vault-derived table:

1. Identify the natural key (the tuple that uniquely identifies a row from the vault frontmatter's perspective).
2. Add a UNIQUE constraint on that key in the table's CREATE statement.
3. The corresponding `Append*` store method uses `ON CONFLICT DO NOTHING`.
4. The corresponding `Replace*` (reindex's path) doesn't need UNIQUE-handling — its delete-then-insert tx is collision-free internally.

This is the durable pattern, not just a one-off fix for `provenance`.

## Consequences

### What this fixes

- The duplicate-row race window from ADR-0009 outcome 2 closes at the storage layer, not deferred to next reindex. API consumers always see a clean row-set.
- The vault-DB bijection is exact, not approximate-modulo-next-reindex. `len(DB rows for entity X) == len(vault provenance for entity X)` becomes invariant after every successful operation, not just after a fresh reindex.
- Future vault-derived tables inherit the pattern by convention; the duplicate-row class of bug doesn't have to be re-discovered each time.

### What this changes for callers

- **API consumers** (`GET /v1/entities/{id}/provenance`-shaped reads): no behavior change; they always see a clean row-set, which they already expected.
- **Plugin authors**: no contract change. Plugins emit `FetchResult.Provenance`; the orchestrator persists it the same way. Duplicate emissions (an over-eager plugin emitting the same provenance twice) become silent no-ops at the storage layer instead of either constraint errors or duplicate rows.
- **Operator tooling**: migration takes a one-shot dedup pass on existing DBs. For most operators with tidy DBs, this is a no-op (no duplicates to remove). For operators whose DBs accumulated duplicates over the ADR-0009-implementation period, the migration cleans up.

### Race / consistency considerations

- **Migration during operation.** `CREATE UNIQUE INDEX` in SQLite locks the table during creation. Existing INSERT/UPDATE traffic blocks for the duration. For personal-vault-scale `provenance` tables (hundreds of rows), this is sub-millisecond. For larger deployments it's a future concern.

- **`ON CONFLICT DO NOTHING` vs. `ON CONFLICT (...) DO UPDATE`.** `DO NOTHING` is the right semantic for vault-derived rows — if the natural key matches an existing row, the existing row is canonical (ReplaceProvenance is the authoritative path; Append's job is to append-what-isn't-already-there, not overwrite). `DO UPDATE` would let a stale `Append` overwrite a fresh `Replace`, which is wrong. Document this explicitly so future Append-path additions don't silently switch to UPDATE.

- **Schema migration discipline (per ADR-0006 + project convention).** The migration is forward-only; no destructive change for operators who never had duplicates. Migration test covers the dedup path and the post-migration UNIQUE behavior.

### Test coverage

- **Migration test.** Pre-migration DB with intentional duplicates → run migration → assert duplicates deduped + UNIQUE index in place.
- **Append-on-duplicate test.** Existing row → AppendProvenance with same `(target_entity_id, source, fetched_at)` → no error, no second row.
- **Replace-during-Append test.** Concurrent ReplaceProvenance + AppendProvenance for same entity → end-state has no duplicates regardless of order.
- **Plugin double-emit test.** Plugin emits same provenance entry twice in one Fetch → DB has one row (already covered by Append-on-duplicate, but worth pinning the plugin-author-surface case).

## Alternatives considered

### A) Acknowledge race in docs only, no schema change

Rejected. ADR-0009 already does this; this ADR exists because the doc-only path leaves the duplicate-row window observable to API consumers. The whole point is to tighten the storage contract.

### B) Application-level dedup in `AppendProvenance`

Rejected. SELECT-then-INSERT is two round-trips + a TOCTOU race; the DB-level UNIQUE makes the dedup atomic at INSERT time. SQLite's `ON CONFLICT DO NOTHING` is the correct primitive.

### C) `ON CONFLICT (...) DO UPDATE`

Rejected. Would let a stale `Append` overwrite a fresh `Replace`. The Replace path is canonical; Append's job is "add if not there," not "overwrite."

### D) Per-source provenance tables (sharded)

Rejected. Per-source sharding is a future-PR concern (per ADR-0009 alternatives) if query performance ever justifies it. Doesn't help with the idempotency question; UNIQUE on the composite key works for the single-table shape.

### E) Time-based dedup window (only treat as duplicate if `fetched_at` is "close")

Rejected. SQLite has no native time-window UNIQUE; this would push dedup into application code with all the TOCTOU races that brings. The composite key `(entity, source, fetched_at)` already encodes "same source at same instant"; rows that legitimately differ on `fetched_at` (a re-fetch a second later) get separate rows. That's correct.

## Migration

This ADR is implementation-only. The migration shape:

1. Land ADR-0009's `ReplaceProvenance` + reindex-call-it implementation PRs first (sequencing matters: this ADR's UNIQUE constraint assumes Replace is the canonical path that Append defers to).
2. Land's migration + `AppendProvenance` update.
3. Update `docs/index-flow.md` §3 to reference UNIQUE in the wipe-set rationale + remove the "self-heals on next reindex" softening that ADR-0009's race section needed.

Two PRs total in the implementation chain (one already in flight from ADR-0009; this ADR enables the second).

## References

- [ADR-0009](0009-provenance-reconciliation.md) — vault-canonical provenance + reindex re-derives. This ADR builds on its Race / consistency section.
- The missing-UNIQUE catch, confirmed against `migrations/001_init.sql`.
- Decision: long-run schema correctness over self-heals-on-next-reindex.
