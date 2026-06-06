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

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
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

func handleCreateEdge(logger *slog.Logger, st store.Store, edgeWriter edgewrite.EdgeWriter, registry *plugins.Registry, bus eventbus.Bus) http.HandlerFunc {
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
		// #268 / #272 thin-label target lazy-ensure: when the
		// manual edge points at a thin-label canonical kind
		// (`day`, `email`, `email-address`, `label`), auto-
		// materialize the target row before CreateEdge so the
		// FK holds without the caller having to pre-create the
		// entity. Same lazy-on-edge pattern the ingest + fill +
		// workflow paths already follow via EmitDayRefs /
		// ensureCanonicalLabelRow.
		//
		// `task` is excluded even though it's also daemon-
		// managed: task rows index `<vault>/tasks/*.md` files
		// and per ADR-0024 §Task only materialize on first-
		// create (no automatic backfill). A POST /v1/edges
		// pointing at `task:foo` for an unknown task should
		// 422 with missing_entity rather than silently land a
		// phantom store row with no backing vault file.
		if targetKind, _, ok := canonical.SplitLabelID(req.To); ok && isLazyMaterializableKind(targetKind) {
			if _, err := canonical.EnsureLabelRow(r.Context(), st, req.To, logger); err != nil {
				logger.WarnContext(r.Context(), "POST /v1/edges: ensure thin-label target failed",
					"target_id", req.To, "kind", targetKind, "err", err)
			}
		}
		err := edgeWriter.CreateEdge(r.Context(), se)
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
		// CausedByEntityID = FromID per the edge-tail-is-cause
		// convention (#264).
		bus.Publish(r.Context(), eventbus.EntityEdgeAddedEvent{
			FromID:           se.From,
			ToID:             se.To,
			EdgeType:         se.Type,
			SourceTag:        eventbus.SourceAgent,
			At:               se.UpdatedAt.UTC(),
			Chain:            eventbus.WorkflowChainFromContext(r.Context()),
			CausedByEntityID: se.From,
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

// updateEdgeTargetRequest mirrors the POST /v1/edges/update-target
// body per #304 Cut B. All four fields are required.
type updateEdgeTargetRequest struct {
	From      string `json:"from"`
	Type      string `json:"type"`
	OldTarget string `json:"old_target"`
	NewTarget string `json:"new_target"`
}

// handleUpdateEdgeTarget implements POST /v1/edges/update-target
// per #304 Cut B. Single transactional API: deletes the old
// (from, type, old_target) edge and creates (from, type, new_target),
// preserving created_at + metadata. 409 conflict when the old
// tuple doesn't match current state (stale rewrite); 422 missing
// entity when new_target doesn't resolve.
//
// Edge-type registry check follows the same shape as
// handleCreateEdge — the new edge keeps the type the old edge had
// (which already passed the registry gate on create), so we only
// re-check for defense in depth.
//
// Lazy thin-label materialization: when new_target is a daemon-
// managed thin-label kind (day, email, email-address, label),
// ensure the row before UpdateEdgeTarget so the FK holds without
// the caller having to pre-create the entity. Mirrors
// handleCreateEdge's ensure-on-write shape.
//
// No vault-side rewrite from this primitive; vault consistency
// is the caller's concern per the v1 cut framing (Cut C's
// centralized edge-write composes vault + DB updates when the
// source entity is vault-resident).
func handleUpdateEdgeTarget(logger *slog.Logger, st store.Store, registry *plugins.Registry, bus eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req updateEdgeTargetRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		switch {
		case strings.TrimSpace(req.From) == "":
			writeError(w, http.StatusBadRequest, "invalid_argument", "from is required")
			return
		case strings.TrimSpace(req.Type) == "":
			writeError(w, http.StatusBadRequest, "invalid_argument", "type is required")
			return
		case strings.TrimSpace(req.OldTarget) == "":
			writeError(w, http.StatusBadRequest, "invalid_argument", "old_target is required")
			return
		case strings.TrimSpace(req.NewTarget) == "":
			writeError(w, http.StatusBadRequest, "invalid_argument", "new_target is required")
			return
		case req.OldTarget == req.NewTarget:
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"old_target == new_target is a no-op; nothing to update")
			return
		}
		if !isRegisteredEdgeKind(registry, req.Type) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("edge type %q is not in the registered edge_kinds", req.Type))
			return
		}

		// Same lazy thin-label ensure as handleCreateEdge: when the
		// new target is a daemon-managed kind (day / email / etc.)
		// the row may not yet exist, but the caller shouldn't have
		// to pre-create it. Best-effort; FK-error fall-through
		// from UpdateEdgeTarget below surfaces missing_entity if
		// ensure didn't run.
		if targetKind, _, ok := canonical.SplitLabelID(req.NewTarget); ok && isLazyMaterializableKind(targetKind) {
			if _, err := canonical.EnsureLabelRow(r.Context(), st, req.NewTarget, logger); err != nil {
				logger.WarnContext(r.Context(), "POST /v1/edges/update-target: ensure thin-label target failed",
					"target_id", req.NewTarget, "kind", targetKind, "err", err)
			}
		}

		newEdge, err := st.UpdateEdgeTarget(r.Context(), req.From, req.Type, req.OldTarget, req.NewTarget)
		if errors.Is(err, store.ErrEdgeStale) {
			writeError(w, http.StatusConflict, "edge_stale",
				fmt.Sprintf("(%s, %s, %s) does not match a current edge — already rewritten, deleted, or never existed; re-read state and retry with the fresh tuple", req.From, req.Type, req.OldTarget))
			return
		}
		if errors.Is(err, store.ErrMissingEntity) {
			writeError(w, http.StatusUnprocessableEntity, "missing_entity",
				fmt.Sprintf("referenced entity not found: %v", err))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.UpdateEdgeTarget", "err", err,
				"from", req.From, "type", req.Type, "old_target", req.OldTarget, "new_target", req.NewTarget)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to update edge target")
			return
		}

		// Publish entity.edge_added on the NEW tuple per ADR-0024
		// Phase 2 — downstream subscribers see the resolved edge
		// as a fresh add. The old (from, type, old_target) edge
		// disappears silently; an explicit entity.edge_removed
		// event is out of scope for Cut B (the bus surface today
		// emits adds only).
		bus.Publish(r.Context(), eventbus.EntityEdgeAddedEvent{
			FromID:           newEdge.From,
			ToID:             newEdge.To,
			EdgeType:         newEdge.Type,
			SourceTag:        eventbus.SourceAgent,
			At:               newEdge.UpdatedAt.UTC(),
			Chain:            eventbus.WorkflowChainFromContext(r.Context()),
			CausedByEntityID: newEdge.From,
		})

		resp := edgeResponse{
			OK: true,
			Edge: edge{
				Type:     newEdge.Type,
				From:     newEdge.From,
				To:       newEdge.To,
				Metadata: newEdge.Metadata,
				Provenance: []provenanceEntry{
					{
						Source:    stubProvenanceSource,
						FetchedAt: newEdge.UpdatedAt.UTC().Format(time.RFC3339),
						OK:        true,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/edges/update-target response", "err", err)
		}
	}
}

// isRegisteredEdgeKind reports whether name is advertised by any
// registered plugin's Capabilities().EdgeKinds OR is a daemon-
// managed canonical edge type (the day-anchored vocabulary per
// ADR-0025 + the triggered_by edge per #268 + the gmail-emitted
// from/to/cc/bcc/tagged_as per #272). Empty registry without a
// daemon-built-in match means the edge name is not valid for
// POST /v1/edges — matches the /v1/kinds shape after the
// bootstrapKinds seed retired.
func isRegisteredEdgeKind(registry *plugins.Registry, name string) bool {
	for _, edge := range canonical.DaemonEdgeTypes() {
		if edge == name {
			return true
		}
	}
	for _, p := range registry.Plugins() {
		for _, k := range p.Capabilities().EdgeKinds {
			if k.Name == name {
				return true
			}
		}
	}
	return false
}

// isLazyMaterializableKind reports whether handleCreateEdge
// should auto-create a thin row for a manual-edge target of the
// given kind. The set is the daemon-managed thin-label kinds
// only — `day` per ADR-0025 cut 1 and `email` / `email-address`
// / `label` per #272. `task` is excluded because task rows
// index `<vault>/tasks/*.md` files per ADR-0024 §Task and only
// materialize on first-create (no automatic backfill); auto-
// creating a phantom `task:<slug>` row from a manual edge would
// land an entity with no backing vault file.
func isLazyMaterializableKind(kind string) bool {
	switch kind {
	case canonical.DayKind,
		canonical.EmailKind,
		canonical.EmailAddressKind,
		canonical.LabelKind:
		return true
	default:
		return false
	}
}

// listEdge is the wire shape for one entry on the GET /v1/edges
// response. Distinct from the POST /v1/edges
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
	// Total reflects the pre-cap edge count for the requested
	// (entity_id, direction, edge_types) tuple per #338.
	// Equals len(Edges) until the limit truncation triggers;
	// then Total > len(Edges) tells the caller more edges
	// exist than the limit surfaced.
	Total int `json:"total"`
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
// [&direction=out|in|both][&limit=N].
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

		// Per-direction cap: each side is independently truncated to
		// `limit`, so direction=both can return up to 2× limit (one page
		// per side) and a node with many outbound edges never starves the
		// inbound side. Capping the concatenated slice instead would slice
		// `[out…, in…][:limit]` and silently drop every inbound edge once
		// the outbound side alone reached `limit`.
		//
		// #338: `total` stays the pre-cap matched count across both
		// directions, so the caller can tell when either side was
		// truncated (total > len(edges)).
		edges := make([]store.Edge, 0)
		total := 0
		if direction == "out" || direction == "both" {
			out, err := st.GetEdgesFor(r.Context(), entityID, types)
			if err != nil {
				logger.ErrorContext(r.Context(), "store.GetEdgesFor",
					"err", err, "entity_id", entityID, "types", types)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to read outbound edges")
				return
			}
			total += len(out)
			if len(out) > limit {
				out = out[:limit]
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
			total += len(in)
			if len(in) > limit {
				in = in[:limit]
			}
			edges = append(edges, in...)
		}

		resp := edgeListResponse{
			OK: true,
			Total: total,
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
