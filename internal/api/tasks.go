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
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
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

// handleTaskResolve implements POST /v1/tasks/{id}/resolve.
// Stamps `resolved_at: <now>` on the task's frontmatter +
// (when auto-archive applies) moves the file to
// `tasks/_archive/<id>.md`. Auto-archive rules per ADR-0024:
// always on for err-tasks; default true for normal tasks
// (operator opts out via `auto_archive_on_done: false` on
// the originating workflow). 404 when no file matches the
// id; 503 when no engine is wired (we need it for the
// per-workflow auto-archive flag lookup).
//
// #304 Cut C3.3 branch: when the request body carries a
// non-empty `option` field, route through the resolution-
// task flow instead of the legacy text-task resolve path —
// re-ingest the chosen entity via the resolver plugin's
// shorthand, then CreateEdge (no prior edge) or
// UpdateEdgeTarget (stale-rewrite) per the locked design,
// then archive. 400 when `option` arrives on a non-
// resolution-task or names an id absent from the task's
// recorded options.
func handleTaskResolve(logger *slog.Logger, reader *tasks.Reader, writer *tasks.Writer, eng *engine.Engine, st store.Store, syncIngester SyncIngester, tasksDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "id is required")
			return
		}
		option, err := decodeResolveOption(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		if option != "" {
			handleResolutionTaskResolve(w, r, logger, writer, st, syncIngester, tasksDir, id, option)
			return
		}
		t, err := reader.Load(id)
		if err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			logger.ErrorContext(r.Context(), "task resolve: load failed", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"task resolve failed")
			return
		}
		// Err-tasks always auto-archive per ADR-0024
		// §"Runtime errors — the err-task pattern". Normal
		// tasks respect the per-workflow opt-out flag (eng
		// nil → default true, defensive).
		autoArchive := true
		if !t.Errored && eng != nil {
			autoArchive = eng.AutoArchiveOnDoneFor(t.Workflow)
		}
		if err := writer.Resolve(id, time.Now().UTC(), autoArchive); err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			logger.ErrorContext(r.Context(), "task resolve: write failed", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"task resolve failed")
			return
		}
		resp := taskResolveResponse{
			OK:            true,
			ID:            id,
			Errored:       t.Errored,
			AutoArchived:  autoArchive,
			ResolvedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode task resolve response", "err", err, "id", id)
		}
	}
}

type taskResolveResponse struct {
	OK           bool   `json:"ok"`
	ID           string `json:"id"`
	Errored      bool   `json:"errored"`
	AutoArchived bool   `json:"auto_archived"`
	ResolvedAt   string `json:"resolved_at"`
}

// defaultResolveIngestTimeout caps the per-resolve plugin
// shorthand re-ingest. Mirrors the centralized edge-write
// service's defaultResolverTimeout (60s, set in main.go for
// Cut C2's auto-mode plumbing) so the operator's resolve
// click doesn't hang indefinitely on a slow plugin fetch.
const defaultResolveIngestTimeout = 60 * time.Second

// handleResolutionTaskResolve runs the resolution-task resolve
// flow for POST /v1/tasks/{id}/resolve when `option` is in the
// request body. Routes per the locked Cut C3 design:
//
//  1. Read + parse the on-disk resolution-task frontmatter.
//  2. Validate optionID is in the recorded options list.
//  3. Re-ingest the chosen candidate via the resolver plugin's
//     `<plugin>: <id>` shorthand. SyncIngester.IngestByName
//     handles the source-shape → canonical-kind hop per Cut C2.
//  4. CreateEdge or UpdateEdgeTarget based on whether a prior
//     edge exists for the (from, type) tuple.
//  5. Archive the on-disk task via tasks.Writer.Resolve.
//
// Cut B's 409 ErrEdgeStale surfaces via 409 on the wire; a
// disambiguation re-trigger on the plugin re-ingest surfaces
// via 409 too (the operator picked an option but the plugin
// can't resolve it cleanly anymore — re-run the workflow to
// regenerate the resolution-task with the current options).
func handleResolutionTaskResolve(w http.ResponseWriter, r *http.Request, logger *slog.Logger, writer *tasks.Writer, st store.Store, syncIngester SyncIngester, tasksDir, id, optionID string) {
	if writer == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"resolution-task resolve requires a tasks writer")
		return
	}
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"resolution-task resolve requires the store")
		return
	}
	if syncIngester == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"resolution-task resolve requires the sync ingester")
		return
	}
	fm, err := parseResolutionTaskFile(tasksDir, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if _, err := resolveOptionInTask(fm, optionID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	// Re-ingest via the resolver plugin's shorthand. The option
	// id IS the plugin's canonical identifier per ADR-0006; the
	// plugin accepts it as the `<plugin>: <id>` shorthand name
	// and the C2 source-shape→canonical-kind hop in
	// resolveCanonicalTarget produces the final `<target_kind>:`
	// id.
	chosenID, options, err := syncIngester.IngestByName(r.Context(), fm.ResolverPlugin, fm.TargetKind, optionID, defaultResolveIngestTimeout)
	if err != nil {
		logger.ErrorContext(r.Context(), "resolution-task resolve: ingest failed",
			"err", err, "task_id", id, "plugin", fm.ResolverPlugin, "option", optionID)
		writeError(w, http.StatusBadGateway, "ingest_failed",
			fmt.Sprintf("ingest of option %q via plugin %s failed: %v", optionID, fm.ResolverPlugin, err))
		return
	}
	if len(options) > 0 {
		// Plugin re-disambiguated on a specific-id query — the
		// option set has shifted under us. Surface as 409 so
		// the operator can re-list the task's current options.
		writeError(w, http.StatusConflict, "ingest_disambiguated",
			fmt.Sprintf("plugin %s returned disambiguation options for the chosen id; rerun the originating workflow", fm.ResolverPlugin))
		return
	}
	if chosenID == "" {
		writeError(w, http.StatusBadGateway, "ingest_failed",
			fmt.Sprintf("ingest of option %q via plugin %s returned empty entity id", optionID, fm.ResolverPlugin))
		return
	}

	finalID, outcome, err := resolveResolutionTaskEdge(r.Context(), st, fm.FromID, fm.EdgeType, chosenID)
	if err != nil {
		if errors.Is(err, store.ErrEdgeStale) {
			writeError(w, http.StatusConflict, "edge_stale", err.Error())
			return
		}
		if errors.Is(err, store.ErrMissingEntity) {
			writeError(w, http.StatusUnprocessableEntity, "missing_entity", err.Error())
			return
		}
		logger.ErrorContext(r.Context(), "resolution-task resolve: edge write failed",
			"err", err, "task_id", id, "from", fm.FromID, "edge_type", fm.EdgeType, "chosen", chosenID)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"resolution-task edge write failed")
		return
	}

	// Resolution-tasks always auto-archive — the operator's
	// pick is a terminal state for this task. No per-workflow
	// opt-out (matches err-task auto-archive semantics).
	now := time.Now().UTC()
	if err := writer.Resolve(id, now, true); err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		logger.ErrorContext(r.Context(), "resolution-task resolve: archive failed",
			"err", err, "task_id", id)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"resolution-task archive failed")
		return
	}

	resp := resolveResolutionTaskResponse{
		OK:           true,
		ID:           id,
		AutoArchived: true,
		ResolvedAt:   now.Format(time.RFC3339),
		ChosenID:     finalID,
		EdgeOutcome:  string(outcome),
		FromID:       fm.FromID,
		EdgeType:     fm.EdgeType,
		TargetKind:   fm.TargetKind,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.ErrorContext(r.Context(), "encode resolution-task resolve response", "err", err, "id", id)
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
