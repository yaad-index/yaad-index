package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGapCallableCandidates_KindFilter pins the #385 store-level kind
// filter on both ListGapCallableCandidates and CountGapCallableCandidates:
// an empty kind preserves the unfiltered behavior, a non-empty kind
// scopes both the listing and the count to that kind.
func TestGapCallableCandidates_KindFilter(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	// Three gap-callable entities (gap_call_done_at NULL + one unfilled
	// gap_state entry): two boardgame, one person.
	for _, e := range []struct{ id, kind string }{
		{"boardgame:a", "boardgame"},
		{"boardgame:b", "boardgame"},
		{"person:x", "person"},
	} {
		require.NoError(t, s.SaveEntity(ctx, &Entity{
			ID:       e.id,
			Kind:     e.kind,
			Data:     map[string]any{"id": e.id},
			GapState: map[string]GapStateEntry{"summary": {}},
		}))
	}

	// List: empty kind → all three; kind=boardgame → two; kind=person → one.
	all, err := s.ListGapCallableCandidates(ctx, "", 50, "")
	require.NoError(t, err)
	assert.Len(t, all, 3, "empty kind lists all")

	bg, err := s.ListGapCallableCandidates(ctx, "", 50, "boardgame")
	require.NoError(t, err)
	require.Len(t, bg, 2, "kind=boardgame lists only boardgame")
	for _, e := range bg {
		assert.Equal(t, "boardgame", e.Kind)
	}

	ppl, err := s.ListGapCallableCandidates(ctx, "", 50, "person")
	require.NoError(t, err)
	require.Len(t, ppl, 1)
	assert.Equal(t, "person:x", ppl[0].ID)

	// Count mirrors the same scoping.
	total, err := s.CountGapCallableCandidates(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, 3, total, "empty kind counts all")

	bgCount, err := s.CountGapCallableCandidates(ctx, "boardgame")
	require.NoError(t, err)
	assert.Equal(t, 2, bgCount, "kind=boardgame count")

	pplCount, err := s.CountGapCallableCandidates(ctx, "person")
	require.NoError(t, err)
	assert.Equal(t, 1, pplCount, "kind=person count")
}
