package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedEntityForNotation upserts a minimal entity row so the notation
// table's FK has a target. Notation tests don't care about entity
// content — just that the FK resolves.
func seedEntityForNotation(t *testing.T, s *sqliteStore, id, kind string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertEntity(context.Background(), &Entity{
		ID: id,
		Kind: kind,
		Data: map[string]any{"title": id},
		Provenance: []ProvenanceEntry{
			{Source: "seed", FetchedAt: &now, OK: true},
		},
	}))
}

// TestNotation_GetMissReturnsNotFound pins the cache-miss signal:
// a notation not in the table returns (zero, ErrNotFound).
func TestNotation_GetMissReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	got, err := s.GetNotation(context.Background(), "https://example.test/missing")
	assert.True(t, errors.Is(err, ErrNotFound), "want ErrNotFound, got %v", err)
	assert.Equal(t, Notation{}, got)
}

func TestNotation_UpsertThenGet(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.UpsertNotation(context.Background(), Notation{
		Notation: "https://en.wikipedia.org/wiki/Tehran",
		EntityID: "wikipedia:tehran",
		Kind: NotationKindURL,
	}))

	got, err := s.GetNotation(context.Background(), "https://en.wikipedia.org/wiki/Tehran")
	require.NoError(t, err)
	assert.Equal(t, "https://en.wikipedia.org/wiki/Tehran", got.Notation)
	assert.Equal(t, "wikipedia:tehran", got.EntityID)
	assert.Equal(t, NotationKindURL, got.Kind)
}

// TestNotation_UpsertEmptyKindDefaultsToURL pins the documented
// default behavior — caller passes Kind="" and the row lands as
// `url`. Convenience for the orchestrator's most-common path.
func TestNotation_UpsertEmptyKindDefaultsToURL(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.UpsertNotation(context.Background(), Notation{
		Notation: "https://en.wikipedia.org/wiki/Tehran",
		EntityID: "wikipedia:tehran",
		// Kind unset
	}))

	got, _ := s.GetNotation(context.Background(), "https://en.wikipedia.org/wiki/Tehran")
	assert.Equal(t, NotationKindURL, got.Kind)
}

// TestNotation_UpsertOverwritesEntityID pins the notation-reassignment
// path: upserting an existing notation moves it to a new entity_id
// without erroring. Caller-driven (e.g. operator merges two entities).
func TestNotation_UpsertOverwritesEntityID(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")
	seedEntityForNotation(t, s, "wikipedia:tehran-merged", "wikipedia-article")

	ctx := context.Background()
	require.NoError(t, s.UpsertNotation(ctx, Notation{
		Notation: "wikipedia: Tehran",
		EntityID: "wikipedia:tehran",
		Kind: NotationKindShorthand,
	}))
	require.NoError(t, s.UpsertNotation(ctx, Notation{
		Notation: "wikipedia: Tehran",
		EntityID: "wikipedia:tehran-merged",
		Kind: NotationKindShorthand,
	}))

	got, err := s.GetNotation(ctx, "wikipedia: Tehran")
	require.NoError(t, err)
	assert.Equal(t, "wikipedia:tehran-merged", got.EntityID)
}

// TestNotation_DeleteForEntity drops every row pointing at one
// entity_id and reports the count. Other entities' notations
// untouched.
func TestNotation_DeleteForEntity(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")
	seedEntityForNotation(t, s, "wikipedia:berlin", "wikipedia-article")

	for _, n := range []Notation{
		{Notation: "https://en.wikipedia.org/wiki/Tehran", EntityID: "wikipedia:tehran", Kind: NotationKindURL},
		{Notation: "wikipedia: Tehran", EntityID: "wikipedia:tehran", Kind: NotationKindShorthand},
		{Notation: "https://en.wikipedia.org/wiki/Berlin", EntityID: "wikipedia:berlin", Kind: NotationKindURL},
	} {
		require.NoError(t, s.UpsertNotation(ctx, n))
	}

	dropped, err := s.DeleteNotationsForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	assert.Equal(t, 2, dropped, "two rows for wikipedia:tehran")

	_, err = s.GetNotation(ctx, "https://en.wikipedia.org/wiki/Tehran")
	assert.True(t, errors.Is(err, ErrNotFound))
	_, err = s.GetNotation(ctx, "wikipedia: Tehran")
	assert.True(t, errors.Is(err, ErrNotFound))

	got, err := s.GetNotation(ctx, "https://en.wikipedia.org/wiki/Berlin")
	require.NoError(t, err, "berlin notation must survive tehran's delete")
	assert.Equal(t, "wikipedia:berlin", got.EntityID)
}

// TestNotation_CascadeDeleteOnEntityRemoval pins the schema-level FK
// cascade: deleting an entity drops every notation row pointing at
// it, no separate notation cleanup needed.
func TestNotation_CascadeDeleteOnEntityRemoval(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.UpsertNotation(ctx, Notation{
		Notation: "https://en.wikipedia.org/wiki/Tehran",
		EntityID: "wikipedia:tehran",
		Kind: NotationKindURL,
	}))

	require.NoError(t, s.DeleteEntityCascade(ctx, "wikipedia:tehran"))

	_, err := s.GetNotation(ctx, "https://en.wikipedia.org/wiki/Tehran")
	assert.True(t, errors.Is(err, ErrNotFound),
		"notation must be cascade-deleted with its entity")
}

// TestNotation_ReplaceNotations covers the reindex re-derive shape
// (per ADR-0009 pattern, mirroring ReplaceProvenance). DELETE + INSERT
// in one transaction.
func TestNotation_ReplaceNotations(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")

	// Seed an existing notation that the replace should remove.
	require.NoError(t, s.UpsertNotation(ctx, Notation{
		Notation: "https://en.wikipedia.org/wiki/Tehran",
		EntityID: "wikipedia:tehran",
		Kind: NotationKindURL,
	}))

	// Replace with a different set.
	require.NoError(t, s.ReplaceNotations(ctx, "wikipedia:tehran", []Notation{
		{Notation: "wikipedia: Tehran", Kind: NotationKindShorthand},
		{Notation: "https://en.wikipedia.org/wiki/Tehran", Kind: NotationKindURL},
		{Notation: "https://fa.wikipedia.org/wiki/تهران", Kind: NotationKindURL},
	}))

	// Old-only-row gone, new rows present.
	for _, n := range []string{
		"wikipedia: Tehran",
		"https://en.wikipedia.org/wiki/Tehran",
		"https://fa.wikipedia.org/wiki/تهران",
	} {
		got, err := s.GetNotation(ctx, n)
		require.NoError(t, err, "notation %q present after replace", n)
		assert.Equal(t, "wikipedia:tehran", got.EntityID)
	}
}

// TestNotation_ReplaceWithEmptyClears pins the documented "empty
// entries clears notations" behavior.
func TestNotation_ReplaceWithEmptyClears(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.UpsertNotation(ctx, Notation{
		Notation: "https://en.wikipedia.org/wiki/Tehran",
		EntityID: "wikipedia:tehran",
		Kind: NotationKindURL,
	}))

	require.NoError(t, s.ReplaceNotations(ctx, "wikipedia:tehran", nil))

	_, err := s.GetNotation(ctx, "https://en.wikipedia.org/wiki/Tehran")
	assert.True(t, errors.Is(err, ErrNotFound))
}

// TestNotation_ReplaceRejectsMismatchedEntityID locks the loud-fail
// invariant — caller bug if a row's entity_id disagrees with the
// target the call names.
func TestNotation_ReplaceRejectsMismatchedEntityID(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForNotation(t, s, "wikipedia:tehran", "wikipedia-article")
	seedEntityForNotation(t, s, "wikipedia:berlin", "wikipedia-article")

	err := s.ReplaceNotations(ctx, "wikipedia:tehran", []Notation{
		{Notation: "https://en.wikipedia.org/wiki/Tehran", EntityID: "wikipedia:tehran"},
		{Notation: "https://en.wikipedia.org/wiki/Berlin", EntityID: "wikipedia:berlin"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entity_id")

	// Rollback: the first row must NOT have landed.
	_, err = s.GetNotation(ctx, "https://en.wikipedia.org/wiki/Tehran")
	assert.True(t, errors.Is(err, ErrNotFound),
		"transaction rolled back; first-row insert must not survive")
}

func TestNotation_ContractRejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	_, err := s.GetNotation(ctx, "")
	assert.Error(t, err, "GetNotation: empty notation")

	assert.Error(t, s.UpsertNotation(ctx, Notation{Notation: "", EntityID: "foo"}),
		"UpsertNotation: empty notation")
	assert.Error(t, s.UpsertNotation(ctx, Notation{Notation: "x", EntityID: ""}),
		"UpsertNotation: empty entity_id")

	_, err = s.DeleteNotationsForEntity(ctx, "")
	assert.Error(t, err, "DeleteNotationsForEntity: empty entity_id")

	assert.Error(t, s.ReplaceNotations(ctx, "", nil),
		"ReplaceNotations: empty entity_id")
}
