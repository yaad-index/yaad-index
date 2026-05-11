package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetNotation looks up the entity slug a given notation resolves to
// (per the source issue a prior PR). Returns ErrNotFound when the notation isn't
// in the table — same sentinel as GetEntity so handler code can use
// errors.Is uniformly.
func (s *sqliteStore) GetNotation(ctx context.Context, notation string) (Notation, error) {
	if notation == "" {
		return Notation{}, errors.New("GetNotation: empty notation")
	}
	var n Notation
	err := s.db.QueryRowContext(ctx, `
		SELECT notation, entity_id, notation_kind
		FROM entity_notations
		WHERE notation = ?
	`, notation).Scan(&n.Notation, &n.EntityID, &n.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return Notation{}, ErrNotFound
	}
	if err != nil {
		return Notation{}, fmt.Errorf("query entity_notations: %w", err)
	}
	return n, nil
}

// UpsertNotation writes one (notation → entity_id, kind) row. Same
// notation already pointing at a different entity_id is the supported
// reassignment path — the PRIMARY KEY on notation forces the upsert
// to overwrite. Empty entity_id rejected (the cascade FK would
// surface a confusing constraint error otherwise).
func (s *sqliteStore) UpsertNotation(ctx context.Context, n Notation) error {
	if n.Notation == "" {
		return errors.New("UpsertNotation: empty notation")
	}
	if n.EntityID == "" {
		return errors.New("UpsertNotation: empty entity_id")
	}
	kind := n.Kind
	if kind == "" {
		// Mirror the schema default so callers that pass a zero-value
		// Notation (only filling Notation + EntityID) get the
		// documented `url` discriminator instead of an empty string.
		kind = NotationKindURL
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO entity_notations (notation, entity_id, notation_kind)
		VALUES (?, ?, ?)
		ON CONFLICT(notation) DO UPDATE SET
			entity_id = excluded.entity_id,
			notation_kind = excluded.notation_kind
	`, n.Notation, n.EntityID, kind)
	if err != nil {
		return fmt.Errorf("upsert entity_notations: %w", err)
	}
	return nil
}

// DeleteNotationsForEntity drops every notation row pointing at the
// given entity_id. Returns the number of rows affected so reindex
// can log the delta. Empty entityID is an error (delete-all is not
// the intended shape — callers wanting that use WipeDerivedState).
func (s *sqliteStore) DeleteNotationsForEntity(ctx context.Context, entityID string) (int, error) {
	if entityID == "" {
		return 0, errors.New("DeleteNotationsForEntity: empty entity_id")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM entity_notations WHERE entity_id = ?`, entityID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete entity_notations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// ReplaceNotations overwrites an entity's notation rows with the
// given list. Wraps the DELETE + INSERTs in a single transaction:
// either every input row replaces the prior set, or the prior set
// stays intact (rollback on any failure). Used by reindex (a prior PR) to
// re-derive entity_notations from canonical vault frontmatter,
// mirroring ReplaceProvenance's contract per ADR-0009.
//
// Empty entries permitted — clears the entity's notations.
func (s *sqliteStore) ReplaceNotations(ctx context.Context, entityID string, entries []Notation) error {
	if entityID == "" {
		return errors.New("ReplaceNotations: empty entity_id")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_notations WHERE entity_id = ?`, entityID,
	); err != nil {
		return fmt.Errorf("delete prior notations for %s: %w", entityID, err)
	}

	for i, n := range entries {
		if n.Notation == "" {
			return fmt.Errorf("notations[%d]: empty notation", i)
		}
		kind := n.Kind
		if kind == "" {
			kind = NotationKindURL
		}
		// Each input row's entity_id must agree with the entity we're
		// replacing for — caller bug if not. Reject loudly so a
		// malformed reindex doesn't silently scatter notations across
		// entities.
		if n.EntityID != "" && n.EntityID != entityID {
			return fmt.Errorf("notations[%d]: entity_id %q != target %q", i, n.EntityID, entityID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity_notations (notation, entity_id, notation_kind)
			VALUES (?, ?, ?)
			ON CONFLICT(notation) DO UPDATE SET
				entity_id = excluded.entity_id,
				notation_kind = excluded.notation_kind
		`, n.Notation, entityID, kind); err != nil {
			return fmt.Errorf("insert notations[%d] for %s: %w", i, entityID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
