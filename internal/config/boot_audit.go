// Boot-time canonical-registry audit logging per #48 slice 4.
// Surfaces what's "interesting" in the merged 4+1-layer
// registry — kinds where a plugin or operator override is
// shaping the final spec — without spamming the log with
// every code-default kind on a vanilla install.

package config

import (
	"log/slog"
	"sort"
)

// LogCanonicalRegistryBootAudit emits structured INFO log lines
// at daemon boot summarizing kinds whose merge is non-trivial,
// and a single DEBUG log line dumping the full per-(kind, field)
// source_layer trail for operator deep-inspection.
//
// A kind is "interesting" (worth INFO) when at least one of:
//
//   - a gap field's source_layer is plugin_extras / operator_defaults
//     / operator (a non-builtin layer contributed to the merge), OR
//   - the instruction's source_layer is operator_defaults or operator.
//
// Kinds where every gap is code_defaults or builtin_kind only —
// the "pure starter" case — stay silent at INFO. The signal-only
// gate keeps a vanilla install's startup log quiet while still
// surfacing every plugin-activated or operator-overridden kind.
//
// Wire shape of INFO line (one per non-trivial kind):
//
//	level=INFO msg="canonical-kind merge" kind=boardgame
//	    plugin_extras=2 operator_defaults=0 operator=3
//	    instruction_layer=operator
//
// DEBUG line dumps the full provenance map as a structured
// attribute so operators piping through jq can introspect each
// (kind, field) → layer tuple.
func LogCanonicalRegistryBootAudit(logger *slog.Logger, reg map[string]CanonicalKindConfig, prov RegistryProvenance) {
	if logger == nil {
		return
	}

	interesting := collectInterestingKinds(reg, prov)
	sort.Strings(interesting)
	for _, kind := range interesting {
		gapProv := prov[kind]
		var pluginCount, opDefaultsCount, opCount int
		for field, layer := range gapProv {
			if field == InstructionProvenanceKey {
				continue
			}
			switch layer {
			case LayerPluginExtras:
				pluginCount++
			case LayerOperatorDefaults:
				opDefaultsCount++
			case LayerOperatorPerKind:
				opCount++
			}
		}
		instrLayer := gapProv[InstructionProvenanceKey]
		logger.Info("canonical-kind merge",
			"kind", kind,
			"plugin_extras", pluginCount,
			"operator_defaults", opDefaultsCount,
			"operator", opCount,
			"instruction_layer", string(instrLayer),
		)
	}

	logger.Debug("canonical-kind merge audit (full registry)",
		"kinds_total", len(reg),
		"kinds_with_overrides", len(interesting),
		"provenance", flattenProvenance(prov),
	)
}

// collectInterestingKinds returns the set of kind names whose
// merge produced at least one non-code/non-builtin source_layer
// for any gap or the instruction.
func collectInterestingKinds(reg map[string]CanonicalKindConfig, prov RegistryProvenance) []string {
	out := make([]string, 0, len(reg))
	for kind := range reg {
		gapProv := prov[kind]
		if gapProv == nil {
			continue
		}
		if isInterestingMerge(gapProv) {
			out = append(out, kind)
		}
	}
	return out
}

// isInterestingMerge reports whether at least one gap field or
// the instruction sits at a layer above the daemon-shipped
// pool (plugin_extras / operator_defaults / operator).
func isInterestingMerge(gapProv map[string]LayerProvenance) bool {
	for _, layer := range gapProv {
		switch layer {
		case LayerPluginExtras, LayerOperatorDefaults, LayerOperatorPerKind:
			return true
		}
	}
	return false
}

// flattenProvenance projects the nested RegistryProvenance map
// to a sorted slice of `kind.field=layer` strings — stable
// structured-log output for DEBUG dumps. Lex-sorted by (kind,
// field) so diffs across runs are clean.
func flattenProvenance(prov RegistryProvenance) []string {
	keys := make([]string, 0, len(prov))
	for kind := range prov {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	out := make([]string, 0)
	for _, kind := range keys {
		fields := make([]string, 0, len(prov[kind]))
		for field := range prov[kind] {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			out = append(out, kind+"."+field+"="+string(prov[kind][field]))
		}
	}
	return out
}
