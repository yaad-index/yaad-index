// /v1/workflows surface per ADR-0024 §"Agent surface":
//   - GET /v1/workflows — list registered workflows.
//   - GET /v1/workflows/discover?entity=<id> — list
//     workflows whose condition matches the given entity.
//   - POST /v1/workflows/trigger — manual-trigger entry
//     point.
//
// Wire shapes mirror the engine's Decision / WorkflowSummary
// types with JSON-friendly field names + RFC3339 timestamps.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yaad-index/yaad-index/internal/workflow/engine"
)

// workflowTriggerRequest is the POST /v1/workflows/trigger
// body per ADR-0024 §"workflow.trigger(input) input
// semantics". Input disambiguates by syntactic shape:
//   - Empty string — target-less manual fire (workflows
//     whose trigger.type=manual).
//   - Entity ID (`<kind>:<slug>`) — direct entity attach.
//   - URL — ingest-or-lookup route (Phase 3.C+ follow-up;
//     currently treated as an entity-id and lookup-misses
//     surface as MissingRef on the Decision).
type workflowTriggerRequest struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

// workflowTriggerResponse is the 200 envelope returned to a
// successful trigger. Mirrors engine.Decision but with the
// At field rendered as RFC 3339 so JSON consumers can parse
// without struct-tag awareness.
type workflowTriggerResponse struct {
	OK          bool              `json:"ok"`
	Workflow    string            `json:"workflow"`
	EntityID    string            `json:"entity_id,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	Fired       bool              `json:"fired"`
	MissingRefs []missingRefEntry `json:"missing_refs,omitempty"`
	Err         string            `json:"err,omitempty"`
	At          string            `json:"at"`
}

type missingRefEntry struct {
	ID string `json:"id"`
}

// handleWorkflowTrigger implements POST /v1/workflows/trigger.
// Returns the engine's Decision shape as JSON; an unknown
// workflow name returns 404; empty-input-on-event-trigger
// returns 422 invalid_argument.
func handleWorkflowTrigger(logger *slog.Logger, eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workflowTriggerRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"name is required")
			return
		}

		decision, err := eng.Dispatch(r.Context(), req.Name, req.Input)
		if err != nil {
			if errors.Is(err, engine.ErrUnknownWorkflow) {
				writeError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			if errors.Is(err, engine.ErrEmptyInputNotAllowed) {
				writeError(w, http.StatusUnprocessableEntity, "invalid_argument", err.Error())
				return
			}
			logger.ErrorContext(r.Context(), "workflow trigger dispatch",
				"err", err, "name", req.Name)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"workflow trigger failed")
			return
		}

		resp := decisionToResponse(decision)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode workflow trigger response",
				"err", err, "name", req.Name)
		}
	}
}

// workflowListResponse + workflowDiscoverResponse are the
// wire envelopes for GET /v1/workflows + GET
// /v1/workflows/discover per ADR-0024 §"Agent surface".
type workflowListResponse struct {
	OK        bool                      `json:"ok"`
	Workflows []engine.WorkflowSummary  `json:"workflows"`
}

type workflowDiscoverResponse struct {
	OK        bool     `json:"ok"`
	EntityID  string   `json:"entity_id"`
	Workflows []string `json:"workflows"`
}

// handleWorkflowList implements GET /v1/workflows — returns
// every currently-registered workflow with metadata
// (name / version / status / trigger type / dedup policy).
func handleWorkflowList(logger *slog.Logger, eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := workflowListResponse{OK: true, Workflows: eng.List()}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode workflow list response", "err", err)
		}
	}
}

// handleWorkflowDiscover implements GET
// /v1/workflows/discover?entity=<id>. Returns the names of
// workflows whose condition matches the given entity per
// ADR-0024 §"workflow.discover(entity_id) performance note".
// Required query param `entity` is the canonical entity id.
// Unknown entity returns 404 not_found; engine without a
// resolver returns 503 service_unavailable.
func handleWorkflowDiscover(logger *slog.Logger, eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entityID := r.URL.Query().Get("entity")
		if entityID == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"entity query parameter is required")
			return
		}
		matches, err := eng.Discover(r.Context(), entityID)
		if err != nil {
			if errors.Is(err, engine.ErrEntityNotFoundForDiscover) {
				writeError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			logger.ErrorContext(r.Context(), "workflow discover failed",
				"err", err, "entity_id", entityID)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"workflow discover failed")
			return
		}
		resp := workflowDiscoverResponse{
			OK:        true,
			EntityID:  entityID,
			Workflows: matches,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode workflow discover response",
				"err", err, "entity_id", entityID)
		}
	}
}

// decisionToResponse maps the engine's Decision shape to
// the HTTP wire envelope. Stringifies the err + the
// timestamp so JSON consumers get a stable wire shape.
func decisionToResponse(d engine.Decision) workflowTriggerResponse {
	resp := workflowTriggerResponse{
		OK:       true,
		Workflow: d.Workflow,
		EntityID: d.EntityID,
		Subject:  d.Subject,
		Fired:    d.Fired,
		At:       d.At.Format(time.RFC3339),
	}
	if d.Err != nil {
		resp.Err = d.Err.Error()
	}
	if len(d.MissingRefs) > 0 {
		resp.MissingRefs = make([]missingRefEntry, len(d.MissingRefs))
		for i, r := range d.MissingRefs {
			resp.MissingRefs[i] = missingRefEntry{ID: r.ID}
		}
	}
	return resp
}
