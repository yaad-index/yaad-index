// Phase 3.C tests: Engine.Dispatch (manual-trigger entry
// point) + the edge-field-completeness fold-in
// (from_title / to_title / timestamp).

package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
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
		Actions: []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
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
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
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
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
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
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "miss", "boardgame:none")
	require.NoError(t, err, "missing entity surfaces as MissingRef, not error")
	assert.Equal(t, "boardgame:none", dec.EntityID)
	require.Len(t, dec.MissingRefs, 1)
	assert.Equal(t, "boardgame:none", dec.MissingRefs[0].ID)
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
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err := eng.Dispatch(context.Background(), "ring", "boardgame:b")
	require.NoError(t, err)

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.Equal(t, "ring", decs[0].Workflow)
	assert.Equal(t, "boardgame:b", decs[0].EntityID)
}

// TestEngine_EdgeFields_FullSet covers the PR-80 fold-in:
// the edge map populated by makeEdgeHandler now includes
// from_title / to_title / timestamp in addition to
// type / from / to.
func TestEngine_EdgeFields_FullSet(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"source:newsletter":         {"title": "May Newsletter"},
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
		Actions:   []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
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
		Actions:   []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
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
		Actions:   []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
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
		Actions:        []parser.Action{{AddComment: &parser.AddCommentAction{Content: "x"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	_, err = eng.Dispatch(context.Background(), "wf", "")
	require.True(t, errors.Is(err, ErrEmptyInputNotAllowed))
}
