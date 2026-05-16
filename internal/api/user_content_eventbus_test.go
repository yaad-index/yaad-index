// Phase 2.2.C tests: UGC create + frontmatter-edit emit on
// the daemon-internal event bus per ADR-0024 §"Internal event
// bus". UGC is operator-authored per ADR-0012, so the source
// tag on every emission is eventbus.SourceOperator.

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// recordingBus captures every Publish call in order so tests
// can assert the exact sequence + content of emissions.
type recordingBus struct {
	mu     sync.Mutex
	events []eventbus.Event
	// inner is a real bus subscribers can listen on; the
	// recordingBus delegates Subscribe to it so existing
	// engine integrations (workflow event subscriptions etc.)
	// still work in fixture tests that swap in this bus.
	inner eventbus.Bus
}

func newRecordingBus() *recordingBus {
	return &recordingBus{inner: eventbus.NewMemoryBus()}
}

func (b *recordingBus) Publish(ctx context.Context, ev eventbus.Event) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.mu.Unlock()
	b.inner.Publish(ctx, ev)
}

func (b *recordingBus) Subscribe(topic eventbus.Topic, handler eventbus.Handler) eventbus.Subscription {
	return b.inner.Subscribe(topic, handler)
}

func (b *recordingBus) snapshot() []eventbus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]eventbus.Event, len(b.events))
	copy(out, b.events)
	return out
}

// newUGCEventbusFixture wires a UGC-capable handler with a
// recordingBus + frontmatter-edge mappings + the operator-
// authority claim path so create + edit calls hit the bus.
func newUGCEventbusFixture(t *testing.T) (http.Handler, store.Store, *recordingBus, auth.Signer) {
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
		nil, nil,
		config.CanonicalKindConfig{},
		map[string]config.CanonicalKindConfig{
			"boardgame": {}, "person": {}, "company": {},
		},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)

	bus := newRecordingBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
		WithUserContentFrontmatterEdges(defaultUGCMappings()),
		WithEventBus(bus),
	)
	return h, st, bus, signer
}

// mintOperatorPairToken issues an agent-on-behalf-of-operator
// pair-claim with the operator authority needed for UGC
// writes.
func mintOperatorPairToken(t *testing.T, signer auth.Signer, agent, operator string) string {
	t.Helper()
	now := time.Now().UTC()
	tok, err := signer.Sign(auth.Claim{
		Subject:   agent,
		Operator:  operator,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	return tok
}

// TestUserContentCreate_EmitsEntityCreated_NoEdges: a UGC create
// with no frontmatter edges emits exactly one entity.created
// event tagged with SourceOperator.
func TestUserContentCreate_EmitsEntityCreated_NoEdges(t *testing.T) {
	t.Parallel()
	h, _, bus, signer := newUGCEventbusFixture(t)
	tok := mintOperatorPairToken(t, signer, "agent:test", "operator:test")

	body := map[string]any{
		"title": "no edges note",
		"body":  "hello",
		"tags":  []string{"misc"},
		"data":  map[string]any{},
	}
	bb, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/user-content", strings.NewReader(string(bb)))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	events := bus.snapshot()
	require.Len(t, events, 1)
	created, ok := events[0].(eventbus.EntityCreatedEvent)
	require.True(t, ok, "first event is entity.created, got %T", events[0])
	assert.Equal(t, "user-content:no-edges-note", created.ID)
	assert.Equal(t, "user-content", created.Kind)
	assert.Equal(t, eventbus.SourceOperator, created.SourceTag)
	assert.False(t, created.At.IsZero())
}

// TestUserContentCreate_EmitsEdgeEvents_WithFrontmatterEdges: a
// UGC create whose frontmatter declares canonical-type edges
// emits entity.created for the UGC entity + entity.edge_added
// for each derived edge + entity.created for each newly-
// materialized thin canonical-label row. All tagged with
// SourceOperator.
func TestUserContentCreate_EmitsEdgeEvents_WithFrontmatterEdges(t *testing.T) {
	t.Parallel()
	h, _, bus, signer := newUGCEventbusFixture(t)
	tok := mintOperatorPairToken(t, signer, "agent:test", "operator:test")

	body := map[string]any{
		"title": "review of brass",
		"body":  "thoughts",
		"tags":  []string{"review"},
		"data": map[string]any{
			"about": []map[string]any{{"name": "Brass Birmingham", "kind": "boardgame"}},
		},
	}
	bb, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/user-content", strings.NewReader(string(bb)))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	events := bus.snapshot()
	// Expected order: entity.created (UGC), entity.created
	// (boardgame thin row), entity.edge_added (UGC → boardgame).
	require.GreaterOrEqual(t, len(events), 3, "got %d events", len(events))

	var createdIDs []string
	var edgeEvents []eventbus.EntityEdgeAddedEvent
	for _, ev := range events {
		switch e := ev.(type) {
		case eventbus.EntityCreatedEvent:
			createdIDs = append(createdIDs, e.ID)
			assert.Equal(t, eventbus.SourceOperator, e.SourceTag,
				"entity.created %s tagged with SourceOperator", e.ID)
		case eventbus.EntityEdgeAddedEvent:
			edgeEvents = append(edgeEvents, e)
		}
	}
	assert.Contains(t, createdIDs, "user-content:review-of-brass",
		"UGC entity.created emitted")
	assert.Contains(t, createdIDs, "boardgame:brass-birmingham",
		"canonical-label thin-row entity.created emitted")
	require.Len(t, edgeEvents, 1, "exactly one edge_added for the about-edge")
	assert.Equal(t, "user-content:review-of-brass", edgeEvents[0].FromID)
	assert.Equal(t, "boardgame:brass-birmingham", edgeEvents[0].ToID)
	assert.Equal(t, "is_about", edgeEvents[0].EdgeType, "edge type from mapping per defaultUGCMappings")
	assert.Equal(t, eventbus.SourceOperator, edgeEvents[0].SourceTag)
}

// TestUserContentFrontmatterEdit_EmitsEdgeAdded_NoCreated: a
// frontmatter edit that adds a new canonical-type edge emits
// entity.edge_added (and entity.created on any newly-
// materialized thin label row) — but NOT entity.created for
// the UGC entity itself (this is the edit path; the entity
// already exists).
func TestUserContentFrontmatterEdit_EmitsEdgeAdded_NoCreated(t *testing.T) {
	t.Parallel()
	h, _, bus, signer := newUGCEventbusFixture(t)
	tok := mintOperatorPairToken(t, signer, "agent:test", "operator:test")

	// First create (no edges) — drains the entity.created from
	// the bus so the edit-side assertion is clean.
	createBody, _ := json.Marshal(map[string]any{
		"title": "evolving review",
		"body":  "v1",
		"tags":  []string{"review"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/user-content", strings.NewReader(string(createBody)))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

	createEvents := len(bus.snapshot())
	require.Equal(t, 1, createEvents, "create-side emitted exactly one entity.created")

	// Now edit the frontmatter to add an `about` edge.
	editBody, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"about": []map[string]any{{"name": "Brass Birmingham", "kind": "boardgame"}},
		},
	})
	req = httptest.NewRequest(http.MethodPut, "/v1/user-content/user-content:evolving-review/frontmatter", strings.NewReader(string(editBody)))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	// Snapshot events emitted on the edit path (skip the
	// create-side first).
	allEvents := bus.snapshot()
	editEvents := allEvents[createEvents:]

	// Expected on edit: entity.created (boardgame thin row),
	// entity.edge_added (UGC → boardgame). NO entity.created
	// for the UGC entity (it already exists).
	var createdIDs []string
	var edgeEvents []eventbus.EntityEdgeAddedEvent
	for _, ev := range editEvents {
		switch e := ev.(type) {
		case eventbus.EntityCreatedEvent:
			createdIDs = append(createdIDs, e.ID)
			assert.Equal(t, eventbus.SourceOperator, e.SourceTag)
		case eventbus.EntityEdgeAddedEvent:
			edgeEvents = append(edgeEvents, e)
		}
	}
	assert.NotContains(t, createdIDs, "user-content:evolving-review",
		"NO entity.created for the UGC entity on edit (already exists)")
	assert.Contains(t, createdIDs, "boardgame:brass-birmingham",
		"NEW canonical-label thin-row emits entity.created")
	require.Len(t, edgeEvents, 1)
	assert.Equal(t, eventbus.SourceOperator, edgeEvents[0].SourceTag)
}

// Note on nil-bus protection: every Publish in
// applyCanonicalTypeEdges + handleUserContentCreate is
// guarded by `bus != nil`. NewHandlerWithRegistry seeds a
// memory-bus when WithEventBus isn't set (see api.go
// `cfg.eventBus == nil` branch), so the production wiring
// never lands a nil bus at the handler. A dedicated nil-bus
// test would have to bypass the handler constructor's
// middleware chain, which adds more setup than the safety
// margin warrants — the inline guards are the test surface.
