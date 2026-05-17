package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

// stubProvenanceSource is the `source` field stamped on the synthesized
// provenance entry for the response edge. The real authn-derived agent
// identity arrives once authn lands (ADR-0001 defers it); until then
// `agent:stub` makes it obvious the entry didn't come from a real
// caller chain. (Edge provenance is response-only today — the store
// doesn't yet persist provenance rows for edges.)
const stubProvenanceSource = "agent:stub"

// edgeRequest mirrors the POST /v1/edges request body per ADR-0002
// lines 246–254. `metadata` is free-form per the ADR.
type edgeRequest struct {
	Type string `json:"type"`
	From string `json:"from"`
	To string `json:"to"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// edge is the response shape per ADR-0002 lines 261–279. Distinct from
// `edgeRef` (the abbreviated `{type, to}` shape used inside entities) and
// from `edgeKind` (the registry entry served by /v1/kinds).
type edge struct {
	Type string `json:"type"`
	From string `json:"from"`
	To string `json:"to"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Provenance []provenanceEntry `json:"provenance"`
}

type edgeResponse struct {
	OK bool `json:"ok"`
	Edge edge `json:"edge"`
}

func handleCreateEdge(logger *slog.Logger, st store.Store, registry *plugins.Registry, bus eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req edgeRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		if strings.TrimSpace(req.Type) == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "type is required")
			return
		}
		if !isRegisteredEdgeKind(registry, req.Type) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("edge type %q is not in the registered edge_kinds", req.Type))
			return
		}
		if strings.TrimSpace(req.From) == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "from is required")
			return
		}
		if strings.TrimSpace(req.To) == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "to is required")
			return
		}

		se := &store.Edge{
			Type: req.Type,
			From: req.From,
			To: req.To,
			Metadata: req.Metadata,
		}
		err := st.CreateEdge(r.Context(), se)
		if errors.Is(err, store.ErrMissingEntity) {
			// missing_entity carries the offending id in the message so
			// callers can correlate without re-deriving. 422 per ADR-0002
			// (RFC 9110 §15.5.21 unprocessable content — request
			// well-formed, can't be processed because of referential
			// integrity).
			writeError(w, http.StatusUnprocessableEntity, "missing_entity",
				fmt.Sprintf("referenced entity not found: %v", err))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.CreateEdge", "err", err,
				"type", req.Type, "from", req.From, "to", req.To)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to create edge")
			return
		}

		// Publish entity.edge_added per ADR-0024 Phase 2: a new edge
		// has landed via the manual-add surface. Source is
		// SourceAgent — workflow-injected edges (Phase 4+) will
		// emit via their own dispatch path and carry workflow:<name>.
		bus.Publish(r.Context(), eventbus.EntityEdgeAddedEvent{
			FromID:    se.From,
			ToID:      se.To,
			EdgeType:  se.Type,
			SourceTag: eventbus.SourceAgent,
			At:        se.UpdatedAt.UTC(),
			Chain:     eventbus.WorkflowChainFromContext(r.Context()),
		})

		// Edge provenance isn't persisted today (the store doesn't write
		// to the provenance table for edges yet). Synthesize a single
		// agent:stub entry on the response so the wire shape stays
		// stable; when ingest writes real provenance for edges, this
		// synthesizer goes away in favour of reading the persisted
		// entries.
		resp := edgeResponse{
			OK: true,
			Edge: edge{
				Type: se.Type,
				From: se.From,
				To: se.To,
				Metadata: se.Metadata,
				Provenance: []provenanceEntry{
					{
						Source: stubProvenanceSource,
						FetchedAt: se.UpdatedAt.UTC().Format(time.RFC3339),
						OK: true,
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/edges response", "err", err)
		}
	}
}

// isRegisteredEdgeKind reports whether name is advertised by any
// registered plugin's Capabilities().EdgeKinds. Empty registry means
// no edge kinds are valid — matches the new /v1/kinds shape after
// the bootstrapKinds seed was retired.
func isRegisteredEdgeKind(registry *plugins.Registry, name string) bool {
	for _, p := range registry.Plugins() {
		for _, k := range p.Capabilities().EdgeKinds {
			if k.Name == name {
				return true
			}
		}
	}
	return false
}

// listEdge is the wire shape for one entry on the GET /v1/edges
// response per yaad-index. Distinct from the POST /v1/edges
// `edge` shape because:
//
// - GET surfaces from_id/to_id (the read-side audience needs both
// ends explicit; the create-side input shape uses from/to).
// - GET doesn't surface synthesized provenance; the create-side
// does for the single-edge created-now response.
// - Field name parity (`from_id`, `to_id`) matches the underlying
// store.Edge field semantics + how SQL stores them, and lets
// the agent decoder distinguish list-vs-single without a JSON
// shape sniff.
type listEdge struct {
	Type string `json:"type"`
	FromID string `json:"from_id"`
	ToID string `json:"to_id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// edgeListResponse is the wire envelope for GET /v1/edges. v1's
// pagination is a placeholder — the cursor field is reserved on
// the wire for forward-compat but always emitted as null today
// (single-hop edge counts per entity are bounded; cursor traversal
// lands when an entity surfaces > 1k edges).
type edgeListResponse struct {
	OK bool `json:"ok"`
	Edges []listEdge `json:"edges"`
	NextCursor *string `json:"next_cursor"`
}

const (
	// edgesDefaultLimit caps the response to a sensible default —
	// any entity with > 200 edges in one direction is unusual; the
	// caller passes ?limit= to raise (capped at edgesMaxLimit).
	edgesDefaultLimit = 200
	edgesMaxLimit = 1000
)

// handleListEdges implements GET /v1/edges?entity_id=X[&edge_types=...]
// [&direction=out|in|both][&limit=N] per yaad-index.
//
// Surfaces single-hop edges that the existing per-entity expansion
// (?with_edges=) couldn't reach: inbound queries (direction=in),
// all-types-at-once (no edge_types arg), bidirectional walks
// (direction=both for the full neighborhood).
//
// Read-only: edge mutation continues to live on POST /v1/edges.
//
// Param semantics:
//
// - `entity_id` (required): full `<kind>:<local-id>`. The role
// (from vs to) depends on direction.
// - `direction` (default `out`): one of `out` | `in` | `both`.
// Anything else → 400 invalid_direction.
// - `edge_types` (optional): comma-separated allowlist (e.g.
// `designed_by,authored_by`). Empty / absent → no type filter.
// - `limit` (optional): per-direction cap. Default 200, max 1000.
// Direction=both produces up to 2x limit (one per side), the
// simplest semantic for v1; tightening is a follow-up if
// callers demand a unified cap.
func handleListEdges(logger *slog.Logger, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entityID := r.URL.Query().Get("entity_id")
		if strings.TrimSpace(entityID) == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"entity_id is required")
			return
		}
		direction := r.URL.Query().Get("direction")
		if direction == "" {
			direction = "out"
		}
		if direction != "out" && direction != "in" && direction != "both" {
			writeError(w, http.StatusBadRequest, "invalid_direction",
				"direction must be one of {out, in, both}")
			return
		}
		types := parseEdgeTypesFilter(r.URL.Query().Get("edge_types"))
		limit := parseEdgesLimit(r.URL.Query().Get("limit"))

		edges := make([]store.Edge, 0)
		if direction == "out" || direction == "both" {
			out, err := st.GetEdgesFor(r.Context(), entityID, types)
			if err != nil {
				logger.ErrorContext(r.Context(), "store.GetEdgesFor",
					"err", err, "entity_id", entityID, "types", types)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to read outbound edges")
				return
			}
			edges = append(edges, out...)
		}
		if direction == "in" || direction == "both" {
			in, err := st.GetEdgesTo(r.Context(), entityID, types)
			if err != nil {
				logger.ErrorContext(r.Context(), "store.GetEdgesTo",
					"err", err, "entity_id", entityID, "types", types)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to read inbound edges")
				return
			}
			edges = append(edges, in...)
		}

		if len(edges) > limit {
			edges = edges[:limit]
		}

		resp := edgeListResponse{
			OK: true,
			Edges: make([]listEdge, len(edges)),
			NextCursor: nil,
		}
		for i, e := range edges {
			resp.Edges[i] = listEdge{
				Type: e.Type,
				FromID: e.From,
				ToID: e.To,
				Metadata: e.Metadata,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/edges (list) response",
				"err", err, "entity_id", entityID)
		}
	}
}

// parseEdgesLimit clamps the optional ?limit= to [1, max] with a
// silent default fallback. Same lenient shape as needs-fill's
// parseNeedsFillLimit — bad strings, negative values, missing all
// produce edgesDefaultLimit.
func parseEdgesLimit(raw string) int {
	if raw == "" {
		return edgesDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return edgesDefaultLimit
	}
	if n > edgesMaxLimit {
		return edgesMaxLimit
	}
	return n
}
