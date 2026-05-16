package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/tasks"
)

func newTaskFixture(t *testing.T, seed map[string]string) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	vault := t.TempDir()
	tasksDir := filepath.Join(vault, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))
	for name, body := range seed {
		require.NoError(t, os.WriteFile(filepath.Join(tasksDir, name+".md"), []byte(body), 0o644))
	}
	reader := tasks.NewReader(vault)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithTasksReader(reader),
	)
}

// TestTaskList_HappyPath: returns every task with summary
// fields filled from frontmatter.
func TestTaskList_HappyPath(t *testing.T) {
	t.Parallel()
	h := newTaskFixture(t, map[string]string{
		"alpha-e1": "---\nkind: task\nworkflow: alpha\nsubject: e1\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n",
		"alpha-err": "---\nkind: task\nerrored: true\nworkflow: alpha\ncreated_at: 2026-05-16T11:00:00Z\n---\n\n## Failures\n\n- y\n",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		OK    bool                `json:"ok"`
		Tasks []tasks.TaskSummary `json:"tasks"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	require.Len(t, resp.Tasks, 2)
	// Sorted by ID
	assert.Equal(t, "alpha-e1", resp.Tasks[0].ID)
	assert.Equal(t, "alpha-err", resp.Tasks[1].ID)
	assert.True(t, resp.Tasks[1].Errored)
}

// TestTaskList_ErroredFilter: ?errored=true returns only
// err-tasks.
func TestTaskList_ErroredFilter(t *testing.T) {
	t.Parallel()
	h := newTaskFixture(t, map[string]string{
		"alpha-e1": "---\nkind: task\nworkflow: alpha\nsubject: e1\ncreated_at: 2026-05-16T10:00:00Z\n---\n",
		"alpha-err": "---\nkind: task\nerrored: true\nworkflow: alpha\ncreated_at: 2026-05-16T11:00:00Z\n---\n",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks?errored=true", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		OK    bool                `json:"ok"`
		Tasks []tasks.TaskSummary `json:"tasks"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Tasks, 1)
	assert.Equal(t, "alpha-err", resp.Tasks[0].ID)
}

// TestTaskList_InvalidErroredParam: malformed ?errored=
// value rejects with 400.
func TestTaskList_InvalidErroredParam(t *testing.T) {
	t.Parallel()
	h := newTaskFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks?errored=yes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestTaskLoad_HappyPath: returns the full task body +
// summary fields.
func TestTaskLoad_HappyPath(t *testing.T) {
	t.Parallel()
	h := newTaskFixture(t, map[string]string{
		"wf-s": "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## candidates\n\nfirst\n",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/wf-s", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		OK   bool        `json:"ok"`
		Task *tasks.Task `json:"task"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	require.NotNil(t, resp.Task)
	assert.Equal(t, "wf-s", resp.Task.ID)
	assert.Equal(t, "wf", resp.Task.Workflow)
	assert.Contains(t, resp.Task.Body, "## candidates")
	assert.Contains(t, resp.Task.Body, "first")
}

// TestTaskLoad_NotFound: unknown id → 404.
func TestTaskLoad_NotFound(t *testing.T) {
	t.Parallel()
	h := newTaskFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/absent", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
