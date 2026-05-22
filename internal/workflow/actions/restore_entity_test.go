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

type fakeRestoreWriter struct {
	mu       sync.Mutex
	calls    []restoreCall
	writeErr error
}

type restoreCall struct {
	workflow string
	entityID string
	reason   string
}

func (f *fakeRestoreWriter) RestoreEntity(_ context.Context, workflow, entityID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, restoreCall{
		workflow: workflow,
		entityID: entityID,
		reason:   reason,
	})
	return f.writeErr
}

func (f *fakeRestoreWriter) snapshot() []restoreCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]restoreCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestRestoreEntity_HappyPath_DefaultsToTriggeringEntity:
// mirror of TestArchiveEntity_HappyPath_DefaultsToTriggeringEntity —
// empty Entity falls back to dec.EntityID.
func TestRestoreEntity_HappyPath_DefaultsToTriggeringEntity(t *testing.T) {
	t.Parallel()
	w := &fakeRestoreWriter{}
	r := New(Options{RestoreWriter: w})
	wf := wfWithActions("github-restore-on-open",
		parser.Action{RestoreEntity: &parser.RestoreEntityAction{
			Reason: "github-state-reopened",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "github-restore-on-open", EntityID: "github:acme_proj_pr_42"},
		Activation{})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.Equal(t, "restore_entity", results[0].Type)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "github-restore-on-open", calls[0].workflow)
	assert.Equal(t, "github:acme_proj_pr_42", calls[0].entityID)
	assert.Equal(t, "github-state-reopened", calls[0].reason)
}

// TestRestoreEntity_RenderedEntityWins: rendered template
// value beats dec.EntityID fallback, same as archive.
func TestRestoreEntity_RenderedEntityWins(t *testing.T) {
	t.Parallel()
	w := &fakeRestoreWriter{}
	r := New(Options{RestoreWriter: w})
	wf := wfWithActions("restore-target",
		parser.Action{RestoreEntity: &parser.RestoreEntityAction{
			Entity: "entity.target_id",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "restore-target", EntityID: "gmail:msg-1"},
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
	assert.Equal(t, "gmail:msg-42", calls[0].entityID)
}

// TestRestoreEntity_EmptyEntityAndNoTrigger_AuthorBug: empty
// entity + empty dec.EntityID surfaces an author-bug error.
func TestRestoreEntity_EmptyEntityAndNoTrigger_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeRestoreWriter{}
	r := New(Options{RestoreWriter: w})
	wf := wfWithActions("restore-empty",
		parser.Action{RestoreEntity: &parser.RestoreEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "restore-empty"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot())
}

// TestRestoreEntity_WriterErrorPropagates: non-soft-skip
// writer errors propagate as wrapped ActionResult.Err.
func TestRestoreEntity_WriterErrorPropagates(t *testing.T) {
	t.Parallel()
	w := &fakeRestoreWriter{writeErr: errors.New("vault disk full")}
	r := New(Options{RestoreWriter: w})
	wf := wfWithActions("restore-err",
		parser.Action{RestoreEntity: &parser.RestoreEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "restore-err", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault disk full")
	assert.Contains(t, results[0].Err.Error(), "restore_entity")
}

// TestRestoreEntity_NoWriterWired_ConfigError: nil RestoreWriter
// surfaces a clear configuration-error result.
func TestRestoreEntity_NoWriterWired_ConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("restore-no-writer",
		parser.Action{RestoreEntity: &parser.RestoreEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "restore-no-writer", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no RestoreWriter wired")
}
