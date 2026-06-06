// Introspection endpoint per ADR-0013 §7.
//
// `GET /v1/structure` returns yaad-index's structural signature: the
// canonical-kind registry (with gaps + per-kind instructions),
// canonical edge-type set, and active plugin metadata. A `version`
// field carries a deterministic config-hash so operator tooling can
// poll + diff snapshots to detect rebuild / config-change /
// plugin-add-remove-upgrade transitions without having to re-run
// the whole structure call's diff client-side.
//
// **Plugin section is built from the registry's cached --init
// capabilities.** Every plugin Registry call to Capabilities() is
// in-memory after the first --init (the subprocess wrapper caches);
// the structure handler is therefore cheap to call repeatedly. There
// is no `POST /v1/plugins/refresh` surface in this PR — refresh-on-
// demand is owned by a future plugin-management ADR per ADRs 0005
// and 0006 + the dispatch's explicit out-of-scope.

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
)

// structureKindEntry mirrors a `canonical_kinds:` registry entry on
// the wire (per ADR-0013 §7). `is_canonical: true` is locked-in for
// v1 — reserved for future "passthrough" kinds when canonical-vs-
// source distinctions need surfacing on the structure shape.
type structureKindEntry struct {
	IsCanonical bool `json:"is_canonical"`
	Gaps map[string]string `json:"gaps"`
	Instruction string `json:"instruction,omitempty"`
}

// structurePluginEntry surfaces one loaded plugin's metadata —
// capability fields the operator's tooling treats as the durable
// "what runtime is this" signature.
type structurePluginEntry struct {
	Name string `json:"name"`
	Version string `json:"version"`
	URLPatterns []string `json:"url_patterns"`
	SupportsSearch bool `json:"supports_search"`
	EmitsKinds []string `json:"emits_kinds"`
	EmitsEdges []string `json:"emits_edges"`
}

type structureResponse struct {
	OK bool `json:"ok"`
	Version string `json:"version"`
	Kinds map[string]structureKindEntry `json:"kinds"`
	EdgeTypes []string `json:"edge_types"`
	Plugins []structurePluginEntry `json:"plugins"`
}

func handleStructure(
	logger *slog.Logger,
	registry *plugins.Registry,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	edgeTypes []string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := buildStructureResponse(registry, canonicalKindReg, edgeTypes)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/structure response", "err", err)
		}
	}
}

// buildStructureResponse assembles the full payload from the loaded
// registry + cfg — pure function over its inputs so the version-hash
// helper can be exercised directly in tests without spinning a real
// HTTP handler.
func buildStructureResponse(
	registry *plugins.Registry,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	edgeTypes []string,
) structureResponse {
	kinds := make(map[string]structureKindEntry, len(canonicalKindReg))
	for kind, cfg := range canonicalKindReg {
		// Project the post-ADR-0016 typed shape down to the
		// pre-ADR-0016 flat wire shape (gaps: map[string]string,
		// instruction: string) so existing yaad-mcp clients
		// continue to consume /v1/structure unchanged. A future PR
		// can surface the typed shape on the wire after the
		// MCP-side migration; v1 keeps the legacy shape.
		flatGaps := make(map[string]string, len(cfg.Gaps))
		for field, spec := range cfg.Gaps {
			flatGaps[field] = spec.Description
		}
		instr := ""
		if cfg.Instruction != nil {
			instr = cfg.Instruction.Text
		}
		kinds[kind] = structureKindEntry{
			IsCanonical: true,
			Gaps: flatGaps,
			Instruction: instr,
		}
	}

	// `make([]string, 0, ...)` rather than `append(nil, ...)` so an
	// empty edgeTypes input serializes as `[]` on the wire, not
	// `null`. ADR-0002 specifies the field as an array; the prior shape
	// emitted null when the operator had no canonical_edge_types
	// configured.
	edges := make([]string, 0, len(edgeTypes))
	edges = append(edges, edgeTypes...)
	sort.Strings(edges)

	pluginEntries := make([]structurePluginEntry, 0)
	if registry != nil {
		for _, p := range registry.Plugins() {
			cap := p.Capabilities()
			pluginEntries = append(pluginEntries, structurePluginEntry{
				Name: cap.Name,
				Version: cap.Version,
				URLPatterns: append([]string(nil), cap.URLPatterns...),
				SupportsSearch: cap.SupportsSearch,
				EmitsKinds: kindNames(cap.EntityKinds),
				EmitsEdges: kindNames(cap.EdgeKinds),
			})
		}
	}
	sort.Slice(pluginEntries, func(i, j int) bool {
		return pluginEntries[i].Name < pluginEntries[j].Name
	})

	return structureResponse{
		OK: true,
		Version: computeStructureVersion(canonicalKindReg, edges, pluginEntries),
		Kinds: kinds,
		EdgeTypes: edges,
		Plugins: pluginEntries,
	}
}

// kindNames extracts just the Name field from a slice of plugin
// KindSpec entries — the structure surface advertises plugin-emitted
// kind names; per-kind metadata (description, snippet_fields) is the
// `/v1/kinds` endpoint's responsibility.
func kindNames(in []plugins.KindSpec) []string {
	out := make([]string, len(in))
	for i, k := range in {
		out[i] = k.Name
	}
	return out
}

// computeStructureVersion produces the deterministic config-hash
// signature surfaced as `version` on the response (per ADR-0013 §7).
//
// SHA-256 over a canonical JSON serialization of:
//
// - canonical_kinds map (sorted-by-key Marshal of
// `config.CanonicalKindConfig`, the registry's own struct shape;
// not the wire `structureKindEntry` — we hash the input config
// directly so the version is independent of wire-shape evolution).
// - edge_types slice (caller-sorted).
// - plugin fingerprints (caller-sorted-by-name slice of
// structurePluginEntry).
//
// Output truncated to the first 16 hex chars for readability — full
// SHA-256 is overkill for the operator-facing diff use case, and 16
// hex chars (64 bits) far exceeds the collision threshold any
// realistic operator deployment will hit. If a future caller needs
// the full digest, this is the one place to extend.
func computeStructureVersion(
	canonicalKinds map[string]config.CanonicalKindConfig,
	edgeTypes []string,
	pluginFingerprints []structurePluginEntry,
) string {
	// Canonicalize the kinds map by emitting it in a sorted-key
	// JSON marshal — Go's encoding/json walks map keys in sorted
	// order since Go 1.12, so this is deterministic without a
	// hand-roll.
	type canonForm struct {
		Kinds map[string]config.CanonicalKindConfig `json:"kinds"`
		EdgeTypes []string `json:"edge_types"`
		Plugins []structurePluginEntry `json:"plugins"`
		// Discrim is a hand-bumped schema-version sentinel. It does
		// NOT auto-detect schema changes; future maintainers must
		// bump this string when the canonical-form layout changes
		// so the version-hash deliberately invalidates client-side
		// snapshot caches across the bump.
		Discrim string `json:"discrim"`
	}
	body, err := json.Marshal(canonForm{
		Kinds: canonicalKinds,
		EdgeTypes: edgeTypes,
		Plugins: pluginFingerprints,
		Discrim: "yaad-index/structure-v1",
	})
	if err != nil {
		// Marshal failure here means a non-JSON-encodable value
		// leaked into the registry; surface the error string in
		// the version so operators see the brokenness instead of
		// a stable-looking hash that masks it.
		return fmt.Sprintf("error:%s", err.Error())
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}
