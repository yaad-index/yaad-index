package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedEntityForAlias upserts a minimal entity row so the alias
// table's FK has a target. Alias tests don't care about entity
// content — just that the FK resolves.
func seedEntityForAlias(t *testing.T, s *sqliteStore, id, kind string) {
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

func TestAlias_ListEmptyForEntityWithoutAliases(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	got, err := s.ListAliasesForEntity(context.Background(), "wikipedia:tehran")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAlias_ReplaceThenList(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran"},
		{Alias: "تهران"},
		{Alias: "fa: تهران", Kind: AliasKindTyped},
	}))

	got, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	require.Len(t, got, 3)
	// ORDER BY alias ASC — verify deterministic shape.
	assert.Equal(t, "Tehran", got[0].Alias)
	assert.Equal(t, AliasKindBare, got[0].Kind, "default kind for bare alias")
	assert.Equal(t, "fa: تهران", got[1].Alias)
	assert.Equal(t, AliasKindTyped, got[1].Kind)
	assert.Equal(t, "تهران", got[2].Alias)
}

// TestAlias_ReplaceOverwritesEntireSet pins the re-derive shape —
// previous aliases are wiped, only the new set lands.
func TestAlias_ReplaceOverwritesEntireSet(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran"},
		{Alias: "Teheran"},
	}))

	// Replace with a single different alias.
	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "تهران"},
	}))

	got, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "تهران", got[0].Alias)
}

func TestAlias_ReplaceWithEmptyClears(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran"},
	}))
	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", nil))

	got, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestAlias_ReplaceMovesAliasBetweenEntities pins the alias-PK
// move shape: an alias that previously pointed at entity A and
// now lands in entity B's replace batch is rewritten in place.
func TestAlias_ReplaceMovesAliasBetweenEntities(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")
	seedEntityForAlias(t, s, "wikipedia:tehran-merged", "wikipedia-article")

	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran"},
	}))
	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran-merged", []Alias{
		{Alias: "Tehran"},
	}))

	// Original entity no longer owns it.
	orig, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	assert.Empty(t, orig)

	// Merged entity now owns it.
	merged, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran-merged")
	require.NoError(t, err)
	require.Len(t, merged, 1)
	assert.Equal(t, "wikipedia:tehran-merged", merged[0].EntityID)
}

// TestAlias_ReplaceRejectsMismatchedEntityID locks the same
// defensive shape ReplaceNotations uses.
func TestAlias_ReplaceRejectsMismatchedEntityID(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")
	seedEntityForAlias(t, s, "wikipedia:berlin", "wikipedia-article")

	err := s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran", EntityID: "wikipedia:tehran"},
		{Alias: "Berlin", EntityID: "wikipedia:berlin"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entity_id")

	// Transaction rolled back — neither row survives.
	got, err := s.ListAliasesForEntity(context.Background(), "wikipedia:tehran")
	require.NoError(t, err)
	assert.Empty(t, got, "rollback must drop the first insert too")
}

func TestAlias_ReplaceRejectsDuplicateWithinBatch(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	err := s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran"},
		{Alias: "Tehran"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	got, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAlias_ReplaceRejectsEmptyAlias(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	err := s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: ""},
	})
	require.Error(t, err)
}

// TestAlias_CascadeDeleteOnEntityRemoval pins the schema-level
// FK cascade.
func TestAlias_CascadeDeleteOnEntityRemoval(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForAlias(t, s, "wikipedia:tehran", "wikipedia-article")

	require.NoError(t, s.ReplaceAliases(ctx, "wikipedia:tehran", []Alias{
		{Alias: "Tehran"},
	}))

	require.NoError(t, s.DeleteEntityCascade(ctx, "wikipedia:tehran"))

	got, err := s.ListAliasesForEntity(ctx, "wikipedia:tehran")
	require.NoError(t, err)
	assert.Empty(t, got, "aliases must cascade-delete with their entity")
}

func TestAlias_ContractRejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	_, err := s.ListAliasesForEntity(ctx, "")
	assert.Error(t, err, "ListAliasesForEntity: empty entity_id")

	assert.Error(t, s.ReplaceAliases(ctx, "", nil),
		"ReplaceAliases: empty entity_id")
}

// TestAlias_TypedAliasPrefix locks the shape gate used at write
// time to classify aliases. Prefix must be non-empty, ascii-clean
// (no whitespace), separator is ": " exactly.
func TestAlias_TypedAliasPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in            string
		wantPrefix    string
		wantLabel     string
		wantOK        bool
	}{
		{"author: Piranesi", "author", "Piranesi", true},
		{"wikipedia: Tehran", "wikipedia", "Tehran", true},
		{"fa: تهران", "fa", "تهران", true},
		// Bare strings — no ": " separator.
		{"Tehran", "", "", false},
		{"Piranesi (novel)", "", "", false},
		// Edge cases the gate must reject.
		{"author:Piranesi", "", "", false},     // missing space after colon
		{": Piranesi", "", "", false},          // empty prefix
		{"a b: Piranesi", "", "", false},       // whitespace in prefix
		{"author: ", "", "", false},            // empty label
		{"author:", "", "", false},             // no separator + no label
		{"nested:key: value", "", "", false},   // colon in prefix
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			p, l, ok := TypedAliasPrefix(c.in)
			assert.Equal(t, c.wantOK, ok, "ok mismatch for %q", c.in)
			assert.Equal(t, c.wantPrefix, p)
			assert.Equal(t, c.wantLabel, l)
		})
	}
}

// TestAlias_ReplaceUnknownEntityRollsBack pins the FK shape —
// an alias pointing at a non-existent entity must reject.
func TestAlias_ReplaceUnknownEntityRollsBack(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	err := s.ReplaceAliases(ctx, "wikipedia:missing", []Alias{
		{Alias: "Missing"},
	})
	require.Error(t, err, "FK to entities(id) must reject")
	_ = errors.Unwrap(err) // ensure error is well-formed
}
