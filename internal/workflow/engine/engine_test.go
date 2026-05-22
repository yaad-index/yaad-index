// Phase 3.B engine tests. Exercises the engine's registry
// + bus-subscription + event-routing + predicate-evaluation
// pipeline against synthetic events and a fake resolver.

package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// fakeResolver is an in-memory EntityResolver. Returns the
// seeded entity for known ids; ErrEntityNotFound for unknowns.
type fakeResolver struct {
	mu       sync.Mutex
	entities map[string]map[string]any
}

func newFakeResolver(entities map[string]map[string]any) *fakeResolver {
	if entities == nil {
		entities = map[string]map[string]any{}
	}
	return &fakeResolver{entities: entities}
}

func (f *fakeResolver) Resolve(_ context.Context, id string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if got, ok := f.entities[id]; ok {
		return got, nil
	}
	return nil, decision.ErrEntityNotFound
}

// quietLogger returns a logger that discards output. Used by
// every test to keep `go test -v` quiet.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newEngineWithBus constructs an Engine wired to a fresh
// in-memory bus + the given resolver entities. Returns the
// engine and the bus so the test can publish events.
func newEngineWithBus(t *testing.T, entities map[string]map[string]any) (*Engine, eventbus.Bus) {
	t.Helper()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(entities)
	eng, err := New(Options{
		Bus:      bus,
		Resolver: resolver,
		Logger:   quietLogger(),
	})
	require.NoError(t, err)
	return eng, bus
}

// manualWorkflow constructs a minimal workflow that the
// engine can register without bus interaction. Manual
// triggers don't subscribe, so this isolates registration
// from event-routing concerns.
func manualWorkflow(name string) *parser.Workflow {
	return &parser.Workflow{
		Name:           name,
		Version:        1,
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        "entity.id",
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
}

// TestEngine_Reconcile_RegistersWorkflows: a Reconcile call
// puts the named workflows in the registry; Registered()
// returns them sorted.
func TestEngine_Reconcile_RegistersWorkflows(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{
		manualWorkflow("beta"),
		manualWorkflow("alpha"),
	}))
	assert.Equal(t, []string{"alpha", "beta"}, eng.Registered())
}

// TestEngine_Reconcile_UnregistersRemoved: Reconcile with a
// shrinking set unregisters the dropped entries.
func TestEngine_Reconcile_UnregistersRemoved(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{
		manualWorkflow("alpha"),
		manualWorkflow("beta"),
	}))
	require.NoError(t, eng.Reconcile([]*parser.Workflow{
		manualWorkflow("alpha"),
	}))
	assert.Equal(t, []string{"alpha"}, eng.Registered())
}

// TestEngine_Reconcile_CompileFailureSkipsRegistration: a
// workflow whose condition fails to compile is logged + left
// out of the registry, but doesn't break the reconcile pass.
func TestEngine_Reconcile_CompileFailureSkipsRegistration(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	good := manualWorkflow("good")
	bad := manualWorkflow("bad")
	bad.Condition = "entity.x >> 0" // syntax error

	require.NoError(t, eng.Reconcile([]*parser.Workflow{good, bad}))
	assert.Equal(t, []string{"good"}, eng.Registered(),
		"bad workflow excluded; good registered")
}

// TestEngine_EdgeCreated_FiresPredicate: an edge_created
// trigger with matching edge_type + entity that satisfies
// the predicate fires the workflow.
func TestEngine_EdgeCreated_FiresPredicate(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "kind": "boardgame", "rating": int64(9)},
	})

	wf := &parser.Workflow{
		Name:           "fire-on-rating",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type: parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{
				EdgeType: "is_about",
			},
		},
		Condition: "entity.rating > 7",
		Subject:   "entity.id",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID:    "source:newsletter",
		ToID:      "boardgame:b",
		EdgeType:  "is_about",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now(),
	})
	eng.WaitForIdle()

	decisions := eng.Decisions()
	require.Len(t, decisions, 1)
	assert.Equal(t, "fire-on-rating", decisions[0].Workflow)
	assert.Equal(t, "boardgame:b", decisions[0].EntityID)
	assert.True(t, decisions[0].Fired)
	assert.Equal(t, "boardgame:b", decisions[0].Subject)
	assert.NoError(t, decisions[0].Err)
}

// TestEngine_EdgeCreated_PredicateFalse_RecordsDecision: an
// event whose predicate evaluates false is still recorded
// (with Fired=false) so the operator can inspect why a
// workflow that matched the trigger didn't fire.
func TestEngine_EdgeCreated_PredicateFalse_RecordsDecision(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(3)},
	})
	wf := &parser.Workflow{
		Name:           "fire-on-rating",
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
		FromID:    "src",
		ToID:      "boardgame:b",
		EdgeType:  "is_about",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now(),
	})
	eng.WaitForIdle()

	decisions := eng.Decisions()
	require.Len(t, decisions, 1)
	assert.False(t, decisions[0].Fired, "predicate rejected → not fired")
	assert.NoError(t, decisions[0].Err)
}

// TestEngine_EdgeCreated_EdgeTypeFilter: an edge event
// carrying the wrong edge type doesn't trigger the
// workflow at all.
func TestEngine_EdgeCreated_EdgeTypeFilter(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "is-about-only",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID:    "src",
		ToID:      "boardgame:b",
		EdgeType:  "authored_by", // not is_about
		SourceTag: eventbus.SourceAgent,
		At:        time.Now(),
	})
	eng.WaitForIdle()

	assert.Empty(t, eng.Decisions(),
		"non-matching edge_type → no decision recorded")
}

// TestEngine_EdgeCreated_TargetKindFilter: the target_kind
// filter resolves the edge's ToID + only fires when the
// resolved entity's kind matches.
func TestEngine_EdgeCreated_TargetKindFilter(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"kind": "boardgame", "rating": int64(9)},
		"person:p":    {"kind": "person", "rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "boardgame-only",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type: parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{
				EdgeType:   "is_about",
				TargetKind: "boardgame",
			},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "person:p", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1, "only the boardgame target fires")
	assert.Equal(t, "boardgame:b", decs[0].EntityID)
}

// TestEngine_EntityCreated_KindFilter: an entity_created
// trigger with Kind set only fires on matching kinds.
func TestEngine_EntityCreated_KindFilter(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"kind": "boardgame"},
	})
	wf := &parser.Workflow{
		Name:           "new-boardgame",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEntityCreated,
			Match: parser.TriggerMatch{Kind: "boardgame"},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// Wrong kind — no fire.
	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "person:p", Kind: "person", SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	// Right kind — fires.
	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "boardgame:b", Kind: "boardgame", SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Equal(t, "boardgame:b", decs[0].EntityID)
}

// TestEngine_FillCompleted_GapAndSourceFilter: the
// fill_completed trigger's Match.Source filter is the
// load-bearing self-loop-break shape — a workflow that
// listens to fills on its own injected gap can filter on
// source=operator so the operator's answer triggers but the
// workflow's own initial injection doesn't.
func TestEngine_FillCompleted_GapAndSourceFilter(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"source:x": {"answered": true},
	})
	wf := &parser.Workflow{
		Name:           "answer-listener",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type: parser.TriggerTypeFillCompleted,
			Match: parser.TriggerMatch{
				Gap:    "is_interesting_to_me",
				Source: "operator",
			},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// Wrong gap → no fire.
	bus.Publish(context.Background(), eventbus.FillCompletedEvent{
		EntityID: "source:x", Gap: "other_gap", SourceTag: eventbus.SourceOperator,
	})
	eng.WaitForIdle()
	// Right gap but wrong source → no fire (self-loop break).
	bus.Publish(context.Background(), eventbus.FillCompletedEvent{
		EntityID: "source:x", Gap: "is_interesting_to_me",
		SourceTag: eventbus.WorkflowSource("answer-listener"),
	})
	eng.WaitForIdle()
	// Right gap + operator source → fires.
	bus.Publish(context.Background(), eventbus.FillCompletedEvent{
		EntityID: "source:x", Gap: "is_interesting_to_me",
		SourceTag: eventbus.SourceOperator,
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Equal(t, "source:x", decs[0].EntityID)
}

// TestEngine_EntityUpdated_FieldChangedFilter: an
// entity_updated trigger only fires on matching field
// names — events for sibling fields don't reach the
// workflow. Mirrors the FillCompleted gap-filter pattern.
func TestEngine_EntityUpdated_FieldChangedFilter(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"github:acme_proj_pr_42": {"state": "closed", "number": int64(42)},
	})
	wf := &parser.Workflow{
		Name:           "github-archive-on-close",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type: parser.TriggerTypeEntityUpdated,
			Match: parser.TriggerMatch{
				FieldChanged: "data.state",
				Kind:         "github-pr",
			},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// Wrong field → no fire.
	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "github:acme_proj_pr_42", Kind: "github-pr",
		Field: "data.comment_count", Old: 1, New: 2,
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	// Right field but wrong kind → no fire.
	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "github:acme_proj_issue_9", Kind: "github-issue",
		Field: "data.state", Old: "open", New: "closed",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	// Right field + right kind → fires.
	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "github:acme_proj_pr_42", Kind: "github-pr",
		Field: "data.state", Old: "open", New: "closed",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Equal(t, "github:acme_proj_pr_42", decs[0].EntityID)
}

// TestEngine_ContextBindings_FedIntoCondition: a workflow
// with a context binding via graph.get(...) sees the bound
// value in the condition expression.
func TestEngine_ContextBindings_FedIntoCondition(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:edition-2": {"rating": int64(3), "previous_edition_id": "boardgame:edition-1"},
		"boardgame:edition-1": {"rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "edition-aware",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Context: []parser.ContextBinding{
			{Name: "prior", Via: "graph.get(entity.previous_edition_id)"},
		},
		Condition: "entity.rating > 7 || (prior != null && prior.rating > 7)",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:edition-2", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].Fired,
		"prior edition's high rating drives the predicate to true")
	assert.Empty(t, decs[0].MissingRefs)
}

// TestEngine_MissingRefs_SurfaceOnDecision: a graph.get(id)
// to a non-resolving id records a MissingRef on the
// decision. The engine de-dups + sorts.
func TestEngine_MissingRefs_SurfaceOnDecision(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(5), "ref": "boardgame:missing"},
	})
	wf := &parser.Workflow{
		Name:           "refs",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Context: []parser.ContextBinding{
			{Name: "other", Via: "graph.get(entity.ref)"},
		},
		Condition: "entity.rating > 7 || (other != null && other.rating > 7)",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	require.Len(t, decs[0].MissingRefs, 1)
	assert.Equal(t, "boardgame:missing", decs[0].MissingRefs[0].ID)
	assert.False(t, decs[0].Fired)
}

// TestEngine_ResolveFailure_OnTriggerEntity: when the
// trigger event's primary entity doesn't resolve (mid-event
// deletion, store transient miss), the engine records a
// MissingRef on the entity id itself.
func TestEngine_ResolveFailure_OnTriggerEntity(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, nil) // empty resolver
	wf := &parser.Workflow{
		Name:           "resolver-miss",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type: parser.TriggerTypeEntityCreated,
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "gone", Kind: "boardgame", SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	require.Len(t, decs[0].MissingRefs, 1)
	assert.Equal(t, "gone", decs[0].MissingRefs[0].ID)
}

// TestEngine_ConditionRuntimeError_RecordsErr: a condition
// that produces a runtime evaluation error (rare; usually
// type-mismatch at Compile but reachable via runtime
// arithmetic) is captured as decision.Err for the engine
// log + future err-task pattern.
func TestEngine_ConditionRuntimeError_RecordsErr(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"name": "Brass"},
	})
	wf := &parser.Workflow{
		Name:           "non-bool",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		// Returns string, not bool — EvalBool surfaces an error.
		Condition: "entity.name",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	require.Error(t, decs[0].Err)
	assert.Contains(t, decs[0].Err.Error(), "condition")
}

// TestEngine_Decisions_RingBufferBound: decisions beyond the
// ring size evict the oldest entries.
func TestEngine_Decisions_RingBufferBound(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"rating": int64(9)},
	})
	eng, err := New(Options{
		Bus:              bus,
		Resolver:         resolver,
		Logger:           quietLogger(),
		DecisionRingSize: 3,
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "rb",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	for i := 0; i < 5; i++ {
		bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
			FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
			SourceTag: eventbus.SourceAgent, At: time.Now(),
		})
		eng.WaitForIdle()
	}
	assert.Len(t, eng.Decisions(), 3, "ring size caps the buffer to 3 most-recent")
}

// TestEngine_NewRequiresBus: nil Bus → constructor error.
func TestEngine_NewRequiresBus(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Bus: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bus is required")
}

// TestEngine_Reconcile_HotReloadRebuildsRegistration: when a
// workflow's shape changes across Reconcile calls (mtime
// bump → new compile), the engine drops the old registration
// + builds a fresh one. We exercise this by changing the
// condition between calls + asserting the new condition
// applies.
func TestEngine_Reconcile_HotReloadRebuildsRegistration(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(5)},
	})

	v1 := &parser.Workflow{
		Name:           "hot-reload",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Condition: "entity.rating > 7",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{v1}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	require.Len(t, eng.Decisions(), 1)
	assert.False(t, eng.Decisions()[0].Fired, "v1: rating 5 fails the > 7 condition")

	// Operator edits the workflow: lower the threshold.
	v2 := *v1
	v2.Condition = "entity.rating > 0"
	require.NoError(t, eng.Reconcile([]*parser.Workflow{&v2}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	decs := eng.Decisions()
	require.Len(t, decs, 2, "second event recorded under v2")
	assert.True(t, decs[1].Fired, "v2: rating 5 satisfies > 0")
}

// TestEngine_UnregisteredWorkflow_DoesNotFire: after a
// Reconcile that drops a workflow, subsequent matching events
// don't produce decisions for it. The bus subscription was
// torn down on unregister.
func TestEngine_UnregisteredWorkflow_DoesNotFire(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "going-away",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))
	require.NoError(t, eng.Reconcile([]*parser.Workflow{})) // drop it

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	assert.Empty(t, eng.Decisions(),
		"unregistered workflow's bus subscription was torn down")
}

// TestEngine_NilResolver_TriggerEntityMissing: a nil
// resolver makes every event's trigger entity resolve as
// missing — the decision records the MissingRef so the
// engine still produces visible output for the operator.
func TestEngine_NilResolver_TriggerEntityMissing(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	eng, err := New(Options{Bus: bus, Logger: quietLogger()})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "noResolver",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeEntityCreated},
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "any", Kind: "boardgame", SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	decs := eng.Decisions()
	require.Len(t, decs, 1)
	require.Len(t, decs[0].MissingRefs, 1)
	assert.Equal(t, "any", decs[0].MissingRefs[0].ID)
}

// TestEngine_SubjectTemplate_Rendered: the workflow's
// subject template is rendered against the activation and
// stored on the decision.
func TestEngine_SubjectTemplate_Rendered(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"slug": "brass-birmingham", "rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "subj",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Subject: "entity.slug",
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Equal(t, "brass-birmingham", decs[0].Subject)
}

// TestEngine_DedupMissingRefs: when multiple eval stages
// (context bindings + condition + subject) each surface the
// same missing id, the final Decision's MissingRefs slice
// has it once.
func TestEngine_DedupMissingRefs(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"rating": int64(5), "ref": "boardgame:gone"},
	})
	wf := &parser.Workflow{
		Name:           "dedup",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Context: []parser.ContextBinding{
			{Name: "a", Via: "graph.get(entity.ref)"},
			{Name: "b", Via: "graph.get(entity.ref)"}, // same missing id
		},
		Condition: "a != null && b != null",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()
	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Len(t, decs[0].MissingRefs, 1, "duplicate missing-ref across stages dedups to 1")
}

// TestEngine_ErrEntityNotFound_SentinelTranslation: a
// resolver that returns the typed sentinel produces a
// MissingRef (not a fatal Err). We exercise this for the
// graph.get path indirectly through context bindings.
func TestEngine_ErrEntityNotFound_SentinelTranslation(t *testing.T) {
	t.Parallel()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b"},
	})
	bus := eventbus.NewMemoryBus()
	eng, err := New(Options{Bus: bus, Resolver: resolver, Logger: quietLogger()})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "sentinel",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Context: []parser.ContextBinding{
			{Name: "missing", Via: `graph.get("does-not-exist")`},
		},
		Condition: "missing == null",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID: "src", ToID: "boardgame:b", EdgeType: "is_about",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].Fired, "missing == null → true → predicate passes")
	require.Len(t, decs[0].MissingRefs, 1)
	assert.Equal(t, "does-not-exist", decs[0].MissingRefs[0].ID)

	// And explicit: fatal resolver errors (anything other
	// than ErrEntityNotFound) should NOT be the same path —
	// they'd produce dec.Err. We don't test that branch here
	// (the resolver is fake-not-found by construction).
	_ = errors.New
}

// TestEngine_ActionTemplates_RenderedAndPassedToRunner: the
// engine pre-renders each action's template fields against
// the activation + ships the rendered values to the runner
// via Activation.RenderedTemplates. Exercises mustache + a
// mix of fields (add_note.target / content +
// add_gap.entity).
func TestEngine_ActionTemplates_RenderedAndPassedToRunner(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "kind": "boardgame", "name": "Brass", "year": int64(2018)},
	})
	rec := &recordingRunner{}
	eng, err := New(Options{
		Bus:      bus,
		Resolver: resolver,
		Runner:   rec,
		Logger:   quietLogger(),
	})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "wf",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEdgeCreated,
			Match: parser.TriggerMatch{EdgeType: "is_about"},
		},
		Subject:     "{{ entity.id }}",
		AddableGaps: []string{"is_interesting_to_me"},
		Actions: []parser.Action{
			{AddNote: &parser.AddNoteAction{
				Target:  "entity.id",
				Content: "{{ entity.name }} ({{ entity.year }})",
			}},
			{AddGap: &parser.AddGapAction{
				Entity: "entity.id",
				Gap:    "is_interesting_to_me",
			}},
		},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID:    "newsletter:1",
		ToID:      "boardgame:b",
		EdgeType:  "is_about",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now(),
	})
	eng.WaitForIdle()

	calls := rec.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, "wf", calls[0].workflow)
	require.Equal(t, "boardgame:b", calls[0].subject, "subject template rendered")
	require.Len(t, calls[0].actions, 2)
	require.NotNil(t, calls[0].rendered)

	// Action 0 — add_note with rendered target + content.
	require.NotNil(t, calls[0].actions[0].AddNote)
	assert.Equal(t, "boardgame:b", calls[0].rendered[0]["target"])
	assert.Equal(t, "Brass (2018)", calls[0].rendered[0]["content"])

	// Action 1 — add_gap with rendered entity field.
	require.NotNil(t, calls[0].actions[1].AddGap)
	assert.Equal(t, "boardgame:b", calls[0].rendered[1]["entity"])
}

// TestEngine_ActionTemplates_CompileFailureSkipsRegistration:
// a workflow whose action template fails to compile is logged
// + excluded from the registry — same shape as condition
// compile failures.
func TestEngine_ActionTemplates_CompileFailureSkipsRegistration(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	good := manualWorkflow("good")
	bad := manualWorkflow("bad")
	// add_note.content with an unmatched mustache open is
	// a template parse error at register time.
	bad.Actions = []parser.Action{
		{AddNote: &parser.AddNoteAction{
			Content: "broken {{ entity.name",
		}},
	}

	require.NoError(t, eng.Reconcile([]*parser.Workflow{good, bad}))
	assert.Equal(t, []string{"good"}, eng.Registered(),
		"bad-template workflow excluded; good registered")
}
