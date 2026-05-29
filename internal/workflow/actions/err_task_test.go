package actions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFileErrTaskWriter_FirstFailureCreates: a fresh err
// task gets the canonical frontmatter (kind: task + errored:
// true + workflow + created_at) + a Failures section header
// + the first failure line.
func TestFileErrTaskWriter_FirstFailureCreates(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	when := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)

	err := w.AppendErrTask(context.Background(), "classify", when,
		"boardgame:b", "condition: cel-go error: undeclared reference 'foo'")
	require.NoError(t, err)

	body, err := os.ReadFile(filepath.Join(vault, "tasks", "classify-err.md"))
	require.NoError(t, err)
	got := string(body)

	assert.Contains(t, got, "kind: task\n")
	assert.Contains(t, got, "errored: true\n")
	assert.Contains(t, got, "workflow: classify\n")
	// #337 Cut 1: failure lines land in the notes section;
	// the prompt section carries the operator instruction.
	assert.Contains(t, got, taskMarkerOpen(TaskSectionPrompt))
	assert.Contains(t, got, taskMarkerOpen(TaskSectionNotes))
	assert.Contains(t, got, taskMarkerClose(TaskSectionNotes))
	assert.Contains(t, got, "- 2026-05-16T18:00:00Z (boardgame:b): condition: cel-go error: undeclared reference 'foo'")
}

// TestFileErrTaskWriter_SubsequentFailuresAppend: failures
// after the first append to the existing Failures section
// without re-creating the frontmatter.
func TestFileErrTaskWriter_SubsequentFailuresAppend(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	t1 := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 16, 18, 5, 0, 0, time.UTC)

	require.NoError(t, w.AppendErrTask(context.Background(), "classify", t1, "pr:1", "first failure"))
	require.NoError(t, w.AppendErrTask(context.Background(), "classify", t2, "pr:2", "second failure"))

	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "classify-err.md"))
	got := string(body)

	// Both entries present
	assert.Contains(t, got, "first failure")
	assert.Contains(t, got, "second failure")
	// Order preserved
	assert.True(t, strings.Index(got, "first failure") < strings.Index(got, "second failure"))
	// #337 Cut 1: single notes section markers, no duplicate
	// schema render — append goes through the parse → inject
	// → render round-trip.
	assert.Equal(t, 1, strings.Count(got, taskMarkerOpen(TaskSectionNotes)))
	assert.Equal(t, 1, strings.Count(got, taskMarkerClose(TaskSectionNotes)))
	// Single frontmatter
	assert.Equal(t, 1, strings.Count(got, "kind: task"))
}

// TestFileErrTaskWriter_OperatorResolved_NextFailureCreatesFresh:
// per ADR-0024 the operator-resolve closes the err task; the
// next failure spawns a fresh one. v1 close-mechanism is
// operator-deletes-the-file. After deletion, the next
// AppendErrTask creates a brand-new err task with a fresh
// created_at + only the new failure line.
func TestFileErrTaskWriter_OperatorResolved_NextFailureCreatesFresh(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	t1 := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)

	require.NoError(t, w.AppendErrTask(context.Background(), "classify", t1, "pr:1", "old failure"))
	require.NoError(t, os.Remove(filepath.Join(vault, "tasks", "classify-err.md")))

	require.NoError(t, w.AppendErrTask(context.Background(), "classify", t2, "pr:2", "new failure"))

	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "classify-err.md"))
	got := string(body)

	assert.NotContains(t, got, "old failure", "prior failures gone after operator resolved")
	assert.Contains(t, got, "new failure")
	assert.Contains(t, got, "created_at: 2026-05-16T19:00:00Z\n", "fresh created_at on the new err task")
}

// TestFileErrTaskWriter_EmptyEntityIDOmitsParens: an
// empty entityID (pre-resolve errors or target-less manual
// fires) renders without the `(entity)` annotation.
func TestFileErrTaskWriter_EmptyEntityIDOmitsParens(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	when := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)

	require.NoError(t, w.AppendErrTask(context.Background(), "wf", when, "", "early failure"))
	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "wf-err.md"))
	got := string(body)
	assert.Contains(t, got, "- 2026-05-16T18:00:00Z: early failure")
	assert.NotContains(t, got, "()", "no empty parentheses when entityID is blank")
}

// TestFileErrTaskWriter_EmbeddedNewlinesCollapsed: error
// messages with internal newlines collapse to a single-line
// entry so the mergeSection helper's line-based shape works
// cleanly.
func TestFileErrTaskWriter_EmbeddedNewlinesCollapsed(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	when := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)

	multiline := "first line\nsecond line\nthird line"
	require.NoError(t, w.AppendErrTask(context.Background(), "wf", when, "e1", multiline))
	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "wf-err.md"))
	got := string(body)

	// All three pieces present on one line, joined by spaces
	// (the collapse).
	line := "- 2026-05-16T18:00:00Z (e1): first line second line third line"
	assert.Contains(t, got, line)
}

// TestFileErrTaskWriter_EmptyWorkflowRejected: defensive —
// an empty workflow name would produce a path under
// `tasks/-err.md`. Reject with a clear error.
func TestFileErrTaskWriter_EmptyWorkflowRejected(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	err := w.AppendErrTask(context.Background(), "", time.Now(), "", "x")
	require.Error(t, err)
}

// TestFileErrTaskWriter_PromptSectionPopulated: the err-task
// prompt section carries the operator-facing failure framing
// per #344 — workflow name interpolated, resolve surface named,
// auto-archive behavior called out. Static content for v1.
func TestFileErrTaskWriter_PromptSectionPopulated(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	when := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)

	require.NoError(t, w.AppendErrTask(context.Background(), "classify", when,
		"boardgame:b", "first failure"))
	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "classify-err.md"))
	got := string(body)

	assert.Contains(t, got, "Workflow `classify`",
		"workflow name interpolated into prompt")
	assert.Contains(t, got, "failed during action dispatch",
		"failure framing present")
	assert.Contains(t, got, "task_resolve",
		"resolve surface named")
	assert.Contains(t, got, "auto-archives on resolve",
		"auto-archive behavior called out")
	// Seed prompt must not leak — SetPrompt replaced it.
	assert.NotContains(t, got, "(populated below)",
		"seed prompt replaced by SetPrompt")
}

// TestFileErrTaskWriter_PromptSurvivesAppends: subsequent
// failures append to the notes section without touching the
// prompt — the prompt content stays put across appends per
// the #344 acceptance criteria.
func TestFileErrTaskWriter_PromptSurvivesAppends(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault, nil, nil, nil)
	t1 := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	require.NoError(t, w.AppendErrTask(context.Background(), "wf", t1, "e1", "first failure"))
	require.NoError(t, w.AppendErrTask(context.Background(), "wf", t2, "e2", "second failure"))
	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "wf-err.md"))
	got := string(body)

	// Prompt content still present after the second append.
	assert.Contains(t, got, "Workflow `wf`")
	assert.Contains(t, got, "failed during action dispatch")
	assert.Equal(t, 1, strings.Count(got, taskMarkerOpen(TaskSectionPrompt)),
		"single prompt section after append")
	// Both failure lines landed in notes.
	assert.Contains(t, got, "first failure")
	assert.Contains(t, got, "second failure")
}

// TestStubErrTaskWriter_NoOps: the stub discards every call
// silently — used by tests + dev binaries without a vault
// wired.
func TestStubErrTaskWriter_NoOps(t *testing.T) {
	t.Parallel()
	err := StubErrTaskWriter{}.AppendErrTask(context.Background(), "wf", time.Now(), "e1", "msg")
	require.NoError(t, err)
}
