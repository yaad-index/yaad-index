package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Wire shape locked by ADR-0002 §"GET /v1/entities/{id}/context" (per
// yaad-index the source issue). The handler returns the root entity plus a
// flattened neighbor list, each neighbor carrying the edge that
// introduced it + the BFS depth at which it was first reached.

// contextEdge is the wire shape for the edge that introduced a
// context neighbor — the full triple `{type, from, to}`. Distinct
// from `edgeRef` (the inline single-hop body shape) because here the
// `from` is load-bearing: at depth ≥ 2 the source is one of the
// previously visited neighbors, not the root.
type contextEdge struct {
	Type string `json:"type"`
	From string `json:"from"`
	To string `json:"to"`
}

type contextNeighbor struct {
	Edge contextEdge `json:"edge"`
	Entity entity `json:"entity"`
	Depth int `json:"depth"`
}

type contextResponse struct {
	Root entity `json:"root"`
	Neighbors []contextNeighbor `json:"neighbors"`
	Truncated bool `json:"truncated"`
}

// Bounds enforced server-side per ADR-0002 ( amendment).
const (
	contextMaxDepth = 3
	contextDefaultMaxResults = 200
	contextMaxResultsCap = 1000
)

func handleEntityContext(logger *slog.Logger, st store.Store, vaultReader *vault.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		id, rerr := resolveEntityID(r.Context(), st, id)
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to resolve entity reference")
			return
		}

		depthRaw := r.URL.Query().Get("depth")
		if depthRaw == "" {
			writeFieldError(w, "depth", "required: depth=0..3")
			return
		}
		depth, err := strconv.Atoi(depthRaw)
		if err != nil {
			writeFieldError(w, "depth", "must be an integer")
			return
		}
		if depth < 0 || depth > contextMaxDepth {
			writeFieldError(w, "depth", fmt.Sprintf("must be in [0, %d]", contextMaxDepth))
			return
		}

		maxResults := contextDefaultMaxResults
		if maxRaw := r.URL.Query().Get("max_results"); maxRaw != "" {
			n, err := strconv.Atoi(maxRaw)
			if err != nil {
				writeFieldError(w, "max_results", "must be an integer")
				return
			}
			if n <= 0 || n > contextMaxResultsCap {
				writeFieldError(w, "max_results", fmt.Sprintf("must be in (0, %d]", contextMaxResultsCap))
				return
			}
			maxResults = n
		}

		edgeTypes := parseEdgeTypesFilter(r.URL.Query().Get("edge_types"))
		// edge_types being set but parsing to empty (e.g. ",,") is treated
		// as "no filter" — the rare "filter to nothing" semantic isn't
		// useful here. parseEdgeTypesFilter only returns non-empty strings.

		// `notes_kind` per #186 Cut 3: scopes the Notes arrays carried
		// on the root + every neighbor entity. Same closed-set rule as
		// /v1/entities/{id}.
		notesKind, err := parseNotesKindFilter(r.URL.Query().Get("notes_kind"))
		if err != nil {
			writeFieldError(w, "notes_kind", err.Error())
			return
		}

		root, neighbors, truncated, err := st.GetContextNeighbors(r.Context(), id, depth, edgeTypes, maxResults)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetContextNeighbors",
				"err", err, "id", id, "depth", depth)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to compute context")
			return
		}

		out := contextResponse{
			Root: filterNotesByKind(vaultMergedEntity(r.Context(), logger, root, vaultReader), notesKind),
			Neighbors: make([]contextNeighbor, len(neighbors)),
			Truncated: truncated,
		}
		for i, n := range neighbors {
			neighbor := n.Entity
			out.Neighbors[i] = contextNeighbor{
				Edge: contextEdge{
					Type: n.Edge.Type,
					From: n.Edge.From,
					To: n.Edge.To,
				},
				Entity: filterNotesByKind(vaultMergedEntity(r.Context(), logger, &neighbor, vaultReader), notesKind),
				Depth: n.Depth,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities/{id}/context response",
				"err", err, "id", id)
		}
	}
}

// parseEdgeTypesFilter splits a comma-separated edge_types value, trims
// whitespace, drops empties. Empty / absent value → nil (no filter).
//
// Mirrors parseWithEdges (in entities.go) for sentinel handling so the
// /context endpoint reaches wildcard parity with the main entity-GET:
// the explicit "all types" sentinels `*` and `all` collapse to the
// no-filter shape, and mixing a sentinel with concrete edge types in
// the same value treats it as "all types" — the broader of the two
// wins, keeping the agent's mental model linear across both endpoints.
//
// Kept structurally distinct from parseWithEdges so a future
// edge_types validation rule (e.g. reject edge types not in
// `canonical_edge_types`) lands here without coupling to the older
// /v1/entities/{id}?with_edges= shape. Both functions should evolve
// together for sentinel + validation behavior.
func parseEdgeTypesFilter(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if t == "*" || t == "all" {
			return nil
		}
		out = append(out, t)
	}
	return out
}

// writeFieldError emits a 400 invalid_argument with a `field` carrier
// per the ADR-0002 amendment for — the agent gets a structured
// hint at which query parameter failed validation.
func writeFieldError(w http.ResponseWriter, field, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": false,
		"error": "invalid_argument",
		"message": message,
		"field": field,
	})
}

// vaultMergedEntity resolves a store.Entity through the same merge
// pipeline `GET /v1/entities/{id}` uses (toAPIEntity → vault overlay).
// vaultReader=nil short-circuits the overlay. ctx is reserved for a
// future vault.Reader.ReadByIDContext signature; today the reader is
// context-free at the file IO boundary, so the parameter is unused.
func vaultMergedEntity(_ context.Context, logger *slog.Logger, e *store.Entity, vaultReader *vault.Reader) entity {
	out := toAPIEntity(e)
	if vaultReader == nil {
		return out
	}
	ve, err := vaultReader.ReadByID(e.Kind, e.ID)
	if err != nil {
		logger.Warn("vault read for context-entity surface errored; serving DB-only shape",
			"id", e.ID, "err", err)
		return out
	}
	return mergeVaultEntity(out, ve)
}
