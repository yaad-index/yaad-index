// entity_aliases store API per #3. Mirrors notations.go on the
// outbound-label axis: alias text → entity_id with a {'bare',
// 'typed'} discriminator. ReplaceAliases follows ADR-0009's re-
// derive pattern (transactional DELETE + INSERTs); search joins
// against this table at /v1/search query time.

package store

import (
	"context"
	"errors"
	"fmt"
)

// ListAliasesForEntity returns every alias pointing at the given
// entity_id, ordered by alias for deterministic output. Empty
// slice (non-nil) for an entity with no aliases. Empty entityID
// rejected — reverse-lookup of "everything" is reindex territory,
// not this method.
func (s *sqliteStore) ListAliasesForEntity(ctx context.Context, entityID string) ([]Alias, error) {
	if entityID == "" {
		return nil, errors.New("ListAliasesForEntity: empty entity_id")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT alias, entity_id, alias_kind
		FROM entity_aliases
		WHERE entity_id = ?
		ORDER BY alias ASC
	`, entityID)
	if err != nil {
		return nil, fmt.Errorf("query entity_aliases for %s: %w", entityID, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Alias, 0)
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Alias, &a.EntityID, &a.Kind); err != nil {
			return nil, fmt.Errorf("scan entity_aliases for %s: %w", entityID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entity_aliases for %s: %w", entityID, err)
	}
	return out, nil
}

// ReplaceAliases transactionally wipes + rewrites the alias rows
// for entityID. Empty entries clears the rows. Each input entry's
// EntityID must be empty or equal to the target entityID; a
// mismatch rejects (caller bug — same defensive shape as
// ReplaceNotations). Kind defaults to AliasKindBare when empty;
// any other value passes through to the column unchanged so a
// future kind addition can land without store-side changes.
//
// The PRIMARY KEY on alias means an existing alias pointing at a
// different entity is rewritten in place by the INSERT's
// ON CONFLICT path — the move-an-alias-between-entities case is
// the supported path. Within a single ReplaceAliases call,
// duplicate aliases reject with a transaction rollback.
func (s *sqliteStore) ReplaceAliases(ctx context.Context, entityID string, entries []Alias) error {
	if entityID == "" {
		return errors.New("ReplaceAliases: empty entity_id")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_aliases WHERE entity_id = ?`, entityID,
	); err != nil {
		return fmt.Errorf("delete prior aliases for %s: %w", entityID, err)
	}

	seen := make(map[string]struct{}, len(entries))
	for i, a := range entries {
		if a.Alias == "" {
			return fmt.Errorf("aliases[%d]: empty alias", i)
		}
		if _, dup := seen[a.Alias]; dup {
			return fmt.Errorf("aliases[%d]: duplicate alias %q within batch", i, a.Alias)
		}
		seen[a.Alias] = struct{}{}
		if a.EntityID != "" && a.EntityID != entityID {
			return fmt.Errorf("aliases[%d]: entity_id %q != target %q", i, a.EntityID, entityID)
		}
		kind := a.Kind
		if kind == "" {
			kind = AliasKindBare
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity_aliases (alias, entity_id, alias_kind)
			VALUES (?, ?, ?)
			ON CONFLICT(alias) DO UPDATE SET
				entity_id = excluded.entity_id,
				alias_kind = excluded.alias_kind
		`, a.Alias, entityID, kind); err != nil {
			return fmt.Errorf("insert aliases[%d] for %s: %w", i, entityID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
