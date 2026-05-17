// Phase 3.C tests: Engine.Dispatch (manual-trigger entry
// point) + the edge-field-completeness fold-in
// (from_title / to_title / timestamp).

package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestDispatch_UnknownWorkflow: Dispatch on a name that
// isn't registered returns ErrUnknownWorkflow.
func TestDispatch_UnknownWorkflow(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	_, err := eng.Dispatch(context.Background(), "no-such", "anything")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownWorkflow)
}

// TestDispatch_EmptyInputOnEventDriven: empty input is only
// allowed for trigger.type=manual. An event-driven workflow
// passed empty input returns ErrEmptyInputNotAllowed.
func TestDispatch_EmptyInputOnEventDriven(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	wf := &parser.Workflow{
		Name:           "evt",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err := eng.Dispatch(context.Background(), "evt", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyInputNotAllowed)
}

// TestDispatch_ManualEmptyInput: a manual workflow with
// empty input fires with an empty entity activation.
// Predicates that access entity fields see has()==false,
// so a non-restrictive condition holds.
func TestDispatch_ManualEmptyInput(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	wf := &parser.Workflow{
		Name:           "daily-summary",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        `"daily"`,
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "daily-summary", "")
	require.NoError(t, err)
	assert.True(t, dec.Fired, "empty manual fire: no condition → defaults true")
	assert.Equal(t, "", dec.EntityID, "target-less: no entity id")
	assert.Equal(t, "daily", dec.Subject)
	assert.Empty(t, dec.MissingRefs)
}

// TestDispatch_EntityIDInput_Resolves: an entity-id input
// resolves via the resolver + the predicate sees the entity.
func TestDispatch_EntityIDInput_Resolves(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "by-id",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Condition:      "entity.rating > 7",
		Subject:        "entity.id",
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "by-id", "boardgame:b")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:b", dec.EntityID)
	assert.True(t, dec.Fired)
	assert.Equal(t, "boardgame:b", dec.Subject)
}

// TestDispatch_UnresolvedEntityID_SurfacesMissingRef: an
// entity-id input that doesn't resolve produces a Decision
// with MissingRefs containing the input id, not a hard
// Dispatch error.
func TestDispatch_UnresolvedEntityID_SurfacesMissingRef(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil) // empty resolver
	wf := &parser.Workflow{
		Name:           "miss",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "miss", "boardgame:none")
	require.NoError(t, err, "missing entity surfaces as MissingRef, not error")
	assert.Equal(t, "boardgame:none", dec.EntityID)
	require.Len(t, dec.MissingRefs, 1)
	assert.Equal(t, "boardgame:none", dec.MissingRefs[0].ID)
}

// fakeIngestRouter is the in-memory IngestRouter for the
// URL-routing Dispatch tests. Records every IngestURL call so
// the test can assert routing-time behavior.
type fakeIngestRouter struct {
	mu        sync.Mutex
	calls     []ingestRouterCall
	returnID  string
	returnErr error
}

type ingestRouterCall struct {
	url     string
	timeout time.Duration
}

func (f *fakeIngestRouter) IngestURL(_ context.Context, url string, timeout time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ingestRouterCall{url: url, timeout: timeout})
	if f.returnErr != nil {
		return "", f.returnErr
	}
	return f.returnID, nil
}

func (f *fakeIngestRouter) snapshot() []ingestRouterCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ingestRouterCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newEngineWithIngestRouter mirrors newEngineWithBus but wires
// the URL-routing IngestRouter dep + a custom IngestTimeout
// (short to keep tests snappy).
func newEngineWithIngestRouter(t *testing.T, entities map[string]map[string]any, router IngestRouter) (*Engine, eventbus.Bus) {
	t.Helper()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(entities)
	eng, err := New(Options{
		Bus:           bus,
		Resolver:      resolver,
		IngestRouter:  router,
		IngestTimeout: time.Second,
		Logger:        quietLogger(),
	})
	require.NoError(t, err)
	return eng, bus
}

// TestDispatch_URLInput_RoutesThroughIngestRouter: a URL-shape
// input (contains `://`) routes through the IngestRouter to
// resolve the canonical entity id; the workflow then fires
// against that id.
func TestDispatch_URLInput_RoutesThroughIngestRouter(t *testing.T) {
	t.Parallel()
	router := &fakeIngestRouter{returnID: "boardgame:brass-birmingham"}
	eng, _ := newEngineWithIngestRouter(t, map[string]map[string]any{
		"boardgame:brass-birmingham": {"id": "boardgame:brass-birmingham", "rating": int64(9)},
	}, router)
	wf := &parser.Workflow{
		Name:           "by-url",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Condition:      "entity.rating > 7",
		Subject:        "entity.id",
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "by-url",
		"https://boardgamegeek.com/boardgame/224517/brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", dec.EntityID,
		"workflow fires against the IngestRouter-resolved id, not the URL")
	assert.True(t, dec.Fired)
	require.Len(t, router.snapshot(), 1)
	assert.Equal(t, "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		router.snapshot()[0].url)
	assert.Equal(t, time.Second, router.snapshot()[0].timeout,
		"engine's IngestTimeout flows through to the router call")
}

// TestDispatch_URLInput_NoRouter_RejectsCleanly: when no
// IngestRouter is wired, URL-shape input returns the typed
// ErrURLInputNotSupported synchronously before any workflow
// runs — no Decision is recorded.
func TestDispatch_URLInput_NoRouter_RejectsCleanly(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil) // no IngestRouter
	wf := manualWorkflow("by-url-no-router")
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err := eng.Dispatch(context.Background(), "by-url-no-router", "https://example.org/x")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrURLInputNotSupported)
	assert.Empty(t, eng.Decisions(), "no Decision recorded on routing failure")
}

// TestDispatch_URLInput_IngestFailure_PropagatesError: a
// router that returns an error surfaces synchronously to the
// caller; no Decision is recorded.
func TestDispatch_URLInput_IngestFailure_PropagatesError(t *testing.T) {
	t.Parallel()
	router := &fakeIngestRouter{returnErr: errors.New("no plugin handles this URL")}
	eng, _ := newEngineWithIngestRouter(t, nil, router)
	wf := manualWorkflow("ingest-fail")
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err := eng.Dispatch(context.Background(), "ingest-fail", "https://no-plugin-for-this/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no plugin handles this URL")
	assert.Empty(t, eng.Decisions(), "no Decision recorded on routing failure")
}

// TestDispatch_EntityIDLooksLikeURL_BypassesRouter: an
// entity-id input that doesn't contain `://` is treated as an
// id — the router isn't invoked. Disambiguation rule per
// engine.Dispatch's docstring.
func TestDispatch_EntityIDLooksLikeURL_BypassesRouter(t *testing.T) {
	t.Parallel()
	router := &fakeIngestRouter{returnErr: errors.New("should not be called")}
	eng, _ := newEngineWithIngestRouter(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(9)},
	}, router)
	wf := &parser.Workflow{
		Name:           "id-shape",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "id-shape", "boardgame:b")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:b", dec.EntityID)
	assert.Empty(t, router.snapshot(), "router NOT called for entity-id-shape input")
}

// TestDispatch_RecordsInRingBuffer: Dispatch's result is
// also appended to the ring buffer (same as event-bus
// decisions).
func TestDispatch_RecordsInRingBuffer(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "ring",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err := eng.Dispatch(context.Background(), "ring", "boardgame:b")
	require.NoError(t, err)

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Equal(t, "ring", decs[0].Workflow)
	assert.Equal(t, "boardgame:b", decs[0].EntityID)
}

// TestDispatch_DedupKeyRendered_DefaultsToEntityID: a
// workflow with no explicit dedup.key gets the parser
// default `entity.id`; the engine renders it + stamps it on
// the Decision + passes it through to the action runner.
func TestDispatch_DedupKeyRendered_DefaultsToEntityID(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: rec, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wf := &parser.Workflow{
		Name:           "wf",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        "entity.id",
		Dedup:          parser.Dedup{Key: "entity.id", Policy: parser.DedupPolicyUpdate},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "wf", "boardgame:b")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:b", dec.DedupKey)
	assert.Equal(t, parser.DedupPolicyUpdate, dec.DedupPolicyApplied)

	calls := rec.snapshot()
	require.Len(t, calls, 1, "policy=update dispatches actions")
}

// TestDispatch_DedupPolicySkip_SuppressesSecondDispatch:
// policy=skip lets the FIRST fire dispatch but suppresses
// subsequent fires with the same (workflow, dedup-key). The
// Decision is still recorded for both with
// DedupPolicyApplied="skip"; only the action runner is
// gated.
func TestDispatch_DedupPolicySkip_SuppressesSecondDispatch(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: rec, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wf := &parser.Workflow{
		Name:           "wf",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        "entity.id",
		Dedup:          parser.Dedup{Key: "entity.id", Policy: parser.DedupPolicySkip},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	d1, err := eng.Dispatch(context.Background(), "wf", "boardgame:b")
	require.NoError(t, err)
	d2, err := eng.Dispatch(context.Background(), "wf", "boardgame:b")
	require.NoError(t, err)

	assert.Equal(t, "boardgame:b", d1.DedupKey)
	assert.Equal(t, "boardgame:b", d2.DedupKey)
	assert.Equal(t, parser.DedupPolicySkip, d1.DedupPolicyApplied)
	assert.Equal(t, parser.DedupPolicySkip, d2.DedupPolicyApplied)
	require.Len(t, rec.snapshot(), 1, "first dispatch runs; second is suppressed by policy=skip")
}

// TestDispatch_DedupPolicySkip_DifferentEntitiesProceed:
// the dedup is per-(workflow, rendered-key), so different
// entities yielding different keys all dispatch under
// policy=skip.
func TestDispatch_DedupPolicySkip_DifferentEntitiesProceed(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:a": {"id": "boardgame:a", "rating": int64(9)},
		"boardgame:b": {"id": "boardgame:b", "rating": int64(8)},
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: rec, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wf := &parser.Workflow{
		Name:           "wf",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        "entity.id",
		Dedup:          parser.Dedup{Key: "entity.id", Policy: parser.DedupPolicySkip},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err = eng.Dispatch(context.Background(), "wf", "boardgame:a")
	require.NoError(t, err)
	_, err = eng.Dispatch(context.Background(), "wf", "boardgame:b")
	require.NoError(t, err)
	require.Len(t, rec.snapshot(), 2, "two distinct keys → two dispatches")
}

// TestDispatch_DedupPolicyReplace_LogsNotImplemented:
// policy=replace is documented as a Phase 5.A carry-over;
// the engine logs a Warn + falls through to update behavior.
// Pinned here so when 5.A.2 wires real replace semantics,
// this test changes intent (suppression of the warn line +
// new task-close behavior) rather than silently no-oping.
func TestDispatch_DedupPolicyReplace_LogsNotImplemented(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: rec, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wf := &parser.Workflow{
		Name:           "wf",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        "entity.id",
		Dedup:          parser.Dedup{Key: "entity.id", Policy: parser.DedupPolicyReplace},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err = eng.Dispatch(context.Background(), "wf", "boardgame:b")
	require.NoError(t, err)
	_, err = eng.Dispatch(context.Background(), "wf", "boardgame:b")
	require.NoError(t, err)
	// Both fires dispatch (replace falls through to update
	// for v1; carry-over to 5.A.2 real semantics).
	assert.Len(t, rec.snapshot(), 2, "replace falls through to update behavior in 5.A")
}

// TestEngine_EdgeFields_FullSet covers the PR-80 fold-in:
// the edge map populated by makeEdgeHandler now includes
// from_title / to_title / timestamp in addition to
// type / from / to.
func TestEngine_EdgeFields_FullSet(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"source:newsletter":          {"title": "May Newsletter"},
		"boardgame:brass-birmingham": {"title": "Brass: Birmingham", "rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "full-edge",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		// Predicate reads every edge field; if any are
		// missing or have the wrong type, evaluation either
		// fails or returns false.
		Condition: `edge.from_title == "May Newsletter" && edge.to_title == "Brass: Birmingham" && edge.type == "is_about"`,
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	now := time.Now().UTC()
	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID:    "source:newsletter",
		ToID:      "boardgame:brass-birmingham",
		EdgeType:  "is_about",
		SourceTag: eventbus.SourceAgent,
		At:        now,
	})

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].Fired,
		"all edge fields populated: from_title / to_title / type / timestamp")
	assert.NoError(t, decs[0].Err)
}

// TestEngine_EdgeFields_MissingTitle_OmittedGracefully:
// when the resolver returns an entity without a "title"
// field, the engine omits from_title / to_title from the
// edge map. Predicates can use has() to guard.
func TestEngine_EdgeFields_MissingTitle_OmittedGracefully(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"source:s":    {}, // no title
		"boardgame:b": {"rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "no-title",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Condition: `!has(edge.from_title) && !has(edge.to_title)`,
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "source:s", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].Fired,
		"missing title fields → omitted → has() returns false")
}

// TestEngine_EdgeFields_TimestampAvailable: edge.timestamp
// is the EntityEdgeAddedEvent's At field, accessible from
// the CEL predicate.
func TestEngine_EdgeFields_TimestampAvailable(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "ts",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		// Just assert the timestamp field is present; CEL's
		// has() returns true when the map key exists.
		Condition: `has(edge.timestamp)`,
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].Fired, "edge.timestamp populated")
}

// recordingRunner records every Run call for engine
// action-hook integration tests. Used to assert
// Engine.runEvaluation dispatches to the runner when
// Fired=true and skips it on Fired=false.
type recordingRunner struct {
	mu    sync.Mutex
	calls []recordedRun
}

type recordedRun struct {
	workflow string
	entityID string
	subject  string
	actions  []parser.Action
	// rendered captures act.RenderedTemplates for tests that
	// assert the engine's per-action template renderings.
	// Cloned at record time so subsequent runner calls don't
	// race with the test's reads.
	rendered map[int]map[string]string
}

func (r *recordingRunner) Run(_ context.Context, wf *parser.Workflow, dec actions.Decision, act actions.Activation) []actions.ActionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	var rendered map[int]map[string]string
	if act.RenderedTemplates != nil {
		rendered = make(map[int]map[string]string, len(act.RenderedTemplates))
		for i, fields := range act.RenderedTemplates {
			fcopy := make(map[string]string, len(fields))
			for k, v := range fields {
				fcopy[k] = v
			}
			rendered[i] = fcopy
		}
	}
	r.calls = append(r.calls, recordedRun{
		workflow: wf.Name,
		entityID: dec.EntityID,
		subject:  dec.Subject,
		actions:  wf.Actions,
		rendered: rendered,
	})
	out := make([]actions.ActionResult, len(wf.Actions))
	for i := range wf.Actions {
		out[i] = actions.ActionResult{ActionIdx: i, Type: "task_append"}
	}
	return out
}

func (r *recordingRunner) snapshot() []recordedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRun, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestEngine_Backstop_TripsAfterThreshold: per ADR-0024
// §"Self-loop detection", more than N fires on the same
// (workflow, entity) pair within the sliding window
// suppresses further fires + writes a single err-task entry.
// Test uses a tight threshold (3) + a long window so the
// sliding-window prune doesn't interfere.
// TestEngine_Cycle_SuppressedWhenWorkflowInChain pins the
// #147 structural cycle detection that replaced the prior
// per-(workflow, entity) rate-limit backstop. When a workflow
// fires on an event whose Chain already names that workflow,
// firing it again would close a loop; the engine suppresses
// the fire + writes a single err-task naming the chain.
func TestEngine_Cycle_SuppressedWhenWorkflowInChain(t *testing.T) {
	t.Parallel()
	errWriter := &fakeErrTaskWriter{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"e:1": {"id": "e:1"},
	})
	runner := actions.New(actions.Options{
		TaskWriter:    &noopTaskWriter{},
		ErrTaskWriter: errWriter,
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "loop",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeEntityCreated},
		Subject:        "entity.id",
		Actions: []parser.Action{{TaskAppend: &parser.TaskAppendAction{
			Section: "s", Content: "'x'",
		}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// Publish an event whose Chain already names the workflow
	// — this is what the engine produces when an action runner
	// emits a child event mid-fire. The handler should detect
	// the cycle and suppress.
	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID:        "e:1",
		Kind:      "any",
		SourceTag: eventbus.WorkflowSource("loop"),
		At:        time.Now().UTC(),
		Chain:     []string{"loop"}, // <-- workflow already in chain
	})

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].SuppressedByCycle, "cycle should be detected")
	assert.Equal(t, []string{"loop"}, decs[0].CycleChain)

	errCalls := errWriter.snapshot()
	require.Len(t, errCalls, 1)
	assert.Equal(t, "loop", errCalls[0].workflow)
	assert.Contains(t, errCalls[0].errMsg, "cycle suppressed")
}

// TestEngine_Cycle_RepeatedTripsDoNotSpamErrTask pins the
// fingerprint dedup: a chain that fires N times for the same
// (workflow, chain-shape) only writes ONE err-task entry —
// subsequent suppressions skip the writer to avoid spam.
func TestEngine_Cycle_RepeatedTripsDoNotSpamErrTask(t *testing.T) {
	t.Parallel()
	errWriter := &fakeErrTaskWriter{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"e:1": {"id": "e:1"},
	})
	runner := actions.New(actions.Options{
		TaskWriter:    &noopTaskWriter{},
		ErrTaskWriter: errWriter,
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "loop",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeEntityCreated},
		Subject:        "entity.id",
		Actions:        []parser.Action{{TaskAppend: &parser.TaskAppendAction{Section: "s", Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	for i := 0; i < 5; i++ {
		bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
			ID: "e:1", Kind: "any",
			SourceTag: eventbus.WorkflowSource("loop"),
			At:        time.Now().UTC(),
			Chain:     []string{"loop"},
		})
	}

	decs := eng.Decisions()
	require.Len(t, decs, 5)
	for i, d := range decs {
		assert.True(t, d.SuppressedByCycle, "fire %d should be suppressed", i+1)
	}
	// Err-task fired only ONCE for the (workflow, chain) pair.
	assert.Len(t, errWriter.snapshot(), 1,
		"repeated suppressions on the same (workflow, chain) fingerprint must not re-spam the err-task")
}

// TestEngine_Cycle_DistinctChainsLogIndependently: two
// different chain shapes that both close on the same workflow
// each get their own err-task entry — the dedup is per-chain,
// not per-workflow.
func TestEngine_Cycle_DistinctChainsLogIndependently(t *testing.T) {
	t.Parallel()
	errWriter := &fakeErrTaskWriter{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"e:1": {"id": "e:1"},
	})
	runner := actions.New(actions.Options{
		TaskWriter:    &noopTaskWriter{},
		ErrTaskWriter: errWriter,
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "router",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeEntityCreated},
		Subject:        "entity.id",
		Actions:        []parser.Action{{TaskAppend: &parser.TaskAppendAction{Section: "s", Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "e:1", Kind: "any",
		SourceTag: eventbus.WorkflowSource("router"),
		At:        time.Now().UTC(),
		Chain:     []string{"router", "tagger"},
	})
	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "e:1", Kind: "any",
		SourceTag: eventbus.WorkflowSource("router"),
		At:        time.Now().UTC(),
		Chain:     []string{"router", "enricher"},
	})

	calls := errWriter.snapshot()
	require.Len(t, calls, 2, "distinct chain shapes log independently")
}

// TestEngine_Cycle_HighVolumeDistinctEntitiesNotSuppressed
// pins the #147 anti-false-positive: 100 distinct events
// from independent fresh-ingest sources (no upstream
// workflow chain) all fire the workflow successfully.
// The prior per-entity rate-limit would have suppressed
// fires 11..100 once they hit the same target entity; cycle
// detection looks at the chain, not the per-entity count,
// so all 100 pass.
func TestEngine_Cycle_HighVolumeDistinctEntitiesNotSuppressed(t *testing.T) {
	t.Parallel()
	errWriter := &fakeErrTaskWriter{}
	bus := eventbus.NewMemoryBus()
	entities := make(map[string]map[string]any, 100)
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("email:%d", i)
		entities[id] = map[string]any{"id": id}
	}
	resolver := newFakeResolver(entities)
	runner := actions.New(actions.Options{
		TaskWriter:    &noopTaskWriter{},
		ErrTaskWriter: errWriter,
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "github-classify",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeEntityCreated},
		Subject:        "entity.id",
		Actions:        []parser.Action{{TaskAppend: &parser.TaskAppendAction{Section: "s", Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	for i := 0; i < 100; i++ {
		bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
			ID:        fmt.Sprintf("email:%d", i),
			Kind:      "any",
			SourceTag: eventbus.SourceAgent,
			At:        time.Now().UTC(),
			// No chain — fresh ingest from agent.
		})
	}

	decs := eng.Decisions()
	require.Len(t, decs, 100)
	for _, d := range decs {
		assert.False(t, d.SuppressedByCycle, "no chain → no cycle suppression")
	}
	assert.Empty(t, errWriter.snapshot(),
		"high-volume fresh-ingest with no chain produces no cycle err-task")
}

// noopTaskWriter is a no-error TaskWriter for backstop +
// dedup tests that need the action runner to succeed
// without exercising real writer side effects.
type noopTaskWriter struct{}

func (*noopTaskWriter) AppendTaskSection(_ context.Context, _, _, _, _, _, _ string) error {
	return nil
}

func (*noopTaskWriter) EnsureMissingRefsSection(_ context.Context, _, _ string, _ []string) error {
	return nil
}

// fakeErrTaskWriter records every AppendErrTask invocation
// so engine-level tests can assert the err-task pattern
// fired with the right workflow / entity / error message.
type fakeErrTaskWriter struct {
	mu    sync.Mutex
	calls []errTaskCall
}

type errTaskCall struct {
	workflow string
	when     time.Time
	entityID string
	errMsg   string
}

func (f *fakeErrTaskWriter) AppendErrTask(_ context.Context, workflow string, when time.Time, entityID, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, errTaskCall{workflow: workflow, when: when, entityID: entityID, errMsg: errMsg})
	return nil
}

func (f *fakeErrTaskWriter) snapshot() []errTaskCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]errTaskCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestEngine_SystemicFailure_AppendsErrTask: a condition
// that fails at runtime (e.g. `1 / 0`) records the failure
// via the configured ErrTaskWriter — per ADR-0024 §"Runtime
// errors". Both event-bus + Dispatch paths share recordDecision
// so this test covers both via the manual-trigger path.
func TestEngine_SystemicFailure_AppendsErrTask(t *testing.T) {
	t.Parallel()
	errWriter := &fakeErrTaskWriter{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"e:1": {"id": "e:1"},
	})
	// Wire a real actions.Runner with the fake err writer so
	// engine.New's actions.ErrTaskWriterFor(runner) pulls
	// the fake.
	runner := actions.New(actions.Options{
		TaskWriter:    nil,
		ErrTaskWriter: errWriter,
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "fail-on-condition",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		// CEL division-by-zero is a runtime failure (not a
		// compile-time error).
		Condition: "(1 / 0) > 0",
		Subject:   "entity.id",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err = eng.Dispatch(context.Background(), "fail-on-condition", "e:1")
	require.NoError(t, err, "Dispatch returns Decision with Err; not a hard error")

	calls := errWriter.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "fail-on-condition", calls[0].workflow)
	assert.Equal(t, "e:1", calls[0].entityID)
	assert.Contains(t, calls[0].errMsg, "condition")
}

// TestEngine_ActionFailure_AppendsErrTask: a per-action
// failure (e.g. add_note with no writer wired) routes
// to the err-task pattern alongside the WARN log line.
func TestEngine_ActionFailure_AppendsErrTask(t *testing.T) {
	t.Parallel()
	errWriter := &fakeErrTaskWriter{}
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"e:1": {"id": "e:1"},
	})
	// NoteWriter is intentionally nil → add_note errors
	// at execute time. ErrTaskWriter records the failure.
	runner := actions.New(actions.Options{
		ErrTaskWriter: errWriter,
	})
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "no-note-writer",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        "entity.id",
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err = eng.Dispatch(context.Background(), "no-note-writer", "e:1")
	require.NoError(t, err)

	calls := errWriter.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "no-note-writer", calls[0].workflow)
	assert.Contains(t, calls[0].errMsg, "action[0] add_note")
	assert.Contains(t, calls[0].errMsg, "no NoteWriter wired")
}

// TestEngine_RunsActionsOnFired: when a workflow's
// condition evaluates true, the engine dispatches the
// workflow's action list to the configured Runner.
func TestEngine_RunsActionsOnFired(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})
	runner := &recordingRunner{}
	eng, err := New(Options{
		Bus:      bus,
		Resolver: resolver,
		Runner:   runner,
		Logger:   quietLogger(),
	})
	require.NoError(t, err)
	wf := &parser.Workflow{
		Name:           "act-on-fire",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Condition: "entity.rating > 7",
		Subject:   "entity.id",
		Actions: []parser.Action{
			{TaskAppend: &parser.TaskAppendAction{Section: "candidates", Content: "'x'"}},
		},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})

	calls := runner.snapshot()
	require.Len(t, calls, 1, "runner.Run called once on Fired=true")
	assert.Equal(t, "act-on-fire", calls[0].workflow)
	assert.Equal(t, "boardgame:b", calls[0].entityID)
	assert.Equal(t, "boardgame:b", calls[0].subject)
	require.Len(t, calls[0].actions, 1)
	require.NotNil(t, calls[0].actions[0].TaskAppend)
}

// TestEngine_SkipsActionsOnFiredFalse: when the predicate
// rejects, the runner is NOT invoked.
func TestEngine_SkipsActionsOnFiredFalse(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"rating": int64(3)},
	})
	runner := &recordingRunner{}
	eng, err := New(Options{Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger()})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "no-act",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Condition: "entity.rating > 7",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})

	assert.Empty(t, runner.snapshot(),
		"Fired=false: runner not invoked")
}

// TestErrors_ExportedSentinels_Match: the public error
// sentinels are reachable + ErrorIs through wrapped errors
// returned by Dispatch.
func TestErrors_ExportedSentinels_Match(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)

	_, err := eng.Dispatch(context.Background(), "ghost", "")
	require.True(t, errors.Is(err, ErrUnknownWorkflow))

	wf := &parser.Workflow{
		Name:           "wf",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeEntityCreated},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err = eng.Dispatch(context.Background(), "wf", "")
	require.True(t, errors.Is(err, ErrEmptyInputNotAllowed))
}
