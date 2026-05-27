// Tests covering the #268 entity-promotion side of
// FileTaskWriter: first-create upserts the task store row +
// emits a triggered_by edge to the triggering source entity;
// subsequent appends leave the row + edge alone.

package actions

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/store"
)

func newEntityTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func quietActionsLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestFileTaskWriter_FirstCreateMaterializesTaskRow pins the
// happy path: first AppendTaskSection upserts a `task:<slug>`
// row with kind=task and the documented data fields.
func TestFileTaskWriter_FirstCreateMaterializesTaskRow(t *testing.T) {
	t.Parallel()
	st := newEntityTestStore(t)
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, st, nil, nil, quietActionsLogger())

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"my-workflow", "subject-1", "dedup-k", "", "Notes", "- line", ""))

	got, err := st.GetEntity(context.Background(), "task:my-workflow-subject-1")
	require.NoError(t, err)
	assert.Equal(t, canonical.TaskKind, got.Kind)
	assert.Equal(t, "my-workflow", got.Data["workflow"])
	assert.Equal(t, "subject-1", got.Data["subject"])
	assert.Equal(t, "dedup-k", got.Data["dedup_key"])
	_, hasCreated := got.Data["created_at"]
	assert.True(t, hasCreated, "created_at stamped on first-create")
}

// TestFileTaskWriter_FirstCreateEmitsTriggeredByEdge: when the
// dispatcher passes an entityID (the workflow's triggering
// source), the writer emits a `triggered_by` edge from the
// task entity to that source so graph walks reach the source
// from the task.
func TestFileTaskWriter_FirstCreateEmitsTriggeredByEdge(t *testing.T) {
	t.Parallel()
	st := newEntityTestStore(t)
	// Source entity must exist for the FK on edges.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "github-pr:acme-corp_widget_pr_42", Kind: "github-pr",
	}))
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, st, nil, nil, quietActionsLogger())

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"pr-watcher", "acme-corp-widget-pr-42",
		"", "github-pr:acme-corp_widget_pr_42",
		"Notes", "- a", ""))

	edges, err := st.GetEdgesFor(context.Background(), "task:pr-watcher-acme-corp-widget-pr-42", []string{canonical.EdgeTypeTriggeredBy})
	require.NoError(t, err)
	require.Len(t, edges, 1, "exactly one triggered_by edge from task → source")
	assert.Equal(t, "github-pr:acme-corp_widget_pr_42", edges[0].To)
}

// TestFileTaskWriter_NoStoreDepIsFileOnly pins the fixture-
// friendly shape: a nil store causes the writer to skip the
// materialization step entirely (file still lands on disk).
func TestFileTaskWriter_NoStoreDepIsFileOnly(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "sub", "", "", "Notes", "- line", ""))
	// No store → no row to verify; the file itself is the only
	// surface. Lack of panic / error is the assertion.
}

// TestFileTaskWriter_SubsequentAppendLeavesStoreAlone: the
// store row materializes once on first-create; later appends
// don't re-upsert / clobber operator-set fields.
func TestFileTaskWriter_SubsequentAppendLeavesStoreAlone(t *testing.T) {
	t.Parallel()
	st := newEntityTestStore(t)
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, st, nil, nil, quietActionsLogger())

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "s", "", "", "Notes", "- one", ""))

	got, err := st.GetEntity(context.Background(), "task:wf-s")
	require.NoError(t, err)
	got.Data["operator_pinned"] = "yes"
	require.NoError(t, st.UpsertEntity(context.Background(), got))

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "s", "", "", "Notes", "- two", ""))

	after, err := st.GetEntity(context.Background(), "task:wf-s")
	require.NoError(t, err)
	assert.Equal(t, "yes", after.Data["operator_pinned"],
		"second AppendTaskSection must NOT clobber prior store mutations")
}

// TestTaskEntityID pins the canonical id shape so a future
// refactor of slugify can't quietly change the wire surface.
func TestTaskEntityID(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "task:my-workflow-subject-x", TaskEntityID("my-workflow", "subject-x"))
	assert.Equal(t, "task:bare-workflow", TaskEntityID("bare-workflow", ""))
	assert.Equal(t, "task:weird-name", TaskEntityID("Weird Name!", ""))
	assert.Equal(t, "", TaskEntityID("", ""))
}
