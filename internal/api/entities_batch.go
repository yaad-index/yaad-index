package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yaad-index/yaad-index/internal/store"
)

// batchMaxIDs is the per-call upper bound from ADR-0002 (lines 144–150).
// Requests above this return a 400 with the `too_many_ids` error code so a
// client can distinguish "request was malformed" from "request was too big"
// and react accordingly (split + retry vs. fix the input).
const batchMaxIDs = 100

type batchRequest struct {
	IDs []string `json:"ids"`
	WithEdges []string `json:"with_edges"`
}

type batchResponse struct {
	OK bool `json:"ok"`
	Entities []entity `json:"entities"`
	Missing []string `json:"missing"`
}

func handleEntitiesBatch(logger *slog.Logger, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req batchRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		if len(req.IDs) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"ids must be a non-empty array")
			return
		}
		if len(req.IDs) > batchMaxIDs {
			writeError(w, http.StatusBadRequest, "too_many_ids",
				fmt.Sprintf("ids exceeds maximum of %d (got %d)", batchMaxIDs, len(req.IDs)))
			return
		}

		// `with_edges` is the inline-edge-expansion list per ADR-0002
		// (lines 144–150), mirroring the single-GET sentinel/comma
		// surface: a present field requests expansion, `*`/`all` (or an
		// empty list) expands all edge types, otherwise the listed types
		// filter the expansion. Absent → no expansion (#452).
		expandEdges, edgeTypes := resolveBatchWithEdges(req.WithEdges)

		// In-flight rule (ADR-0002 lines 167–169): an in-flight ingest leaves
		// a placeholder entity with sparse `data`. Such placeholders MUST be
		// returned in `entities`, not `missing`. `missing` is reserved for
		// IDs the server has never seen. store.GetEntities preserves this
		// distinction by virtue of returning placeholder rows from the
		// entities table (when ingest writes them) and only sentinel-
		// missing for ids with no row at all.
		matched, missing, err := st.GetEntities(r.Context(), req.IDs)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntities", "err", err, "ids_len", len(req.IDs))
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load entities")
			return
		}

		// Inline-edge expansion: one GetEdgesForMany over the whole
		// matched frontier (not N per-entity round-trips), grouped back
		// onto each source id. Same wire shape as single-GET — each
		// entity carries its edges + the ADR-0018 archived flag on each
		// endpoint. A store error fails the request (matching single-GET).
		var edgesByFrom map[string][]store.Edge
		if expandEdges {
			fromIDs := make([]string, len(matched))
			for i := range matched {
				fromIDs[i] = matched[i].ID
			}
			allEdges, err := st.GetEdgesForMany(r.Context(), fromIDs, edgeTypes)
			if err != nil {
				logger.ErrorContext(r.Context(), "store.GetEdgesForMany", "err", err,
					"ids_len", len(fromIDs), "types", edgeTypes)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to expand edges")
				return
			}
			edgesByFrom = make(map[string][]store.Edge, len(matched))
			for _, e := range allEdges {
				edgesByFrom[e.From] = append(edgesByFrom[e.From], e)
			}
		}

		entities := make([]entity, 0, len(matched))
		for i := range matched {
			if expandEdges {
				matched[i].Edges = edgeRefsFromStoreEdges(edgesByFrom[matched[i].ID])
			}
			out := toAPIEntity(&matched[i])
			// ADR-0018 step 3: stamp the archived flag on each endpoint,
			// same as single-GET. Per-entity (like single-GET); a lookup
			// failure downgrades to no-flag without failing the row.
			if expandEdges && len(out.Edges) > 0 {
				stampEdgeArchivedFlags(r.Context(), logger, st, out.Edges)
			}
			entities = append(entities, out)
		}
		if missing == nil {
			missing = []string{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(batchResponse{
			OK: true,
			Entities: entities,
			Missing: missing,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities/batch response", "err", err)
		}
	}
}

// resolveBatchWithEdges interprets the batch request's `with_edges`
// field, mirroring the single-GET sentinel/comma semantics (parseWithEdges).
// A nil slice (field absent) means no expansion. A present slice requests
// expansion; the `*` / `all` sentinels — or an empty/blank-only list —
// collapse to "all edge types" (nil filter), otherwise the trimmed,
// non-empty values are the concrete type filter. Returns
// (expand, edgeTypes) so the handler branches identically to single-GET.
func resolveBatchWithEdges(withEdges []string) (expand bool, edgeTypes []string) {
	if withEdges == nil {
		return false, nil
	}
	for _, t := range withEdges {
		tt := strings.TrimSpace(t)
		if tt == "" {
			continue
		}
		if tt == "*" || tt == "all" {
			return true, nil
		}
		edgeTypes = append(edgeTypes, tt)
	}
	return true, edgeTypes
}

// rejectGetOnBatch returns the canonical method-not-allowed envelope for any
// non-POST request that lands on /v1/entities/batch. Without this, Go's
// method-aware mux would route GET /v1/entities/batch to the GET
// /v1/entities/{id} handler (with id="batch") because that pattern matches
// the path and method — the resulting 404 would tell callers "no entity with
// id batch", which is misleading. Carving the path out preserves 405
// semantics and a useful Allow header.
func rejectGetOnBatch() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"POST is required for /v1/entities/batch")
	}
}
