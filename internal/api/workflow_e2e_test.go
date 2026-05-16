// End-to-end test of the workflow agent surface per the
// ADR-0024 §"Acceptance" criteria for #71: operator runs
// `workflow.trigger`, engine fires, action runs, task is
// created, `task.list` returns the new task. Exercises the
// full HTTP loop without touching MCP / CLI surfaces (those
// wrap the same HTTP endpoints).

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
	"github.com/yaad-index/yaad-index/internal/workflow/tasks"
)

// TestWorkflowAgentSurface_EndToEnd: trigger a manual
// workflow that runs task_append against a resolvable
// entity → task lands in the vault → task.list returns it
// → task.load returns its body → task.resolve archives it.
// Mirrors the issue #71 acceptance check.
func TestWorkflowAgentSurface_EndToEnd(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	vault := t.TempDir()
	// Action runner wired with a real FileTaskWriter so the
	// task lands on disk where the reader will see it.
	taskWriter := actions.NewFileTaskWriter(vault)
	runner := actions.New(actions.Options{TaskWriter: taskWriter})

	bus := eventbus.NewMemoryBus()
	resolver := &triggerFakeResolver{entities: map[string]map[string]any{
		"boardgame:b": {"id": "boardgame:b", "rating": int64(9)},
	}}
	eng, err := engine.New(engine.Options{
		Bus:      bus,
		Resolver: resolver,
		Runner:   runner,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	// Manual workflow that writes a candidate line on fire.
	wf := &parser.Workflow{
		Name:           "morning-brief",
		Version:        1,
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        parser.Trigger{Type: parser.TriggerTypeManual},
		Condition:      "entity.rating > 7",
		Subject:        "entity.id",
		Dedup:          parser.Dedup{Policy: parser.DedupPolicyUpdate, Key: "entity.id"},
		Actions: []parser.Action{{TaskAppend: &parser.TaskAppendAction{
			Section: "candidates",
			Content: `"first-fire"`,
		}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	reader := tasks.NewReader(vault)
	writer := tasks.NewWriter(vault)
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithEventBus(bus),
		WithWorkflowEngine(eng),
		WithTasksReader(reader),
		WithTasksWriter(writer),
	)

	// 1. workflow.trigger
	triggerBody, _ := json.Marshal(map[string]string{
		"name": "morning-brief", "input": "boardgame:b",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/trigger",
		strings.NewReader(string(triggerBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "trigger body=%s", rec.Body.String())

	var trig workflowTriggerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&trig))
	assert.True(t, trig.Fired, "workflow fired")
	assert.Equal(t, "boardgame:b", trig.Subject)

	// 2. task.list returns the new task.
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		OK    bool                `json:"ok"`
		Tasks []tasks.TaskSummary `json:"tasks"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	require.Len(t, list.Tasks, 1, "task lands in vault after fire")
	taskID := list.Tasks[0].ID
	assert.Equal(t, "morning-brief", list.Tasks[0].Workflow)
	assert.Equal(t, "boardgame:b", list.Tasks[0].Subject)

	// 3. task.load returns the body with the rendered content.
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var load struct {
		OK   bool        `json:"ok"`
		Task *tasks.Task `json:"task"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&load))
	require.NotNil(t, load.Task)
	assert.Contains(t, load.Task.Body, "## candidates")
	assert.Contains(t, load.Task.Body, "first-fire")

	// 4. task.resolve archives it.
	req = httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/resolve", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var res struct {
		OK           bool `json:"ok"`
		AutoArchived bool `json:"auto_archived"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&res))
	assert.True(t, res.AutoArchived)

	// Post-resolve: active list empty; archive file exists.
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var afterList struct {
		Tasks []tasks.TaskSummary `json:"tasks"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&afterList))
	assert.Empty(t, afterList.Tasks, "active task list empty after archive")

	archivePath := filepath.Join(vault, "tasks", "_archive", taskID+".md")
	assert.FileExists(t, archivePath)
}
