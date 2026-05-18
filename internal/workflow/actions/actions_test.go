package actions

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	mu           sync.Mutex
	calls        []taskWriterCall
	missingCalls []missingRefsCall
	writeErr     error
}

type taskWriterCall struct {
	workflow         string
	subject          string
	dedupKey         string
	entityID         string
	section          string
	content          string
	ifAlreadyPresent string
}

type missingRefsCall struct {
	workflow string
	subject  string
	refs     []string
}

func (f *fakeTaskWriter) AppendTaskSection(_ context.Context, workflow, subject, dedupKey, entityID, section, content, ifAlreadyPresent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskWriterCall{
		workflow:         workflow,
		subject:          subject,
		dedupKey:         dedupKey,
		entityID:         entityID,
		section:          section,
		content:          content,
		ifAlreadyPresent: ifAlreadyPresent,
	})
	return f.writeErr
}

func (f *fakeTaskWriter) EnsureMissingRefsSection(_ context.Context, workflow, subject string, refs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missingCalls = append(f.missingCalls, missingRefsCall{
		workflow: workflow,
		subject:  subject,
		refs:     append([]string(nil), refs...),
	})
	return nil
}

func (f *fakeTaskWriter) missingSnapshot() []missingRefsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]missingRefsCall, len(f.missingCalls))
	copy(out, f.missingCalls)
	return out
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

// TestRunner_TaskAppend_MissingRefsThreadedToWriter: the
// task_append runner invokes EnsureMissingRefsSection
// after writing content, threading dec.MissingRefs through
// so the section stays in sync. Empty refs still result in
// the call (the writer's no-op-if-section-absent semantics
// handle the empty case cleanly).
func TestRunner_TaskAppend_MissingRefsThreadedToWriter(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})
	wf := wfWithActions("wf",
		parser.Action{TaskAppend: &parser.TaskAppendAction{
			Section: "candidates", Content: "x",
		}},
	)
	dec := Decision{
		Workflow:    "wf",
		Subject:     "s",
		MissingRefs: []string{"id:a", "id:b"},
	}
	results := r.Run(context.Background(), wf, dec, Activation{})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)

	missingCalls := w.missingSnapshot()
	require.Len(t, missingCalls, 1, "EnsureMissingRefsSection runs after AppendTaskSection")
	assert.Equal(t, "wf", missingCalls[0].workflow)
	assert.Equal(t, "s", missingCalls[0].subject)
	assert.Equal(t, []string{"id:a", "id:b"}, missingCalls[0].refs)
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
			Content:          "Acme Game (2026)",
			IfAlreadyPresent: parser.IfAlreadyPresentSkip,
		}},
	)
	dec := Decision{Workflow: "review-queue", Subject: "boardgame-acme"}

	results := r.Run(context.Background(), wf, dec, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "task_append", results[0].Type)
	assert.NoError(t, results[0].Err)

	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "review-queue", calls[0].workflow)
	assert.Equal(t, "boardgame-acme", calls[0].subject)
	assert.Equal(t, "candidates", calls[0].section)
	assert.Equal(t, "Acme Game (2026)", calls[0].content)
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

// TestRunner_NoNoteWriter_ConfigError: a dispatcher
// constructed without a NoteWriter surfaces a clear
// config-error message on add_note actions — not
// ErrActionNotImplemented (those come from the
// production StubNoteWriter that ships in Phase 4.B).
func TestRunner_NoNoteWriter_ConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{Content: "hi"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "add_note", results[0].Type)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no NoteWriter wired")
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
// don't block subsequent actions. Uses the plugin_dispatch
// no-PluginDispatcher config-error path as the mid-list
// failure to exercise the continue-past-failure shape.
func TestRunner_MultipleActions_AllRun(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	// PluginDispatcher omitted → plugin_dispatch errors with
	// "no PluginDispatcher wired" config error.
	r := New(Options{TaskWriter: w})

	wf := wfWithActions("multi",
		parser.Action{TaskAppend: &parser.TaskAppendAction{Section: "a", Content: "1"}},
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{Plugin: "yaad-bgg", Command: "fetch"}},
		parser.Action{TaskAppend: &parser.TaskAppendAction{Section: "b", Content: "2"}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "multi"}, Activation{})
	require.Len(t, results, 3)
	assert.NoError(t, results[0].Err, "first task_append succeeds")
	require.Error(t, results[1].Err, "plugin_dispatch no-PluginDispatcher errors mid-list")
	assert.Contains(t, results[1].Err.Error(), "no PluginDispatcher wired")
	assert.NoError(t, results[2].Err, "later task_append still runs after the mid-list error")
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

// TestRunner_RenderedTemplates_UsedWhenPresent: when the
// engine ships a populated RenderedTemplates entry for an
// action, the runner uses the rendered value rather than the
// raw action.<Field>. Exercises add_note's target +
// content fields together.
func TestRunner_RenderedTemplates_UsedWhenPresent(t *testing.T) {
	t.Parallel()
	cw := &fakeNoteWriter{}
	r := New(Options{NoteWriter: cw})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{
			Target:  "entity.id",
			Content: "rating={{ entity.rating }}",
		}},
	)
	act := Activation{
		RenderedTemplates: map[int]map[string]string{
			0: {
				"target":  "pr:99",
				"content": "rating=9",
			},
		},
	}
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "fallback:1"}, act)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	require.Len(t, cw.snapshot(), 1)
	assert.Equal(t, "pr:99", cw.snapshot()[0].entityID)
	assert.Equal(t, "rating=9", cw.snapshot()[0].body)
}

// TestRunner_RenderedTemplates_FallbackOnNilMap: when
// Activation.RenderedTemplates is nil, the runner reads the
// raw action.<Field> verbatim and does NOT log a warning
// (legacy / no-renderer path is expected for tests).
func TestRunner_RenderedTemplates_FallbackOnNilMap(t *testing.T) {
	t.Parallel()
	cw := &fakeNoteWriter{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := New(Options{NoteWriter: cw, Logger: logger})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{
			Target:  "pr:55",
			Content: "raw content",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.Equal(t, "pr:55", cw.snapshot()[0].entityID)
	assert.Equal(t, "raw content", cw.snapshot()[0].body)
	assert.NotContains(t, logBuf.String(), "rendered-template missing",
		"nil RenderedTemplates is the silent legacy path; no Warn expected")
}

// TestRunner_RenderedTemplates_DriftWarnsOnMissingKey: when
// Activation.RenderedTemplates is non-nil but lacks the
// expected (idx, field) entry, the runner falls back to raw +
// logs a drift Warn. Production engines wire this signal so
// "engine forgot to populate" surfaces at execute time.
func TestRunner_RenderedTemplates_DriftWarnsOnMissingKey(t *testing.T) {
	t.Parallel()
	cw := &fakeNoteWriter{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := New(Options{NoteWriter: cw, Logger: logger})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{
			Target:  "fallback:target",
			Content: "fallback content",
		}},
	)
	// Engine "rendered" only `target` but forgot `content`.
	act := Activation{
		RenderedTemplates: map[int]map[string]string{
			0: {"target": "pr:42"},
		},
	}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, act)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	// target uses rendered, content falls back to raw.
	assert.Equal(t, "pr:42", cw.snapshot()[0].entityID)
	assert.Equal(t, "fallback content", cw.snapshot()[0].body)
	logStr := logBuf.String()
	assert.Contains(t, logStr, "rendered-template missing")
	assert.Contains(t, logStr, `field=content`)
	assert.Contains(t, logStr, "action_idx=0")
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
