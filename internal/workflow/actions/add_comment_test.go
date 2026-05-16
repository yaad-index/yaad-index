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

// fakeCommentWriter records every AppendComment call so
// tests can assert the runner translated the action +
// decision into the right entity + body. Returns an
// injected error if writeErr is non-nil.
type fakeCommentWriter struct {
	mu       sync.Mutex
	calls    []commentCall
	writeErr error
}

type commentCall struct {
	entityID string
	body     string
}

func (f *fakeCommentWriter) AppendComment(_ context.Context, entityID, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commentCall{entityID: entityID, body: body})
	return f.writeErr
}

func (f *fakeCommentWriter) snapshot() []commentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]commentCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestAddComment_HappyPath: an add_comment action with a
// target defaults to dec.EntityID, body flows through.
func TestAddComment_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakeCommentWriter{}
	r := New(Options{CommentWriter: w})
	wf := wfWithActions("pr-review",
		parser.Action{AddComment: &parser.AddCommentAction{Content: "review needed"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "pr-review", EntityID: "pr:123"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	assert.Equal(t, "pr:123", w.snapshot()[0].entityID)
	assert.Equal(t, "review needed", w.snapshot()[0].body)
}

// TestAddComment_ExplicitTarget: when action.target is set,
// it wins over decision.entity_id.
func TestAddComment_ExplicitTarget(t *testing.T) {
	t.Parallel()
	w := &fakeCommentWriter{}
	r := New(Options{CommentWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddComment: &parser.AddCommentAction{
			Target:  "pr:456",
			Content: "explicit",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "pr:123"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "pr:456", w.snapshot()[0].entityID)
}

// TestAddComment_NoTarget_AuthorBug: action without target
// + decision without entity_id (e.g. manual target-less)
// returns ErrActionAuthorBug.
func TestAddComment_NoTarget_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeCommentWriter{}
	r := New(Options{CommentWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddComment: &parser.AddCommentAction{Content: "x"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot())
}

// TestAddComment_EmptyContent_AuthorBug: content required.
func TestAddComment_EmptyContent_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeCommentWriter{}
	r := New(Options{CommentWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddComment: &parser.AddCommentAction{Target: "pr:1"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddComment_WriterError: CommentWriter errors are
// surfaced with the underlying cause wrapped.
func TestAddComment_WriterError(t *testing.T) {
	t.Parallel()
	w := &fakeCommentWriter{writeErr: errors.New("vault unavailable")}
	r := New(Options{CommentWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddComment: &parser.AddCommentAction{Content: "x"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault unavailable")
}

// TestStubCommentWriter_ReturnsNotImplemented: the
// production-default writer (Phase 4.B stub) returns
// ErrActionNotImplemented with the attempted entity + body
// length surfaced for operator debugging.
func TestStubCommentWriter_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubCommentWriter{}.AppendComment(context.Background(), "pr:1", "review")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
	assert.Contains(t, err.Error(), "pr:1")
}
