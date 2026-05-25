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

// helper assert for `errors.Is`-style sentinel checks (silences
// the `errors` import when no test actually uses it).
var _ = errors.Is
