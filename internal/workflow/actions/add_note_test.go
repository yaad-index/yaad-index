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

// fakeNoteWriter records every AppendNote call so
// tests can assert the runner translated the action +
// decision into the right entity + body. Returns an
// injected error if writeErr is non-nil.
type fakeNoteWriter struct {
	mu       sync.Mutex
	calls    []commentCall
	writeErr error
}

type commentCall struct {
	workflow string
	entityID string
	body     string
	field    string
	kind     string
}

func (f *fakeNoteWriter) AppendNote(_ context.Context, workflow, entityID, body, field, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commentCall{
		workflow: workflow,
		entityID: entityID,
		body:     body,
		field:    field,
		kind:     kind,
	})
	return f.writeErr
}

func (f *fakeNoteWriter) snapshot() []commentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]commentCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestAddNote_HappyPath: an add_note action with a
// target defaults to dec.EntityID, body flows through.
func TestAddNote_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakeNoteWriter{}
	r := New(Options{NoteWriter: w})
	wf := wfWithActions("pr-review",
		parser.Action{AddNote: &parser.AddNoteAction{Content: "review needed"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "pr-review", EntityID: "pr:123"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	assert.Equal(t, "pr:123", w.snapshot()[0].entityID)
	assert.Equal(t, "review needed", w.snapshot()[0].body)
}

// TestAddNote_ExplicitTarget: when action.target is set,
// it wins over decision.entity_id.
func TestAddNote_ExplicitTarget(t *testing.T) {
	t.Parallel()
	w := &fakeNoteWriter{}
	r := New(Options{NoteWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{
			Target:  "pr:456",
			Content: "explicit",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "pr:123"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "pr:456", w.snapshot()[0].entityID)
}

// TestAddNote_NoTarget_AuthorBug: action without target
// + decision without entity_id (e.g. manual target-less)
// returns ErrActionAuthorBug.
func TestAddNote_NoTarget_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeNoteWriter{}
	r := New(Options{NoteWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{Content: "x"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot())
}

// TestAddNote_EmptyContent_AuthorBug: content required.
func TestAddNote_EmptyContent_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeNoteWriter{}
	r := New(Options{NoteWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{Target: "pr:1"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddNote_WriterError: NoteWriter errors are
// surfaced with the underlying cause wrapped.
func TestAddNote_WriterError(t *testing.T) {
	t.Parallel()
	w := &fakeNoteWriter{writeErr: errors.New("vault unavailable")}
	r := New(Options{NoteWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddNote: &parser.AddNoteAction{Content: "x"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault unavailable")
}

// TestStubNoteWriter_ReturnsNotImplemented: the stub
// NoteWriter (test/dev default) returns
// ErrActionNotImplemented with the workflow + entity + body
// length surfaced for operator debugging.
func TestStubNoteWriter_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubNoteWriter{}.AppendNote(context.Background(), "wf", "pr:1", "review", "", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
	assert.Contains(t, err.Error(), "wf")
	assert.Contains(t, err.Error(), "pr:1")
}

// TestAddNote_WorkflowAttribution: the workflow name from
// the recorded Decision flows through to the NoteWriter
// as the first arg, so the production vault impl can stamp
// the Note.Author as `workflow:<name>` per ADR-0024's
// Source vocabulary.
func TestAddNote_WorkflowAttribution(t *testing.T) {
	t.Parallel()
	w := &fakeNoteWriter{}
	r := New(Options{NoteWriter: w})
	wf := wfWithActions("bgg-news",
		parser.Action{AddNote: &parser.AddNoteAction{Content: "found a match"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "bgg-news", EntityID: "pr:1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	assert.Equal(t, "bgg-news", w.snapshot()[0].workflow)
}
