package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// ctxAwareResolver mirrors fakeResolver but honors context cancellation,
// like a real DB-backed resolver. It lets the test observe that the engine
// processes a fill-driven event with a context detached from the
// publishing request's cancellation.
type ctxAwareResolver struct {
	entities map[string]map[string]any
}

func (r *ctxAwareResolver) Resolve(ctx context.Context, id string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if got, ok := r.entities[id]; ok {
		return got, nil
	}
	return nil, decision.ErrEntityNotFound
}

// TestEngine_FillCompleted_FiresAfterPublisherContextCancelled regresses
// the "fill_field sets is_actionable=yes but the workflow never spawns the
// task" bug. The engine processes events asynchronously, so the publishing
// HTTP request's context (a POST /v1/entities/{id}/fill call) is cancelled
// by the time the worker dequeues. The worker must process with a context
// detached from that cancellation (context.WithoutCancel at enqueue) — or
// every resolve / spawn fails with context.Canceled and the workflow
// silently no-ops even though its condition matches.
func TestEngine_FillCompleted_FiresAfterPublisherContextCancelled(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := &ctxAwareResolver{entities: map[string]map[string]any{
		"gmail:msg": {"id": "gmail:msg", "kind": "gmail", "is_actionable": "yes"},
	}}
	eng, err := New(Options{Bus: bus, Resolver: resolver, Logger: quietLogger()})
	require.NoError(t, err)

	wf := &parser.Workflow{
		Name:           "gmail-actionable-to-task",
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeFillCompleted,
			Match: parser.TriggerMatch{Gap: "is_actionable"},
		},
		Condition: `entity.is_actionable == "yes"`,
		Subject:   "entity.id",
		Actions:   []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'task'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// Publish the fill event with an ALREADY-CANCELLED context — exactly
	// what the async worker sees once the /fill request has returned.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bus.Publish(ctx, eventbus.FillCompletedEvent{
		EntityID:  "gmail:msg",
		Gap:       "is_actionable",
		SourceTag: eventbus.SourceOperator,
		At:        time.Now(),
	})
	eng.WaitForIdle()

	decisions := eng.Decisions()
	require.Len(t, decisions, 1, "the fill event must produce a decision even with a cancelled publisher context")
	d := decisions[0]
	assert.Equal(t, "gmail-actionable-to-task", d.Workflow)
	assert.Equal(t, "gmail:msg", d.EntityID)
	assert.NoError(t, d.Err, "worker must run with a context detached from the request's cancellation")
	assert.True(t, d.Fired, "the workflow must fire despite the cancelled publisher context")
}
