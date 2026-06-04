package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RenameEntity re-keys an entity from oldID to newID in a single
// transaction. SQLite has no ON UPDATE CASCADE on the entity-id FKs, so
// an in-place `UPDATE entities SET id = ?` would orphan every child row;
// instead this inserts the new row, re-points every referencing table,
// and drops the old row.
//
// Table touch-set (every column that holds an entity id — keep this in
// sync when a future migration adds one, the same discipline as
// WipeDerivedState):
//
//   - entities          — new row inserted (carrying kind / created_at /
//     gap state / archived state from the old row, newData as payload),
//     old row deleted last.
//   - edges             — from_id + to_id re-pointed old -> new.
//   - provenance        — target_entity_id (entity rows) +
//     target_edge_from / target_edge_to (edge rows) re-pointed.
//   - entity_notations  — entity_id re-pointed (ON DELETE CASCADE would
//     otherwise drop them with the old row).
//   - entity_aliases    — existing aliases re-homed to newID, plus the
//     bare old slug is aliased to newID so the old `<kind>:<old-slug>`
//     reference still resolves via the alias resolver — UNLESS that bare
//     slug already belongs to another entity, whose alias is left
//     untouched (no resolver-path hijack).
//
// reindex_files is intentionally NOT touched: it is per-file bookkeeping
// that self-heals on the next reindex walk (the on-disk path changes with
// the rename, same as MoveToSubfolder leaves it to reindex).
//
// FK ordering: the new entities row is inserted first so every re-point
// targets a live row; the old row is deleted last once nothing references
// it. Returns ErrNotFound when oldID has no entities row.
func (s *sqliteStore) RenameEntity(ctx context.Context, oldID, newID string, newData map[string]any) error {
	if oldID == "" || newID == "" {
		return errors.New("RenameEntity: empty id")
	}
	if oldID == newID {
		return errors.New("RenameEntity: oldID == newID")
	}

	dataJSON, err := json.Marshal(newData)
	if err != nil {
		return fmt.Errorf("marshal newData for %s: %w", newID, err)
	}
	now := time.Now().UTC().Format(sqliteTimeFormat)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Insert the new row, carrying over the columns a rename must
	// preserve. created_at survives (a rename is not a re-creation);
	// updated_at is bumped to now.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entities (id, kind, data, gap_call_done_at, gap_state, archived_at, created_at, updated_at)
		SELECT ?, kind, ?, gap_call_done_at, gap_state, archived_at, created_at, ?
		FROM entities WHERE id = ?`,
		newID, string(dataJSON), now, oldID,
	)
	if err != nil {
		return fmt.Errorf("insert renamed entity %s -> %s: %w", oldID, newID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("rows affected: %w", err)
	} else if n == 0 {
		return fmt.Errorf("RenameEntity %s: %w", oldID, ErrNotFound)
	}

	// 2. Re-point edges. The new id is collision-checked by the caller
	// (no rows reference it yet), so no composite-PK clash on either
	// direction; a self-edge (old, old) becomes (new, new) cleanly.
	if _, err := tx.ExecContext(ctx,
		`UPDATE edges SET from_id = ? WHERE from_id = ?`, newID, oldID,
	); err != nil {
		return fmt.Errorf("re-point outbound edges %s -> %s: %w", oldID, newID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE edges SET to_id = ? WHERE to_id = ?`, newID, oldID,
	); err != nil {
		return fmt.Errorf("re-point inbound edges %s -> %s: %w", oldID, newID, err)
	}

	// 3. Re-point provenance — entity rows and both edge endpoints.
	if _, err := tx.ExecContext(ctx,
		`UPDATE provenance SET target_entity_id = ? WHERE target_kind = 'entity' AND target_entity_id = ?`,
		newID, oldID,
	); err != nil {
		return fmt.Errorf("re-point entity provenance %s -> %s: %w", oldID, newID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE provenance SET target_edge_from = ? WHERE target_kind = 'edge' AND target_edge_from = ?`,
		newID, oldID,
	); err != nil {
		return fmt.Errorf("re-point edge-from provenance %s -> %s: %w", oldID, newID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE provenance SET target_edge_to = ? WHERE target_kind = 'edge' AND target_edge_to = ?`,
		newID, oldID,
	); err != nil {
		return fmt.Errorf("re-point edge-to provenance %s -> %s: %w", oldID, newID, err)
	}

	// 4. Re-point the CASCADE-on-delete notations (they would otherwise
	// die with the old row).
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity_notations SET entity_id = ? WHERE entity_id = ?`, newID, oldID,
	); err != nil {
		return fmt.Errorf("re-point notations %s -> %s: %w", oldID, newID, err)
	}

	// 5. Guarantee the bare old slug is owned by oldID — but WITHOUT
	// stealing it from a third entity. resolveEntityID strips the
	// `<kind>:` prefix and looks the bare slug up kind-scoped, so this
	// bare alias row is what keeps the old `<kind>:<old-slug>` reference
	// resolving after the rename (even when the old entity had no
	// self-alias — an operator could have cleared its alias list). The
	// `DO UPDATE ... WHERE existing == oldID` clause is load-bearing: if
	// the bare slug already belongs to some OTHER entity, this no-ops and
	// leaves that entity's alias intact rather than hijacking its resolver
	// path. The re-home below then carries this (oldID-owned) alias to the
	// new id.
	if bareOld := bareSlugOf(oldID); bareOld != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity_aliases (alias, entity_id, alias_kind)
			VALUES (?, ?, ?)
			ON CONFLICT(alias) DO UPDATE SET
				entity_id = excluded.entity_id,
				alias_kind = excluded.alias_kind
			WHERE entity_aliases.entity_id = excluded.entity_id`,
			bareOld, oldID, AliasKindBare,
		); err != nil {
			return fmt.Errorf("ensure old-slug alias %q for %s: %w", bareOld, oldID, err)
		}
	}

	// 6. Re-home the old entity's aliases onto the new id. The bare-slug
	// alias ensured above rides along, becoming the old -> new
	// back-reference; a third entity's identically-named alias (left
	// untouched above) stays put.
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity_aliases SET entity_id = ? WHERE entity_id = ?`, newID, oldID,
	); err != nil {
		return fmt.Errorf("re-home aliases %s -> %s: %w", oldID, newID, err)
	}

	// 7. Drop the old row — nothing references it anymore.
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE id = ?`, oldID); err != nil {
		return fmt.Errorf("delete old entity %s: %w", oldID, err)
	}

	return tx.Commit()
}

// bareSlugOf returns the slug portion of a `<kind>:<slug>` id — the form
// the alias resolver matches against. Empty when the id has no `:` or an
// empty slug.
func bareSlugOf(id string) string {
	idx := strings.IndexByte(id, ':')
	if idx < 0 || idx == len(id)-1 {
		return ""
	}
	return id[idx+1:]
}
