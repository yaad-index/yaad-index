package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListGapCallableSurfaceableCandidates pins the #523 predicate: drop
// pure-pointer stubs (nil data → the JSON literal "null") and all-filled
// or all-deferred rows; keep NULL-gap_state config-gap rows and rows that
// carry an unfilled, non-deferred gap.
func TestListGapCallableSurfaceableCandidates(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	now := time.Now().UTC()

	// stub: nil Data → data column "null"; no gap_state → dropped.
	require.NoError(t, s.SaveEntity(ctx, &Entity{ID: "k:stub", Kind: "boardgame"}))
	// all-filled: data + gap_state whose only entry is filled → dropped.
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "k:filled", Kind: "boardgame", Data: map[string]any{"x": 1},
		GapState: map[string]GapStateEntry{"summary": {Source: "agent", FilledAt: &now}},
	}))
	// all-deferred: data + gap_state whose only entry is deferred → dropped.
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "k:deferred", Kind: "boardgame", Data: map[string]any{"x": 1},
		GapState: map[string]GapStateEntry{"summary": {Deferred: true, DeferredAt: &now}},
	}))
	// config-gap: data, NULL gap_state → kept (list surfaces these).
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "k:configgap", Kind: "boardgame", Data: map[string]any{"x": 1},
	}))
	// unfilled: data + gap_state with an unfilled entry → kept.
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "k:unfilled", Kind: "boardgame", Data: map[string]any{"x": 1},
		GapState: map[string]GapStateEntry{"summary": {}},
	}))

	got, err := s.ListGapCallableSurfaceableCandidates(ctx, "", 50, "")
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	assert.True(t, ids["k:configgap"], "config-gap (NULL gap_state) kept")
	assert.True(t, ids["k:unfilled"], "unfilled gap kept")
	assert.False(t, ids["k:stub"], "pure-pointer stub (nil data) dropped")
	assert.False(t, ids["k:filled"], "all-filled dropped")
	assert.False(t, ids["k:deferred"], "all-deferred dropped")
	assert.Len(t, got, 2)
}
