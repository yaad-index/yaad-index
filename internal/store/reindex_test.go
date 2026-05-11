package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReindexFiles_RoundTrip(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	want := ReindexFile{
		Path: "/vault/wikipedia-article/foo.md",
		Mtime: now.Add(-time.Hour),
		ContentHash: "abc123",
		LastIndexedAt: now,
		EntityID: "wikipedia:foo",
		EntityKind: "wikipedia-article",
	}
	require.NoError(t, s.UpsertReindexFile(ctx, want))

	got, err := s.ListReindexFiles(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, want.Path, got[0].Path)
	assert.Equal(t, want.ContentHash, got[0].ContentHash)
	assert.Equal(t, want.EntityID, got[0].EntityID)
	assert.Equal(t, want.EntityKind, got[0].EntityKind)
	assert.True(t, want.Mtime.Equal(got[0].Mtime), "mtime: want %s, got %s", want.Mtime, got[0].Mtime)
	assert.True(t, want.LastIndexedAt.Equal(got[0].LastIndexedAt))
}

func TestReindexFiles_UpsertReplacesByPath(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	first := ReindexFile{
		Path: "/vault/x.md",
		Mtime: time.Now().UTC(),
		ContentHash: "v1",
		LastIndexedAt: time.Now().UTC(),
		EntityID: "x:1",
		EntityKind: "x",
	}
	require.NoError(t, s.UpsertReindexFile(ctx, first))

	first.ContentHash = "v2"
	first.LastIndexedAt = first.LastIndexedAt.Add(time.Minute)
	require.NoError(t, s.UpsertReindexFile(ctx, first))

	rows, err := s.ListReindexFiles(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "upsert replaces; no second row")
	assert.Equal(t, "v2", rows[0].ContentHash)
}

func TestReindexFiles_DeleteByPath(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertReindexFile(ctx, ReindexFile{
		Path: "/vault/keep.md", Mtime: time.Now().UTC(), ContentHash: "k",
		LastIndexedAt: time.Now().UTC(), EntityID: "x:keep", EntityKind: "x",
	}))
	require.NoError(t, s.UpsertReindexFile(ctx, ReindexFile{
		Path: "/vault/drop.md", Mtime: time.Now().UTC(), ContentHash: "d",
		LastIndexedAt: time.Now().UTC(), EntityID: "x:drop", EntityKind: "x",
	}))

	dropped, err := s.DeleteReindexFile(ctx, "/vault/drop.md")
	require.NoError(t, err)
	assert.True(t, dropped)

	dropped2, err := s.DeleteReindexFile(ctx, "/vault/drop.md")
	require.NoError(t, err)
	assert.False(t, dropped2, "second delete: row already gone")

	rows, err := s.ListReindexFiles(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "/vault/keep.md", rows[0].Path)
}

func TestReindexFiles_UpsertRejectsEmptyArgs(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	assert.Error(t, s.UpsertReindexFile(ctx, ReindexFile{EntityID: "x"}), "empty path")
	assert.Error(t, s.UpsertReindexFile(ctx, ReindexFile{Path: "/x.md"}), "empty entity_id")
}

func TestDeleteEntityCascade_RemovesEntityEdgesProvenance(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	// Seed two entities and an edge between them, plus provenance on each.
	a := &Entity{
		ID: "x:a", Kind: "x", Data: map[string]any{"k": "a"},
		Provenance: []ProvenanceEntry{{Source: "src:a", FetchedAt: ts("2026-01-01T00:00:00Z"), OK: true}},
	}
	b := &Entity{
		ID: "x:b", Kind: "x", Data: map[string]any{"k": "b"},
		Provenance: []ProvenanceEntry{{Source: "src:b", FetchedAt: ts("2026-01-02T00:00:00Z"), OK: true}},
	}
	require.NoError(t, s.SaveEntity(ctx, a))
	require.NoError(t, s.SaveEntity(ctx, b))
	require.NoError(t, s.CreateEdge(ctx, &Edge{Type: "rel", From: a.ID, To: b.ID}))

	require.NoError(t, s.DeleteEntityCascade(ctx, a.ID))

	_, err := s.GetEntity(ctx, a.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	stillThere, err := s.GetEntity(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "x:b", stillThere.ID)

	// Edges referencing the deleted entity (in either direction) gone.
	bEdges, err := s.GetEdgesFor(ctx, b.ID, nil)
	require.NoError(t, err)
	assert.Empty(t, bEdges, "edges into deleted entity should be cascade-removed")
}

func TestDeleteEntityCascade_MissingEntityReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	err := s.DeleteEntityCascade(context.Background(), "x:nope")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestWipeDerivedState_DropsEntitiesEdgesProvenanceBookkeeping(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "x:1", Kind: "x", Data: map[string]any{"k": "v"},
		Provenance: []ProvenanceEntry{{Source: "src", FetchedAt: ts("2026-01-01T00:00:00Z"), OK: true}},
	}))
	require.NoError(t, s.UpsertReindexFile(ctx, ReindexFile{
		Path: "/v/x.md", Mtime: time.Now().UTC(), ContentHash: "h",
		LastIndexedAt: time.Now().UTC(), EntityID: "x:1", EntityKind: "x",
	}))

	require.NoError(t, s.WipeDerivedState(ctx))

	_, err := s.GetEntity(ctx, "x:1")
	assert.ErrorIs(t, err, ErrNotFound)
	rows, err := s.ListReindexFiles(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestWipeDerivedState_PreservesPluginCache locks the negative case:
// WipeDerivedState targets only state derivable from the vault.
// Plugin capabilities cache is loader state — independent of the
// vault — and must survive the wipe.
//
// (The fill_token surface that this test previously also covered is
// gone as of a prior PR's drop-fill_token rewrite — the entity ID is the
// durable callback and there's no in-memory or DB-side token state
// to verify. The fill_tokens table itself was dropped via migration
// 005; the negative case lives in sqlite_test.go.)
func TestWipeDerivedState_PreservesPluginCache(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertPluginCapabilities(ctx, "wikipedia", "1.0", []byte(`{}`)))
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "x:bg", Kind: "x", Data: map[string]any{"k": "v"},
	}))

	require.NoError(t, s.WipeDerivedState(ctx))

	_, found, err := s.GetPluginCapabilities(ctx, "wikipedia")
	require.NoError(t, err)
	assert.True(t, found, "plugin cache row preserved across wipe")
}
