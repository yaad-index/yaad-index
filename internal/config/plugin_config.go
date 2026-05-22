package config

import (
	"encoding/json"
	"fmt"
)

// PluginConfigEnvName is the env-var name daemon-side code uses to
// deliver the JSON-marshalled `config:` block to subprocess
// plugins per ADR-0006 (2026-05-22 amendment / #192). Every
// plugin reads the same name; per-subprocess env isolation
// keeps the value scoped to its target.
const PluginConfigEnvName = "YAAD_PLUGIN_CONFIG"

// DaemonInjectedNameKey is the reserved key the daemon writes
// into every plugin's JSON config payload, carrying the operator
// yaml's `plugins[N].name:` value. Plugins read it to know their
// instance identity (e.g. multi-instance yaad-github reads
// `_name` to decide between `github-personal` / `github-work`
// scoped URL patterns + envelope notations).
//
// The `_`-prefix marks it as a daemon-injected field per
// ADR-0006's "Daemon-injected fields" convention — operators
// cannot supply `_`-prefixed keys themselves; the daemon rejects
// any operator-supplied key starting with `_` at Load time.
const DaemonInjectedNameKey = "_name"

// validatePluginConfig enforces the v2 shape of the per-plugin
// `config:` sub-block per ADR-0006 (2026-05-22 amendment / #192):
//
//   - Arbitrary YAML structure is allowed (scalars, lists, nested
//     maps). The plugin owns its schema; the daemon validates
//     operator input against the plugin's declared JSON Schema
//     at registry-load time, not here.
//   - Operator keys MUST NOT start with `_` — that prefix is
//     reserved for daemon-injected fields (e.g. `_name`).
//
// The pre-#192 scalar-only constraint + per-key prefix-strip
// env-var conversion is REMOVED — plugins now read the whole
// config as a single JSON document from PluginConfigEnvName per
// ADR-0006's amendment.
//
// Returns nil for an empty / nil config map (the omitempty path).
func validatePluginConfig(pluginName string, cfg map[string]any) error {
	if len(cfg) == 0 {
		return nil
	}
	for k := range cfg {
		if len(k) > 0 && k[0] == '_' {
			return fmt.Errorf("plugin %q: config key %q starts with `_` (reserved for daemon-injected fields per ADR-0006); rename it",
				pluginName, k)
		}
	}
	return nil
}

// MarshalPluginConfig returns the JSON document the daemon
// delivers via PluginConfigEnvName for a given plugin. The
// daemon-injected `_name` field is set to pluginName so plugins
// can read their instance identity without needing a parallel
// env var or operator-supplied duplicate.
//
// Empty / nil operator config still produces a non-empty JSON
// document carrying `{"_name": "..."}` — plugins that declare
// no schema can ignore the payload, but the `_name` field is
// always present.
//
// Caller is responsible for calling validatePluginConfig at
// Load time; this function trusts the input shape (modulo the
// `_`-prefix reservation, which Load enforces).
func MarshalPluginConfig(pluginName string, cfg map[string]any) ([]byte, error) {
	out := make(map[string]any, len(cfg)+1)
	for k, v := range cfg {
		out[k] = v
	}
	out[DaemonInjectedNameKey] = pluginName
	return json.Marshal(out)
}

// PluginConfigEnv returns the single `KEY=VALUE` env-var string
// ready for `exec.Cmd.Env` — the JSON-marshalled `config:`
// block delivered via PluginConfigEnvName. The daemon-side
// caller threads this through subprocess.WithConfigEnv.
//
// Returns an error only when the operator config can't be
// JSON-encoded (e.g. nested types that don't marshal cleanly);
// happy path returns a single-element slice the caller appends
// to the subprocess env.
func PluginConfigEnv(pluginName string, cfg map[string]any) ([]string, error) {
	payload, err := MarshalPluginConfig(pluginName, cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal plugin %q config: %w", pluginName, err)
	}
	return []string{PluginConfigEnvName + "=" + string(payload)}, nil
}
