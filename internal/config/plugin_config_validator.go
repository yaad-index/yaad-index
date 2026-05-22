package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrConfigSchemaInvalid surfaces when a plugin's declared
// JSON Schema can't be compiled (the plugin emitted a malformed
// schema document). Daemon-startup fails fast — the operator
// can't fix this; it's a plugin-author bug.
var ErrConfigSchemaInvalid = errors.New("plugin config_schema is not a valid JSON Schema")

// ErrConfigSchemaViolation surfaces when the operator's
// `plugins[N].config:` block fails to satisfy the plugin's
// declared schema. Daemon-startup fails fast — the operator
// needs to fix the yaml.
var ErrConfigSchemaViolation = errors.New("plugin config does not satisfy the plugin's config_schema")

// ValidatePluginConfigAgainstSchema compiles the plugin's declared
// JSON Schema (the raw bytes from its --init capabilities) and
// validates the operator's `config:` map against it. Wires the
// `_name` daemon-injected field into the validation payload so
// schemas that require it pass.
//
// Empty / nil schemaJSON → skip-validate per #192 Q4 (plugins
// without a declared schema still get YAAD_PLUGIN_CONFIG; they
// can choose to read or ignore it).
//
// On schema compile failure: returns a wrapped
// ErrConfigSchemaInvalid (plugin-author bug, operator can't fix).
// On schema violation: returns a wrapped
// ErrConfigSchemaViolation (operator yaml needs fixing).
func ValidatePluginConfigAgainstSchema(pluginName string, cfg map[string]any, schemaJSON []byte) error {
	if len(bytes.TrimSpace(schemaJSON)) == 0 {
		return nil
	}

	compiler := jsonschema.NewCompiler()
	resourceURL := "yaad-index://plugin/" + pluginName + "/config_schema.json"
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConfigSchemaInvalid, pluginName, err)
	}
	if err := compiler.AddResource(resourceURL, schemaDoc); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConfigSchemaInvalid, pluginName, err)
	}
	schema, err := compiler.Compile(resourceURL)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConfigSchemaInvalid, pluginName, err)
	}

	// Build the validation payload with the daemon-injected
	// `_name` field. Plugins that declare `_name` as a required
	// field in their schema get it for free; plugins that don't
	// just ignore it.
	payload := make(map[string]any, len(cfg)+1)
	for k, v := range cfg {
		payload[k] = v
	}
	payload[DaemonInjectedNameKey] = pluginName

	// jsonschema/v6 validates against the same loose any-shape
	// the JSON-decoder produces; we round-trip through JSON to
	// normalize the operator-side yaml.v3 quirks (e.g. yaml
	// integers decode as `int`, JSON expects float64).
	normalized, err := normalizeViaJSON(payload)
	if err != nil {
		return fmt.Errorf("%w: %s: normalize payload: %v", ErrConfigSchemaInvalid, pluginName, err)
	}
	if err := schema.Validate(normalized); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConfigSchemaViolation, pluginName, err)
	}
	return nil
}

// normalizeViaJSON round-trips a value through json.Marshal +
// json.Unmarshal so yaml-decoded int / int64 surface as the
// float64 JSON Schema expects. Cheap; called once per plugin
// at daemon startup.
func normalizeViaJSON(v any) (any, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, err
	}
	return out, nil
}
