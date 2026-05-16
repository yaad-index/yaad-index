package actions

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// fakeTaskWriter records each AppendTaskSection call so
// tests can assert the runner translated the action +
// decision into the right find-or-create-then-append
// invocation. Returns an injected error if WriteErr is
// non-nil.
type fakeTaskWriter struct {
	mu       sync.Mutex
	calls    []taskWriterCall
	writeErr error
}

type taskWriterCall struct {
	workflow         string
	subject          string
	section          string
	content          string
	ifAlreadyPresent string
}

func (f *fakeTaskWriter) AppendTaskSection(_ context.Context, workflow, subject, section, content, ifAlreadyPresent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskWriterCall{
		workflow:         workflow,
		subject:          subject,
		section:          section,
		content:          content,
		ifAlreadyPresent: ifAlreadyPresent,
	})
	return f.writeErr
}

func (f *fakeTaskWriter) snapshot() []taskWriterCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]taskWriterCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func wfWithActions(name string, acts ...parser.Action) *parser.Workflow {
	return &parser.Workflow{
		Name:           name,
		Version:        1,
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Actions:        acts,
	}
}

// TestRunner_TaskAppend_HappyPath: a task_append action
// produces one AppendTaskSection call with the right
// workflow / subject / section / content + the action's
// if_already_present value flowing through.
func TestRunner_TaskAppend_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})

	wf := wfWithActions("review-queue",
		parser.Action{TaskAppend: &parser.TaskAppendAction{
			Section:          "candidates",
			Content:          "Brass: Birmingham (2018)",
			IfAlreadyPresent: parser.IfAlreadyPresentSkip,
		}},
	)
	dec := Decision{Workflow: "review-queue", Subject: "boardgame-brass"}

	results := r.Run(context.Background(), wf, dec, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "task_append", results[0].Type)
	assert.NoError(t, results[0].Err)

	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "review-queue", calls[0].workflow)
	assert.Equal(t, "boardgame-brass", calls[0].subject)
	assert.Equal(t, "candidates", calls[0].section)
	assert.Equal(t, "Brass: Birmingham (2018)", calls[0].content)
	assert.Equal(t, parser.IfAlreadyPresentSkip, calls[0].ifAlreadyPresent)
}

// TestRunner_TaskAppend_DefaultsIfAlreadyPresent: when
// the action omits if_already_present, the runner sends
// the documented default ("skip") to the writer.
func TestRunner_TaskAppend_DefaultsIfAlreadyPresent(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})

	wf := wfWithActions("wf",
		parser.Action{TaskAppend: &parser.TaskAppendAction{
			Section: "s", Content: "c",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	assert.Equal(t, parser.IfAlreadyPresentSkip, w.snapshot()[0].ifAlreadyPresent,
		"empty if_already_present defaults to skip at runtime")
}

// TestRunner_TaskAppend_WriterError: TaskWriter errors are
// surfaced as ActionResult.Err with the underlying cause
// wrapped.
func TestRunner_TaskAppend_WriterError(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{writeErr: errors.New("vault unavailable")}
	r := New(Options{TaskWriter: w})

	wf := wfWithActions("wf", parser.Action{TaskAppend: &parser.TaskAppendAction{
		Section: "s", Content: "c",
	}})
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault unavailable")
}

// TestRunner_TaskAppend_NoWriter: a runner constructed
// without a TaskWriter returns a clear configuration
// error rather than silently skipping. This is the
// "engine started without vault wiring" path.
func TestRunner_TaskAppend_NoWriter(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf", parser.Action{TaskAppend: &parser.TaskAppendAction{
		Section: "s", Content: "c",
	}})
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no TaskWriter wired")
}

// TestRunner_TaskAppend_EmptySection: a workflow-author
// mistake (empty section) surfaces as ErrActionAuthorBug
// — distinguishable from runtime errors so the engine /
// future err-task pattern can categorize.
func TestRunner_TaskAppend_EmptySection(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})
	wf := wfWithActions("wf", parser.Action{TaskAppend: &parser.TaskAppendAction{
		Content: "c",
	}})
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot(), "no writer call on author-bug rejection")
}

// TestRunner_StubActions_ReturnNotImplemented: the three
// stub primitives (add_comment, plugin_dispatch, add_gap)
// return ErrActionNotImplemented per Phase 4.A's
// stub-but-reject policy. Phase 4.B / 4.C replace with
// real impls.
// TestRunner_PluginDispatch_StillStubbed: plugin_dispatch
// stays an in-dispatcher stub returning ErrActionNotImplemented
// in Phase 4.B (Phase 4.C replaces it). The other primitives
// now route to real runner-side code (with stub writers
// at the production wiring layer per Path B).
func TestRunner_PluginDispatch_StillStubbed(t *testing.T) {
	t.Parallel()
	r := New(Options{TaskWriter: &fakeTaskWriter{}})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{Plugin: "yaad-bgg", Command: "fetch"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "plugin_dispatch", results[0].Type)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionNotImplemented)
}

// TestRunner_NoCommentWriter_ConfigError: a dispatcher
// constructed without a CommentWriter surfaces a clear
// config-error message on add_comment actions — not
// ErrActionNotImplemented (those come from the
// production StubCommentWriter that ships in Phase 4.B).
func TestRunner_NoCommentWriter_ConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf",
		parser.Action{AddComment: &parser.AddCommentAction{Content: "hi"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "add_comment", results[0].Type)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no CommentWriter wired")
}

// TestRunner_NoGapWriter_ConfigError: same shape for
// add_gap when GapWriter is nil.
func TestRunner_NoGapWriter_ConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "g"}},
	)
	wf.AddableGaps = []string{"g"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "add_gap", results[0].Type)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no GapWriter wired")
}

// TestRunner_MultipleActions_AllRun: a workflow with
// multiple actions has each run in order; failures in one
// don't block subsequent actions. Uses plugin_dispatch
// (the only remaining in-dispatcher stub in Phase 4.B) as
// the mid-list error to exercise the continue-past-failure
// path.
func TestRunner_MultipleActions_AllRun(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})

	wf := wfWithActions("multi",
		parser.Action{TaskAppend: &parser.TaskAppendAction{Section: "a", Content: "1"}},
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{Plugin: "yaad-bgg", Command: "fetch"}},
		parser.Action{TaskAppend: &parser.TaskAppendAction{Section: "b", Content: "2"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "multi"}, Activation{})
	require.Len(t, results, 3)
	assert.NoError(t, results[0].Err, "first task_append succeeds")
	assert.ErrorIs(t, results[1].Err, ErrActionNotImplemented, "stub plugin_dispatch errors mid-list")
	assert.NoError(t, results[2].Err, "later task_append still runs after stub error")
	require.Len(t, w.snapshot(), 2)
}

// TestRunner_EmptyActions_NilResult: a workflow with no
// actions returns nil — defensive (the parser should
// reject empty-actions but the runner handles it
// gracefully).
func TestRunner_EmptyActions_NilResult(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("empty")
	results := r.Run(context.Background(), wf, Decision{Workflow: "empty"}, Activation{})
	assert.Nil(t, results)
}

// TestNopRunner_RecordsTypes: the NopRunner reports per-
// action types without invoking any side-effects.
func TestNopRunner_RecordsTypes(t *testing.T) {
	t.Parallel()
	r := NopRunner{}
	wf := wfWithActions("nop",
		parser.Action{TaskAppend: &parser.TaskAppendAction{Section: "s", Content: "c"}},
		parser.Action{AddGap: &parser.AddGapAction{Gap: "g"}},
	)
	results := r.Run(context.Background(), wf, Decision{}, Activation{})
	require.Len(t, results, 2)
	assert.Equal(t, "task_append", results[0].Type)
	assert.Equal(t, "add_gap", results[1].Type)
	for _, r := range results {
		assert.NoError(t, r.Err, "NopRunner never errors")
	}
}
