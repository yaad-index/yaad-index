package plugins

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGapSpec_UnmarshalJSON_StringShorthand pins the pre-ADR-0019
// plugin-side wire shape: a bare JSON string in the gaps map gets
// decoded as `{Type: "string", Description: <s>}`. Existing plugins
// emitting only string descriptions continue to parse unchanged.
func TestGapSpec_UnmarshalJSON_StringShorthand(t *testing.T) {
	t.Parallel()

	var got GapSpec
	require.NoError(t, json.Unmarshal([]byte(`"Short summary"`), &got))
	assert.Equal(t, "string", got.Type, "shorthand defaults to string")
	assert.Equal(t, "Short summary", got.Description)
	assert.Empty(t, got.FillStrategy, "shorthand omits FillStrategy (callers default to both)")
}

// TestGapSpec_UnmarshalJSON_TypedLongForm: ADR-0019 step 3 typed
// shape with all the new fields.
func TestGapSpec_UnmarshalJSON_TypedLongForm(t *testing.T) {
	t.Parallel()

	body := `{
		"prompt": "How do you rate this?",
		"type": "int",
		"range": [1, 10],
		"fill_strategy": "operator"
	}`
	var got GapSpec
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, "int", got.Type)
	assert.Equal(t, "How do you rate this?", got.Description, "prompt aliases to Description")
	assert.Equal(t, []int{1, 10}, got.Range)
	assert.Equal(t, "operator", got.FillStrategy)
}

// TestGapSpec_UnmarshalJSON_DescriptionWinsOverPrompt: when both
// `description` and `prompt` are set, `description` wins (matches
// the YAML side semantic). Plugins shouldn't set both, but if they
// do, the resolution is deterministic.
func TestGapSpec_UnmarshalJSON_DescriptionWinsOverPrompt(t *testing.T) {
	t.Parallel()

	body := `{
		"description": "from description",
		"prompt": "from prompt",
		"type": "string"
	}`
	var got GapSpec
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, "from description", got.Description)
}

// TestGapSpec_UnmarshalJSON_TypeDefaultsToString: a long-form gap
// without `type` defaults to "string" so the validation path treats
// it consistently with the shorthand.
func TestGapSpec_UnmarshalJSON_TypeDefaultsToString(t *testing.T) {
	t.Parallel()
	var got GapSpec
	require.NoError(t, json.Unmarshal([]byte(`{"description": "x"}`), &got))
	assert.Equal(t, "string", got.Type)
}

// TestGapSpec_Validate_AcceptsValidShapes covers the well-known
// types each accepting their type-appropriate extra fields.
func TestGapSpec_Validate_AcceptsValidShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec GapSpec
	}{
		{"int with range", GapSpec{Type: "int", Description: "rating", Range: []int{1, 10}, FillStrategy: "operator"}},
		{"int without range", GapSpec{Type: "int", Description: "count"}},
		{"string with max_length", GapSpec{Type: "string", Description: "name", MaxLength: 80}},
		{"string without max_length", GapSpec{Type: "string", Description: "name"}},
		{"bool", GapSpec{Type: "bool", Description: "owned", FillStrategy: "operator"}},
		{"text", GapSpec{Type: "text", Description: "notes"}},
		{"enum", GapSpec{Type: "enum", Description: "region", Values: []string{"us", "eu"}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.NoError(t, tc.spec.Validate("test.gap"))
		})
	}
}

// TestGapSpec_Validate_RejectsBadShapes mirrors the operator-config
// rejection table from.
func TestGapSpec_Validate_RejectsBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec GapSpec
		errHint string
	}{
		{"bad fill_strategy", GapSpec{Type: "string", Description: "x", FillStrategy: "humans"}, "fill_strategy"},
		{"int range single element", GapSpec{Type: "int", Description: "x", Range: []int{5}}, "must be [min, max]"},
		{"int range min > max", GapSpec{Type: "int", Description: "x", Range: []int{10, 1}}, "min 10 > max 1"},
		{"int with values", GapSpec{Type: "int", Description: "x", Values: []string{"a"}}, "type=int does not accept"},
		{"string with range", GapSpec{Type: "string", Description: "x", Range: []int{1, 10}}, "type=string does not accept range"},
		{"bool with max_length", GapSpec{Type: "bool", Description: "x", MaxLength: 4}, "type=bool"},
		{"enum without values", GapSpec{Type: "enum", Description: "x"}, "type=enum requires non-empty values"},
		{"enum empty values entry", GapSpec{Type: "enum", Description: "x", Values: []string{"a", ""}}, "empty string not allowed"},
		{"enum duplicate values", GapSpec{Type: "enum", Description: "x", Values: []string{"a", "a"}}, "duplicate"},
		{"enum with range", GapSpec{Type: "enum", Description: "x", Values: []string{"a"}, Range: []int{1, 10}}, "type=enum does not accept"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.Validate("test.gap")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errHint)
		})
	}
}

// TestGapSpec_Validate_LegacyTypePassesThrough: pre-ADR-0019 plugin
// types like "date" pass through validation unchanged so existing
// plugins keep loading. The closed-enum extra-field validation
// only fires on the ADR-0019 well-known types.
func TestGapSpec_Validate_LegacyTypePassesThrough(t *testing.T) {
	t.Parallel()
	spec := GapSpec{Type: "date", Description: "Birth date."}
	assert.NoError(t, spec.Validate("test.gap"))
}

// TestCapabilities_GapsRoundTripJSON exercises the full path: JSON
// in → Capabilities struct → gaps map populated with both shapes
// alongside each other on the same kind. Mirrors what subprocess
// runInit does on plugin --init output.
func TestCapabilities_GapsRoundTripJSON(t *testing.T) {
	t.Parallel()
	body := `{
		"name": "fixture-plugin",
		"version": "0.1.0",
		"url_patterns": ["https://example.test/*"],
		"entity_kinds": [],
		"edge_kinds": [],
		"canonical_kinds_extras": {
			"boardgame": {
				"gaps": {
					"summary": "Short summary",
					"rating": {"prompt": "How do you rate this?", "type": "int", "range": [1, 10], "fill_strategy": "operator"},
					"played": {"type": "bool", "description": "Have you played?", "fill_strategy": "operator"}
				}
			}
		}
	}`
	var caps Capabilities
	require.NoError(t, json.Unmarshal([]byte(body), &caps))
	gaps := caps.CanonicalKindsExtras["boardgame"].Gaps
	require.Len(t, gaps, 3)

	assert.Equal(t, "string", gaps["summary"].Type)
	assert.Equal(t, "Short summary", gaps["summary"].Description)

	assert.Equal(t, "int", gaps["rating"].Type)
	assert.Equal(t, "How do you rate this?", gaps["rating"].Description)
	assert.Equal(t, []int{1, 10}, gaps["rating"].Range)
	assert.Equal(t, "operator", gaps["rating"].FillStrategy)

	assert.Equal(t, "bool", gaps["played"].Type)
	assert.Equal(t, "Have you played?", gaps["played"].Description)
}
