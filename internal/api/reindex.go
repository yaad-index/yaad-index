package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/reindex"
)

// reindexRunner is the narrow surface the HTTP handler depends on —
// captured by the production wiring and a test-double in unit tests.
type reindexRunner interface {
	Run(ctx context.Context, mode reindex.Mode) (reindex.Summary, error)
}

// reindexRequest is the optional JSON body for POST /v1/reindex. An
// empty body is treated as the default (incremental). Operators
// triggering a hard rebuild post `{"mode": "full"}`.
type reindexRequest struct {
	Mode string `json:"mode"`
}

// HandleReindex constructs the POST /v1/reindex handler. Public so
// main.go can wire it via api.WithReindexHandler; tests can construct
// it directly with a fake runner.
//
// Request body is optional. `{}` or empty body → incremental mode.
// `{"mode": "incremental"}` → incremental. `{"mode": "full"}` → full.
// Any other value → 400 invalid_argument.
//
// Response on success is the JSON-encoded reindex.Summary with status
// 200. Any error from the reindex layer surfaces as 500 internal_error
// (the request itself was valid; the failure is server-side).
func HandleReindex(logger *slog.Logger, runner reindexRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mode := reindex.Incremental
		if r.ContentLength != 0 {
			var req reindexRequest
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("decode request body: %v", err))
				return
			}
			switch req.Mode {
			case "", "incremental":
				mode = reindex.Incremental
			case "full":
				mode = reindex.Full
			default:
				writeError(w, http.StatusBadRequest, "invalid_argument",
					fmt.Sprintf("mode %q: must be \"incremental\" or \"full\"", req.Mode))
				return
			}
		}

		summary, err := runner.Run(r.Context(), mode)
		if err != nil {
			logger.Error("reindex failed", "err", err, "mode", mode.String())
			writeError(w, http.StatusInternalServerError, "internal_error",
				fmt.Sprintf("reindex %s: %v", mode.String(), err))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(summary)
	}
}
