package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit = 100
)

type searchResult struct {
	ID string `json:"id"`
	Kind string `json:"kind"`
	Snippet string `json:"snippet"`
	Score float64 `json:"score"`
}

type searchResponse struct {
	OK bool `json:"ok"`
	Results []searchResult `json:"results"`
	Total int `json:"total"`
	Limit int `json:"limit"`
	Offset int `json:"offset"`
}

// handleSearch implements GET /v1/search.
//
// Snippet semantics (per ADR-0008 / a prior PR): the snippet field on each
// hit is the entity's agent-filled `summary` (read from `data["summary"]`,
// which a prior PR's vault-first fill mirrors into the DB row). Substring
// extraction from arbitrary data fields — the per-kind SnippetFields
// chain that lived here pre-a prior PR — is gone; snippet is now a property
// of the entity, not a query-time computation. Entities that haven't
// been agent-filled return empty snippet, and that's the correct
// semantics: the agent flow + plugin starter-summary emission close the
// gap, not a fallback string-extraction.
//
// The DB-side LIKE on `data` (in store.Search) already matches against
// the summary because the summary is mirrored into the data column;
// no FTS5 schema change is required for v1. A future ADR may swap to
// a real FTS index keyed on summary + body.
func handleSearch(logger *slog.Logger, st store.Store, registry *plugins.Registry) http.HandlerFunc {
	maxChars := readSnippetMaxChars()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		query := strings.TrimSpace(q.Get("q"))
		kind := strings.TrimSpace(q.Get("kind"))
		// q-or-kind required: a request with both empty has nothing
		// for the search backend to filter on. Listing every entity
		// across every kind is GET /v1/entities/batch territory, not
		// /v1/search.
		if query == "" && kind == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "q or kind is required")
			return
		}

		if kind != "" && !isRegisteredEntityKind(registry, kind) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("kind %q is not in the registered entity_kinds", kind))
			return
		}

		limit, err := parseBoundedInt(q.Get("limit"), searchDefaultLimit, 1, searchMaxLimit)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("limit: %v", err))
			return
		}

		offset, err := parseBoundedInt(q.Get("offset"), 0, 0, -1)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("offset: %v", err))
			return
		}

		// Archive-state filter per ADR-0018 step 2. Default = exclude
		// archived (most callers want the active set). Mutually
		// exclusive flags: passing both `?include_archived=true` and
		// `?archived_only=true` is a bad request.
		archivedFilter, err := parseArchivedFilter(q.Get("include_archived"), q.Get("archived_only"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("archived filter: %v", err))
			return
		}

		hits, total, err := st.Search(r.Context(), query, kind, limit, offset, archivedFilter)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.Search", "err", err,
				"q", query, "kind", kind, "limit", limit, "offset", offset)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to search")
			return
		}

		results := make([]searchResult, 0, len(hits))
		for _, h := range hits {
			// Snippet = entity's agent-filled summary, capped at
			// maxChars. Empty-summary → empty-snippet is the correct
			// semantic for un-filled entities (per ADR-0008).
			snippet := truncate(stringField(h.Data, "summary"), maxChars)
			results = append(results, searchResult{
				ID: h.ID,
				Kind: h.Kind,
				Snippet: snippet,
				Score: h.Score,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(searchResponse{
			OK: true,
			Results: results,
			Total: total,
			Limit: limit,
			Offset: offset,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/search response", "err", err)
		}
	}
}

// parseArchivedFilter resolves the operator-facing
// `?include_archived` / `?archived_only` query params into the
// store-side `ArchivedFilter` per ADR-0018 step 2. Defaults to
// exclude (the active set). Both flags set simultaneously is a
// bad request — the caller must pick one shape.
//
// Truthy values for either flag: "true", "1", "yes" (case-
// insensitive). Anything else (including empty string when the
// param is absent) is treated as false.
func parseArchivedFilter(rawInclude, rawOnly string) (store.ArchivedFilter, error) {
	include := isTruthy(rawInclude)
	only := isTruthy(rawOnly)
	if include && only {
		return store.ArchivedExclude, fmt.Errorf("`include_archived` and `archived_only` are mutually exclusive; pick one")
	}
	switch {
	case only:
		return store.ArchivedOnly, nil
	case include:
		return store.ArchivedInclude, nil
	default:
		return store.ArchivedExclude, nil
	}
}

// isTruthy maps the operator-facing query-param values to a bool.
// Mirrors what most clients (curl, MCP wrappers, browser-side
// fetch) emit when they want a "set this flag" semantic. The empty
// string AND any non-truthy value both mean false — operators don't
// have to know the exact spelling.
func isTruthy(s string) bool {
	switch strings.ToLower(s) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// parseBoundedInt parses raw as a base-10 int and applies the bounds:
// returns def if raw is empty; rejects unparseable values; rejects values
// below min; rejects values above max if max >= 0 (a negative max disables
// the upper bound). Used for query-string params (limit, offset, …).
func parseBoundedInt(raw string, def, min, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("not a base-10 integer: %q", raw)
	}
	return boundInt(n, min, max)
}

// boundIntPtr applies the same bounds as parseBoundedInt, but for a value
// already parsed by encoding/json (e.g. a `wait_seconds` int field). Nil
// means absent → returns def. Same `max < 0 disables upper bound` sentinel.
func boundIntPtr(p *int, def, min, max int) (int, error) {
	if p == nil {
		return def, nil
	}
	return boundInt(*p, min, max)
}

// boundInt is the shared bound-check used by both parseBoundedInt and
// boundIntPtr. Negative max disables the upper bound (sentinel).
func boundInt(n, min, max int) (int, error) {
	if n < min {
		return 0, fmt.Errorf("must be >= %d, got %d", min, n)
	}
	if max >= 0 && n > max {
		return 0, fmt.Errorf("must be <= %d, got %d", max, n)
	}
	return n, nil
}

// isRegisteredEntityKind reports whether name is advertised by any
// registered plugin's Capabilities().EntityKinds. Mirror of
// isRegisteredEdgeKind in edges.go.
func isRegisteredEntityKind(registry *plugins.Registry, name string) bool {
	for _, p := range registry.Plugins() {
		for _, k := range p.Capabilities().EntityKinds {
			if k.Name == name {
				return true
			}
		}
	}
	return false
}
