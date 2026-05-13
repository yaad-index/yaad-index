package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginConfigEnvName_BasicCases pins the env-var naming
// convention per yaad-index #7: <PLUGIN_UPPER>_<KEY_UPPER>, with
// prefix-strip when the key already starts with the plugin name.
func TestPluginConfigEnvName_BasicCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, plugin, key, want string
	}{
		{"clean key", "bgg", "api_key", "BGG_API_KEY"},
		{"key starts with plugin name — strip prefix", "bgg", "bgg_api_key", "BGG_API_KEY"},
		{"key starts with plugin name — multi-word", "bgg", "bgg_timeout_seconds", "BGG_TIMEOUT_SECONDS"},
		{"key NOT starting with plugin name — kept verbatim", "bgg", "timeout_seconds", "BGG_TIMEOUT_SECONDS"},
		{"plugin name with mixed case — normalized", "BGG", "api_key", "BGG_API_KEY"},
		{"key with embedded plugin name — NOT prefix, kept", "bgg", "foo_bgg_bar", "BGG_FOO_BGG_BAR"},
		{"single-char key", "bgg", "x", "BGG_X"},
		{"plugin name only as key — falls back", "bgg", "bgg_", "BGG_BGG_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, PluginConfigEnvName(tc.plugin, tc.key))
		})
	}
}

// TestPluginConfigEnvName_EmptyInputs pins the defensive fallback:
// empty plugin or key returns "" so callers can detect a config-
// shape bug separately.
func TestPluginConfigEnvName_EmptyInputs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", PluginConfigEnvName("", "api_key"))
	assert.Equal(t, "", PluginConfigEnvName("bgg", ""))
	assert.Equal(t, "", PluginConfigEnvName("", ""))
}

// TestPluginConfigEnvVars_ScalarConversion pins the scalar-to-env
// conversion: string/int/bool/float each format to the right
// shell-env string.
func TestPluginConfigEnvVars_ScalarConversion(t *testing.T) {
	t.Parallel()

	out := PluginConfigEnvVars("bgg", map[string]any{
		"api_key":         "abc-123",
		"timeout_seconds": 30,
		"verbose":         true,
		"backoff_factor":  1.5,
		"empty_marker":    nil,
	})

	// Sorted by env-var name so the order is deterministic.
	assert.Equal(t, []string{
		"BGG_API_KEY=abc-123",
		"BGG_BACKOFF_FACTOR=1.5",
		"BGG_EMPTY_MARKER=",
		"BGG_TIMEOUT_SECONDS=30",
		"BGG_VERBOSE=true",
	}, out)
}

// TestPluginConfigEnvVars_EmptyMap pins the empty-input contract:
// nil/empty map → nil result. Callers can append unconditionally.
func TestPluginConfigEnvVars_EmptyMap(t *testing.T) {
	t.Parallel()

	assert.Nil(t, PluginConfigEnvVars("bgg", nil))
	assert.Nil(t, PluginConfigEnvVars("bgg", map[string]any{}))
}

// TestPluginConfigEnvVars_BoolFalse pins the bool false → "false"
// shape — distinct from the nil → "" case.
func TestPluginConfigEnvVars_BoolFalse(t *testing.T) {
	t.Parallel()

	out := PluginConfigEnvVars("bgg", map[string]any{"verbose": false})
	assert.Equal(t, []string{"BGG_VERBOSE=false"}, out)
}

// TestValidatePluginConfig_AcceptsScalars pins the v1 allowed
// types: string, bool, int, float, nil. All pass.
func TestValidatePluginConfig_AcceptsScalars(t *testing.T) {
	t.Parallel()

	err := validatePluginConfig("bgg", map[string]any{
		"api_key":         "x",
		"timeout_seconds": 30,
		"verbose":         true,
		"backoff_factor":  1.5,
		"unset":           nil,
	})
	require.NoError(t, err)
}

// TestValidatePluginConfig_RejectsNestedMap pins the v1 scope:
// nested map values are rejected (defer to <PLUGIN>_CONFIG_JSON
// when first plugin needs nesting).
func TestValidatePluginConfig_RejectsNestedMap(t *testing.T) {
	t.Parallel()

	err := validatePluginConfig("bgg", map[string]any{
		"nested": map[string]any{"inner": "value"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-scalar")
}

// TestValidatePluginConfig_RejectsList pins the v1 scope: lists
// rejected for the same reason as nested maps.
func TestValidatePluginConfig_RejectsList(t *testing.T) {
	t.Parallel()

	err := validatePluginConfig("bgg", map[string]any{
		"items": []any{"a", "b"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-scalar")
}

// TestValidatePluginConfig_RejectsInvalidKey pins the key-shape
// rule: keys must be lowercase ASCII snake_case. Uppercase,
// hyphens, dots all reject.
func TestValidatePluginConfig_RejectsInvalidKey(t *testing.T) {
	t.Parallel()

	cases := []string{
		"BadKey",        // uppercase
		"with-hyphen",   // hyphen
		"with.dot",      // dot
		"1_starts_num",  // leading digit
		"",              // empty
	}
	for _, k := range cases {
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			err := validatePluginConfig("bgg", map[string]any{k: "x"})
			require.Error(t, err, "key %q must reject", k)
		})
	}
}

// TestValidatePluginConfig_RejectsPrefixCollision pins the
// duplicate-after-collapse rule: `bgg_api_key` and `api_key` both
// collapse to BGG_API_KEY → operator must pick one.
func TestValidatePluginConfig_RejectsPrefixCollision(t *testing.T) {
	t.Parallel()

	err := validatePluginConfig("bgg", map[string]any{
		"bgg_api_key": "from-prefixed",
		"api_key":     "from-bare",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BGG_API_KEY",
		"error must name the colliding env var")
}

// TestValidatePluginConfig_EmptyIsNoOp pins the nil/empty input
// contract: validation passes (the config: block is optional).
func TestValidatePluginConfig_EmptyIsNoOp(t *testing.T) {
	t.Parallel()

	assert.NoError(t, validatePluginConfig("bgg", nil))
	assert.NoError(t, validatePluginConfig("bgg", map[string]any{}))
}
