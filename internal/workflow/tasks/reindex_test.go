package tasks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/store"
)

func newReindexTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func writeTaskFile(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
}

// TestIndexFromVault_MaterializesMissingTaskRows pins the
// migration shape: pre-existing task files (no store row) get a
// `task:<slug>` row upserted on the first reindex call.
func TestIndexFromVault_MaterializesMissingTaskRows(t *testing.T) {
	t.Parallel()
	st := newReindexTestStore(t)
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	writeTaskFile(t, tasksDir, "alpha-pr-42.md", `---
kind: task
workflow: alpha
subject: pr-42
created_at: 2099-05-25T12:00:00Z
---

## Body

- something
`)
	writeTaskFile(t, tasksDir, "beta-err.md", `---
kind: task
workflow: beta
errored: true
created_at: 2099-05-25T12:01:00Z
---

## Failures

- 2099-05-25T12:01:00Z: oops
`)

	reader := NewReader(root)
	n, err := IndexFromVault(context.Background(), st, reader, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both task files materialize on first pass")

	got, err := st.GetEntity(context.Background(), "task:alpha-pr-42")
	require.NoError(t, err)
	assert.Equal(t, canonical.TaskKind, got.Kind)
	assert.Equal(t, "alpha", got.Data["workflow"])
	assert.Equal(t, "pr-42", got.Data["subject"])

	errTask, err := st.GetEntity(context.Background(), "task:beta-err")
	require.NoError(t, err)
	assert.Equal(t, true, errTask.Data["errored"])
}

// TestIndexFromVault_IdempotentOnReRun pins that pre-existing
// store rows are preserved — operator fills / set_property
// writes that landed between daemon restarts must not be
// clobbered by the next reindex sweep.
func TestIndexFromVault_IdempotentOnReRun(t *testing.T) {
	t.Parallel()
	st := newReindexTestStore(t)
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	writeTaskFile(t, tasksDir, "gamma-x.md", `---
kind: task
workflow: gamma
subject: x
created_at: 2099-05-25T12:00:00Z
---
`)

	reader := NewReader(root)
	n1, err := IndexFromVault(context.Background(), st, reader, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, 1, n1)

	// Operator-equivalent mutation: stamp an extra field on the
	// store row so the reindex sweep would clobber it if it
	// re-upserted.
	got, err := st.GetEntity(context.Background(), "task:gamma-x")
	require.NoError(t, err)
	got.Data["operator_flag"] = "set"
	require.NoError(t, st.UpsertEntity(context.Background(), got))

	n2, err := IndexFromVault(context.Background(), st, reader, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "second pass materializes nothing — row already present")

	after, err := st.GetEntity(context.Background(), "task:gamma-x")
	require.NoError(t, err)
	assert.Equal(t, "set", after.Data["operator_flag"],
		"reindex must NOT clobber prior store-side mutations")
}

// TestIndexFromVault_MissingDirectoryNoOp covers the daemon-
// startup shape where the operator hasn't authored any
// workflows yet (no tasks/ dir on disk): reindex returns
// (0, nil) rather than erroring.
func TestIndexFromVault_MissingDirectoryNoOp(t *testing.T) {
	t.Parallel()
	st := newReindexTestStore(t)
	root := t.TempDir()
	// No tasks/ dir created.

	reader := NewReader(root)
	n, err := IndexFromVault(context.Background(), st, reader, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestIndexFromVault_NilStoreNoOp pins the test-friendly shape
// where a fixture wires the reader but not a store — the helper
// short-circuits cleanly.
func TestIndexFromVault_NilStoreNoOp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reader := NewReader(root)
	n, err := IndexFromVault(context.Background(), nil, reader, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestIndexFromVault_RebuildsTriggeredByFromVia pins the
// post-PR-271-review behavior: the startup reindex walks each
// task file's `via:` breadcrumb list and emits a triggered_by
// edge per non-`unknown` source so pre-#268 task files (which
// landed without the spawn-side edge surface) get full graph-
// reachability without requiring a re-spawn.
func TestIndexFromVault_RebuildsTriggeredByFromVia(t *testing.T) {
	t.Parallel()
	st := newReindexTestStore(t)

	// Seed two source entities so the triggered_by FK can hold.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "github-pr:acme-corp_widget_pr_42", Kind: "github-pr",
	}))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "gmail:msg-a", Kind: "gmail",
	}))

	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	writeTaskFile(t, tasksDir, "watcher-pr-42.md", `---
kind: task
workflow: watcher
subject: pr-42
created_at: 2099-05-25T12:00:00Z
via:
  - workflow: watcher
    entity: github-pr:acme-corp_widget_pr_42
  - workflow: watcher
    entity: gmail:msg-a
  - workflow: watcher
    entity: unknown
---

## Body
`)

	reader := NewReader(root)
	_, err := IndexFromVault(context.Background(), st, reader, quietLogger())
	require.NoError(t, err)

	taskID := "task:watcher-pr-42"
	edges, err := st.GetEdgesFor(context.Background(), taskID, []string{canonical.EdgeTypeTriggeredBy})
	require.NoError(t, err)
	require.Len(t, edges, 2, "two non-`unknown` via entries become two triggered_by edges")
	gotTargets := map[string]bool{}
	for _, e := range edges {
		gotTargets[e.To] = true
	}
	assert.True(t, gotTargets["github-pr:acme-corp_widget_pr_42"])
	assert.True(t, gotTargets["gmail:msg-a"])
}

// TestIndexFromVault_TriggeredBySkipsMissingSource: a via
// breadcrumb naming a source that's no longer in the store
// (operator-pruned, or never-ingested) skips silently — the
// reindex log stays clean for the common migration shape.
func TestIndexFromVault_TriggeredBySkipsMissingSource(t *testing.T) {
	t.Parallel()
	st := newReindexTestStore(t)
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	writeTaskFile(t, tasksDir, "wf-x.md", `---
kind: task
workflow: wf
subject: x
created_at: 2099-05-25T12:00:00Z
via:
  - workflow: wf
    entity: nonexistent:gone
---
`)

	reader := NewReader(root)
	_, err := IndexFromVault(context.Background(), st, reader, quietLogger())
	require.NoError(t, err, "missing source via doesn't fail the reindex")

	edges, err := st.GetEdgesFor(context.Background(), "task:wf-x", []string{canonical.EdgeTypeTriggeredBy})
	require.NoError(t, err)
	assert.Empty(t, edges, "edge to missing source skipped")
}

// helper assert for `errors.Is`-style sentinel checks (silences
// the `errors` import when no test actually uses it).
var _ = errors.Is
