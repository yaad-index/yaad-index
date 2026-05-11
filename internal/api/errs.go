package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorResponse is the canonical non-2xx envelope from ADR-0002.
type errorResponse struct {
	OK bool `json:"ok"`
	Error string `json:"error"`
	Message string `json:"message"`
}

// writeError emits the canonical error envelope with the given status, code,
// and human-readable message. Intentionally minimal — full middleware lands
// in a later PR (ADR-0002 action item 3).
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := errorResponse{OK: false, Error: code, Message: message}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode error envelope", "err", err, "code", code)
	}
}
