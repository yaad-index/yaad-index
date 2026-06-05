package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestDispatch_PausedManualDoesNotFire pins #440: a paused workflow stays
// registered (workflow_list still surfaces it with its status) but does
// not fire — here on the manual target-less dispatch path (runEvaluation).
func TestDispatch_PausedManualDoesNotFire(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	wf := &parser.Workflow{
		Name:           "paused-daily",
		Status:         parser.StatusPaused,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        `"daily"`,
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "paused-daily", "")
	require.NoError(t, err)
	assert.False(t, dec.Fired, "paused workflow must not fire on manual dispatch (#440)")

	var found bool
	for _, s := range eng.List() {
		if s.Name == "paused-daily" {
			found = true
			assert.Equal(t, parser.StatusPaused, s.Status, "workflow_list reflects paused status (#440)")
		}
	}
	assert.True(t, found, "paused workflow still appears in workflow_list")
}

// TestDispatch_DraftWithTargetDoesNotFire pins that a draft workflow is
// inert on the with-target dispatch path (evaluateAndRecord) too — even
// when its condition would hold for the resolved entity.
func TestDispatch_DraftWithTargetDoesNotFire(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	})
	wf := &parser.Workflow{
		Name:           "draft-by-id",
		Status:         parser.StatusDraft,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Condition:      "entity.rating > 7",
		Subject:        "entity.id",
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "draft-by-id", "boardgame:b")
	require.NoError(t, err)
	assert.False(t, dec.Fired, "draft workflow must not fire even when the condition would hold (#440)")
}

// TestDispatch_ActiveThenPaused_NoStaleFiredDecision pins the #440 review
// fix: after a workflow fires while active, gets paused, and is dispatched
// again, the skip must record a FRESH non-fired decision — otherwise
// Dispatch's findRecentDecision surfaces the stale Fired=true from the
// earlier active run.
func TestDispatch_ActiveThenPaused_NoStaleFiredDecision(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	wf := &parser.Workflow{
		Name:           "toggle",
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Subject:        `"daily"`,
		Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	dec, err := eng.Dispatch(context.Background(), "toggle", "")
	require.NoError(t, err)
	require.True(t, dec.Fired, "active workflow fires")

	// Operator pauses it (empty ContentHash → Reconcile re-registers).
	paused := *wf
	paused.Status = parser.StatusPaused
	require.NoError(t, eng.Reconcile([]*parser.Workflow{&paused}))

	dec2, err := eng.Dispatch(context.Background(), "toggle", "")
	require.NoError(t, err)
	assert.False(t, dec2.Fired,
		"after pause, dispatch records + returns a fresh non-fired decision, not the stale active one (#440)")
}

// TestWorkflowActive pins the gate predicate: only active (and the unset
// default) is active; paused / draft are not.
func TestWorkflowActive(t *testing.T) {
	t.Parallel()
	assert.True(t, workflowActive(&parser.Workflow{Status: parser.StatusActive}))
	assert.True(t, workflowActive(&parser.Workflow{Status: ""}), "unset defaults to active")
	assert.False(t, workflowActive(&parser.Workflow{Status: parser.StatusPaused}))
	assert.False(t, workflowActive(&parser.Workflow{Status: parser.StatusDraft}))
}
