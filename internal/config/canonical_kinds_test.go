package config

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultGaps_Stable pins the built-in gap-set every canonical
// kind has by default per ADR-0016 §1. The three are present, the
// types are right, the result is a fresh copy each call (so
// callers can mutate without aliasing).
func TestDefaultGaps_Stable(t *testing.T) {
	t.Parallel()

	g1 := DefaultGaps()
	require.Contains(t, g1, "name", "name gap is built-in")
	require.Contains(t, g1, "tags", "tags gap is built-in")
	require.Contains(t, g1, "summary", "summary gap is built-in")
	assert.Equal(t, "string", g1["name"].Type)
	assert.Equal(t, "[]string", g1["tags"].Type)
	assert.Equal(t, "string", g1["summary"].Type)

	// Mutation isolation: caller mutating the returned map must
	// not affect the next call's defaults.
	g1["name"] = GapSpec{Type: "string", Description: "MUTATED"}
	g2 := DefaultGaps()
	assert.Equal(t, "The name of the entity.", g2["name"].Description,
		"DefaultGaps must return a fresh copy on each call")
}

// TestMergeCanonicalRegistry_FourLayerOrder pins ADR-0016 §4's
// four-layer merge precedence: each layer can add or override
// the previous, with later layers winning. Test wires all four
// layers contributing to the same gap field name and asserts the
// final winner.
func TestMergeCanonicalRegistry_FourLayerOrder(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Layer 2: plugin extras for kind=person.
	pluginGaps := map[string]map[string]GapSpec{
		"person": {
			"summary": {Type: "string", Description: "PLUGIN summary"},
			"birthdate": {Type: "date", Description: "Birth date"},
		},
	}
	// Layer 3: operator-defaults (root, applies to every kind).
	opDefaults := CanonicalKindConfig{
		Gaps: map[string]GapSpec{
			"summary": {Type: "string", Description: "ROOT summary"},
			"external_url": {Type: "string", Description: "Authoritative URL"},
		},
	}
	// Layer 4: operator per-kind (only for person).
	opPerKind := map[string]CanonicalKindConfig{
		"person": {
			Gaps: map[string]GapSpec{
				"summary": {Type: "string", Description: "PER_KIND summary"},
			},
		},
	}

	merged := MergeCanonicalRegistry(
		pluginGaps, []string{"person"}, opDefaults, opPerKind, logger)

	require.Contains(t, merged, "person")
	person := merged["person"]

	// Layer 4 wins over layer 3 wins over layer 2 wins over layer 1
	// for the same field.
	assert.Equal(t, "PER_KIND summary", person.Gaps["summary"].Description,
		"per-kind operator (layer 4) wins over root + plugin")

	// Layer 1 (code default) survives when no layer overrides it.
	assert.Equal(t, "The name of the entity.", person.Gaps["name"].Description,
		"code-default name gap surfaces unchanged")
	assert.Equal(t, "Relevant tags for this entity.", person.Gaps["tags"].Description)

	// Layer 2 plugin gap that no later layer touches.
	assert.Equal(t, "Birth date", person.Gaps["birthdate"].Description)
	assert.Equal(t, "date", person.Gaps["birthdate"].Type, "typed gap propagates Type")

	// Layer 3 root-defaults adds a gap to every kind.
	assert.Equal(t, "Authoritative URL", person.Gaps["external_url"].Description)
}

// TestMergeCanonicalRegistry_PluginAutoActivation pins ADR-0016
// §2's plugin-auto-activation contract: kinds named in
// canonical_kinds_emitted appear in the merged registry without
// any operator config, with code defaults as the only Layer
// covered.
func TestMergeCanonicalRegistry_PluginAutoActivation(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	merged := MergeCanonicalRegistry(
		nil,
		[]string{"boardgame", "person"}, // plugins emit these
		CanonicalKindConfig{}, // no operator-defaults
		nil, // no operator-per-kind
		logger,
	)

	require.Contains(t, merged, "boardgame", "plugin-emitted kind auto-activates")
	require.Contains(t, merged, "person", "plugin-emitted kind auto-activates")

	// Universal code defaults appear on every auto-activated kind.
	for _, kind := range []string{"boardgame", "person"} {
		k := merged[kind]
		assert.Contains(t, k.Gaps, "name")
		assert.Contains(t, k.Gaps, "tags")
		assert.Contains(t, k.Gaps, "summary")
		require.NotNil(t, k.Instruction, "merged Instruction is always non-nil")
		assert.False(t, k.Instruction.Enabled,
			"code-default instruction.enabled is false (operator opts in)")
		assert.Empty(t, k.Instruction.Text)
	}

	// person: only the 3 universal defaults (no kind-specific
	// built-ins).
	assert.Len(t, merged["person"].Gaps, 3,
		"person has only the 3 universal code-default gaps")

	// boardgame: 3 universal defaults + 5 ADR-0019 step 4 built-ins
	// (rating/owned/want/played/knows_how_to_play). All five carry
	// fill_strategy=operator.
	bg := merged["boardgame"]
	assert.Len(t, bg.Gaps, 8, "boardgame: 3 universal + 5 ADR-0019 operator-strategy built-ins")
	for _, field := range []string{"rating", "owned", "want", "played", "knows_how_to_play"} {
		require.Contains(t, bg.Gaps, field, "boardgame built-in %s missing", field)
		assert.Equal(t, "operator", bg.Gaps[field].FillStrategy,
			"boardgame.%s.fill_strategy", field)
	}
	assert.Equal(t, "int", bg.Gaps["rating"].Type)
	assert.Equal(t, []int{1, 10}, bg.Gaps["rating"].Range)
	assert.Equal(t, "bool", bg.Gaps["owned"].Type)
	assert.Equal(t, "bool", bg.Gaps["played"].Type)
}

// TestMergeCanonicalRegistry_InstructionTwoLayer pins ADR-0016 §4
// last paragraph: instruction merges only at the operator-defaults
// + per-kind layers (NOT plugins, NOT code beyond the empty
// default). Per-kind operator wins over root operator; if neither
// is set, code default applies.
func TestMergeCanonicalRegistry_InstructionTwoLayer(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	rootInstr := &InstructionSpec{Enabled: true, Text: "ROOT"}
	perKindInstr := &InstructionSpec{Enabled: true, Text: "PER_KIND"}

	merged := MergeCanonicalRegistry(
		nil,
		[]string{"person", "boardgame", "city"},
		CanonicalKindConfig{Instruction: rootInstr},
		map[string]CanonicalKindConfig{
			"person": {Instruction: perKindInstr},
			// boardgame: no per-kind override → root applies.
			// city: omitted from opPerKind entirely → root applies.
		},
		logger,
	)

	assert.Equal(t, "PER_KIND", merged["person"].Instruction.Text,
		"per-kind operator wins over root for kinds it covers")
	assert.True(t, merged["person"].Instruction.Enabled)

	assert.Equal(t, "ROOT", merged["boardgame"].Instruction.Text,
		"root operator-defaults applies when per-kind doesn't set instruction")
	assert.Equal(t, "ROOT", merged["city"].Instruction.Text,
		"root operator-defaults applies to kinds with no per-kind block at all")
}

// TestMergeCanonicalRegistry_OperatorPerKindActivatesNonPluginKind
// covers the path where an operator pre-declares a canonical kind
// that no active plugin emits. The kind appears in the merged
// registry with code defaults + operator overrides (no plugin
// extras for it).
func TestMergeCanonicalRegistry_OperatorPerKindActivatesNonPluginKind(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	merged := MergeCanonicalRegistry(
		nil,
		nil, // no plugins emit anything
		CanonicalKindConfig{},
		map[string]CanonicalKindConfig{
			"future_kind": {
				Gaps: map[string]GapSpec{"foo": {Type: "string", Description: "Foo."}},
			},
		},
		logger,
	)

	require.Contains(t, merged, "future_kind",
		"operator-only kind activates without a plugin emitting it")
	k := merged["future_kind"]
	assert.Equal(t, "Foo.", k.Gaps["foo"].Description)
	// Code defaults still surface.
	assert.Equal(t, "string", k.Gaps["name"].Type)
	assert.Equal(t, "[]string", k.Gaps["tags"].Type)
}

// TestGapSpec_UnmarshalYAML pins ADR-0016 §7's two-shape decode:
// shorthand string and long-form struct. Both produce equivalent
// GapSpec values.
func TestGapSpec_UnmarshalYAML(t *testing.T) {
	t.Parallel()
	// Test the unmarshalers are exercised through the Config Load
	// path; the canonical_kinds_test paths in config_test.go cover
	// shorthand. Here we add a long-form test for typed fields.
	body := []byte(`
plugins: []
canonical_kinds:
 person:
 gaps:
 birthdate:
 type: date
 description: "Birth date in YYYY-MM-DD."
 occupation: "Job title or role."
`)
	cfg, err := Load(writeConfig(t, string(body)))
	require.NoError(t, err)
	person := cfg.CanonicalKinds["person"]

	// Long form preserves Type.
	assert.Equal(t, "date", person.Gaps["birthdate"].Type)
	assert.Equal(t, "Birth date in YYYY-MM-DD.", person.Gaps["birthdate"].Description)

	// Shorthand defaults Type to string.
	assert.Equal(t, "string", person.Gaps["occupation"].Type)
	assert.Equal(t, "Job title or role.", person.Gaps["occupation"].Description)
}

// TestInstructionSpec_UnmarshalYAML pins the two-shape decode for
// instruction. Shorthand string → Enabled=true with Text=string.
// Long form decodes both fields.
func TestInstructionSpec_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	body := []byte(`
plugins: []
canonical_kinds:
 shorthand_kind:
 instruction: "Skip if absent."
 long_form_kind:
 instruction:
 enabled: false
 text: "Plugin-disabled fill."
`)
	cfg, err := Load(writeConfig(t, string(body)))
	require.NoError(t, err)

	// Shorthand → enabled=true.
	require.NotNil(t, cfg.CanonicalKinds["shorthand_kind"].Instruction)
	assert.True(t, cfg.CanonicalKinds["shorthand_kind"].Instruction.Enabled)
	assert.Equal(t, "Skip if absent.", cfg.CanonicalKinds["shorthand_kind"].Instruction.Text)

	// Long form → exact decode.
	require.NotNil(t, cfg.CanonicalKinds["long_form_kind"].Instruction)
	assert.False(t, cfg.CanonicalKinds["long_form_kind"].Instruction.Enabled)
	assert.Equal(t, "Plugin-disabled fill.",
		cfg.CanonicalKinds["long_form_kind"].Instruction.Text)
}

// TestLoad_CanonicalKindsDefaults pins ADR-0016 §3's top-level
// `canonical_kinds_defaults:` sibling key. Both gaps and
// instruction parse; merging is exercised in the merge tests
// above.
func TestLoad_CanonicalKindsDefaults(t *testing.T) {
	t.Parallel()

	body := []byte(`
plugins: []
canonical_kinds_defaults:
 instruction:
 enabled: true
 text: "Fill carefully."
 gaps:
 external_url:
 type: string
 description: "Authoritative URL."
canonical_kinds:
 person: {}
`)
	cfg, err := Load(writeConfig(t, string(body)))
	require.NoError(t, err)

	require.NotNil(t, cfg.CanonicalKindsDefaults.Instruction)
	assert.True(t, cfg.CanonicalKindsDefaults.Instruction.Enabled)
	assert.Equal(t, "Fill carefully.", cfg.CanonicalKindsDefaults.Instruction.Text)
	assert.Equal(t, "Authoritative URL.",
		cfg.CanonicalKindsDefaults.Gaps["external_url"].Description)
}

// TestLegacyRegistryWireShape pins the legacy projection: typed
// shape collapses to flat gap descriptions + bare instruction text
// for backward-compat with pre-ADR-0016 yaad-mcp clients.
func TestLegacyRegistryWireShape(t *testing.T) {
	t.Parallel()

	reg := map[string]CanonicalKindConfig{
		"person": {
			Gaps: map[string]GapSpec{
				"name": {Type: "string", Description: "Full name."},
				"birthdate": {Type: "date", Description: "Birth date."},
			},
			Instruction: &InstructionSpec{Enabled: true, Text: "Cite sources."},
		},
		"city": {
			// No instruction → projected as empty Instruction string.
			Gaps: map[string]GapSpec{"name": {Type: "string", Description: "City name."}},
		},
	}
	out := LegacyRegistryWireShape(reg)
	require.Contains(t, out, "person")
	assert.Equal(t, "Full name.", out["person"].Gaps["name"])
	assert.Equal(t, "Birth date.", out["person"].Gaps["birthdate"],
		"typed gaps lose Type on the legacy projection")
	assert.Equal(t, "Cite sources.", out["person"].Instruction)

	require.Contains(t, out, "city")
	assert.Empty(t, out["city"].Instruction,
		"nil Instruction collapses to empty string on legacy wire shape")
}

// TestGapSpec_FillStrategyParsing pins ADR-0019 step 2 long-form
// shape: fill_strategy + type + range/values/max_length all parse
// from operator config. Per-type validation runs at Load time so
// typos fail server start.
func TestGapSpec_FillStrategyParsing(t *testing.T) {
	t.Parallel()

	body := `
plugins: []
canonical_kinds:
 boardgame:
 gaps:
 summary:
 prompt: "Short summary"
 fill_strategy: agent
 rating:
 prompt: "How do you rate this?"
 type: int
 range: [1, 10]
 fill_strategy: operator
 played:
 type: bool
 description: "Have you played it?"
 fill_strategy: operator
 region:
 type: enum
 values: ["us", "eu", "asia"]
 description: "Operator region."
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err)

	gaps := cfg.CanonicalKinds["boardgame"].Gaps
	require.Len(t, gaps, 4)

	// `prompt:` reads into Description (alias).
	assert.Equal(t, "Short summary", gaps["summary"].Description)
	assert.Equal(t, "agent", gaps["summary"].FillStrategy)

	assert.Equal(t, "int", gaps["rating"].Type)
	assert.Equal(t, []int{1, 10}, gaps["rating"].Range)
	assert.Equal(t, "operator", gaps["rating"].FillStrategy)

	assert.Equal(t, "bool", gaps["played"].Type)
	assert.Equal(t, "operator", gaps["played"].FillStrategy)

	assert.Equal(t, "enum", gaps["region"].Type)
	assert.Equal(t, []string{"us", "eu", "asia"}, gaps["region"].Values)
	// fill_strategy omitted → default-empty (callers default to "both").
	assert.Equal(t, "", gaps["region"].FillStrategy)
}

// TestGapSpec_StringShorthandPreserved pins backward-compat: pre-
// ADR-0016 string-shape gap configs (still common in operator
// yaml in the wild) parse cleanly through the new typed-gap path.
func TestGapSpec_StringShorthandPreserved(t *testing.T) {
	t.Parallel()
	body := `
plugins: []
canonical_kinds:
 person:
 gaps:
 name: "Full name"
 birthdate: "Birth date in YYYY-MM-DD"
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err)
	gaps := cfg.CanonicalKinds["person"].Gaps
	assert.Equal(t, "string", gaps["name"].Type, "shorthand defaults to string")
	assert.Equal(t, "Full name", gaps["name"].Description)
	assert.Equal(t, "", gaps["name"].FillStrategy)
}

// TestGapSpec_RejectInvalidFillStrategy pins the validation: a
// typo in fill_strategy fails server start so the operator catches
// it before any agent or operator hits the gap.
func TestGapSpec_RejectInvalidFillStrategy(t *testing.T) {
	t.Parallel()
	body := `
plugins: []
canonical_kinds:
 boardgame:
 gaps:
 rating:
 type: int
 description: "rating"
 fill_strategy: humans # typo, must be agent|operator|both
`
	_, err := Load(writeConfig(t, body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fill_strategy")
	assert.Contains(t, err.Error(), "humans")
}

// TestGapSpec_RejectIntRangeShape: range must be exactly [min, max]
// with min ≤ max.
func TestGapSpec_RejectIntRangeShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		hint string
	}{
		{
			name: "single-element-range",
			yaml: `
plugins: []
canonical_kinds:
 bg:
 gaps:
 rating: { type: int, description: r, range: [5] }
`,
			hint: "must be [min, max]",
		},
		{
			name: "min-greater-than-max",
			yaml: `
plugins: []
canonical_kinds:
 bg:
 gaps:
 rating: { type: int, description: r, range: [10, 1] }
`,
			hint: "min 10 > max 1",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeConfig(t, tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.hint)
		})
	}
}

// TestGapSpec_RejectEnumWithoutValues: type=enum requires non-empty
// values list. Without values the agent-fill path can't validate
// the operator's choice.
func TestGapSpec_RejectEnumWithoutValues(t *testing.T) {
	t.Parallel()
	body := `
plugins: []
canonical_kinds:
 bg:
 gaps:
 region:
 type: enum
 description: "operator region"
`
	_, err := Load(writeConfig(t, body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "values")
}

// TestGapSpec_LegacyTypeDatePassesThrough pins the back-compat path:
// pre-ADR-0019 `type: date` (descriptive-only per ADR-0016 §7) is
// not in the closed ADR-0019 enum but must still parse so existing
// operator configs keep loading.
func TestGapSpec_LegacyTypeDatePassesThrough(t *testing.T) {
	t.Parallel()
	body := `
plugins: []
canonical_kinds:
 person:
 gaps:
 birthdate:
 type: date
 description: "Birth date."
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err, "legacy type=date must keep loading")
	assert.Equal(t, "date", cfg.CanonicalKinds["person"].Gaps["birthdate"].Type)
}

// TestGapSpec_RejectMixedTypeFields: a string-typed gap can't
// declare range; an int-typed gap can't declare values.
func TestGapSpec_RejectMixedTypeFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		hint string
	}{
		{
			name: "string-with-range",
			yaml: `
plugins: []
canonical_kinds:
 bg:
 gaps:
 rating: { type: string, description: r, range: [1, 10] }
`,
			hint: "type=string does not accept range",
		},
		{
			name: "int-with-values",
			yaml: `
plugins: []
canonical_kinds:
 bg:
 gaps:
 rating: { type: int, description: r, values: ["a", "b"] }
`,
			hint: "type=int does not accept",
		},
		{
			name: "bool-with-max-length",
			yaml: `
plugins: []
canonical_kinds:
 bg:
 gaps:
 played: { type: bool, description: p, max_length: 4 }
`,
			hint: "type=bool",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeConfig(t, tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.hint)
		})
	}
}

// TestBuiltinKindGaps_BoardgameSurface pins ADR-0019 step 4 — the
// five operator-strategy gaps the boardgame canonical kind ships
// with. Hand-checked against the issue spec; if the names or
// strategies drift the daemon's operator-fill surface for boardgame
// silently changes shape.
func TestBuiltinKindGaps_BoardgameSurface(t *testing.T) {
	t.Parallel()
	got := BuiltinKindGaps("boardgame")
	require.Len(t, got, 5)

	require.Contains(t, got, "rating")
	assert.Equal(t, "int", got["rating"].Type)
	assert.Equal(t, []int{1, 10}, got["rating"].Range)
	assert.Equal(t, "operator", got["rating"].FillStrategy)

	for _, field := range []string{"owned", "want", "played", "knows_how_to_play"} {
		require.Contains(t, got, field, "missing built-in %q", field)
		assert.Equal(t, "bool", got[field].Type, "%s.type", field)
		assert.Equal(t, "operator", got[field].FillStrategy, "%s.fill_strategy", field)
		assert.NotEmpty(t, got[field].Description, "%s.description", field)
	}
}

// TestBuiltinKindGaps_UnknownKindEmpty: kinds with no built-in
// extras get an empty (non-nil) map. Mirrors DefaultGaps's
// fresh-copy contract.
func TestBuiltinKindGaps_UnknownKindEmpty(t *testing.T) {
	t.Parallel()
	got := BuiltinKindGaps("not-a-real-kind")
	require.NotNil(t, got, "unknown kind returns non-nil empty map")
	assert.Empty(t, got)
}

// TestMergeCanonicalRegistry_OperatorOverridesBuiltinKindGap pins
// the layering: operator config can override a kind-specific
// built-in (Layer 1.5). Operator declares boardgame.rating with a
// different prompt + fill_strategy → operator's spec wins.
func TestMergeCanonicalRegistry_OperatorOverridesBuiltinKindGap(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	merged := MergeCanonicalRegistry(
		nil,
		[]string{"boardgame"},
		CanonicalKindConfig{},
		map[string]CanonicalKindConfig{
			"boardgame": {
				Gaps: map[string]GapSpec{
					"rating": {
						Type: "string",
						Description: "operator's custom rating prompt",
						FillStrategy: "agent",
					},
				},
			},
		},
		logger,
	)
	bg := merged["boardgame"]
	assert.Equal(t, "string", bg.Gaps["rating"].Type, "operator override wins")
	assert.Equal(t, "agent", bg.Gaps["rating"].FillStrategy)
	// Other built-ins still present (operator only touched rating).
	for _, field := range []string{"owned", "want", "played", "knows_how_to_play"} {
		assert.Contains(t, bg.Gaps, field, "untouched built-in %s preserved", field)
	}
}

// TestMergeCanonicalRegistry_PluginExtraOverridesBuiltinKindGap:
// plugin extras can override the built-in kind gaps (Layer 1.5 is
// behind plugin extras at Layer 2).
func TestMergeCanonicalRegistry_PluginExtraOverridesBuiltinKindGap(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	merged := MergeCanonicalRegistry(
		map[string]map[string]GapSpec{
			"boardgame": {
				"rating": {
					Type: "int",
					Description: "plugin's rating prompt",
					Range: []int{1, 5},
					FillStrategy: "agent",
				},
			},
		},
		[]string{"boardgame"},
		CanonicalKindConfig{},
		nil,
		logger,
	)
	bg := merged["boardgame"]
	assert.Equal(t, "plugin's rating prompt", bg.Gaps["rating"].Description,
		"plugin extra overrides ADR-0019 built-in")
	assert.Equal(t, []int{1, 5}, bg.Gaps["rating"].Range)
	assert.Equal(t, "agent", bg.Gaps["rating"].FillStrategy)
}
