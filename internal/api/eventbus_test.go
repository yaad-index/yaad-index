// Phase 2.2.A integration tests for the eventbus wiring on the
// edges + fill + operator-fill mutation endpoints (ADR-0024
// workflow engine §"Internal event bus"). Tests capture published
// events through a real eventbus.Bus subscription to assert that:
//
//   - POST /v1/edges publishes entity.edge_added.
//   - POST /v1/entities/{id}/fill publishes fill.completed per
//     filled gap, source=agent.
//   - POST /v1/entities/{id}/fill publishes
//     fill.completed per Set op, source=operator. Clear / Defer
//     ops do NOT emit (they remove or postpone — not fills).
//   - A handler constructed without WithEventBus default-wires to
//     an in-memory bus with no subscribers; mutation endpoints
//     still succeed and the no-op Publish is invisible.

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// eventCapture is a thread-safe recorder subscribers append to so
// tests can assert post-request emission shape without racing the
// publisher goroutine. Per-topic helpers exist for the load-
// bearing assertions; raw events stay accessible for shape checks.
type eventCapture struct {
	mu     sync.Mutex
	events []eventbus.Event
}

func (c *eventCapture) handler(_ context.Context, e eventbus.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *eventCapture) snapshot() []eventbus.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]eventbus.Event, len(c.events))
	copy(out, c.events)
	return out
}

// subscribeAll registers the capture's handler on all three event
// topics so tests can ask one record-set for any emission shape.
func subscribeAll(bus eventbus.Bus, cap *eventCapture) []eventbus.Subscription {
	subs := make([]eventbus.Subscription, 0, len(eventbus.AllTopics))
	for _, t := range eventbus.AllTopics {
		subs = append(subs, bus.Subscribe(t, cap.handler))
	}
	return subs
}

func unsubscribeAll(subs []eventbus.Subscription) {
	for _, s := range subs {
		s.Unsubscribe()
	}
}

// newAPIWithStoreAndBus mirrors newAPIWithStore but wires an
// explicit eventbus.Bus into the handler so tests can subscribe
// before issuing requests + observe what fired.
func newAPIWithStoreAndBus(t *testing.T) (http.Handler, store.Store, eventbus.Bus) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New(:memory:)")
	t.Cleanup(func() { _ = st.Close() })

	bus := eventbus.NewMemoryBus()
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithEventBus(bus),
	)
	return h, st, bus
}

// TestEdges_Create_EmitsEntityEdgeAdded pins the POST /v1/edges
// emission path: a successful manual edge add publishes one
// entity.edge_added event carrying FromID + ToID + EdgeType, with
// SourceAgent (the manual-add surface defaults to agent source —
// workflow-injected edges land via Phase 4+ dispatch).
func TestEdges_Create_EmitsEntityEdgeAdded(t *testing.T) {
	t.Parallel()
	h, st, bus := newAPIWithStoreAndBus(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEntity(t, st, "person:martin-wallace", "person")

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To:   "person:martin-wallace",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()
	require.Len(t, events, 1, "exactly one event published on edge create")
	got, ok := events[0].(eventbus.EntityEdgeAddedEvent)
	require.True(t, ok, "event type = entity.edge_added; got %T", events[0])
	assert.Equal(t, eventbus.TopicEntityEdgeAdded, got.Topic())
	assert.Equal(t, "boardgame:brass-birmingham", got.FromID)
	assert.Equal(t, "person:martin-wallace", got.ToID)
	assert.Equal(t, "designed_by", got.EdgeType)
	assert.Equal(t, eventbus.SourceAgent, got.SourceTag)
	assert.False(t, got.At.IsZero(), "publisher stamps occurred-at")
}

// TestEdges_CreateFails_NoEvent: when POST /v1/edges rejects (bad
// type, missing entity, etc.), no entity.edge_added event fires —
// the bus stays empty.
func TestEdges_CreateFails_NoEvent(t *testing.T) {
	t.Parallel()
	h, _, bus := newAPIWithStoreAndBus(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:no-such",
		To:   "person:nobody",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusOK, rec.Code, "create should fail on missing endpoints")

	assert.Empty(t, cap.snapshot(), "no event on failed edge create")
}

// newFillFixtureWithBus mirrors newFillFixture but wires an
// explicit eventbus.Bus so the fill-event tests can subscribe.
func newFillFixtureWithBus(t *testing.T) (http.Handler, store.Store, string, eventbus.Bus) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	bus := eventbus.NewMemoryBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithEventBus(bus),
	)
	seedFillEntity(t, st, root, fillTestEntityID, "boardgame", fillTestGaps)
	return h, st, root, bus
}

// TestFill_EmitsFillCompletedPerGap covers the agent-strategy
// fill path: each field in the request body's `fields` map fires
// one fill.completed event with SourceAgent. Events are ordered
// by gap name (the handler sorts before publishing) so multi-gap
// fills are reproducible.
func TestFill_EmitsFillCompletedPerGap(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, _, _, bus := newFillFixtureWithBus(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{
			"summary": "Heavy economic euro.",
			"tags":    []string{"heavy", "economic"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()
	require.Len(t, events, 2, "one fill.completed per filled gap")

	got0, ok := events[0].(eventbus.FillCompletedEvent)
	require.True(t, ok, "first event type")
	got1, ok := events[1].(eventbus.FillCompletedEvent)
	require.True(t, ok, "second event type")

	// Sorted by gap name: "summary" < "tags".
	assert.Equal(t, fillTestEntityID, got0.EntityID)
	assert.Equal(t, "summary", got0.Gap)
	assert.Equal(t, eventbus.SourceAgent, got0.SourceTag)
	assert.Equal(t, fillTestEntityID, got1.EntityID)
	assert.Equal(t, "tags", got1.Gap)
	assert.Equal(t, eventbus.SourceAgent, got1.SourceTag)
}

// TestFill_RejectedFill_NoEvent: when the fill returns 409 (one
// of the field names isn't in the open-gap set), the call short-
// circuits before any DB write — and therefore before any
// fill.completed publish. No event lands.
func TestFill_RejectedFill_NoEvent(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, _, _, bus := newFillFixtureWithBus(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{
			"summary":     "ok value",
			"not_a_field": "nope",
		},
	})
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())

	assert.Empty(t, cap.snapshot(),
		"rejected fill must not emit any fill.completed events")
}

// newOperatorFillFixtureWithBus mirrors newOperatorFillFixture
// but wires an eventbus.Bus so operator-fill emission tests can
// subscribe. The boardgame canonical kind is registered with its
// ADR-0019 built-ins so set ops on `rating` / `owned` validate.
func newOperatorFillFixtureWithBus(t *testing.T) (http.Handler, store.Store, string, auth.Signer, eventbus.Bus) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	reg := config.MergeCanonicalRegistry(
		nil,
		[]string{"boardgame"},
		config.CanonicalKindConfig{},
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)

	bus := eventbus.NewMemoryBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
		WithEventBus(bus),
	)
	return h, st, root, signer, bus
}

// TestOperatorFill_EmitsFillCompletedSourceOperator pins the
// operator-strategy fill path: each Set op fires one
// fill.completed event with SourceOperator (distinguishing it
// from /fill's SourceAgent).
func TestOperatorFill_EmitsFillCompletedSourceOperator(t *testing.T) {
	t.Parallel()
	h, st, root, signer, bus := newOperatorFillFixtureWithBus(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:brass-birmingham"
	seedBoardgameForFill(t, st, root, id)

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"rating": 9,
			"owned":  true,
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()
	require.Len(t, events, 2, "one fill.completed per Set op")

	for _, ev := range events {
		fc, ok := ev.(eventbus.FillCompletedEvent)
		require.True(t, ok, "event type")
		assert.Equal(t, id, fc.EntityID)
		assert.Equal(t, eventbus.SourceOperator, fc.SourceTag)
	}

	// Sorted: "owned" < "rating".
	assert.Equal(t, "owned", events[0].(eventbus.FillCompletedEvent).Gap)
	assert.Equal(t, "rating", events[1].(eventbus.FillCompletedEvent).Gap)
}

// TestOperatorFill_DeferOp_DoesNotEmit: a defer op marks a gap as
// postponed without writing data. It's not a fill, so no
// fill.completed event fires.
func TestOperatorFill_DeferOp_DoesNotEmit(t *testing.T) {
	t.Parallel()
	h, st, root, signer, bus := newOperatorFillFixtureWithBus(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:defer-test"
	seedBoardgameForFill(t, st, root, id)

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"rating": map[string]any{"defer": true},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	assert.Empty(t, cap.snapshot(),
		"defer ops mark the gap as postponed; they aren't fills, so no event")
}

// TestOperatorFill_ClearOp_DoesNotEmit: a clear op (JSON null
// value) removes the field. Not a fill, no event.
func TestOperatorFill_ClearOp_DoesNotEmit(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, st, root, signer, bus := newOperatorFillFixtureWithBus(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:clear-test"
	seedBoardgameForFill(t, st, root, id)

	// Seed the field first via a Set op so the subsequent Clear
	// has something to remove.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{"rating": 7}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Now subscribe — earlier event from the seed-set isn't
	// observed (we set up capture after the seed call).
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{"rating": nil}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	assert.Empty(t, cap.snapshot(),
		"clear op removes data; it isn't a fill, so no event")
}

// TestOperatorFill_MixedSetAndClear_EmitsOnlyForSet: a request
// mixing Set and Clear ops emits one event for the Set op and
// none for the Clear. Pins the filter.
func TestOperatorFill_MixedSetAndClear_EmitsOnlyForSet(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, st, root, signer, bus := newOperatorFillFixtureWithBus(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:mixed-test"
	seedBoardgameForFill(t, st, root, id)

	// Seed `rating` so the subsequent clear has something to
	// remove; the subscription is set up after.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{"rating": 7}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"rating": nil,  // clear
			"owned":  true, // set
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()
	require.Len(t, events, 1, "only the Set op emits")
	fc := events[0].(eventbus.FillCompletedEvent)
	assert.Equal(t, "owned", fc.Gap)
	assert.Equal(t, eventbus.SourceOperator, fc.SourceTag)
}

// TestHandler_NoEventBusOption_DefaultsToNoOp: a handler
// constructed without WithEventBus default-wires to an in-memory
// bus with no subscribers. Mutation endpoints succeed; the
// no-op Publish is invisible to anyone outside the handler.
//
// (This is the legacy-test-compat path — every existing test
// that calls NewHandlerWithRegistry without WithEventBus must
// keep working.)
func TestHandler_NoEventBusOption_DefaultsToNoOp(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:no-bus", "boardgame")
	seedEntity(t, st, "person:no-bus", "person")

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:no-bus",
		To:   "person:no-bus",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code,
		"endpoint succeeds with default no-subscriber bus; body=%s", rec.Body.String())
}

// newCanonicalTypeFixtureWithBus mirrors newCanonicalTypeFixture
// but wires an eventbus.Bus so canonical-type-edge tests can
// subscribe before issuing the fill request.
func newCanonicalTypeFixtureWithBus(t *testing.T, gapKinds []string) (http.Handler, store.Store, string, auth.Signer, eventbus.Bus) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	opPerKind := map[string]config.CanonicalKindConfig{
		"source": {
			Gaps: map[string]config.GapSpec{
				"subjects": {
					Type:         config.CanonicalTypeName,
					Description:  "Canonical entities mentioned in this source.",
					FillStrategy: "both",
					Kinds:        gapKinds,
				},
			},
		},
		"boardgame": {},
		"person":    {},
	}
	reg := config.MergeCanonicalRegistry(
		nil, nil,
		config.CanonicalKindConfig{}, opPerKind,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)

	bus := eventbus.NewMemoryBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
		WithEventBus(bus),
	)
	return h, st, root, signer, bus
}

// TestOperatorFill_CanonicalType_EmitsEdgesAndThinRows pins the
// fill-produces-edges semantic (ADR-0024 §Fill-gap injection
// "Workflow-injected gap fills are permanent on the entity"):
// each canonical_type fill entry materializes a thin label row
// (if not pre-existing) AND creates one edge from the source.
// Both the entity.created (per new thin row) and entity.edge_added
// (per edge) events fire with SourceOperator on operator-fill.
func TestOperatorFill_CanonicalType_EmitsEdgesAndThinRows(t *testing.T) {
	t.Parallel()
	h, st, root, signer, bus := newCanonicalTypeFixtureWithBus(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:newsletter-may"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Brass: Birmingham", "kind": "boardgame"},
				map[string]any{"name": "Martin Wallace", "kind": "person"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	events := cap.snapshot()

	// Expected events: 1 fill.completed (the "subjects" gap fill)
	// + 2 entity.created (the two new thin label rows) + 2
	// entity.edge_added (the two new edges) = 5 events.
	var fills, creates, edges int
	createdIDs := map[string]string{}  // id -> kind
	edgeTargets := map[string]string{} // toID -> edgeType
	for _, e := range events {
		switch ev := e.(type) {
		case eventbus.FillCompletedEvent:
			fills++
			assert.Equal(t, "subjects", ev.Gap)
			assert.Equal(t, eventbus.SourceOperator, ev.SourceTag)
		case eventbus.EntityCreatedEvent:
			creates++
			createdIDs[ev.ID] = ev.Kind
			assert.Equal(t, eventbus.SourceOperator, ev.SourceTag)
		case eventbus.EntityEdgeAddedEvent:
			edges++
			edgeTargets[ev.ToID] = ev.EdgeType
			assert.Equal(t, id, ev.FromID)
			assert.Equal(t, eventbus.SourceOperator, ev.SourceTag)
		}
	}
	assert.Equal(t, 1, fills, "one fill.completed for the subjects gap")
	assert.Equal(t, 2, creates, "one entity.created per new thin label row")
	assert.Equal(t, 2, edges, "one entity.edge_added per canonical-type edge")

	assert.Equal(t, "boardgame", createdIDs["boardgame:brass-birmingham"])
	assert.Equal(t, "person", createdIDs["person:martin-wallace"])
	assert.Equal(t, "subjects", edgeTargets["boardgame:brass-birmingham"])
	assert.Equal(t, "subjects", edgeTargets["person:martin-wallace"])
}

// TestOperatorFill_CanonicalType_ExistingThinRow_NoEntityCreated:
// when the thin label row already exists in the store
// (pre-materialized by a prior fill or operator-fill on a
// different source), only entity.edge_added fires — not
// entity.created. The `created` return from EnsureLabelRow is
// the load-bearing signal.
func TestOperatorFill_CanonicalType_ExistingThinRow_NoEntityCreated(t *testing.T) {
	t.Parallel()
	h, st, root, signer, bus := newCanonicalTypeFixtureWithBus(t, []string{"boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:reuse-test"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	// Pre-materialize the target thin row so the next fill skips
	// the "create" branch.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   "boardgame:brass-birmingham",
		Kind: "boardgame",
	}))

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Brass: Birmingham", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var creates, edges int
	for _, e := range cap.snapshot() {
		switch e.(type) {
		case eventbus.EntityCreatedEvent:
			creates++
		case eventbus.EntityEdgeAddedEvent:
			edges++
		}
	}
	assert.Equal(t, 0, creates,
		"pre-existing thin row → no entity.created (the `created` return gates this)")
	assert.Equal(t, 1, edges,
		"edge still fires regardless of thin-row reuse")
}

// TestOperatorFill_CanonicalType_ClearOp_DeletesEdges_NoEvents:
// a Clear op on a canonical_type gap wipes the prior edges
// (DeleteEdgesByTypeFrom) without creating new ones. No
// entity.edge_added fires (we only emit on adds, not removes —
// removal events aren't in the Phase 2 topic set per ADR).
func TestOperatorFill_CanonicalType_ClearOp_DeletesEdges_NoEvents(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, st, root, signer, bus := newCanonicalTypeFixtureWithBus(t, []string{"boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:clear-edges-test"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	// Seed: fill produces an edge.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	// Clear: empty list wipes the prior edge without creating
	// new ones.
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"subjects": []any{},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	for _, e := range cap.snapshot() {
		_, isEdge := e.(eventbus.EntityEdgeAddedEvent)
		assert.False(t, isEdge,
			"edge-delete path doesn't emit entity.edge_added (no add happened)")
		_, isCreate := e.(eventbus.EntityCreatedEvent)
		assert.False(t, isCreate,
			"clearing edges doesn't create new entities")
	}
}

// TestFill_CanonicalType_AgentPath_EmitsSourceAgent verifies the
// agent-fill canonical_type path (/fill endpoint) emits the
// canonical-type-edge events with SourceAgent (distinguished from
// operator-fill's SourceOperator). Uses the same shared
// applyCanonicalTypeEdges helper so the source plumbing is the
// only difference.
func TestFill_CanonicalType_AgentPath_EmitsSourceAgent(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, st, root, signer, bus := newCanonicalTypeFixtureWithBus(t, []string{"boardgame"})
	const id = "source:agent-path"
	seedSourceForCanonicalTypeFill(t, st, root, id)
	tok := mintToken(t, signer, "agent-fixture", "alice")

	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	// Agent-fill: agent-conduit auth (Subject != Operator). The
	// /fill endpoint accepts the {name, kind} object form on
	// canonical_type gaps; pre-formed `kind:slug` strings are
	// operator-only.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"fields": map[string]any{
				"subjects": []any{
					map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
				},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	for _, e := range cap.snapshot() {
		switch ev := e.(type) {
		case eventbus.EntityCreatedEvent:
			assert.Equal(t, eventbus.SourceAgent, ev.SourceTag,
				"agent-fill thin-row creates emit SourceAgent")
		case eventbus.EntityEdgeAddedEvent:
			assert.Equal(t, eventbus.SourceAgent, ev.SourceTag,
				"agent-fill canonical-type edges emit SourceAgent")
		case eventbus.FillCompletedEvent:
			assert.Equal(t, eventbus.SourceAgent, ev.SourceTag,
				"agent-fill fill.completed emits SourceAgent")
		}
	}
}

// TestEventbus_Wiring_PublishedAtsRecent pins that the At field
// stamped by the handler is recent (within a small window of
// wall-clock now) so subscribers can rely on it for ordering /
// recency-based dedup.
func TestEventbus_Wiring_PublishedAtsRecent(t *testing.T) {
	t.Parallel()
	h, st, bus := newAPIWithStoreAndBus(t)
	cap := &eventCapture{}
	defer unsubscribeAll(subscribeAll(bus, cap))

	seedEntity(t, st, "boardgame:at-test", "boardgame")
	seedEntity(t, st, "person:at-test", "person")

	before := time.Now().UTC()
	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:at-test",
		To:   "person:at-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	after := time.Now().UTC()

	events := cap.snapshot()
	require.Len(t, events, 1)
	at := events[0].(eventbus.EntityEdgeAddedEvent).At
	assert.True(t, !at.Before(before.Add(-time.Second)) && !at.After(after.Add(time.Second)),
		"At should be within [before-1s, after+1s]; got %v (before=%v after=%v)", at, before, after)
}
