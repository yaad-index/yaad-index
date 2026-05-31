package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// recordingArchiveWriter captures the engine's archive_when
// dispatch shape so tests can assert what the hook handed to the
// archive surface.
type recordingArchiveWriter struct {
	mu    sync.Mutex
	calls []recordedArchive
	err   error
}

type recordedArchive struct {
	workflow string
	entityID string
	reason   string
}

func (r *recordingArchiveWriter) ArchiveEntity(_ context.Context, workflow, entityID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedArchive{workflow: workflow, entityID: entityID, reason: reason})
	return r.err
}

func (r *recordingArchiveWriter) snapshot() []recordedArchive {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedArchive, len(r.calls))
	copy(out, r.calls)
	return out
}

// stubArchiveStateProbe returns a canned EntityView, optionally
// keyed by entityID so a single fixture can serve multiple
// workflows in one test.
type stubArchiveStateProbe struct {
	views map[string]decision.EntityView
	err   error
}

func (p *stubArchiveStateProbe) EntityArchiveState(_ context.Context, entityID string) (decision.EntityView, error) {
	if p.err != nil {
		return decision.EntityView{}, p.err
	}
	if v, ok := p.views[entityID]; ok {
		return v, nil
	}
	return decision.EntityView{}, nil
}

// failingActionRunner records each Run call and returns one
// pre-canned action result with the Err field set so the engine's
// runActions accumulates an "any action errored" flag.
type failingActionRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *failingActionRunner) Run(_ context.Context, wf *parser.Workflow, _ actions.Decision, _ actions.Activation) []actions.ActionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	out := make([]actions.ActionResult, len(wf.Actions))
	for i := range wf.Actions {
		out[i] = actions.ActionResult{ActionIdx: i, Type: "task_append", Err: errors.New("simulated action failure")}
	}
	return out
}

func archiveWhenWorkflowFixture(name string, predicate *parser.ArchiveWhen) *parser.Workflow {
	return &parser.Workflow{
		Name:           name,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEntityCreated,
			Match: parser.TriggerMatch{Kinds: []string{"gmail"}},
		},
		Subject: "entity.id",
		Actions: []parser.Action{
			{TaskAppend: &parser.TaskAppendAction{Section: "observed", Content: "'x'"}},
		},
		ArchiveWhen: predicate,
	}
}

// TestEngine_ArchiveWhen_FiresOnTrueAfterActions pins the ADR-0030
// §3 happy path: a workflow with archive_when whose predicate
// evaluates true post-action triggers archive on the source
// entity via the wired ArchiveWriter.
func TestEngine_ArchiveWhen_FiresOnTrueAfterActions(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1"},
	})
	runner := &recordingRunner{}
	archive := &recordingArchiveWriter{}
	probe := &stubArchiveStateProbe{
		views: map[string]decision.EntityView{
			"gmail:msg-1": {HasUnfilledGaps: false},
		},
	}
	eng, err := New(Options{
		Bus:               bus,
		Resolver:          resolver,
		Runner:            runner,
		Logger:            quietLogger(),
		ArchiveStateProbe: probe,
		ArchiveWriter:     archive,
	})
	require.NoError(t, err)
	wf := archiveWhenWorkflowFixture("gmail-catch-all",
		&parser.ArchiveWhen{AllGapsResolved: true})
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "gmail:msg-1", Kind: "gmail",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	archives := archive.snapshot()
	require.Len(t, archives, 1, "archive_when predicate true → ArchiveEntity invoked once")
	assert.Equal(t, "gmail-catch-all", archives[0].workflow)
	assert.Equal(t, "gmail:msg-1", archives[0].entityID)
	assert.Contains(t, archives[0].reason, "archive_when",
		"reason carries the predicate marker so the audit log surfaces the trigger")
}

// TestEngine_ArchiveWhen_SkipsOnPredicateFalse pins that the hook
// does NOT archive when the predicate evaluates false against the
// post-action entity state.
func TestEngine_ArchiveWhen_SkipsOnPredicateFalse(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-2": {"id": "gmail:msg-2"},
	})
	archive := &recordingArchiveWriter{}
	probe := &stubArchiveStateProbe{
		views: map[string]decision.EntityView{
			"gmail:msg-2": {HasUnfilledGaps: true}, // gaps remain → predicate false
		},
	}
	eng, err := New(Options{
		Bus:               bus,
		Resolver:          resolver,
		Runner:            &recordingRunner{},
		Logger:            quietLogger(),
		ArchiveStateProbe: probe,
		ArchiveWriter:     archive,
	})
	require.NoError(t, err)
	wf := archiveWhenWorkflowFixture("gmail-catch-all-not-yet",
		&parser.ArchiveWhen{AllGapsResolved: true})
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "gmail:msg-2", Kind: "gmail",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	assert.Empty(t, archive.snapshot(),
		"predicate false → ArchiveEntity must not be invoked")
}

// TestEngine_ArchiveWhen_NoOpForWorkflowWithoutPredicate pins
// backward-compat per ADR-0030: workflows without archive_when
// behave exactly as today.
func TestEngine_ArchiveWhen_NoOpForWorkflowWithoutPredicate(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-3": {"id": "gmail:msg-3"},
	})
	archive := &recordingArchiveWriter{}
	probe := &stubArchiveStateProbe{} // unused
	eng, err := New(Options{
		Bus:               bus,
		Resolver:          resolver,
		Runner:            &recordingRunner{},
		Logger:            quietLogger(),
		ArchiveStateProbe: probe,
		ArchiveWriter:     archive,
	})
	require.NoError(t, err)
	wf := archiveWhenWorkflowFixture("gmail-no-archive", nil) // ArchiveWhen=nil
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "gmail:msg-3", Kind: "gmail",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	assert.Empty(t, archive.snapshot(),
		"workflow without archive_when must never invoke ArchiveEntity")
}

// TestEngine_ArchiveWhen_SkipsOnActionErrors pins ADR-0030 §3:
// a workflow that errored mid-action chain does NOT trigger
// archive evaluation, regardless of predicate truth.
func TestEngine_ArchiveWhen_SkipsOnActionErrors(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-4": {"id": "gmail:msg-4"},
	})
	archive := &recordingArchiveWriter{}
	probe := &stubArchiveStateProbe{
		views: map[string]decision.EntityView{
			"gmail:msg-4": {HasUnfilledGaps: false}, // predicate would be true
		},
	}
	eng, err := New(Options{
		Bus:               bus,
		Resolver:          resolver,
		Runner:            &failingActionRunner{}, // forces action failure
		Logger:            quietLogger(),
		ArchiveStateProbe: probe,
		ArchiveWriter:     archive,
	})
	require.NoError(t, err)
	wf := archiveWhenWorkflowFixture("gmail-action-fail",
		&parser.ArchiveWhen{AllGapsResolved: true})
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "gmail:msg-4", Kind: "gmail",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	assert.Empty(t, archive.snapshot(),
		"action failure must hold the archive even when predicate would be true")
}

// TestEngine_ArchiveWhen_LogAndContinueOnWriterFailure pins
// ADR-0030 §5: a vault-side archive failure does NOT propagate
// up — the workflow's overall run still records claimed/fired
// status normally, the WARN log carries the failure for ops.
// We assert by confirming ArchiveEntity was called AND the
// engine did not surface the error to the caller (no panic, no
// observable failure on Decisions()).
func TestEngine_ArchiveWhen_LogAndContinueOnWriterFailure(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-5": {"id": "gmail:msg-5"},
	})
	archive := &recordingArchiveWriter{err: errors.New("simulated archive failure")}
	probe := &stubArchiveStateProbe{
		views: map[string]decision.EntityView{
			"gmail:msg-5": {HasUnfilledGaps: false},
		},
	}
	eng, err := New(Options{
		Bus:               bus,
		Resolver:          resolver,
		Runner:            &recordingRunner{},
		Logger:            quietLogger(),
		ArchiveStateProbe: probe,
		ArchiveWriter:     archive,
	})
	require.NoError(t, err)
	wf := archiveWhenWorkflowFixture("gmail-archive-fail",
		&parser.ArchiveWhen{AllGapsResolved: true})
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID: "gmail:msg-5", Kind: "gmail",
		SourceTag: eventbus.SourceAgent, At: time.Now(),
	})
	eng.WaitForIdle()

	// Archive was attempted (predicate true) — the WARN already
	// recorded the failure on the logger; the workflow's overall
	// run status is unaffected.
	require.Len(t, archive.snapshot(), 1,
		"archive_when must still invoke the writer; failure is logged not propagated")
}
