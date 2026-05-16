// Tests for the POST /v1/workflows/trigger endpoint per
// ADR-0024 §"Agent surface". The handler routes to the
// engine's Dispatch path; these tests pin the wire-shape
// (request body parsing, status codes for the failure
// modes, success envelope).

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// triggerFakeResolver is an in-memory EntityResolver for the
// workflow trigger tests. Returns the seeded map for known
// ids; decision.ErrEntityNotFound otherwise.
type triggerFakeResolver struct {
	entities map[string]map[string]any
}

func (r *triggerFakeResolver) Resolve(_ context.Context, id string) (map[string]any, error) {
	if got, ok := r.entities[id]; ok {
		return got, nil
	}
	return nil, decision.ErrEntityNotFound
}

// newTriggerFixture wires an api Handler with a workflow
// engine that has the given workflow registered + the given
// entities resolvable.
func newTriggerFixture(t *testing.T, wf *parser.Workflow, entities map[string]map[string]any) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	bus := eventbus.NewMemoryBus()
	eng, err := engine.New(engine.Options{
		Bus:      bus,
		Resolver: &triggerFakeResolver{entities: entities},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	if wf != nil {
		require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithEventBus(bus),
		WithWorkflowEngine(eng),
	)
}

func postWorkflowTrigger(t *testing.T, h http.Handler, name, input string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "input": input})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/trigger", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWorkflowTrigger_HappyPath: a manual workflow fired
// against a resolvable entity-id returns Fired=true + the
// rendered subject.
func TestWorkflowTrigger_HappyPath(t *testing.T) {
	t.Parallel()
	wf := &parser.Workflow{
		Name:           "happy",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Condition:      "entity.rating > 7",
		Subject:        "entity.id",
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "'x'"}}},
	}
	h := newTriggerFixture(t, wf, map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})

	rec := postWorkflowTrigger(t, h, "happy", "boardgame:b")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp workflowTriggerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "happy", resp.Workflow)
	assert.Equal(t, "boardgame:b", resp.EntityID)
	assert.True(t, resp.Fired)
	assert.Equal(t, "boardgame:b", resp.Subject)
	assert.Empty(t, resp.MissingRefs)
	assert.Empty(t, resp.Err)
	assert.NotEmpty(t, resp.At)
}

// TestWorkflowTrigger_UnknownWorkflow: an unregistered
// workflow name returns 404 not_found.
func TestWorkflowTrigger_UnknownWorkflow(t *testing.T) {
	t.Parallel()
	h := newTriggerFixture(t, nil, nil)
	rec := postWorkflowTrigger(t, h, "ghost", "")
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// TestWorkflowTrigger_EmptyInputOnEventDriven: empty input
// against an event-driven trigger returns 422
// invalid_argument.
func TestWorkflowTrigger_EmptyInputOnEventDriven(t *testing.T) {
	t.Parallel()
	wf := &parser.Workflow{
		Name:           "evt",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Actions: []parser.Action{{AddComment: &parser.AddCommentAction{Content: "'x'"}}},
	}
	h := newTriggerFixture(t, wf, nil)
	rec := postWorkflowTrigger(t, h, "evt", "")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
}

// TestWorkflowTrigger_MissingEntity_SurfacesAsMissingRef:
// an entity-id input that doesn't resolve returns 200 with
// a MissingRef on the Decision (per ADR-0024 §"Missing-
// reference handling"). The trigger doesn't "fail" — the
// workflow's decision surfaces the gap.
func TestWorkflowTrigger_MissingEntity_SurfacesAsMissingRef(t *testing.T) {
	t.Parallel()
	wf := &parser.Workflow{
		Name:           "miss",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "'x'"}}},
	}
	h := newTriggerFixture(t, wf, nil)
	rec := postWorkflowTrigger(t, h, "miss", "boardgame:none")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp workflowTriggerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.MissingRefs, 1)
	assert.Equal(t, "boardgame:none", resp.MissingRefs[0].ID)
}

// TestWorkflowTrigger_EmptyName: omitting `name` returns
// 400 invalid_argument.
func TestWorkflowTrigger_EmptyName(t *testing.T) {
	t.Parallel()
	h := newTriggerFixture(t, nil, nil)
	body, _ := json.Marshal(map[string]string{"input": "x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/trigger", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// TestWorkflowTrigger_MalformedJSON: invalid JSON body
// returns 400.
func TestWorkflowTrigger_MalformedJSON(t *testing.T) {
	t.Parallel()
	h := newTriggerFixture(t, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/trigger", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// TestWorkflowTrigger_RouteUnregisteredWhenNoEngine: when
// the handler is constructed without WithWorkflowEngine,
// the route is unregistered → 404.
func TestWorkflowTrigger_RouteUnregisteredWhenNoEngine(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), st, testRegistryWithSeed())

	rec := postWorkflowTrigger(t, h, "any", "")
	require.Equal(t, http.StatusNotFound, rec.Code,
		"endpoint stays unregistered without WithWorkflowEngine")
}

// TestWorkflowTrigger_FiredFalse_RecordsDecision: a
// trigger whose predicate evaluates false still returns
// 200 with the Decision shape (Fired=false). Consumers can
// branch on `fired` to decide whether to act on the
// returned subject etc.
func TestWorkflowTrigger_FiredFalse_RecordsDecision(t *testing.T) {
	t.Parallel()
	wf := &parser.Workflow{
		Name:           "no-fire",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Condition:      "entity.rating > 7",
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "'x'"}}},
	}
	h := newTriggerFixture(t, wf, map[string]map[string]any{
		"boardgame:b": {"rating": int64(3)},
	})
	rec := postWorkflowTrigger(t, h, "no-fire", "boardgame:b")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp workflowTriggerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Fired)
	assert.Empty(t, resp.Err)
}
