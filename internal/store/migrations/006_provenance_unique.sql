-- Two partial UNIQUE indexes on the provenance table per ADR-0010.
-- The fetch path and fill path use disjoint timestamp columns; the
-- partial-WHERE clauses dodge SQLite's NULL-distinctness in unique
-- indexes (each NULL would otherwise be considered a separate row).
--
-- Pre-migration dedup: GROUP BY treats NULL = NULL (different from
-- UNIQUE), so a duplicate fill-path row (where fetched_at is NULL)
-- groups correctly with another fill-path row of the same source.
-- MIN(rowid) keeps the earliest occurrence of each (entity, source,
-- fetched_at, filled_at) tuple; later duplicates are deleted.
DELETE FROM provenance WHERE rowid NOT IN (
    SELECT MIN(rowid) FROM provenance
    GROUP BY target_entity_id, source, fetched_at, filled_at
);

-- Ingest-path uniqueness. Plugin Fetch writes (entity, source,
-- fetched_at) — fetched_at non-NULL, filled_at NULL.
CREATE UNIQUE INDEX idx_prov_unique_fetch
    ON provenance(target_entity_id, source, fetched_at)
    WHERE fetched_at IS NOT NULL;

-- Fill-path uniqueness. Agent fill writes (entity, source,
-- filled_at) — fetched_at NULL, filled_at non-NULL.
CREATE UNIQUE INDEX idx_prov_unique_fill
    ON provenance(target_entity_id, source, filled_at)
    WHERE filled_at IS NOT NULL;
