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

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
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

// newTaskResolveFixture wires reader + writer + engine so
// the POST /v1/tasks/{id}/resolve route registers. The
// engine's AutoArchiveOnDoneFor lookup needs a registered
// workflow that matches the task's `workflow:` frontmatter
// field.
func newTaskResolveFixture(t *testing.T, seed map[string]string, wf *parser.Workflow) (http.Handler, string) {
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
	writer := tasks.NewWriter(vault)

	bus := eventbus.NewMemoryBus()
	eng, err := engine.New(engine.Options{
		Bus:      bus,
		Resolver: &triggerFakeResolver{},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	if wf != nil {
		require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithEventBus(bus),
		WithWorkflowEngine(eng),
		WithTasksReader(reader),
		WithTasksWriter(writer),
	)
	return h, vault
}

// TestTaskResolve_NormalTask_AutoArchives: a normal task
// whose workflow defaults to auto_archive_on_done=true gets
// stamped + moved to _archive/.
func TestTaskResolve_NormalTask_AutoArchives(t *testing.T) {
	t.Parallel()
	h, vault := newTaskResolveFixture(t,
		map[string]string{
			"wf-s": "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n",
		},
		&parser.Workflow{
			Name:           "wf",
			Version:        1,
			Status:         parser.StatusActive,
			AllowedPlugins: []string{"yaad-gmail"},
			Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
			Subject:        "entity.id",
			Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
		},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/wf-s/resolve", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		OK           bool   `json:"ok"`
		ID           string `json:"id"`
		Errored      bool   `json:"errored"`
		AutoArchived bool   `json:"auto_archived"`
		ResolvedAt   string `json:"resolved_at"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.True(t, resp.AutoArchived, "auto_archive_on_done default true")
	assert.False(t, resp.Errored)
	assert.NotEmpty(t, resp.ResolvedAt)

	_, err := os.Stat(filepath.Join(vault, "tasks", "wf-s.md"))
	assert.True(t, os.IsNotExist(err), "active task gone after archive")
	body, err := os.ReadFile(filepath.Join(vault, "tasks", "_archive", "wf-s.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "resolved_at:")
}

// TestTaskResolve_OptedOutWorkflow_StaysInPlace: a workflow
// with auto_archive_on_done=false keeps the resolved task
// in the active dir for audit-trail purposes.
func TestTaskResolve_OptedOutWorkflow_StaysInPlace(t *testing.T) {
	t.Parallel()
	autoArchiveFalse := false
	h, vault := newTaskResolveFixture(t,
		map[string]string{
			"keep-s": "---\nkind: task\nworkflow: keep\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n",
		},
		&parser.Workflow{
			Name:              "keep",
			Version:           1,
			Status:            parser.StatusActive,
			AllowedPlugins:    []string{"yaad-gmail"},
			Trigger:           parser.Trigger{Type: parser.TriggerTypeManual},
			Subject:           "entity.id",
			Actions:           []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
			AutoArchiveOnDone: &autoArchiveFalse,
		},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/keep-s/resolve", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		OK           bool `json:"ok"`
		AutoArchived bool `json:"auto_archived"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.AutoArchived, "workflow opted out")
	_, err := os.Stat(filepath.Join(vault, "tasks", "keep-s.md"))
	assert.NoError(t, err, "task stays in active dir")
}

// TestTaskResolve_ErrTask_AlwaysAutoArchives: err-tasks
// (errored: true) always auto-archive on resolve regardless
// of workflow opt-out (per ADR-0024 §"Runtime errors").
func TestTaskResolve_ErrTask_AlwaysAutoArchives(t *testing.T) {
	t.Parallel()
	autoArchiveFalse := false
	h, vault := newTaskResolveFixture(t,
		map[string]string{
			"keep-err": "---\nkind: task\nerrored: true\nworkflow: keep\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## Failures\n\n- x\n",
		},
		&parser.Workflow{
			Name:              "keep",
			Version:           1,
			Status:            parser.StatusActive,
			AllowedPlugins:    []string{"yaad-gmail"},
			Trigger:           parser.Trigger{Type: parser.TriggerTypeManual},
			Subject:           "entity.id",
			Actions:           []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
			AutoArchiveOnDone: &autoArchiveFalse,
		},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/keep-err/resolve", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		OK           bool `json:"ok"`
		Errored      bool `json:"errored"`
		AutoArchived bool `json:"auto_archived"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Errored)
	assert.True(t, resp.AutoArchived, "err-tasks always auto-archive")
	_, err := os.Stat(filepath.Join(vault, "tasks", "_archive", "keep-err.md"))
	assert.NoError(t, err, "err-task archived")
}

// TestTaskResolve_NotFound: missing task → 404.
func TestTaskResolve_NotFound(t *testing.T) {
	t.Parallel()
	h, _ := newTaskResolveFixture(t, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/absent/resolve", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTaskResolve_AutoArchive_RemovesFromTaskList pins #368: after
// a successful auto-archive resolve, (a) the archive copy exists,
// (b) the active file is gone, (c) GET /v1/tasks no longer
// includes the resolved id in its listing — closing the
// "task_list keeps showing resolved task" symptom #368 records.
func TestTaskResolve_AutoArchive_RemovesFromTaskList(t *testing.T) {
	t.Parallel()
	h, vault := newTaskResolveFixture(t,
		map[string]string{
			"wf-archive-test": "---\nkind: task\nworkflow: wf\nsubject: archive-test\ncreated_at: 2026-05-31T10:00:00Z\n---\n\n## archive-test\n\nx\n",
		},
		&parser.Workflow{
			Name:           "wf",
			Version:        1,
			Status:         parser.StatusActive,
			AllowedPlugins: []string{"yaad-gmail"},
			Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
			Subject:        "entity.id",
			Actions:        []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}},
		},
	)

	// Pre-resolve: task_list MUST include the id.
	pre := httptest.NewRecorder()
	h.ServeHTTP(pre, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	require.Equal(t, http.StatusOK, pre.Code, "pre-resolve list body=%s", pre.Body.String())
	assert.Contains(t, pre.Body.String(), `"id":"wf-archive-test"`,
		"task_list must show the task before resolve")

	// Resolve.
	resolveRec := httptest.NewRecorder()
	h.ServeHTTP(resolveRec, httptest.NewRequest(http.MethodPost, "/v1/tasks/wf-archive-test/resolve", nil))
	require.Equal(t, http.StatusOK, resolveRec.Code, "resolve body=%s", resolveRec.Body.String())

	// (a) Archive copy exists.
	_, archiveErr := os.Stat(filepath.Join(vault, "tasks", "_archive", "wf-archive-test.md"))
	assert.NoError(t, archiveErr, "archive copy must exist post-resolve")

	// (b) Active file is GONE.
	_, activeErr := os.Stat(filepath.Join(vault, "tasks", "wf-archive-test.md"))
	assert.True(t, os.IsNotExist(activeErr),
		"active file must NOT exist post-resolve (the #368 fix)")

	// (c) task_list no longer includes the id.
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	require.Equal(t, http.StatusOK, post.Code, "post-resolve list body=%s", post.Body.String())
	assert.NotContains(t, post.Body.String(), `"id":"wf-archive-test"`,
		"task_list must NOT include the resolved task")
}
