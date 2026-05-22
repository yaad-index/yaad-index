package config

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginConfigEnvName_Constant pins the uniform env-var name
// per ADR-0006 (2026-05-22 amendment / #192) — every plugin reads
// the same `YAAD_PLUGIN_CONFIG` name; per-subprocess env isolation
// keeps the value scoped to its target.
func TestPluginConfigEnvName_Constant(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "YAAD_PLUGIN_CONFIG", PluginConfigEnvName)
}

// TestMarshalPluginConfig_InjectsName pins the daemon-injected
// `_name` field — every payload carries the entry's `name:` so
// multi-instance plugins can read their instance identity
// without operator-side duplication.
func TestMarshalPluginConfig_InjectsName(t *testing.T) {
	t.Parallel()
	payload, err := MarshalPluginConfig("github-work", map[string]any{
		"repos":     []string{"acme/proj", "beta/widget"},
		"base_url":  "https://ghes.example.com/api/v3",
	})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	assert.Equal(t, "github-work", got["_name"], "daemon-injected `_name` carries the entry name")
	assert.Equal(t, "https://ghes.example.com/api/v3", got["base_url"])
	// List values round-trip cleanly through json.Marshal.
	repos, ok := got["repos"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"acme/proj", "beta/widget"}, repos)
}

// TestMarshalPluginConfig_EmptyOperatorConfig pins the "no
// operator yaml" path — the daemon still emits a non-empty JSON
// document carrying just `_name`. Plugins without a schema
// declaration ignore the payload; plugins with a schema may
// require `_name` so the field MUST be present.
func TestMarshalPluginConfig_EmptyOperatorConfig(t *testing.T) {
	t.Parallel()
	payload, err := MarshalPluginConfig("bgg", nil)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	require.Len(t, got, 1, "only the daemon-injected field survives an empty operator config")
	assert.Equal(t, "bgg", got["_name"])
}

// TestPluginConfigEnv_ReturnsSingleEnvEntry pins the
// subprocess-env shape — one `YAAD_PLUGIN_CONFIG=<json>` entry,
// ready for `exec.Cmd.Env`.
func TestPluginConfigEnv_ReturnsSingleEnvEntry(t *testing.T) {
	t.Parallel()
	out, err := PluginConfigEnv("bgg", map[string]any{"api_key": "abc-123"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.True(t, len(out[0]) > len("YAAD_PLUGIN_CONFIG="),
		"the env entry carries the JSON payload after the `=` separator")
	assert.Equal(t, "YAAD_PLUGIN_CONFIG=", out[0][:len("YAAD_PLUGIN_CONFIG=")])

	jsonPart := out[0][len("YAAD_PLUGIN_CONFIG="):]
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonPart), &decoded))
	assert.Equal(t, "abc-123", decoded["api_key"])
	assert.Equal(t, "bgg", decoded["_name"])
}

// TestValidatePluginConfig_AcceptsArbitraryYAML pins the v2
// scope: scalars, lists, nested maps all pass (the plugin owns
// its schema; daemon validates against `config_schema` separately).
func TestValidatePluginConfig_AcceptsArbitraryYAML(t *testing.T) {
	t.Parallel()
	err := validatePluginConfig("github", map[string]any{
		"token":       "abc",
		"repos":       []any{"acme/proj", "beta/widget"},
		"recent_days": 7,
		"nested": map[string]any{
			"inner_list": []any{1, 2, 3},
			"inner_map":  map[string]any{"k": "v"},
		},
	})
	require.NoError(t, err)
}

// TestValidatePluginConfig_RejectsUnderscorePrefix pins the
// reserved-namespace rule: operator keys starting with `_` are
// rejected so the daemon's `_name` injection (and any future
// daemon-injected fields) can't be shadowed by operator yaml.
func TestValidatePluginConfig_RejectsUnderscorePrefix(t *testing.T) {
	t.Parallel()
	err := validatePluginConfig("github", map[string]any{
		"_name": "trying-to-spoof",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "_name")
	assert.Contains(t, err.Error(), "daemon-injected")
}

// TestValidatePluginConfig_EmptyIsNoOp pins the nil/empty input
// contract: validation passes (the config: block is optional).
func TestValidatePluginConfig_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	assert.NoError(t, validatePluginConfig("bgg", nil))
	assert.NoError(t, validatePluginConfig("bgg", map[string]any{}))
}

// TestValidatePluginConfigAgainstSchema_EmptySchema pins the
// skip-validate path per #192 Q4: a plugin that declares no
// config_schema gets its operator yaml passed through unchanged
// without daemon-side validation.
func TestValidatePluginConfigAgainstSchema_EmptySchema(t *testing.T) {
	t.Parallel()
	err := ValidatePluginConfigAgainstSchema("bgg",
		map[string]any{"anything": "goes"}, nil)
	assert.NoError(t, err)
	err = ValidatePluginConfigAgainstSchema("bgg",
		map[string]any{"anything": "goes"}, []byte(""))
	assert.NoError(t, err)
	err = ValidatePluginConfigAgainstSchema("bgg",
		map[string]any{"anything": "goes"}, []byte("   \n\t"))
	assert.NoError(t, err)
}

// TestValidatePluginConfigAgainstSchema_HappyPath pins the
// validate-and-pass path: a config matching the plugin's schema
// returns nil.
func TestValidatePluginConfigAgainstSchema_HappyPath(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"repos": {"type": "array", "items": {"type": "string"}, "minItems": 1},
			"recent_days": {"type": "integer", "minimum": 1}
		},
		"required": ["repos"]
	}`)
	err := ValidatePluginConfigAgainstSchema("github",
		map[string]any{
			"repos":       []any{"acme/proj", "beta/widget"},
			"recent_days": 7,
		}, schema)
	require.NoError(t, err)
}

// TestValidatePluginConfigAgainstSchema_RejectsMissingRequired
// pins the operator-error path: a `required` field missing from
// the operator yaml surfaces as ErrConfigSchemaViolation.
func TestValidatePluginConfigAgainstSchema_RejectsMissingRequired(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
		"type": "object",
		"properties": {
			"repos": {"type": "array"}
		},
		"required": ["repos"]
	}`)
	err := ValidatePluginConfigAgainstSchema("github",
		map[string]any{"recent_days": 7}, schema)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConfigSchemaViolation),
		"missing-required is an operator-yaml error: %v", err)
}

// TestValidatePluginConfigAgainstSchema_RejectsWrongType pins
// type mismatches — `repos: "not-a-list"` against a schema
// requiring an array.
func TestValidatePluginConfigAgainstSchema_RejectsWrongType(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
		"type": "object",
		"properties": {
			"repos": {"type": "array"}
		}
	}`)
	err := ValidatePluginConfigAgainstSchema("github",
		map[string]any{"repos": "this should be a list"}, schema)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConfigSchemaViolation), "wrong-type is operator-error: %v", err)
}

// TestValidatePluginConfigAgainstSchema_MalformedSchema pins the
// plugin-author-bug path: a schema that doesn't parse surfaces
// as ErrConfigSchemaInvalid (operator can't fix this).
func TestValidatePluginConfigAgainstSchema_MalformedSchema(t *testing.T) {
	t.Parallel()
	err := ValidatePluginConfigAgainstSchema("github",
		map[string]any{}, []byte(`{not valid json`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConfigSchemaInvalid),
		"malformed schema is a plugin-author error: %v", err)
}

// TestValidatePluginConfigAgainstSchema_AccessesInjectedName pins
// that the schema validator sees the daemon-injected `_name` —
// schemas that require it pass without operator-side duplication.
func TestValidatePluginConfigAgainstSchema_AccessesInjectedName(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
		"type": "object",
		"properties": {
			"_name": {"type": "string", "pattern": "^github(-.+)?$"}
		},
		"required": ["_name"]
	}`)
	err := ValidatePluginConfigAgainstSchema("github-work",
		map[string]any{"repos": []any{"a/b"}}, schema)
	assert.NoError(t, err, "daemon-injected `_name` satisfies the required+pattern rule")

	// Wrong-pattern name (operator names the plugin `notgithub`)
	// fails the schema's `_name` pattern.
	err = ValidatePluginConfigAgainstSchema("notgithub",
		map[string]any{"repos": []any{"a/b"}}, schema)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConfigSchemaViolation))
}
