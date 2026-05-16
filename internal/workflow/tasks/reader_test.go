package tasks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTask(t *testing.T, vault, name, content string) {
	t.Helper()
	tasksDir := filepath.Join(vault, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tasksDir, name+".md"), []byte(content), 0o644))
}

// TestReader_List_HappyPath: a tasks dir with two tasks
// returns both summaries sorted by id.
func TestReader_List_HappyPath(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "alpha-e1", "---\nkind: task\nworkflow: alpha\nsubject: e1\ndedup_key: alpha|e1\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## candidates\n\nfirst\n")
	writeTask(t, vault, "beta-e2", "---\nkind: task\nworkflow: beta\nsubject: e2\ncreated_at: 2026-05-16T11:00:00Z\n---\n\n## notes\n\nb\n")

	r := NewReader(vault)
	list, err := r.List(ListOptions{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "alpha-e1", list[0].ID)
	assert.Equal(t, "alpha", list[0].Workflow)
	assert.Equal(t, "e1", list[0].Subject)
	assert.Equal(t, "alpha|e1", list[0].DedupKey)
	assert.False(t, list[0].Errored)
	assert.Equal(t, time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC), list[0].CreatedAt)
	assert.Equal(t, "beta-e2", list[1].ID)
}

// TestReader_List_ErroredFilter: --errored=true returns only
// err-tasks; --errored=false returns only normal tasks.
func TestReader_List_ErroredFilter(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "alpha-e1", "---\nkind: task\nworkflow: alpha\nsubject: e1\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\na\n")
	writeTask(t, vault, "alpha-err", "---\nkind: task\nerrored: true\nworkflow: alpha\ncreated_at: 2026-05-16T11:00:00Z\n---\n\n## Failures\n\n- x\n")

	r := NewReader(vault)
	tru := true
	erred, err := r.List(ListOptions{Errored: &tru})
	require.NoError(t, err)
	require.Len(t, erred, 1)
	assert.Equal(t, "alpha-err", erred[0].ID)
	assert.True(t, erred[0].Errored)

	fal := false
	normal, err := r.List(ListOptions{Errored: &fal})
	require.NoError(t, err)
	require.Len(t, normal, 1)
	assert.Equal(t, "alpha-e1", normal[0].ID)
	assert.False(t, normal[0].Errored)
}

// TestReader_List_MissingDir: tasks/ directory absent →
// nil list + nil error (operator hasn't authored workflows
// yet).
func TestReader_List_MissingDir(t *testing.T) {
	t.Parallel()
	r := NewReader(t.TempDir())
	list, err := r.List(ListOptions{})
	require.NoError(t, err)
	assert.Nil(t, list)
}

// TestReader_List_SkipsHidden_NonMarkdown_Subdirs: dotfiles,
// non-.md files, and subdirectories under tasks/ don't
// appear in the list.
func TestReader_List_SkipsHidden_NonMarkdown_Subdirs(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "real", "---\nkind: task\nworkflow: x\ncreated_at: 2026-05-16T10:00:00Z\n---\n")
	require.NoError(t, os.WriteFile(filepath.Join(vault, "tasks", "README.txt"), []byte("not a task"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(vault, "tasks", ".hidden.md"), []byte("dotfile"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(vault, "tasks", "subdir"), 0o755))

	r := NewReader(vault)
	list, err := r.List(ListOptions{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "real", list[0].ID)
}

// TestReader_Load_HappyPath: returns the summary + the body
// (post-frontmatter) verbatim.
func TestReader_Load_HappyPath(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	body := "---\nkind: task\nworkflow: wf\nsubject: s\ndedup_key: wf|s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## candidates\n\nfirst line\nsecond line\n"
	writeTask(t, vault, "wf-s", body)

	r := NewReader(vault)
	tk, err := r.Load("wf-s")
	require.NoError(t, err)
	assert.Equal(t, "wf-s", tk.ID)
	assert.Equal(t, "wf", tk.Workflow)
	assert.Equal(t, "s", tk.Subject)
	assert.Equal(t, "wf|s", tk.DedupKey)
	assert.Contains(t, tk.Body, "## candidates")
	assert.Contains(t, tk.Body, "first line")
}

// TestReader_Load_NotFound: missing id → ErrTaskNotFound.
func TestReader_Load_NotFound(t *testing.T) {
	t.Parallel()
	r := NewReader(t.TempDir())
	_, err := r.Load("absent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskNotFound)
}

// TestReader_Load_PathTraversalRejected: ids with `/` or `\`
// are rejected so a caller can't escape the tasks/ dir via
// id like `../../etc/passwd`. HTTP handler also URL-escapes
// but the reader's defensive check is the last line.
func TestReader_Load_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	r := NewReader(t.TempDir())
	_, err := r.Load("../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path separator")
}

// TestReader_BodylessFile_GracefulDegradation: a task file
// with no frontmatter parses as a bodyless task without
// erroring.
func TestReader_BodylessFile_GracefulDegradation(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "raw", "no frontmatter just markdown\n")
	r := NewReader(vault)
	tk, err := r.Load("raw")
	require.NoError(t, err)
	assert.Empty(t, tk.Workflow)
	assert.Contains(t, tk.Body, "no frontmatter")
}
