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

// unroutedURLResponse is the 400 wire shape for ADR-0028 §3
// unmatched-URL fail-fast. Adds `instance: "unrouted"` + `url` +
// `plugin` to the canonical error envelope so callers (operator
// CLI, agent) see exactly which URL failed routing under which
// plugin without having to parse the message string.
type unroutedURLResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Instance string `json:"instance"`
	Plugin   string `json:"plugin"`
	URL      string `json:"url"`
	Message  string `json:"message"`
}

// writeUnroutedError emits the ADR-0028 §3 unrouted-URL response:
// 400 status, `instance: "unrouted"` sentinel, plus the URL +
// plugin + a human-readable message that surfaces the underlying
// routing diagnostic (formatted match template, glob field name).
func writeUnroutedError(w http.ResponseWriter, plugin, url string, cause error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	body := unroutedURLResponse{
		OK:       false,
		Error:    "unrouted_url",
		Instance: "unrouted",
		Plugin:   plugin,
		URL:      url,
		Message:  cause.Error(),
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode unrouted-URL envelope", "err", err, "plugin", plugin, "url", url)
	}
}
