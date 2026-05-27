// Tests for the #314 task-write auto-commit signal.
// FileTaskWriter / FileErrTaskWriter / WriteResolutionTask
// must invoke the wired Committer's OnWrite after every
// successful on-disk write — both the first-create branch
// and the subsequent-mutation branch — so the vault auto-
// committer stages + commits workflow-driven task-body
// updates alongside entity writes.

package actions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

func testNow() time.Time { return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC) }

type recordingCommitter struct {
	mu    sync.Mutex
	calls []recordingCommitterCall
}

type recordingCommitterCall struct {
	relPath string
	message string
	author  string
}

func (c *recordingCommitter) OnWrite(_ context.Context, relPath, message, author string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, recordingCommitterCall{relPath: relPath, message: message, author: author})
	return nil
}

func (c *recordingCommitter) Close() error { return nil }

func (c *recordingCommitter) snapshot() []recordingCommitterCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]recordingCommitterCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestFileTaskWriter_AppendTaskSection_NotifiesCommitter_Create
// pins the #314 contract for the first-create branch: a fresh
// task file write signals the committer with the task's
// relative path + a `task: ...: create` message.
func TestFileTaskWriter_AppendTaskSection_NotifiesCommitter_Create(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	c := &recordingCommitter{}
	w := NewFileTaskWriter(vault, nil, nil, nil, c, nil)
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"linkedin-hiring", "hiring-2026-05", "", "", "alerts",
		"Acme is hiring", parser.IfAlreadyPresentSkip))
	calls := c.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "tasks/linkedin-hiring-hiring-2026-05.md", calls[0].relPath)
	assert.Contains(t, calls[0].message, "task: linkedin-hiring hiring-2026-05: create")
}

// TestFileTaskWriter_AppendTaskSection_NotifiesCommitter_Append
// pins the #314 contract for the subsequent-append branch — the
// load-bearing case the issue surfaced ("workflow-driven
// mutations to tasks/<id>.md leave the file unstaged after first
// commit"). Two appends → two committer notifications, both
// stamped with `task: ...: append`.
func TestFileTaskWriter_AppendTaskSection_NotifiesCommitter_Append(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	c := &recordingCommitter{}
	w := NewFileTaskWriter(vault, nil, nil, nil, c, nil)
	ctx := context.Background()
	require.NoError(t, w.AppendTaskSection(ctx, "wf", "subj", "", "", "alerts", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(ctx, "wf", "subj", "", "", "alerts", "second", parser.IfAlreadyPresentSkip))
	calls := c.snapshot()
	require.Len(t, calls, 2)
	assert.Contains(t, calls[0].message, ": create")
	assert.Contains(t, calls[1].message, ": append")
}

// TestFileTaskWriter_ResolveTaskLine_NotifiesCommitter pins the
// #166 line-flip path. A successful flip writes the file then
// signals the committer; a no-match returns early WITHOUT
// notifying (no on-disk write, so no commit signal is owed).
func TestFileTaskWriter_ResolveTaskLine_NotifiesCommitter(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	c := &recordingCommitter{}
	w := NewFileTaskWriter(vault, nil, nil, nil, c, nil)
	ctx := context.Background()
	require.NoError(t, w.AppendTaskSection(ctx, "wf", "subj", "", "", "candidates",
		"- [ ] acme/widget#42 needs triage", parser.IfAlreadyPresentSkip))
	c.mu.Lock()
	c.calls = nil // clear the create-time signal so the next snapshot only shows the flip.
	c.mu.Unlock()

	require.NoError(t, w.ResolveTaskLine(ctx, "wf", "subj", "candidates",
		"acme/widget#42", parser.TaskResolveModeCheck))
	calls := c.snapshot()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].message, ": resolve-line")

	// No-match → no commit signal (file unchanged).
	c.mu.Lock()
	c.calls = nil
	c.mu.Unlock()
	require.NoError(t, w.ResolveTaskLine(ctx, "wf", "subj", "candidates",
		"never-matches", parser.TaskResolveModeCheck))
	assert.Empty(t, c.snapshot(), "no-match resolve-line must NOT signal the committer (no on-disk write)")
}

// TestFileTaskWriter_NilCommitterIsNoOp pins the back-compat
// path: tests + dev binaries without a wired committer keep
// working — every write succeeds with zero committer calls.
func TestFileTaskWriter_NilCommitterIsNoOp(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil, nil)
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "alerts", "x", parser.IfAlreadyPresentSkip))
}

// TestFileErrTaskWriter_AppendErrTask_NotifiesCommitter pins
// the #314 contract for the err-task surface: every append
// (first-create AND subsequent failure entries) signals the
// committer.
func TestFileErrTaskWriter_AppendErrTask_NotifiesCommitter(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	c := &recordingCommitter{}
	w := NewFileErrTaskWriter(vault, nil, c, nil)
	ctx := context.Background()
	require.NoError(t, w.AppendErrTask(ctx, "wf", testNow(), "email:m1", "first failure"))
	require.NoError(t, w.AppendErrTask(ctx, "wf", testNow(), "email:m2", "second failure"))
	calls := c.snapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, "tasks/wf-err.md", calls[0].relPath)
	assert.Contains(t, calls[0].message, ": err-create")
	assert.Contains(t, calls[1].message, ": err-append")
}

// TestFileTaskWriter_WriteResolutionTask_NotifiesCommitter pins
// the #314 contract for Cut C3.1's resolution-task surface.
// Idempotency-hit (second write with the same tuple) skips the
// file write — and therefore skips the committer signal.
func TestFileTaskWriter_WriteResolutionTask_NotifiesCommitter(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	c := &recordingCommitter{}
	w := NewFileTaskWriter(vault, nil, nil, nil, c, nil)
	d := &edgewrite.ResolutionDeferred{
		From: "email:m1", EdgeType: "mentions", TargetKind: "boardgame",
		RawTarget: "Brass", ResolverPlugin: "yaad-bgg",
		Options: map[string]plugins.DisambiguationOption{
			"boardgame:brass-birmingham": {Label: "Brass: Birmingham"},
		},
	}
	ctx := context.Background()
	_, created, err := w.WriteResolutionTask(ctx, d)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, c.snapshot(), 1, "first write signals committer")

	// Second write — idempotency-hit, file untouched → no signal.
	_, created, err = w.WriteResolutionTask(ctx, d)
	require.NoError(t, err)
	require.False(t, created)
	assert.Len(t, c.snapshot(), 1, "idempotency-hit must NOT re-signal committer")
}
