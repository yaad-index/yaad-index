// /v1/tasks surface per ADR-0024 §"Agent surface":
//   - GET /v1/tasks[?errored=true|false] — list tasks
//     written by workflow runners (Phase 4 task_append +
//     Phase 5.B err-task).
//   - GET /v1/tasks/{id} — load one task with body.
//
// Filesystem-walk against `<vault>/tasks/*.md` in v1;
// entity-promotion deferred until a query / edge need
// surfaces. Reuses internal/workflow/tasks.Reader.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/workflow/tasks"
)

// taskListResponse + taskLoadResponse are the wire envelopes
// for GET /v1/tasks + GET /v1/tasks/{id}.
type taskListResponse struct {
	OK    bool                `json:"ok"`
	Tasks []tasks.TaskSummary `json:"tasks"`
}

type taskLoadResponse struct {
	OK   bool        `json:"ok"`
	Task *tasks.Task `json:"task"`
}

// handleTaskList implements GET /v1/tasks. Optional query
// param `errored=true` (or `false`) filters by the
// frontmatter `errored:` flag (Phase 5.B err-task surface).
// No param → list every task.
func handleTaskList(logger *slog.Logger, reader *tasks.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts := tasks.ListOptions{}
		if q := r.URL.Query().Get("errored"); q != "" {
			switch q {
			case "true":
				t := true
				opts.Errored = &t
			case "false":
				f := false
				opts.Errored = &f
			default:
				writeError(w, http.StatusBadRequest, "invalid_argument",
					fmt.Sprintf("errored query parameter must be 'true' or 'false', got %q", q))
				return
			}
		}
		list, err := reader.List(opts)
		if err != nil {
			logger.ErrorContext(r.Context(), "task list failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"task list failed")
			return
		}
		if list == nil {
			list = []tasks.TaskSummary{}
		}
		resp := taskListResponse{OK: true, Tasks: list}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode task list response", "err", err)
		}
	}
}

// handleTaskLoad implements GET /v1/tasks/{id}. Returns the
// full task (frontmatter summary + body). 404 when no file
// matches the id; 400 when the id contains a path
// separator (defensive against traversal attempts).
func handleTaskLoad(logger *slog.Logger, reader *tasks.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "id is required")
			return
		}
		t, err := reader.Load(id)
		if err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			logger.ErrorContext(r.Context(), "task load failed", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"task load failed")
			return
		}
		resp := taskLoadResponse{OK: true, Task: t}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode task load response", "err", err, "id", id)
		}
	}
}
