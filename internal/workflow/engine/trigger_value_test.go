// Regression for #456: entity_updated events expose the changed
// field's transition as trigger.old_value / trigger.new_value so
// a workflow can condition on the actual before/after values, not
// just the field name (trigger.cause). Non-update events omit the
// keys, keeping has(trigger.new_value) false per the file's
// has()-safety contract.

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestEngine_EntityUpdated_TriggerNewOldValueVisible pins that an
// entity_updated event's Old/New land as trigger.old_value /
// trigger.new_value and a condition referencing them evaluates
// against the transition.
func TestEngine_EntityUpdated_TriggerNewOldValueVisible(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"widget:alpha-prime": {"id": "widget:alpha-prime", "kind": "widget"},
	})
	wf := &parser.Workflow{
		Name:           "value-transition-only",
		AllowedPlugins: []string{"yaad-fetch"},
		Trigger: parser.Trigger{
			Type: parser.TriggerTypeEntityUpdated,
			Match: parser.TriggerMatch{
				FieldChanged: "data.state",
				Kinds:        []string{"widget"},
			},
		},
		Condition: `trigger.old_value == "open" && trigger.new_value == "closed"`,
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// Transition that doesn't match the value predicate → no fire.
	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "widget:alpha-prime", Kind: "widget",
		Field: "data.state", Old: "open", New: "in_review",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	// open → closed transition → fires.
	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "widget:alpha-prime", Kind: "widget",
		Field: "data.state", Old: "open", New: "closed",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	var fired []Decision
	for _, d := range eng.Decisions() {
		if d.Fired {
			fired = append(fired, d)
		}
	}
	require.Len(t, fired, 1, "only the open→closed transition satisfies the value predicate")
	assert.Equal(t, "widget:alpha-prime", fired[0].EntityID)
}

// TestEngine_NonUpdateEvent_TriggerValueAbsent pins the
// has()-safety contract: an entity_created event (no Old/New)
// omits the trigger value keys, so has(trigger.new_value) is
// false and a workflow keyed on it never fires.
func TestEngine_NonUpdateEvent_TriggerValueAbsent(t *testing.T) {
	t.Parallel()
	eng, bus := newEngineWithBus(t, map[string]map[string]any{
		"widget:beta-second": {"id": "widget:beta-second", "kind": "widget"},
	})
	wf := &parser.Workflow{
		Name:           "value-present-guard",
		AllowedPlugins: []string{"yaad-fetch"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEntityCreated,
			Match: parser.TriggerMatch{Kinds: []string{"widget"}},
		},
		Condition: `has(trigger.new_value)`,
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID:        "widget:beta-second",
		Kind:      "widget",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now(),
	})
	eng.WaitForIdle()

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.False(t, decs[0].Fired,
		"entity_created omits trigger.new_value → has() is false → no fire")
}
