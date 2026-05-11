package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// entityKind / edgeKind / kindsResponse mirror the wire shape locked in
// ADR-0002 (`GET /v1/kinds`, lines 298–331). Field names map to JSON via
// snake_case tags.
type entityKind struct {
	Name string `json:"name"`
	Description string `json:"description"`
	SourcePlugins []string `json:"source_plugins"`
}

type edgeKind struct {
	Name string `json:"name"`
	Description string `json:"description"`
	FromKind string `json:"from_kind"`
	ToKind string `json:"to_kind"`
	SourcePlugins []string `json:"source_plugins"`
}

type kindsResponse struct {
	OK bool `json:"ok"`
	EntityKinds []entityKind `json:"entity_kinds"`
	EdgeKinds []edgeKind `json:"edge_kinds"`
}

// handleKinds aggregates entity / edge kinds across every registered
// plugin's capabilities and emits the union, deduped by name with
// source_plugins unioned across plugins emitting the same kind. Empty
// registry → empty arrays + ok=true (per the source issue's acceptance).
//
// Sort order is alphabetical by name on both arrays so successive
// calls produce byte-identical responses with a stable plugin set.
// Description is taken from the first plugin to advertise the kind;
// from_kind / to_kind are taken from the same source. If two plugins
// disagree on description / from_kind / to_kind for the same kind
// name that's a config issue the operator should fix — the handler
// is deterministic but doesn't try to merge conflicting metadata.
func handleKinds(logger *slog.Logger, registry *plugins.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := aggregateKinds(registry)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/kinds response", "err", err)
		}
	}
}

func aggregateKinds(registry *plugins.Registry) kindsResponse {
	entityIdx := make(map[string]*entityKind)
	edgeIdx := make(map[string]*edgeKind)

	for _, p := range registry.Plugins() {
		caps := p.Capabilities()
		pluginName := caps.Name
		if pluginName == "" {
			pluginName = p.Name()
		}
		for _, ks := range caps.EntityKinds {
			if existing, ok := entityIdx[ks.Name]; ok {
				existing.SourcePlugins = appendUnique(existing.SourcePlugins, pluginName)
				continue
			}
			entityIdx[ks.Name] = &entityKind{
				Name: ks.Name,
				Description: ks.Description,
				SourcePlugins: []string{pluginName},
			}
		}
		for _, ks := range caps.EdgeKinds {
			if existing, ok := edgeIdx[ks.Name]; ok {
				existing.SourcePlugins = appendUnique(existing.SourcePlugins, pluginName)
				continue
			}
			edgeIdx[ks.Name] = &edgeKind{
				Name: ks.Name,
				Description: ks.Description,
				FromKind: ks.FromKind,
				ToKind: ks.ToKind,
				SourcePlugins: []string{pluginName},
			}
		}
	}

	resp := kindsResponse{
		OK: true,
		EntityKinds: make([]entityKind, 0, len(entityIdx)),
		EdgeKinds: make([]edgeKind, 0, len(edgeIdx)),
	}
	for _, e := range entityIdx {
		sort.Strings(e.SourcePlugins)
		resp.EntityKinds = append(resp.EntityKinds, *e)
	}
	for _, e := range edgeIdx {
		sort.Strings(e.SourcePlugins)
		resp.EdgeKinds = append(resp.EdgeKinds, *e)
	}
	sort.Slice(resp.EntityKinds, func(i, j int) bool {
		return resp.EntityKinds[i].Name < resp.EntityKinds[j].Name
	})
	sort.Slice(resp.EdgeKinds, func(i, j int) bool {
		return resp.EdgeKinds[i].Name < resp.EdgeKinds[j].Name
	})
	return resp
}

func appendUnique(slice []string, s string) []string {
	for _, x := range slice {
		if x == s {
			return slice
		}
	}
	return append(slice, s)
}
