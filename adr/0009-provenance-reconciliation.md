# ADR-0009 — Provenance reconciliation: vault is canonical for provenance, reindex re-derives

## Status

Accepted (2026-05-02).

## Context

[ADR-0008](0008-vault-as-source-of-truth.md) established the vault-as-source-of-truth, DB-as-derived-index model. That redesign covered entities, edges, notes, and the fill/ingest endpoints. **Provenance was the one surface left half-migrated.**

What we have today:

- **Vault frontmatter** — every entity's `*.md` file accumulates the full provenance list under the `provenance:` key. Plugin fetches, agent fills, manual edits all append entries. The vault list is the canonical record.
- **DB `provenance` table** — entity-keyed audit rows. A dual-write contract is in place: ingest writes the vault first, then `AppendProvenance` mirrors to the DB. Concurrency-safe via `AppendProvenance`'s tx wrapper.
- **Reindex** — the vault walker is in place, but `Reindexer.upsertEntity` deliberately does NOT touch DB provenance during re-derivation. Its only DB write is `store.UpsertEntity`. Note in `internal/reindex/reindex.go` explicitly defers this work to a future PR.

The result: the DB's `provenance` rows are a **half-derived** state. They mirror what the API has written, but they're not regenerable from the vault. A `WipeDerivedState` followed by reindex leaves the table empty until the next ingest / fill writes new rows. The vault still holds the full record — data isn't lost — but the DB-side mirror diverges until ingest / fill fires.

This was a known gap ([docs/index-flow.md §2 / §3](../docs/index-flow.md)) and is the motivation for this ADR.

## Decision

**Provenance becomes a fully derived state. The vault frontmatter `provenance:` list is canonical; the DB table is regenerable via reindex.**

Three concrete changes implement this:

### 1. `store.ReplaceProvenance(ctx, entityID, entries) error`

A new store method that REPLACES (not appends) an entity's provenance with the given list. Transactional: `BeginTx` → `DELETE FROM provenance WHERE target_entity_id = ?` → `INSERT` each entry → `Commit`. Either every row matches the input list, or none do (rollback on any failure).

This sits alongside the existing `AppendProvenance` (kept; semantics unchanged). `Append` remains the right primitive for the live ingest / fill paths where provenance accumulates incrementally. `Replace` is the primitive reindex uses.

### 2. Reindex re-derives provenance from frontmatter

`internal/reindex/reindex.go::upsertEntity` is extended:

```go
func (r *Reindexer) upsertEntity(ctx context.Context, e *vault.Entity) error {
 se := &store.Entity{ID: e.ID, Kind: e.Kind, Data: e.Data}
 if err := r.store.UpsertEntity(ctx, se); err != nil {
 return fmt.Errorf("UpsertEntity %s: %w", e.ID, err)
 }
 // New: re-derive DB provenance from vault frontmatter.
 entries := vaultProvenanceToStore(e.Provenance)
 if err := r.store.ReplaceProvenance(ctx, e.ID, entries); err != nil {
 return fmt.Errorf("ReplaceProvenance %s: %w", e.ID, err)
 }
 return nil
}
```

Every parsed entity in a reindex pass gets its DB provenance replaced with the vault's list — incremental walks (when `(mtime, content_hash)` changed) and full walks (after `WipeDerivedState`) both run this. Hand-edits to the vault frontmatter's `provenance:` list become authoritative on the next reindex.

### 3. Ingest / fill — keep dual-write, vault-first

The live API paths (`internal/api/ingest_tracker.go`, `internal/api/fill.go`) continue to:

1. Append to vault frontmatter `provenance:` (atomic temp+rename via `vault.Writer`).
2. Call `store.AppendProvenance` on the DB (transactional, succeeds-or-rolls-back).

The dual-write keeps DB rows current between reindex passes (so `GET /v1/entities/{id}` returns fresh provenance immediately, no reindex roundtrip). On any conflict (race, DB error, schema drift), the next reindex pass overwrites with the vault's list — vault wins.

We considered making ingest / fill **skip** the DB write and rely entirely on reindex to populate provenance. Rejected: that introduces a window where a freshly-ingested entity has no DB-side provenance until reindex runs, which breaks the agent-AI-fill flow (the agent reads provenance to know what's already been fetched). Dual-write with vault-canonical is the right compromise — derived-but-eagerly-mirrored.

## Consequences

### What this fixes

- `WipeDerivedState` + reindex now produces a fully-recovered DB. Provenance rows match the vault after a full wipe-and-rebuild, not just after the next ingest.
- Hand-edits to the vault's `provenance:` list flow through reindex into the DB. This isn't a common path (operators rarely hand-edit provenance) but it closes the vault-canonical contract.
- The "data isn't lost" invariant strengthens: the vault always has the full record, and any DB-side divergence self-heals on the next reindex.

### What this changes for callers

- **API consumers** (`GET /v1/entities/{id}`): no behavior change. Provenance reads come from the DB; the DB is up-to-date because of dual-write.
- **Plugin authors**: no contract change. Plugins emit `FetchResult.Provenance`; the orchestrator persists it the same way.
- **Operator tooling** (`yaad-index reindex --full`): now produces a complete DB state, not a partial one. Documentation in [docs/index-flow.md](../docs/index-flow.md) §2 / §3 will be updated to reflect the new behavior (the "Provenance is NOT re-derived by reindex today" paragraph becomes "Provenance IS re-derived by reindex per ADR-0009").

### Race / consistency considerations

- **Reindex during live writes.** If reindex runs while an ingest is mid-flight, the live `AppendProvenance` could land between the reindex's `ReplaceProvenance` for the same entity. Two outcomes possible:
 1. **Reindex's `ReplaceProvenance` wins** (live `AppendProvenance` lands first, then reindex replaces): the in-flight entry's row is gone from the DB until the next reindex picks up the vault frontmatter (which still has it via the live ingest's vault-first write). Side effect: the fill orchestrator may re-issue a fetch for an entity it just wrote provenance for. `AppendProvenance` is idempotent at the action level (operator + AI re-issue fine), so this is a duplicate-work blip, not a correctness break.
 2. **Live `AppendProvenance` wins** (reindex replaces first, then `AppendProvenance` adds): the entity has reindex's reconciled list PLUS a fresh row for the in-flight entry. **`provenance` has no UNIQUE constraint on `(target_entity_id, source, fetched_at)` (per `migrations/001_init.sql`), so this is a duplicate row, not idempotent dedup.** Self-heals on the next reindex (which `Replace`s back to the vault's canonical list, which the vault-first write already includes). Window: between the live write and the next reindex, a reader sees the duplicate.
 
 Both outcomes are tolerable because the vault remains authoritative and self-heals on the next reindex. Operator-driven reindex during live writes is unusual; for the normal startup + nightly cadence, no race window exists.

 **Optional schema tightening:** add `UNIQUE(target_entity_id, source, fetched_at)` + `INSERT ... ON CONFLICT DO NOTHING` to `AppendProvenance` for true row-level idempotency. Out of scope for this ADR (additive migration; can land as a follow-up if the duplicate-row window becomes an observable problem).

- **Sequential-failure window inside `Reindexer.upsertEntity`.** `UpsertEntity` (single-statement autocommit) and `ReplaceProvenance` (own `BeginTx`) are NOT wrapped in a single enclosing transaction. If the process crashes between them, the entity row is updated but the provenance is stale. The next reindex pass re-runs both, restoring consistency. Tolerable because reindex is idempotent + the vault stays authoritative; documented here so a future reader knows the contract.

- **Schema-versioned `ProvenanceEntry`.** The vault frontmatter's `provenance:` list and the DB's `provenance` table use the same shape (per ADR-0008's frontmatter schema). Adding a field to `ProvenanceEntry` requires both the vault marshaler/unmarshaler and the DB schema migration to land together. ADR-0009 doesn't introduce a new field; future fields follow the existing migration discipline.

- **`AppendProvenance` is preserved**, not replaced. Code paths that legitimately accumulate (live ingest, fill) keep using `Append`; only reindex uses `Replace`.

### Test coverage

- Reindex test: write entity to vault with N provenance entries → run reindex → assert DB has exactly N matching entries.
- Hand-edit test: append a provenance entry directly to a vault file's frontmatter (no API call) → run reindex → assert DB picks up the new entry.
- Wipe-and-rebuild test: `WipeDerivedState` → reindex → assert DB provenance matches every vault file's list, including entities the live API has never re-touched.
- Race-of-no-real-consequence: live `AppendProvenance` during a `ReplaceProvenance` for the same entity — both should succeed transactionally, end state should be either-or per the consistency note above.

## Alternatives considered

### A) Drop the DB `provenance` table; read provenance from vault on every API call

Rejected. `GET /v1/entities/{id}` would have to read the file each time. Adds filesystem I/O to the hot path of every entity fetch. SQLite indexed reads are orders of magnitude faster than parsing markdown frontmatter; the DB-as-derived-index model is correct.

### B) Make reindex authoritative — ingest skips the DB write, relies on reindex

Rejected. See "We considered making ingest / fill skip..." in the Decision section. Window between ingest and next reindex breaks the agent-AI-fill flow.

### C) Move provenance into the entity `data:` map

Rejected. Provenance is structurally different from data — it's a list of audit records, not entity content. The reserved-data-keys discipline (per AGENTS.md) keeps vault-derived structure separate from plugin-emitted data; provenance fits the structure side.

### D) Per-source `provenance` table

Rejected for now. The current single-table-with-source-column shape is simple and matches the vault list. Per-source sharding is a future-PR concern if query performance ever justifies it.

## Migration

This ADR is implementation-only — no schema change needed. The migration shape:

1. Add `store.ReplaceProvenance` (store-only, no behavior change for callers).
2. Update `Reindexer.upsertEntity` to call `ReplaceProvenance` (reindex-only behavior change).
3. Update `docs/index-flow.md` §2 (re-derived list) and §3 (Wipe `provenance` row) to reflect the new state.

Existing dual-write paths in ingest / fill stay unchanged.

## References

- [ADR-0008](0008-vault-as-source-of-truth.md) — vault-as-source-of-truth (foundation)
- [docs/index-flow.md](../docs/index-flow.md) §2 (re-derive scope) + §3 (wipe set rationale)
