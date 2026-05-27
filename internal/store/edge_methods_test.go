package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedEntityForEdges writes a minimal entity with the given id + kind so
// CreateEdge has a valid endpoint. Provenance + edges intentionally
// empty — the edge tests don't assert on either.
func seedEntityForEdges(t *testing.T, s Store, id, kind string) {
	t.Helper()
	require.NoError(t, s.SaveEntity(context.Background(), &Entity{
		ID: id,
		Kind: kind,
		Data: map[string]any{"id": id},
		Provenance: nil,
		Edges: nil,
	}), "seed %s", id)
}

func TestCreateAndGetEdge_RoundTrip(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	seedEntityForEdges(t, s, "boardgame:brass-birmingham", "boardgame")
	seedEntityForEdges(t, s, "person:martin-wallace", "person")

	want := &Edge{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
		Metadata: map[string]any{
			"role": "lead designer",
		},
	}
	require.NoError(t, s.CreateEdge(ctx, want), "CreateEdge")
	assert.False(t, want.CreatedAt.IsZero(), "CreatedAt: want non-zero after CreateEdge")
	assert.False(t, want.UpdatedAt.IsZero(), "UpdatedAt: want non-zero after CreateEdge")

	got, err := s.GetEdgesFor(ctx, "boardgame:brass-birmingham", nil)
	require.NoError(t, err, "GetEdgesFor")
	require.Len(t, got, 1, "edges")
	assert.Equal(t, want.Type, got[0].Type)
	assert.Equal(t, want.From, got[0].From)
	assert.Equal(t, want.To, got[0].To)
	assert.Equal(t, want.Metadata, got[0].Metadata)
}

func TestGetEdgesFor_EmptyResultForUnknownEntity(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	got, err := s.GetEdgesFor(context.Background(), "boardgame:no-edges-here", nil)
	require.NoError(t, err, "GetEdgesFor on unknown id")
	assert.Empty(t, got, "edges")
}

func TestGetEdgesFor_TypeFilter(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")
	seedEntityForEdges(t, s, "person:lewis", "person")

	// Two edges of different types from the same source.
	require.NoError(t,
		s.CreateEdge(ctx, &Edge{Type: "authored_by", From: "book:lotr", To: "person:tolkien"}),
		"seed authored_by")
	// `dedicated_to` isn't in any registered plugin's edge_kinds — but
	// the store doesn't validate against the kind registry; that's an
	// API-layer concern. Used here to exercise the filter without
	// polluting the canonical edge_kinds set.
	require.NoError(t,
		s.CreateEdge(ctx, &Edge{Type: "dedicated_to", From: "book:lotr", To: "person:lewis"}),
		"seed dedicated_to")

	all, err := s.GetEdgesFor(ctx, "book:lotr", nil)
	require.NoError(t, err, "GetEdgesFor (all)")
	assert.Len(t, all, 2, "unfiltered: want 2 edges")

	authored, err := s.GetEdgesFor(ctx, "book:lotr", []string{"authored_by"})
	require.NoError(t, err, "GetEdgesFor (filtered)")
	require.Len(t, authored, 1, "filtered to authored_by: want exactly that one")
	assert.Equal(t, "authored_by", authored[0].Type)

	multi, err := s.GetEdgesFor(ctx, "book:lotr", []string{"authored_by", "dedicated_to"})
	require.NoError(t, err, "GetEdgesFor (multi-filter)")
	assert.Len(t, multi, 2, "multi-filter: want both edges")

	none, err := s.GetEdgesFor(ctx, "book:lotr", []string{"published_by"})
	require.NoError(t, err, "GetEdgesFor (no match)")
	assert.Empty(t, none, "filter excluding all edges")
}

func TestCreateEdge_MissingFromReturnsErrMissingEntity(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	// Only seed `to`; `from` is unknown.
	seedEntityForEdges(t, s, "person:martin-wallace", "person")

	err := s.CreateEdge(ctx, &Edge{
		Type: "designed_by",
		From: "boardgame:no-such-thing",
		To: "person:martin-wallace",
	})
	require.ErrorIs(t, err, ErrMissingEntity, "CreateEdge with unknown from")
}

func TestCreateEdge_MissingToReturnsErrMissingEntity(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	seedEntityForEdges(t, s, "boardgame:brass-birmingham", "boardgame")

	err := s.CreateEdge(ctx, &Edge{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:no-such-person",
	})
	require.ErrorIs(t, err, ErrMissingEntity, "CreateEdge with unknown to")
}

func TestCreateEdge_IsIdempotentOnSameTriple(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "boardgame:brass-birmingham", "boardgame")
	seedEntityForEdges(t, s, "person:martin-wallace", "person")

	// First create.
	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
		Metadata: map[string]any{
			"role": "lead designer",
		},
	}), "first CreateEdge")

	// Re-create with new metadata — same (type, from, to) triple. Per ADR
	// the (type, from, to) is the edge identity; re-posting updates
	// metadata + updated_at, doesn't dup.
	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
		Metadata: map[string]any{
			"role": "co-designer",
		},
	}), "second CreateEdge")

	got, err := s.GetEdgesFor(ctx, "boardgame:brass-birmingham", nil)
	require.NoError(t, err, "GetEdgesFor")
	require.Len(t, got, 1, "edges after re-create: want 1 (idempotent on triple)")
	assert.Equal(t, "co-designer", got[0].Metadata["role"])
}

func TestGetEdgesFor_OrdersByCreatedAtAscending(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "boardgame:bg", "boardgame")
	seedEntityForEdges(t, s, "person:p1", "person")
	seedEntityForEdges(t, s, "person:p2", "person")
	seedEntityForEdges(t, s, "person:p3", "person")

	// Inject deliberate created_at ordering (third row inserted first
	// chronologically would otherwise sort differently from a SaveEntity
	// loop's wall-clock ordering).
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := earlier.Add(time.Hour)
	later := mid.Add(time.Hour)

	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "designed_by", From: "boardgame:bg", To: "person:p2",
		CreatedAt: mid,
	}), "seed mid")
	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "designed_by", From: "boardgame:bg", To: "person:p1",
		CreatedAt: earlier,
	}), "seed earlier")
	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "designed_by", From: "boardgame:bg", To: "person:p3",
		CreatedAt: later,
	}), "seed later")

	got, err := s.GetEdgesFor(ctx, "boardgame:bg", nil)
	require.NoError(t, err, "GetEdgesFor")
	gotOrder := make([]string, len(got))
	for i, e := range got {
		gotOrder[i] = e.To
	}
	assert.Equal(t, []string{"person:p1", "person:p2", "person:p3"}, gotOrder,
		"order: want created_at ASC")
}

// --- UpdateEdgeTarget (#304 Cut B) -------------------------------------

func TestUpdateEdgeTarget_HappyPath(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")
	seedEntityForEdges(t, s, "person:tolkien-real", "person")

	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
		Metadata: map[string]any{"role": "primary"},
	}))
	priorEdges, _ := s.GetEdgesFor(ctx, "book:lotr", nil)
	require.Len(t, priorEdges, 1)
	priorCreatedAt := priorEdges[0].CreatedAt
	// Allow at least one wall-clock tick so updated_at can advance
	// past the prior row's timestamp on the rewrite.
	time.Sleep(2 * time.Millisecond)

	got, err := s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:tolkien", "person:tolkien-real")
	require.NoError(t, err)
	require.Equal(t, "person:tolkien-real", got.To)
	require.Equal(t, "book:lotr", got.From)
	require.Equal(t, "authored_by", got.Type)
	assert.Equal(t, map[string]any{"role": "primary"}, got.Metadata,
		"metadata preserved across rewrite")
	assert.True(t, got.CreatedAt.Equal(priorCreatedAt),
		"created_at preserved (audit-trail contract)")
	assert.True(t, got.UpdatedAt.After(priorCreatedAt),
		"updated_at advances to the rewrite moment")

	// Old edge gone.
	allFromLotr, err := s.GetEdgesFor(ctx, "book:lotr", nil)
	require.NoError(t, err)
	require.Len(t, allFromLotr, 1, "exactly one edge after rewrite")
	assert.Equal(t, "person:tolkien-real", allFromLotr[0].To)
}

func TestUpdateEdgeTarget_StaleTuple(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")

	// No edge exists; tuple is stale by definition.
	_, err := s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:wrong-target", "person:tolkien")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeStale,
		"missing tuple → ErrEdgeStale so handlers map to 409")
}

func TestUpdateEdgeTarget_StaleAfterConcurrentRewrite(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")
	seedEntityForEdges(t, s, "person:tolkien-real", "person")
	seedEntityForEdges(t, s, "person:c-s-lewis", "person")

	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
	}))

	// First rewrite succeeds.
	_, err := s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:tolkien", "person:tolkien-real")
	require.NoError(t, err)

	// Second rewrite, racing on the now-stale old tuple, must reject.
	_, err = s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:tolkien", "person:c-s-lewis")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeStale,
		"concurrent rewrite collapses: loser sees ErrEdgeStale")
}

func TestUpdateEdgeTarget_MissingNewTarget(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")

	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
	}))

	_, err := s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:tolkien", "person:does-not-exist")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingEntity,
		"new target FK failure → ErrMissingEntity so handlers map to 422")

	// Old edge MUST survive the failed rewrite (transaction rolled
	// back).
	got, err := s.GetEdgesFor(ctx, "book:lotr", nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "person:tolkien", got[0].To,
		"failed rewrite rolls back; original edge preserved")
}

func TestUpdateEdgeTarget_NoopRejected(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")

	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
	}))

	_, err := s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:tolkien", "person:tolkien")
	require.Error(t, err, "old==new is a no-op; caller should short-circuit")
}

func TestUpdateEdgeTarget_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	cases := []struct {
		name                            string
		from, edgeType, oldTo, newTo    string
	}{
		{"empty from", "", "authored_by", "old", "new"},
		{"empty edgeType", "book:lotr", "", "old", "new"},
		{"empty oldTo", "book:lotr", "authored_by", "", "new"},
		{"empty newTo", "book:lotr", "authored_by", "old", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.UpdateEdgeTarget(ctx, tc.from, tc.edgeType, tc.oldTo, tc.newTo)
			require.Error(t, err)
		})
	}
}

func TestUpdateEdgeTarget_PreservesMetadataAndNullMetadata(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedEntityForEdges(t, s, "book:lotr", "book")
	seedEntityForEdges(t, s, "person:tolkien", "person")
	seedEntityForEdges(t, s, "person:tolkien-real", "person")

	// Edge with nil metadata.
	require.NoError(t, s.CreateEdge(ctx, &Edge{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
	}))

	got, err := s.UpdateEdgeTarget(ctx, "book:lotr", "authored_by", "person:tolkien", "person:tolkien-real")
	require.NoError(t, err)
	assert.Nil(t, got.Metadata,
		"NULL metadata column survives rewrite — not coerced to empty map")
}
