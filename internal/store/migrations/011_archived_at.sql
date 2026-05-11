-- ADR-0018 step 1 of 6: archive lifecycle column on entities.
--
-- `archived_at` is the operator-facing tristate timestamp:
--   NULL     → entity is active (the default state on creation).
--   non-NULL → entity is archived; the value is the moment the
--              archive endpoint was hit.
--
-- The archive flag is what the upcoming /v1/list_entities and
-- /v1/search default-filters key off (`WHERE archived_at IS NULL`),
-- and what the archive/restore endpoints toggle. Pre-v1 there's no
-- data migration: the column starts NULL across the board, so all
-- existing entities remain "active" — semantically a no-op for any
-- caller that doesn't yet know about the column.
--
-- TEXT (not TIMESTAMPTZ) per this repo's SQLite-wide convention —
-- ADR-0018's Postgres-shaped DDL is paraphrased; matches the way
-- entities.created_at / entities.updated_at are stored. Format
-- follows the rest of the codebase (RFC3339 / sqlite-flavored time
-- strings produced via clock.Now().Format(sqliteTimeFormat)).
--
-- Index supports the default-filter scan path on list / search.
-- A partial index on `archived_at IS NULL` would be tighter for
-- the active-set query, but a full index keeps both `archived_only`
-- and the default scan covered without branch-specific tuning. v1
-- tradeoff; revisit if the active-set scan becomes hot.

ALTER TABLE entities ADD COLUMN archived_at TEXT;
CREATE INDEX idx_entities_archived_at ON entities (archived_at);
