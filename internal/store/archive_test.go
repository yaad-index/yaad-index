package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchiveRestoreEntity_RoundTrip pins the basic ADR-0018 step 2
// contract: ArchiveEntity flips a row to archived (archived_at non-
// NULL); RestoreEntity flips it back (archived_at NULL). Both are
// idempotent on already-in-state rows.
func TestArchiveRestoreEntity_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "person:archive-test", Kind: "person",
		Data: map[string]any{"name": "Archive Test"},
	}))

	// Active baseline.
	got, err := s.GetEntity(ctx, "person:archive-test")
	require.NoError(t, err)
	require.Nil(t, got.ArchivedAt, "fresh entity is active (archived_at NULL)")

	// Archive flips the flag.
	require.NoError(t, s.ArchiveEntity(ctx, "person:archive-test"))
	got, err = s.GetEntity(ctx, "person:archive-test")
	require.NoError(t, err)
	require.NotNil(t, got.ArchivedAt, "post-archive: archived_at non-NULL")
	firstArchive := *got.ArchivedAt

	// Idempotence: re-archiving doesn't re-stamp the timestamp
	// (COALESCE semantics in the UPDATE).
	require.NoError(t, s.ArchiveEntity(ctx, "person:archive-test"))
	got, err = s.GetEntity(ctx, "person:archive-test")
	require.NoError(t, err)
	require.NotNil(t, got.ArchivedAt)
	assert.True(t, firstArchive.Equal(*got.ArchivedAt),
		"archived_at unchanged on re-archive (preserves original archive event)")

	// Restore flips it back.
	require.NoError(t, s.RestoreEntity(ctx, "person:archive-test"))
	got, err = s.GetEntity(ctx, "person:archive-test")
	require.NoError(t, err)
	assert.Nil(t, got.ArchivedAt, "post-restore: archived_at NULL")

	// Idempotence: re-restoring is also a no-op.
	require.NoError(t, s.RestoreEntity(ctx, "person:archive-test"))
	got, err = s.GetEntity(ctx, "person:archive-test")
	require.NoError(t, err)
	assert.Nil(t, got.ArchivedAt)
}

// TestArchiveRestoreEntity_NotFound covers the missing-id path:
// neither toggle succeeds when no row exists.
func TestArchiveRestoreEntity_NotFound(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	err := s.ArchiveEntity(ctx, "person:never-existed")
	assert.True(t, errors.Is(err, ErrNotFound),
		"ArchiveEntity on missing id: want ErrNotFound, got %v", err)

	err = s.RestoreEntity(ctx, "person:never-existed")
	assert.True(t, errors.Is(err, ErrNotFound),
		"RestoreEntity on missing id: want ErrNotFound, got %v", err)
}

// TestSearch_ArchivedFilter pins the ADR-0018 step 2 list/search
// default-exclude + include + only-archived shapes. Three rows in
// fixture: one active "Brass: Birmingham", one archived "Brass:
// Lancashire", one unrelated "Caverna" — searches that would match
// Brass-prefix should partition correctly across the three filters.
func TestSearch_ArchivedFilter(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:brass-birmingham", Kind: "boardgame",
		Data: map[string]any{"title": "Brass: Birmingham"},
	}))
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:brass-lancashire", Kind: "boardgame",
		Data: map[string]any{"title": "Brass: Lancashire"},
	}))
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:caverna", Kind: "boardgame",
		Data: map[string]any{"title": "Caverna"},
	}))
	require.NoError(t, s.ArchiveEntity(ctx, "boardgame:brass-lancashire"))

	t.Run("default ArchivedExclude hides archived", func(t *testing.T) {
		hits, total, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedExclude, false)
		require.NoError(t, err)
		require.Equal(t, 1, total, "want 1 active Brass hit (Birmingham)")
		require.Len(t, hits, 1)
		assert.Equal(t, "boardgame:brass-birmingham", hits[0].ID)
	})

	t.Run("ArchivedInclude returns active+archived", func(t *testing.T) {
		hits, total, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedInclude, false)
		require.NoError(t, err)
		require.Equal(t, 2, total, "want both Brass hits (Birmingham + Lancashire)")
		ids := []string{hits[0].ID, hits[1].ID}
		assert.Contains(t, ids, "boardgame:brass-birmingham")
		assert.Contains(t, ids, "boardgame:brass-lancashire")
	})

	t.Run("ArchivedOnly returns archived only", func(t *testing.T) {
		hits, total, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedOnly, false)
		require.NoError(t, err)
		require.Equal(t, 1, total, "want 1 archived Brass hit (Lancashire)")
		require.Len(t, hits, 1)
		assert.Equal(t, "boardgame:brass-lancashire", hits[0].ID)
	})
}
