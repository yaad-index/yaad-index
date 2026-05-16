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
	w := NewFileErrTaskWriter(vault)
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
	assert.Contains(t, got, "## Failures\n")
	assert.Contains(t, got, "- 2026-05-16T18:00:00Z (boardgame:b): condition: cel-go error: undeclared reference 'foo'")
}

// TestFileErrTaskWriter_SubsequentFailuresAppend: failures
// after the first append to the existing Failures section
// without re-creating the frontmatter.
func TestFileErrTaskWriter_SubsequentFailuresAppend(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileErrTaskWriter(vault)
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
	// Single section header
	assert.Equal(t, 1, strings.Count(got, "## Failures"))
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
	w := NewFileErrTaskWriter(vault)
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
	w := NewFileErrTaskWriter(vault)
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
	w := NewFileErrTaskWriter(vault)
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
	w := NewFileErrTaskWriter(vault)
	err := w.AppendErrTask(context.Background(), "", time.Now(), "", "x")
	require.Error(t, err)
}

// TestStubErrTaskWriter_NoOps: the stub discards every call
// silently — used by tests + dev binaries without a vault
// wired.
func TestStubErrTaskWriter_NoOps(t *testing.T) {
	t.Parallel()
	err := StubErrTaskWriter{}.AppendErrTask(context.Background(), "wf", time.Now(), "e1", "msg")
	require.NoError(t, err)
}
