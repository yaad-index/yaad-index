// /v1/canonical_registry/* surface per #48 slice 3:
//
//   - GET /v1/canonical_registry/effective — current merged
//     registry, with per-gap source_layer provenance naming
//     which of the 4 layers supplied each spec.
//   - GET /v1/canonical_registry/available — Layer 1.5 daemon-
//     shipped kinds that exist as starter-pool defaults but
//     aren't currently active in the merged registry. Operators
//     inspect "what could I opt into?" before writing config.
//
// Operator-facing visibility into the canonical_kinds merge.
// Pairs with `/v1/cv-status` (drift signal: what's silently
// dropping) and `/v1/structure` (catalog snapshot).

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/yaad-index/yaad-index/internal/config"
)

// canonicalRegistryEffectiveGap is the wire shape per gap field
// under `/v1/canonical_registry/effective`. Embeds the
// existing GapSpec JSON tags + the `source_layer` provenance
// tag from the merge.
type canonicalRegistryEffectiveGap struct {
	Type         string   `json:"type,omitempty"`
	Description  string   `json:"description"`
	FillStrategy string   `json:"fill_strategy,omitempty"`
	Range        []int    `json:"range,omitempty"`
	MaxLength    int      `json:"max_length,omitempty"`
	Values       []string `json:"values,omitempty"`
	Kinds        []string `json:"kinds,omitempty"`
	SourceLayer  string   `json:"source_layer"`
}

// canonicalRegistryEffectiveInstruction is the wire shape for
// the per-kind instruction with its source_layer.
type canonicalRegistryEffectiveInstruction struct {
	Enabled     bool   `json:"enabled"`
	Text        string `json:"text,omitempty"`
	SourceLayer string `json:"source_layer"`
}

// canonicalRegistryEffectiveKind is the per-kind envelope.
// `gaps` is keyed by field name + the inner shape adds
// source_layer. `instruction` lifts its own source_layer to a
// sibling field for parity with the per-gap shape.
type canonicalRegistryEffectiveKind struct {
	Gaps           map[string]canonicalRegistryEffectiveGap `json:"gaps"`
	Instruction    canonicalRegistryEffectiveInstruction    `json:"instruction"`
	ResolverPlugin string                                   `json:"resolver_plugin,omitempty"`
}

type canonicalRegistryEffectiveResponse struct {
	OK    bool                                       `json:"ok"`
	Kinds map[string]canonicalRegistryEffectiveKind  `json:"kinds"`
}

// handleCanonicalRegistryEffective implements
// GET /v1/canonical_registry/effective. Returns the merged
// registry annotated with per-(kind, field) source_layer
// provenance.
func handleCanonicalRegistryEffective(
	logger *slog.Logger,
	reg map[string]config.CanonicalKindConfig,
	prov config.RegistryProvenance,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := canonicalRegistryEffectiveResponse{
			OK:    true,
			Kinds: make(map[string]canonicalRegistryEffectiveKind, len(reg)),
		}
		for kind, cfg := range reg {
			kindProv := prov[kind]
			gaps := make(map[string]canonicalRegistryEffectiveGap, len(cfg.Gaps))
			for field, spec := range cfg.Gaps {
				layer := kindProv[field]
				gaps[field] = canonicalRegistryEffectiveGap{
					Type:         spec.Type,
					Description:  spec.Description,
					FillStrategy: spec.FillStrategy,
					Range:        spec.Range,
					MaxLength:    spec.MaxLength,
					Values:       spec.Values,
					Kinds:        spec.Kinds,
					SourceLayer:  string(layer),
				}
			}
			instrLayer := kindProv[config.InstructionProvenanceKey]
			instr := canonicalRegistryEffectiveInstruction{
				SourceLayer: string(instrLayer),
			}
			if cfg.Instruction != nil {
				instr.Enabled = cfg.Instruction.Enabled
				instr.Text = cfg.Instruction.Text
			}
			out.Kinds[kind] = canonicalRegistryEffectiveKind{
				Gaps:           gaps,
				Instruction:    instr,
				ResolverPlugin: cfg.ResolverPlugin,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode canonical_registry/effective response", "err", err)
		}
	}
}

// canonicalRegistryAvailableGap is a smaller wire shape than
// the effective variant — `available` lists kinds NOT in the
// active registry, so there's no source_layer to report (they
// haven't merged into anything).
type canonicalRegistryAvailableGap struct {
	Type         string   `json:"type,omitempty"`
	Description  string   `json:"description"`
	FillStrategy string   `json:"fill_strategy,omitempty"`
	Range        []int    `json:"range,omitempty"`
	MaxLength    int      `json:"max_length,omitempty"`
	Values       []string `json:"values,omitempty"`
}

type canonicalRegistryAvailableKind struct {
	Gaps map[string]canonicalRegistryAvailableGap `json:"gaps"`
}

type canonicalRegistryAvailableResponse struct {
	OK    bool                                      `json:"ok"`
	Kinds map[string]canonicalRegistryAvailableKind `json:"kinds"`
	// Names is the sorted kind-name list so MCP / CLI callers
	// have a stable ordered key set without re-sorting Kinds'
	// JSON map iteration order.
	Names []string `json:"names"`
}

// handleCanonicalRegistryAvailable implements
// GET /v1/canonical_registry/available. Returns every kind from
// `config.BuiltinKindGapsList` that's NOT in the active merged
// registry — operators inspect what they could opt into. Active
// kinds are excluded so callers don't confuse "available + not
// active" with "active + ride-default."
func handleCanonicalRegistryAvailable(
	logger *slog.Logger,
	active map[string]config.CanonicalKindConfig,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := canonicalRegistryAvailableResponse{
			OK:    true,
			Kinds: map[string]canonicalRegistryAvailableKind{},
			Names: []string{},
		}
		for _, kind := range config.BuiltinKindGapsList() {
			if _, isActive := active[kind]; isActive {
				continue
			}
			builtin := config.BuiltinKindGaps(kind)
			if len(builtin) == 0 {
				continue
			}
			gaps := make(map[string]canonicalRegistryAvailableGap, len(builtin))
			for field, spec := range builtin {
				gaps[field] = canonicalRegistryAvailableGap{
					Type:         spec.Type,
					Description:  spec.Description,
					FillStrategy: spec.FillStrategy,
					Range:        spec.Range,
					MaxLength:    spec.MaxLength,
					Values:       spec.Values,
				}
			}
			out.Kinds[kind] = canonicalRegistryAvailableKind{Gaps: gaps}
			out.Names = append(out.Names, kind)
		}
		sort.Strings(out.Names)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode canonical_registry/available response", "err", err)
		}
	}
}
