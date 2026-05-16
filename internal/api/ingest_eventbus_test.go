// Phase 2.2.B.2 integration tests for ingestTracker emissions per
// ADR-0024 §"Internal event bus (v1 core)". The four emission
// sites are:
//
//   - persistEnvelope (plugin-path UpsertEntity) → entity.created
//     gated on cache-hit detection (pre-upsert GetEntity probe).
//   - runSimulation fixture-path UpsertEntity → entity.created
//     gated the same way.
//   - materializeThinLabelRowsFromEdges → entity.created per new
//     thin canonical-label row.
//   - persistCanonicalEdges → entity.edge_added per landed edge.
//
// All ingest emissions carry SourceAgent (ingest is the
// agent-initiated mutation path via POST /v1/ingest).

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// newAPIWithBusForIngest wires a real eventbus through the handler
// + registers the seed fixture plugins. Returns the handler, store,
// and bus so tests can subscribe before issuing /v1/ingest.
func newAPIWithBusForIngest(t *testing.T) (http.Handler, store.Store, eventbus.Bus) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	bus := eventbus.NewMemoryBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithEventBus(bus),
	)
	return h, st, bus
}

// TestIngest_FixturePath_FreshIngest_EmitsEntityCreated covers the
// runSimulation fixture path: a fresh ingest (entity not yet in the
// store) emits one entity.created with SourceAgent carrying the
// fixture's ID + kind. The cache-hit probe correctly identifies
// the new path.
func TestIngest_FixturePath_FreshIngest_EmitsEntityCreated(t *testing.T) {
	t.Parallel()
	h, st, bus := newAPIWithBusForIngest(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	// Verify the entity doesn't pre-exist (sanity).
	_, err := st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	require.True(t, errors.Is(err, store.ErrNotFound), "entity should not pre-exist")

	rec := postIngest(t, h, map[string]any{
		"url":          "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()

	// Exactly one entity.created for the primary entity. The
	// brass-birmingham fixture has no canonical edges, so no
	// thin-row materialization or persistCanonicalEdges emit.
	var creates int
	var got eventbus.EntityCreatedEvent
	for _, e := range events {
		if ev, ok := e.(eventbus.EntityCreatedEvent); ok {
			creates++
			got = ev
		}
	}
	require.Equal(t, 1, creates, "exactly one entity.created on fresh ingest; events=%+v", events)
	assert.Equal(t, "boardgame:brass-birmingham", got.ID)
	assert.Equal(t, "boardgame", got.Kind)
	assert.Equal(t, eventbus.SourceAgent, got.SourceTag)
	assert.False(t, got.At.IsZero(), "publisher stamps occurred-at")
}

// TestIngest_FixturePath_ReIngest_DoesNotReEmitEntityCreated covers
// the load-bearing cache-hit-no-create rule: a re-ingest of the
// same URL on a known entity does NOT publish entity.created
// again. Per ADR-0024 §"Cache-hit re-fetch semantics".
func TestIngest_FixturePath_ReIngest_DoesNotReEmitEntityCreated(t *testing.T) {
	t.Parallel()
	h, _, bus := newAPIWithBusForIngest(t)

	// First ingest — entity becomes known. Capture set up
	// after to filter out this first event.
	rec := postIngest(t, h, map[string]any{
		"url":          "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	// Second ingest — same URL, entity already exists.
	rec = postIngest(t, h, map[string]any{
		"url":           "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds":  2,
		"force_refetch": true, // force re-fetch through the simulator
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()
	for _, e := range events {
		_, isCreate := e.(eventbus.EntityCreatedEvent)
		assert.False(t, isCreate,
			"re-ingest of known entity must NOT publish entity.created (ADR cache-hit semantics)")
	}
}

// TestIngest_TrackerWithoutBus_DoesNotPanic: a handler constructed
// without WithEventBus default-wires to a no-subscriber bus.
// Ingest still succeeds; no emission visible to anyone outside
// the handler.
func TestIngest_TrackerWithoutBus_DoesNotPanic(t *testing.T) {
	t.Parallel()
	h, _ := newAPIWithStore(t) // no WithEventBus

	rec := postIngest(t, h, map[string]any{
		"url":          "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	assert.Equal(t, http.StatusOK, rec.Code,
		"ingest succeeds without an explicit bus; body=%s", rec.Body.String())
}

// newCanonicalEdgesFixture constructs a handler wired with a
// canonical-edges-emitting plugin + canonical guard so the ingest
// path runs persistCanonicalEdges + materializeThinLabelRowsFromEdges.
// Returns the handler, store, bus, and the URL the test should ingest.
func newCanonicalEdgesFixture(t *testing.T) (http.Handler, store.Store, eventbus.Bus, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	bus := eventbus.NewMemoryBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	const ingestURL = "https://example.test/ce-source/abc"
	const sourceID = "source:ce-fixture"

	plug := &fixture.Plugin{
		NameValue: "ce-fixture",
		MatchFunc: func(u string) bool { return u == ingestURL },
		CapabilitiesValue: plugins.Capabilities{
			Name: "ce-fixture",
			EntityKinds: []plugins.KindSpec{
				{Name: "source", Description: "Test source"},
			},
			EdgeKinds: []plugins.KindSpec{
				{Name: "is_about", Description: "Source covers topic", FromKind: "source", ToKind: "boardgame"},
			},
		},
		FetchFunc: func(_ context.Context, _ string) (*plugins.FetchResult, error) {
			return &plugins.FetchResult{
				Entity: &store.Entity{
					ID:   sourceID,
					Kind: "source",
					Data: map[string]any{"title": "CE Fixture"},
				},
				CanonicalEdges: []*store.Edge{
					{
						Type: "is_about",
						From: sourceID,
						To:   "boardgame:brass-birmingham",
					},
				},
			}, nil
		},
	}
	reg := plugins.NewRegistry()
	reg.Register(plug)

	// Canonical guard: allow `source` + `boardgame` kinds and
	// `is_about` edge type so persistCanonicalEdges + materialize
	// don't drop the test data.
	guard := config.NewCanonicalGuard([]string{"source", "boardgame", canonical.SourceTypeKind}, []string{"is_about"})

	h := NewHandlerWithRegistry(logger, st, reg,
		WithEventBus(bus),
		WithCanonicalGuard(guard),
	)
	return h, st, bus, ingestURL
}

// TestIngest_CanonicalEdges_EmitsThinRowCreatesAndEdgeAdds covers
// persistCanonicalEdges + materializeThinLabelRowsFromEdges:
// ingesting an entity with a canonical edge to a not-yet-known
// label produces entity.created for the source + entity.created
// for the thin label row + entity.edge_added for the canonical
// edge. All with SourceAgent.
func TestIngest_CanonicalEdges_EmitsThinRowCreatesAndEdgeAdds(t *testing.T) {
	t.Parallel()
	h, _, bus, ingestURL := newCanonicalEdgesFixture(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := postIngest(t, h, map[string]any{
		"url":          ingestURL,
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()

	var creates []eventbus.EntityCreatedEvent
	var edges []eventbus.EntityEdgeAddedEvent
	for _, e := range events {
		switch ev := e.(type) {
		case eventbus.EntityCreatedEvent:
			creates = append(creates, ev)
		case eventbus.EntityEdgeAddedEvent:
			edges = append(edges, ev)
		}
	}

	// 2 entity.created (source entity + thin boardgame label row)
	// + 1 entity.edge_added.
	require.Len(t, creates, 2, "creates: source + thin label; got %+v", creates)
	require.Len(t, edges, 1, "one canonical edge; got %+v", edges)

	createdIDs := map[string]string{}
	for _, c := range creates {
		createdIDs[c.ID] = c.Kind
		assert.Equal(t, eventbus.SourceAgent, c.SourceTag)
	}
	assert.Equal(t, "source", createdIDs["source:ce-fixture"])
	assert.Equal(t, "boardgame", createdIDs["boardgame:brass-birmingham"])

	got := edges[0]
	assert.Equal(t, "source:ce-fixture", got.FromID)
	assert.Equal(t, "boardgame:brass-birmingham", got.ToID)
	assert.Equal(t, "is_about", got.EdgeType)
	assert.Equal(t, eventbus.SourceAgent, got.SourceTag)
}

// TestIngest_CanonicalEdges_PreExistingThinRow_NoDuplicateCreate:
// when the thin canonical-label row already exists (e.g. seeded by
// a prior ingest or an operator-fill on a different source), the
// materialize path's skip-if-exists branch fires — no second
// entity.created for that label. The entity.edge_added still
// fires (each edge is new).
func TestIngest_CanonicalEdges_PreExistingThinRow_NoDuplicateCreate(t *testing.T) {
	t.Parallel()
	h, st, bus, ingestURL := newCanonicalEdgesFixture(t)

	// Pre-seed the thin label row so the materialize path skips
	// the create branch on this ingest.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   "boardgame:brass-birmingham",
		Kind: "boardgame",
	}))

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := postIngest(t, h, map[string]any{
		"url":          ingestURL,
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()
	createdIDs := map[string]struct{}{}
	var edgeCount int
	for _, e := range events {
		switch ev := e.(type) {
		case eventbus.EntityCreatedEvent:
			createdIDs[ev.ID] = struct{}{}
		case eventbus.EntityEdgeAddedEvent:
			edgeCount++
		}
	}
	// Source entity is still new → its entity.created fires.
	// boardgame:brass-birmingham was pre-seeded → no
	// entity.created.
	assert.Contains(t, createdIDs, "source:ce-fixture")
	assert.NotContains(t, createdIDs, "boardgame:brass-birmingham",
		"pre-existing thin row must not re-emit entity.created")
	assert.Equal(t, 1, edgeCount, "edge still fires regardless of thin-row reuse")
}
