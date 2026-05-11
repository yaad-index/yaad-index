package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

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
		// (lines 144–150). Today the param is accepted (so callers
		// exercising the documented surface still get a 200) but ignored
		// — store.GetEntities returns entities with empty edge arrays.
		// Real expansion lands with the edge-side cutover.
		_ = req.WithEdges

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

		entities := make([]entity, 0, len(matched))
		for i := range matched {
			entities = append(entities, toAPIEntity(&matched[i]))
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
