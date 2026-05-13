package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// pluginConfigKey is the shape rule for keys under a plugin's
// `config:` sub-block per yaad-index #7. Lowercase ASCII + digits +
// underscore; must start with a letter. Matches the yaml-key
// convention used elsewhere in the config (kindOrFieldName has the
// same shape but for canonical-kind names).
var pluginConfigKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validatePluginConfig enforces the v1 scope of the per-plugin
// `config:` sub-block (yaad-index #7):
//
//   - Keys match pluginConfigKey shape (lowercase snake_case).
//   - Values are scalar — string, bool, int (Go's yaml decoder
//     surfaces YAML ints as `int`), float64, or nil. Nested maps
//     and lists are rejected; the v1 scope is flat key→scalar
//     conversion to env vars.
//   - Duplicate-after-prefix-collapse keys (e.g. operator writes
//     both `bgg_api_key` and `api_key` on the bgg plugin) are
//     rejected with a clear message naming both source keys —
//     otherwise the env-var conversion would silently lose one.
//
// Returns nil for an empty / nil config map (the omitempty path).
func validatePluginConfig(pluginName string, cfg map[string]any) error {
	if len(cfg) == 0 {
		return nil
	}
	collapsed := make(map[string]string, len(cfg))
	for k, v := range cfg {
		if !pluginConfigKey.MatchString(k) {
			return fmt.Errorf("plugin %q: config key %q must be lowercase snake_case ASCII (regex %s)",
				pluginName, k, pluginConfigKey.String())
		}
		switch v.(type) {
		case string, bool, int, int64, float64, nil:
			// scalar — accepted
		default:
			return fmt.Errorf("plugin %q: config key %q has non-scalar value (got %T); v1 supports string/bool/int/float only — nested config defers to <PLUGIN>_CONFIG_JSON",
				pluginName, k, v)
		}
		envName := PluginConfigEnvName(pluginName, k)
		if prev, dup := collapsed[envName]; dup {
			return fmt.Errorf("plugin %q: config keys %q and %q both produce env var %s — drop one (or rename) so the env var has a single source",
				pluginName, prev, k, envName)
		}
		collapsed[envName] = k
	}
	return nil
}

// PluginConfigEnvName returns the env var name a given (plugin, key)
// pair maps to under the yaad-index #7 convention:
//
//	<PLUGIN_NAME_UPPER>_<KEY_UPPER>
//
// With a prefix-strip pass: when the key already starts with the
// plugin-name + `_`, the redundant prefix is dropped before
// upper-casing. So `bgg.config.bgg_api_key` → `BGG_API_KEY` (not
// the awkward `BGG_BGG_API_KEY`). Operators get a clean env-var
// name regardless of whether they namespace their config keys.
//
// Returns "" for an empty plugin or key — the caller treats that
// as a config-shape bug surfaced separately by validatePluginConfig.
func PluginConfigEnvName(pluginName, key string) string {
	if pluginName == "" || key == "" {
		return ""
	}
	pn := strings.ToLower(pluginName)
	k := key
	if pn != "" && strings.HasPrefix(k, pn+"_") {
		k = k[len(pn)+1:]
	}
	if k == "" {
		// Operator wrote literally `bgg_` as a key — fall back to
		// the un-stripped form so the env-var conversion stays
		// well-defined.
		k = key
	}
	return strings.ToUpper(pn) + "_" + strings.ToUpper(k)
}

// PluginConfigEnvVars converts a per-plugin scalar config map to a
// sorted slice of `KEY=VALUE` env-var strings ready for
// `exec.Cmd.Env`. Sorted by env-var name for deterministic spawn
// ordering (helps tests + log greps).
//
// Scalar conversion:
//   - string → as-is
//   - int / int64 / float64 → fmt.Sprint
//   - bool → "true" / "false"
//   - nil → empty string (the env var is set but valueless; matches
//     `KEY=` shell semantics for "set but empty")
//
// Caller is responsible for calling validatePluginConfig at Load
// time; this function trusts the map shape.
func PluginConfigEnvVars(pluginName string, cfg map[string]any) []string {
	if len(cfg) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg))
	for k, v := range cfg {
		envName := PluginConfigEnvName(pluginName, k)
		if envName == "" {
			continue
		}
		var val string
		switch t := v.(type) {
		case string:
			val = t
		case bool:
			if t {
				val = "true"
			} else {
				val = "false"
			}
		case nil:
			val = ""
		default:
			val = fmt.Sprint(t)
		}
		out = append(out, envName+"="+val)
	}
	sort.Strings(out)
	return out
}
