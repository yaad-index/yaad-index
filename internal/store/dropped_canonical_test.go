package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per ADR-0013 §3 / yaad-index a prior PR: the dropped-canonical
// counters surface canonical-vocabulary drift on `/v1/cv-status`.
// Tests exercise increment + idempotency + list-ordering +
// input-validation + persistence-across-restart at the store
// layer, before a prior PR wires them into the orchestrator + API.

func newStore(t *testing.T) Store {
	t.Helper()
	st, err := New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestDroppedCanonicalKind_FirstIncInsertsCount1(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	require.NoError(t, st.IncDroppedCanonicalKind(context.Background(), "wikipedia", "person"))
	rows, err := st.ListDroppedCanonicalKinds(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "wikipedia", rows[0].Plugin)
	assert.Equal(t, "person", rows[0].Kind)
	assert.Equal(t, int64(1), rows[0].Count)
	assert.False(t, rows[0].FirstSeenAt.IsZero())
	assert.False(t, rows[0].LastSeenAt.IsZero())
}

func TestDroppedCanonicalKind_RepeatedIncBumpsCountAndLastSeen(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person"))
	rowsAfter1, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	require.Len(t, rowsAfter1, 1)
	first := rowsAfter1[0].FirstSeenAt
	last1 := rowsAfter1[0].LastSeenAt

	// Second increment — count bumps, first_seen_at unchanged,
	// last_seen_at refreshes (>= prior, may be equal at sub-second
	// resolution since SQLite stores TEXT seconds).
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person"))
	rowsAfter2, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	require.Len(t, rowsAfter2, 1)
	assert.Equal(t, int64(2), rowsAfter2[0].Count)
	assert.Equal(t, first, rowsAfter2[0].FirstSeenAt, "first_seen_at pinned to earliest observation")
	assert.False(t, rowsAfter2[0].LastSeenAt.Before(last1),
		"last_seen_at must be >= prior observation")
}

func TestDroppedCanonicalKind_ListOrderingIsDeterministic(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	// Insert in non-sorted order; expect (plugin ASC, kind ASC).
	for _, pair := range [][2]string{
		{"wikipedia", "person"},
		{"bgg", "boardgame"},
		{"wikipedia", "boardgame"},
		{"bgg", "person"},
	} {
		require.NoError(t, st.IncDroppedCanonicalKind(ctx, pair[0], pair[1]))
	}
	rows, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	got := make([][2]string, len(rows))
	for i, r := range rows {
		got[i] = [2]string{r.Plugin, r.Kind}
	}
	assert.Equal(t, [][2]string{
		{"bgg", "boardgame"},
		{"bgg", "person"},
		{"wikipedia", "boardgame"},
		{"wikipedia", "person"},
	}, got)
}

func TestDroppedCanonicalKind_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	require.Error(t, st.IncDroppedCanonicalKind(ctx, "", "person"))
	require.Error(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", ""))
	require.Error(t, st.IncDroppedCanonicalEdge(ctx, "", "is_about"))
	require.Error(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", ""))
}

func TestDroppedCanonicalEdge_BasicLifecycle(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "bgg", "designed_by"))

	rows, err := st.ListDroppedCanonicalEdges(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "bgg", rows[0].Plugin)
	assert.Equal(t, "designed_by", rows[0].EdgeType)
	assert.Equal(t, int64(1), rows[0].Count)
	assert.Equal(t, "wikipedia", rows[1].Plugin)
	assert.Equal(t, "is_about", rows[1].EdgeType)
	assert.Equal(t, int64(2), rows[1].Count)
}

func TestDroppedCanonical_EmptyTablesListsCleanly(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	kinds, err := st.ListDroppedCanonicalKinds(context.Background())
	require.NoError(t, err)
	assert.Empty(t, kinds)
	edges, err := st.ListDroppedCanonicalEdges(context.Background())
	require.NoError(t, err)
	assert.Empty(t, edges)
}

// TestDroppedCanonicalKinds_ClearWipesAllRows pins the
// yaad-index #31 clear primitive: ClearDroppedCanonicalKinds
// removes every row in one call (the "reindex consumed drift"
// semantic). Idempotent on an already-empty table.
func TestDroppedCanonicalKinds_ClearWipesAllRows(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person"))
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "boardgame"))
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "bgg", "person"))

	pre, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	require.Len(t, pre, 3, "fixture sanity: three rows pre-clear")

	require.NoError(t, st.ClearDroppedCanonicalKinds(ctx))

	post, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	assert.Empty(t, post, "clear wipes every row regardless of plugin / kind")

	// Idempotent: second call on the empty table is a no-op success.
	require.NoError(t, st.ClearDroppedCanonicalKinds(ctx))
}

// TestDroppedCanonicalEdges_ClearWipesAllRows is the edge-type
// counterpart.
func TestDroppedCanonicalEdges_ClearWipesAllRows(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "bgg", "designed_by"))

	pre, err := st.ListDroppedCanonicalEdges(ctx)
	require.NoError(t, err)
	require.Len(t, pre, 2)

	require.NoError(t, st.ClearDroppedCanonicalEdges(ctx))

	post, err := st.ListDroppedCanonicalEdges(ctx)
	require.NoError(t, err)
	assert.Empty(t, post)

	require.NoError(t, st.ClearDroppedCanonicalEdges(ctx))
}

// TestDroppedCanonical_ClearTablesAreSiblings pins that clearing
// one table doesn't affect the other — the kind + edge counter
// tables stay isolated (mirrors the no-cross-contamination
// invariant the Inc paths already enforce).
func TestDroppedCanonical_ClearTablesAreSiblings(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "is_about"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))

	require.NoError(t, st.ClearDroppedCanonicalKinds(ctx))

	kinds, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	assert.Empty(t, kinds, "kinds cleared")

	edges, err := st.ListDroppedCanonicalEdges(ctx)
	require.NoError(t, err)
	require.Len(t, edges, 1, "edges untouched by kind-clear")
}

func TestDroppedCanonical_SeparateTablesNoCrossContamination(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := context.Background()
	// Same plugin emits the same name as both a dropped kind and
	// a dropped edge type — ensure they live in separate tables
	// and don't cross-contaminate the row sets.
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "is_about"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))

	kinds, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	require.Len(t, kinds, 1)
	assert.Equal(t, "is_about", kinds[0].Kind)

	edges, err := st.ListDroppedCanonicalEdges(ctx)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "is_about", edges[0].EdgeType)
}
