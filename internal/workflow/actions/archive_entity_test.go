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

type fakeArchiveWriter struct {
	mu       sync.Mutex
	calls    []archiveCall
	writeErr error
}

type archiveCall struct {
	workflow string
	entityID string
	reason   string
}

func (f *fakeArchiveWriter) ArchiveEntity(_ context.Context, workflow, entityID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, archiveCall{
		workflow: workflow,
		entityID: entityID,
		reason:   reason,
	})
	return f.writeErr
}

func (f *fakeArchiveWriter) snapshot() []archiveCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]archiveCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestArchiveEntity_HappyPath_DefaultsToTriggeringEntity: when
// the action omits `entity`, the runner falls back to
// dec.EntityID. Mirrors the AddCanonicalEdge / AddNote /
// SetProperty default-entity shape.
func TestArchiveEntity_HappyPath_DefaultsToTriggeringEntity(t *testing.T) {
	t.Parallel()
	w := &fakeArchiveWriter{}
	r := New(Options{ArchiveWriter: w})
	wf := wfWithActions("classify-and-archive",
		parser.Action{ArchiveEntity: &parser.ArchiveEntityAction{
			Reason: "classified-into-canonical-edge",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify-and-archive", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.Equal(t, "archive_entity", results[0].Type)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "classify-and-archive", calls[0].workflow)
	assert.Equal(t, "gmail:msg-1", calls[0].entityID,
		"defaults to dec.EntityID when action.entity is empty")
	assert.Equal(t, "classified-into-canonical-edge", calls[0].reason)
}

// TestArchiveEntity_RenderedEntityWins: when the engine pre-
// renders the entity template, the runner uses the rendered
// value rather than the raw action field. Same pattern as the
// other CEL-templated fields.
func TestArchiveEntity_RenderedEntityWins(t *testing.T) {
	t.Parallel()
	w := &fakeArchiveWriter{}
	r := New(Options{ArchiveWriter: w})
	wf := wfWithActions("archive-target",
		parser.Action{ArchiveEntity: &parser.ArchiveEntityAction{
			Entity: "entity.target_id",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "archive-target", EntityID: "gmail:msg-1"},
		Activation{
			RenderedTemplates: map[int]map[string]string{
				0: {"entity": "gmail:msg-42"},
			},
		},
	)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "gmail:msg-42", calls[0].entityID,
		"rendered template value wins over dec.EntityID fallback")
}

// TestArchiveEntity_EmptyEntityAndNoTrigger_AuthorBug: when
// the rendered entity is empty AND dec.EntityID is also empty
// (e.g. manual trigger with no input), the runner surfaces an
// author-bug error rather than silently archiving nothing.
func TestArchiveEntity_EmptyEntityAndNoTrigger_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeArchiveWriter{}
	r := New(Options{ArchiveWriter: w})
	wf := wfWithActions("archive-empty",
		parser.Action{ArchiveEntity: &parser.ArchiveEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "archive-empty"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot(),
		"writer not called when entity resolution fails")
}

// TestArchiveEntity_ReasonOptional: omitting reason yields an
// empty-string reason to the writer. The writer treats empty
// reason as "use workflow name implicitly" — surfaced via the
// workflow argument, not via this field.
func TestArchiveEntity_ReasonOptional(t *testing.T) {
	t.Parallel()
	w := &fakeArchiveWriter{}
	r := New(Options{ArchiveWriter: w})
	wf := wfWithActions("archive-no-reason",
		parser.Action{ArchiveEntity: &parser.ArchiveEntityAction{
			Entity: "entity.id",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "archive-no-reason", EntityID: "gmail:msg-1"},
		Activation{},
	)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].reason)
}

// TestArchiveEntity_WriterErrorPropagates: when the underlying
// writer returns an error (any non-soft-skip surface — e.g. a
// vault I/O failure), the runner wraps it and lands a failed
// ActionResult. The runner's contract is to forward; the
// idempotent / not-found behaviors live in the writer.
func TestArchiveEntity_WriterErrorPropagates(t *testing.T) {
	t.Parallel()
	w := &fakeArchiveWriter{writeErr: errors.New("vault disk full")}
	r := New(Options{ArchiveWriter: w})
	wf := wfWithActions("archive-err",
		parser.Action{ArchiveEntity: &parser.ArchiveEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "archive-err", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault disk full")
	assert.Contains(t, results[0].Err.Error(), "archive_entity")
}

// TestArchiveEntity_NoWriterWired_ConfigError: a runner with
// no ArchiveWriter (production misconfig) surfaces a clear
// configuration-error result rather than silently dropping
// the action. Same shape as the other writer-nil checks.
func TestArchiveEntity_NoWriterWired_ConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("archive-no-writer",
		parser.Action{ArchiveEntity: &parser.ArchiveEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "archive-no-writer", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no ArchiveWriter wired")
}

// TestStubArchiveWriter_RejectsWithErrActionNotImplemented:
// production-default StubArchiveWriter (when Options.ArchiveWriter
// stays nil) rejects with ErrActionNotImplemented so dev / test
// binaries without vault wiring surface a clear failure.
func TestStubArchiveWriter_RejectsWithErrActionNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubArchiveWriter{}.ArchiveEntity(context.Background(),
		"any-workflow", "gmail:msg-1", "any-reason")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
}
