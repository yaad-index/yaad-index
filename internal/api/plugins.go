package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/yaad-index/yaad-index/internal/config"
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
	// Commands are the imperative command entries this plugin
	// advertises per ADR-0022 §1 + the 2026-05-22 amendment for
	// #107. Each entry serializes either as the bare-name string
	// (when operator_only=false — the back-compat shape) or as the
	// long-form object `{"name":"...","operator_only":true}` when
	// the per-command operator-only gate is engaged. Empty list
	// signals a plugin with no command-shape invocation surface
	// (yaad-wikipedia, yaad-bgg today).
	Commands []plugins.CommandSpec `json:"commands"`
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
	// Instances enumerates the operator-configured runtime-config
	// variants for this plugin per ADR-0028 §1. Single-implicit
	// plugins (no `instances:` block in operator config) surface
	// `[{name: "default", enabled: true}]`. Multi-instance plugins
	// surface one entry per configured instance in declaration
	// order. The `enabled` flag is reserved for Cut 5; until that
	// lands every instance reports `enabled: true`.
	Instances []pluginInstanceEntry `json:"instances"`
}

type pluginInstanceEntry struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
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
func handlePlugins(logger *slog.Logger, registry *plugins.Registry, pluginInstanceConfigs map[string][]config.InstanceEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := pluginsResponse{
			OK: true,
			Plugins: enumeratePlugins(registry, pluginInstanceConfigs),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/plugins response", "err", err)
		}
	}
}

func enumeratePlugins(registry *plugins.Registry, pluginInstanceConfigs map[string][]config.InstanceEntry) []pluginEntry {
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
			Commands: copyCommands(caps.Commands),
			SourceNamespace: caps.SourceNamespace,
			EntityKinds: mapEntityKinds(caps.EntityKinds),
			EdgeKinds: mapEdgeKinds(caps.EdgeKinds),
			Instances: instanceList(pluginInstanceConfigs, name),
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

// copyCommands mirrors copyOrEmpty for the CommandSpec slice the
// /v1/plugins enumerator surfaces. Empty input emits `[]` not `null`.
func copyCommands(in []plugins.CommandSpec) []plugins.CommandSpec {
	if len(in) == 0 {
		return []plugins.CommandSpec{}
	}
	out := make([]plugins.CommandSpec, len(in))
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

// instanceList returns the per-plugin instance list for the
// /v1/plugins response per ADR-0028 §1 + §7. When
// pluginInstanceConfigs has no entry for this plugin (e.g. dev
// binaries without operator config — handler test paths),
// returns a synthesized `[{name: "default", enabled: true}]` so
// the response shape is stable across deployment modes. For
// configured plugins, each entry's `enabled` field reflects the
// operator's actual InstanceEntry.IsEnabled() per Cut 5 — nil /
// true → true, explicit false → false.
func instanceList(pluginInstanceConfigs map[string][]config.InstanceEntry, name string) []pluginInstanceEntry {
	instances, ok := pluginInstanceConfigs[name]
	if !ok || len(instances) == 0 {
		return []pluginInstanceEntry{{Name: "default", Enabled: true}}
	}
	out := make([]pluginInstanceEntry, 0, len(instances))
	for _, inst := range instances {
		out = append(out, pluginInstanceEntry{Name: inst.Name, Enabled: inst.IsEnabled()})
	}
	return out
}
