package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// pluginEntry mirrors the per-plugin subset of plugins.Capabilities
// surfaced on /v1/plugins (yaad-index). The endpoint is the
// inverse of /v1/kinds: /v1/kinds aggregates "kind → plugins";
// /v1/plugins enumerates "plugin → kinds + url_patterns + commands
// + source_namespace + version". Consumers (yaad-mcp SKILL.md
// generators, agent capability probes) get a single round-trip to
// the daemon for the full per-plugin view.
type pluginEntry struct {
	Name string `json:"name"`
	// Version is the plugin binary's `--version` output (or the
	// `version` field from --init capabilities). Surfaced so
	// SKILL.md generators can pin example output to a known
	// plugin build.
	Version string `json:"version,omitempty"`
	// URLPatterns are the plugin's declared regex matchers for
	// URL-shape dispatch (per ADR-0006 + ADR-0022 §4). Empty list
	// signals a poll-driven plugin (yaad-gmail) — no URL-shape
	// invocation surface.
	URLPatterns []string `json:"url_patterns"`
	// Commands are the bare command names this plugin advertises
	// per ADR-0022 §1. Empty list signals a plugin with no
	// command-shape invocation surface (yaad-wikipedia, yaad-bgg
	// today).
	Commands []string `json:"commands"`
	// EntityKinds + EdgeKinds carry the plugin's per-kind metadata
	// (name + description + from_kind/to_kind on edges). Matches
	// the shape /v1/kinds uses but scoped to one plugin.
	EntityKinds []pluginKindEntry `json:"entity_kinds"`
	EdgeKinds []pluginEdgeEntry `json:"edge_kinds"`
	// SourceNamespace is the ADR-0021 vault-path prefix the plugin
	// emits source-shape entities under (e.g. "wikipedia", "bgg",
	// "gmail"). Empty when the plugin doesn't emit source-shape
	// entities.
	SourceNamespace string `json:"source_namespace,omitempty"`
}

type pluginKindEntry struct {
	Name string `json:"name"`
	Description string `json:"description,omitempty"`
}

type pluginEdgeEntry struct {
	Name string `json:"name"`
	Description string `json:"description,omitempty"`
	FromKind string `json:"from_kind,omitempty"`
	ToKind string `json:"to_kind,omitempty"`
}

type pluginsResponse struct {
	OK bool `json:"ok"`
	Plugins []pluginEntry `json:"plugins"`
}

// handlePlugins enumerates every registered plugin's --init
// capabilities subset. Sort order is registry order (matching
// the dispatch order plugins are walked at /v1/ingest time); empty
// registry → ok=true with empty `plugins` array. Empty list fields
// (url_patterns, commands, entity_kinds, edge_kinds) marshal as
// `[]` rather than `null` so consumers get a stable shape they
// don't have to nil-guard.
func handlePlugins(logger *slog.Logger, registry *plugins.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := pluginsResponse{
			OK: true,
			Plugins: enumeratePlugins(registry),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/plugins response", "err", err)
		}
	}
}

func enumeratePlugins(registry *plugins.Registry) []pluginEntry {
	regPlugins := registry.Plugins()
	out := make([]pluginEntry, 0, len(regPlugins))
	for _, p := range regPlugins {
		caps := p.Capabilities()
		name := caps.Name
		if name == "" {
			name = p.Name()
		}
		entry := pluginEntry{
			Name: name,
			Version: caps.Version,
			URLPatterns: copyOrEmpty(caps.URLPatterns),
			Commands: copyOrEmpty(caps.Commands),
			SourceNamespace: caps.SourceNamespace,
			EntityKinds: mapEntityKinds(caps.EntityKinds),
			EdgeKinds: mapEdgeKinds(caps.EdgeKinds),
		}
		out = append(out, entry)
	}
	// Registry order is dispatch order; preserve it. Within each
	// plugin, entity_kinds + edge_kinds are sorted by name for
	// stable diff output across repeated calls.
	for i := range out {
		sort.Slice(out[i].EntityKinds, func(a, b int) bool {
			return out[i].EntityKinds[a].Name < out[i].EntityKinds[b].Name
		})
		sort.Slice(out[i].EdgeKinds, func(a, b int) bool {
			return out[i].EdgeKinds[a].Name < out[i].EdgeKinds[b].Name
		})
	}
	return out
}

// copyOrEmpty returns a fresh slice of strings — either a copy of
// the input or an empty (non-nil) slice when the input is empty.
// Used so JSON marshal emits `[]` rather than `null` for absent
// declarations; consumers get a stable shape.
func copyOrEmpty(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func mapEntityKinds(in []plugins.KindSpec) []pluginKindEntry {
	out := make([]pluginKindEntry, 0, len(in))
	for _, k := range in {
		out = append(out, pluginKindEntry{
			Name: k.Name,
			Description: k.Description,
		})
	}
	return out
}

func mapEdgeKinds(in []plugins.KindSpec) []pluginEdgeEntry {
	out := make([]pluginEdgeEntry, 0, len(in))
	for _, k := range in {
		out = append(out, pluginEdgeEntry{
			Name: k.Name,
			Description: k.Description,
			FromKind: k.FromKind,
			ToKind: k.ToKind,
		})
	}
	return out
}
