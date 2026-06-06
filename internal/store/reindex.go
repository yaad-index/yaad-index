package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LastReindexAt returns MAX(last_indexed_at) across the
// reindex_files table — the most-recent moment any file was
// re-derived (per ADR-0013 §3). The second
// return is false when the table is empty (no reindex has ever
// run); the caller surfaces that as `null` on the cv-status wire.
func (s *sqliteStore) LastReindexAt(ctx context.Context) (time.Time, bool, error) {
	var lastStr sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(last_indexed_at) FROM reindex_files`,
	).Scan(&lastStr)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("query last_reindex_at: %w", err)
	}
	if !lastStr.Valid || lastStr.String == "" {
		return time.Time{}, false, nil
	}
	t, err := parseSQLiteTime(lastStr.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse last_reindex_at: %w", err)
	}
	return t, true, nil
}

func (s *sqliteStore) ListReindexFiles(ctx context.Context) ([]ReindexFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, mtime, content_hash, last_indexed_at, entity_id, entity_kind
		 FROM reindex_files
		 ORDER BY path ASC`)
	if err != nil {
		return nil, fmt.Errorf("query reindex_files: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ReindexFile
	for rows.Next() {
		var f ReindexFile
		var mtime, lastIndexed string
		if err := rows.Scan(&f.Path, &mtime, &f.ContentHash, &lastIndexed, &f.EntityID, &f.EntityKind); err != nil {
			return nil, fmt.Errorf("scan reindex_files: %w", err)
		}
		if t, err := time.Parse(sqliteTimeFormat, mtime); err == nil {
			f.Mtime = t
		} else {
			return nil, fmt.Errorf("parse reindex_files.mtime %q: %w", mtime, err)
		}
		if t, err := time.Parse(sqliteTimeFormat, lastIndexed); err == nil {
			f.LastIndexedAt = t
		} else {
			return nil, fmt.Errorf("parse reindex_files.last_indexed_at %q: %w", lastIndexed, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reindex_files: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) UpsertReindexFile(ctx context.Context, f ReindexFile) error {
	if f.Path == "" {
		return errors.New("UpsertReindexFile: path is empty")
	}
	if f.EntityID == "" {
		return errors.New("UpsertReindexFile: entity_id is empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reindex_files (path, mtime, content_hash, last_indexed_at, entity_id, entity_kind)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			mtime = excluded.mtime,
			content_hash = excluded.content_hash,
			last_indexed_at = excluded.last_indexed_at,
			entity_id = excluded.entity_id,
			entity_kind = excluded.entity_kind`,
		f.Path,
		f.Mtime.UTC().Format(sqliteTimeFormat),
		f.ContentHash,
		f.LastIndexedAt.UTC().Format(sqliteTimeFormat),
		f.EntityID,
		f.EntityKind,
	)
	if err != nil {
		return fmt.Errorf("upsert reindex_files %s: %w", f.Path, err)
	}
	return nil
}

func (s *sqliteStore) DeleteReindexFile(ctx context.Context, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM reindex_files WHERE path = ?`, path)
	if err != nil {
		return false, fmt.Errorf("delete reindex_files %s: %w", path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// DeleteEntityCascade removes the entity row plus every edge that
// references it (in either direction) plus every provenance row tied
// to it (entity provenance + edge provenance for the deleted edges).
// Wrapped in a transaction so a partial failure leaves no orphans.
func (s *sqliteStore) DeleteEntityCascade(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("DeleteEntityCascade: id is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Inbound + outbound edge provenance: every (target_edge_*) row
	// whose triple references this entity, regardless of position.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM provenance
		 WHERE target_kind = 'edge'
		 AND (target_edge_from = ? OR target_edge_to = ?)`,
		id, id,
	); err != nil {
		return fmt.Errorf("delete edge provenance for %s: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edges WHERE from_id = ? OR to_id = ?`, id, id,
	); err != nil {
		return fmt.Errorf("delete edges for %s: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM provenance WHERE target_kind = 'entity' AND target_entity_id = ?`, id,
	); err != nil {
		return fmt.Errorf("delete entity provenance for %s: %w", id, err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete entity %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("rows affected: %w", err)
	} else if n == 0 {
		return fmt.Errorf("DeleteEntityCascade %s: %w", id, ErrNotFound)
	}

	return tx.Commit()
}

// WipeDerivedState drops every row from the vault-derived tables
// in a single transaction. Used by `yaad-index reindex --full` and
// the HTTP equivalent (POST /v1/reindex with mode=full).
//
// Per ADR-0008's "vault is source of truth, DB is derived" model:
// only state regenerable from the vault belongs in the wipe set.
// Anything that represents non-vault state (plugin loader caches,
// agent-flow tokens, schema metadata) MUST be preserved across a
// full wipe — dropping those would produce divergent state on the
// next server start (plugin re-init storms, lost in-flight tokens,
// broken migration accounting).
//
// Wiped — derived from the vault, regenerable by reindex:
//
// - `entities` — id/kind/data/created_at/updated_at; re-derived from each `<vault>/<kind>/<slug>.md` frontmatter on the next reindex walk.
// - `edges` — typed relationships; re-derived from the same vault files (frontmatter `edges:` list + body `## Edges` wikilinks merged on read).
// - `provenance` — fetch / fill audit rows; re-derived from the vault frontmatter `provenance:` list.
// - `reindex_files` — per-file (mtime, content_hash, last_indexed_at) bookkeeping; self-rebuilds on the next walk.
//
// Excluded — not derived from the vault, preserved across the wipe:
//
// - `plugin_capabilities` — operator-config-driven plugin loader cache (ADR-0006 +). Wiping forces every plugin to re-run `--init` on the next server start, wasteful for the legitimate full-reindex use case (vault state drift, not plugin changes). Operator-driven cache clearing has its own subcommand: `yaad-index plugins clear-cache`.
// - `schema_migrations` — migration accounting. Dropping rows would make the next server start re-apply every migration, breaking schema-version checks.
//
// Adding a new table. When a future PR adds a new table that IS
// derived from the vault, append it to the slice below AND describe
// it in the "Wiped" list above. When the new table is NOT derived
// (e.g. a future auth/session table), add it to "Excluded" — silence
// in the doc means a future editor has to read the slice to know the
// wipe set.
func (s *sqliteStore) WipeDerivedState(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Listed alphabetically for stable diffs when new tables are
	// added. Order today is alphabetical AND FK-safe by coincidence:
	// the only FK in the wipe set is `edges.from_id` / `edges.to_id`
	// → `entities(id)` (migration 001_init.sql), and `PRAGMA
	// foreign_keys = ON` is enforced (sqlite.go). Alphabetical
	// happens to delete the child (`edges`) before the parent
	// (`entities`), which is what the FK requires. Future tables
	// that introduce new parent/child relationships MUST explicitly
	// verify the deletion order — pin the FK chain in this comment
	// + reorder the slice if alphabetical breaks the child-first
	// rule.
	derivedTables := []string{
		"edges",
		"entities",
		"provenance",
		"reindex_files",
	}
	for _, table := range derivedTables {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s`, table)); err != nil {
			return fmt.Errorf("wipe %s: %w", table, err)
		}
	}
	return tx.Commit()
}
