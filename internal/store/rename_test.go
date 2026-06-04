package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenameEntity_ReKeysRowEdgesProvenanceAliases pins the #425 Cut 2
// re-key: a rename moves the row, both edge directions, provenance,
// notations, and aliases onto the new id in one transaction, and the
// bare old slug keeps resolving to the new id.
func TestRenameEntity_ReKeysRowEdgesProvenanceAliases(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	old := &Entity{
		ID: "x:old", Kind: "x", Data: map[string]any{"id": "x:old", "title": "Old"},
		Provenance: []ProvenanceEntry{{Source: "user", FilledAt: ts("2026-01-01T00:00:00Z"), OK: true}},
	}
	out := &Entity{ID: "x:out", Kind: "x", Data: map[string]any{"id": "x:out"}}
	in := &Entity{ID: "x:in", Kind: "x", Data: map[string]any{"id": "x:in"}}
	require.NoError(t, s.SaveEntity(ctx, old))
	require.NoError(t, s.SaveEntity(ctx, out))
	require.NoError(t, s.SaveEntity(ctx, in))
	require.NoError(t, s.CreateEdge(ctx, &Edge{Type: "rel", From: old.ID, To: out.ID})) // outbound
	require.NoError(t, s.CreateEdge(ctx, &Edge{Type: "rel", From: in.ID, To: old.ID}))  // inbound
	require.NoError(t, s.ReplaceAliases(ctx, old.ID, []Alias{{Alias: "old", EntityID: old.ID, Kind: AliasKindBare}}))
	require.NoError(t, s.ReplaceNotations(ctx, old.ID, []Notation{{Notation: "oldnote", EntityID: old.ID, Kind: NotationKindShorthand}}))

	require.NoError(t, s.RenameEntity(ctx, old.ID, "x:new", map[string]any{"id": "x:new", "title": "New"}))

	// Old row gone, new row present with the new payload.
	_, err := s.GetEntity(ctx, old.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	got, err := s.GetEntity(ctx, "x:new")
	require.NoError(t, err)
	assert.Equal(t, "New", got.Data["title"])

	// Outbound edge re-pointed (x:new -> x:out), none left on the old id.
	outEdges, err := s.GetEdgesFor(ctx, "x:new", nil)
	require.NoError(t, err)
	require.Len(t, outEdges, 1)
	assert.Equal(t, "x:out", outEdges[0].To)
	staleOut, err := s.GetEdgesFor(ctx, "x:old", nil)
	require.NoError(t, err)
	assert.Empty(t, staleOut)

	// Inbound edge re-pointed (x:in -> x:new).
	inEdges, err := s.GetEdgesTo(ctx, "x:new", nil)
	require.NoError(t, err)
	require.Len(t, inEdges, 1)
	assert.Equal(t, "x:in", inEdges[0].From)

	// Provenance + notations re-pointed onto the new id, gone from the old.
	assert.Equal(t, 1, countRows(t, s, "provenance", "target_entity_id", "x:new"))
	assert.Equal(t, 0, countRows(t, s, "provenance", "target_entity_id", "x:old"))
	assert.Equal(t, 1, countRows(t, s, "entity_notations", "entity_id", "x:new"))
	assert.Equal(t, 0, countRows(t, s, "entity_notations", "entity_id", "x:old"))

	// The bare old slug resolves to the new id (back-compat alias).
	resolved, err := s.ResolveAlias(ctx, "old", "x")
	require.NoError(t, err)
	assert.Equal(t, "x:new", resolved)
}

// TestRenameEntity_GuaranteesOldSlugAliasWithoutPriorAlias pins that the
// old slug resolves to the new id even when the old entity carried no
// self-alias (an operator could have cleared it) — RenameEntity inserts
// the back-compat alias explicitly, not only by re-homing an existing one.
func TestRenameEntity_GuaranteesOldSlugAliasWithoutPriorAlias(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveEntity(ctx, &Entity{ID: "x:old", Kind: "x", Data: map[string]any{"id": "x:old"}}))
	require.NoError(t, s.RenameEntity(ctx, "x:old", "x:new", map[string]any{"id": "x:new"}))

	resolved, err := s.ResolveAlias(ctx, "old", "x")
	require.NoError(t, err)
	assert.Equal(t, "x:new", resolved)
}

func TestRenameEntity_MissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	err := s.RenameEntity(context.Background(), "x:nope", "x:new", map[string]any{"id": "x:new"})
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRenameEntity_RejectsSameOrEmptyID(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	assert.Error(t, s.RenameEntity(ctx, "x:a", "x:a", nil), "same id")
	assert.Error(t, s.RenameEntity(ctx, "", "x:a", nil), "empty old id")
	assert.Error(t, s.RenameEntity(ctx, "x:a", "", nil), "empty new id")
}

// countRows is a tiny helper for asserting re-key fan-out across the
// vault-derived tables.
func countRows(t *testing.T, s *sqliteStore, table, col, val string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table+" WHERE "+col+" = ?", val).Scan(&n))
	return n
}
