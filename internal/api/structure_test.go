package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// Per ADR-0013 §7 / alice2-index: the introspection endpoint
// surfaces the structural signature — kinds (with gaps + per-kind
// instructions), canonical edge-types, plugin metadata, and a
// version hash that bumps on rebuild / config-change /
// plugin-add-remove-upgrade. The plugin section is built from each
// plugin's `--init` capabilities (cached at startup); refresh-on-
// demand is out of scope.

func newStructureAPI(
	t *testing.T,
	reg map[string]config.CanonicalKindConfig,
	edgeTypes []string,
	registry *plugins.Registry,
) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	if registry == nil {
		registry = testRegistryWithSeed()
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{}
	if reg != nil {
		opts = append(opts, WithCanonicalKindRegistry(reg))
	}
	if len(edgeTypes) > 0 {
		opts = append(opts, WithCanonicalEdgeTypes(edgeTypes))
	}
	return NewHandlerWithRegistry(logger, st, registry, opts...)
}

func decodeStructure(t *testing.T, rec *httptest.ResponseRecorder) structureResponse {
	t.Helper()
	var got structureResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got),
		"decode /v1/structure response; body=%s", rec.Body.String())
	return got
}

// Empty config (no canonical_kinds, no edge_types, no plugins) →
// empty sections + a stable version string. Asserts on the typed
// shape AND on the raw JSON shape — the cold-reviewer's a prior PR catch was that
// `assert.Empty` accepts both nil and []string{}, so a regression
// where we emitted `null` for edge_types passed the typed assertion.
// The raw-JSON check pins `edge_types: []` (not `null`) per
// ADR-0002's specified array shape.
func TestStructure_EmptyConfig_EmptySections(t *testing.T) {
	t.Parallel()
	// Use an empty registry to avoid the testRegistryWithSeed() default
	// from polluting the plugins section.
	h := newStructureAPI(t, nil, nil, plugins.NewRegistry())
	req := httptest.NewRequest(http.MethodGet, "/v1/structure", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeStructure(t, rec)
	assert.True(t, got.OK)
	assert.NotEmpty(t, got.Version, "version always present even on empty config")
	assert.Empty(t, got.Kinds)
	assert.Empty(t, got.EdgeTypes)
	assert.Empty(t, got.Plugins)

	// Raw JSON shape — ADR-0002 + the cold-reviewer's a prior PR review require the
	// edge_types and plugins fields serialize as `[]`, not `null`,
	// when empty. The typed `Empty` assertions above accept both.
	body := rec.Body.String()
	assert.Contains(t, body, `"edge_types":[]`,
		"empty edge_types must serialize as array, not null")
	assert.Contains(t, body, `"plugins":[]`,
		"empty plugins must serialize as array, not null")
	assert.NotContains(t, body, `"edge_types":null`)
	assert.NotContains(t, body, `"plugins":null`)
}

// Populated config → all three sections populate verbatim.
func TestStructure_PopulatedConfig_VerbatimSections(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"person": {
			Gaps: config.GapsFromMap(map[string]string{"name": "Full name.", "summary": "One-paragraph summary."}),
			Instruction: config.InstructionFromString("Skip if absent."),
		},
		"city": {
			Gaps: config.GapsFromMap(map[string]string{"name": "City name."}),
		},
	}
	h := newStructureAPI(t, reg, []string{"is_about", "lives_in"}, plugins.NewRegistry())
	req := httptest.NewRequest(http.MethodGet, "/v1/structure", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeStructure(t, rec)

	require.Len(t, got.Kinds, 2)
	person := got.Kinds["person"]
	assert.True(t, person.IsCanonical)
	assert.Equal(t, "Full name.", person.Gaps["name"])
	assert.Equal(t, "Skip if absent.", person.Instruction)

	city := got.Kinds["city"]
	assert.True(t, city.IsCanonical)
	assert.Equal(t, "City name.", city.Gaps["name"])
	assert.Empty(t, city.Instruction, "absent per-kind instruction → omitted")

	assert.Equal(t, []string{"is_about", "lives_in"}, got.EdgeTypes)
}

// Same config across two calls → identical version.
func TestStructure_VersionStableAcrossCalls(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"person": {Gaps: config.GapsFromMap(map[string]string{"name": "Full name."}), Instruction: config.InstructionFromString("x")},
	}
	h := newStructureAPI(t, reg, []string{"is_about"}, plugins.NewRegistry())
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))
	v1 := decodeStructure(t, rec1).Version
	v2 := decodeStructure(t, rec2).Version
	assert.Equal(t, v1, v2)
	assert.NotEmpty(t, v1)
}

// Config change between calls (canonical_kinds added) → version differs.
// Asserted via the version-computation helper directly so the test
// doesn't have to rebuild the handler.
func TestStructure_VersionBumpsOnConfigChange(t *testing.T) {
	t.Parallel()
	regA := map[string]config.CanonicalKindConfig{
		"person": {Gaps: config.GapsFromMap(map[string]string{"name": "Full name."})},
	}
	regB := map[string]config.CanonicalKindConfig{
		"person": {Gaps: config.GapsFromMap(map[string]string{"name": "Full name."})},
		"city": {Gaps: config.GapsFromMap(map[string]string{"name": "City name."})},
	}
	rA := buildStructureResponse(plugins.NewRegistry(), regA, []string{"is_about"})
	rB := buildStructureResponse(plugins.NewRegistry(), regB, []string{"is_about"})
	assert.NotEqual(t, rA.Version, rB.Version,
		"adding a kind to canonical_kinds must bump version")

	// Edge-type change too.
	rC := buildStructureResponse(plugins.NewRegistry(), regA, []string{"is_about", "lives_in"})
	assert.NotEqual(t, rA.Version, rC.Version,
		"adding an edge type must bump version")
}

// Plugin section: a fixture plugin with known --init capabilities →
// response includes its name, version, url_patterns, supports_search,
// emits_kinds, emits_edges.
func TestStructure_PluginSection_Populated(t *testing.T) {
	t.Parallel()
	reg := plugins.NewRegistry()
	reg.Register(&fixture.Plugin{
		NameValue: "fake",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "fake",
			Version: "9.9.9",
			URLPatterns: []string{"https://fake.test/*"},
			SupportsSearch: true,
			EntityKinds: []plugins.KindSpec{
				{Name: "fake-article"},
			},
			EdgeKinds: []plugins.KindSpec{
				{Name: "is_about"},
				{Name: "references"},
			},
		},
	})
	h := newStructureAPI(t, nil, nil, reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeStructure(t, rec)
	require.Len(t, got.Plugins, 1)
	p := got.Plugins[0]
	assert.Equal(t, "fake", p.Name)
	assert.Equal(t, "9.9.9", p.Version)
	assert.Equal(t, []string{"https://fake.test/*"}, p.URLPatterns)
	assert.True(t, p.SupportsSearch)
	assert.Equal(t, []string{"fake-article"}, p.EmitsKinds)
	assert.Equal(t, []string{"is_about", "references"}, p.EmitsEdges)
}

// Plugin section is built from cached --init capabilities — calling
// the handler N times must produce stable plugin metadata (the real
// subprocess plugin's plugin_capabilities table cache makes the
// Capabilities() call in-memory after the first --init at startup).
// fixture.Plugin's CapabilitiesValue is similarly returned by the
// in-memory accessor, so this test verifies the stable-shape
// invariant: N requests produce identical plugin metadata, no
// drift across calls.
func TestStructure_PluginCapabilities_StableAcrossRequests(t *testing.T) {
	t.Parallel()
	reg := plugins.NewRegistry()
	reg.Register(&fixture.Plugin{
		NameValue: "stable",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "stable",
			Version: "1.0.0",
		},
	})
	h := newStructureAPI(t, nil, nil, reg)
	var seenVersion string
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		got := decodeStructure(t, rec)
		require.Len(t, got.Plugins, 1)
		if seenVersion == "" {
			seenVersion = got.Plugins[0].Version
		}
		assert.Equal(t, seenVersion, got.Plugins[0].Version,
			"plugin Version stable across N requests (i=%d) — cached --init contract", i)
	}
	assert.Equal(t, "1.0.0", seenVersion)
}

// is_canonical=true on every kind for v1 — locked-in until passthrough
// kinds are introduced.
func TestStructure_IsCanonicalAlwaysTrueV1(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"person": {Gaps: config.GapsFromMap(map[string]string{"name": "x"})},
		"boardgame": {Gaps: config.GapsFromMap(map[string]string{"name": "y"})},
	}
	h := newStructureAPI(t, reg, nil, plugins.NewRegistry())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))
	got := decodeStructure(t, rec)
	for kind, entry := range got.Kinds {
		assert.True(t, entry.IsCanonical, "is_canonical must be true on %s in v1", kind)
	}
}

// Wire-level instruction omitempty: per-kind instruction absent →
// `instruction` field absent on the wire (cold-reviewer-style omitempty
// drift guard, mirroring the cache-hit envelope contract).
func TestStructure_PerKindInstructionOmitemptyOnWire(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"person": {Gaps: config.GapsFromMap(map[string]string{"name": "x"})}, // no instruction
	}
	h := newStructureAPI(t, reg, nil, plugins.NewRegistry())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	kinds := raw["kinds"].(map[string]any)
	person := kinds["person"].(map[string]any)
	_, has := person["instruction"]
	assert.False(t, has, "absent instruction must be omitted on wire (omitempty)")
}

// Plugin section sorted by name (deterministic regardless of
// registration order). Pins the version hash's stability against
// registry insertion order.
func TestStructure_PluginsSortedByName(t *testing.T) {
	t.Parallel()
	reg := plugins.NewRegistry()
	reg.Register(&fixture.Plugin{
		NameValue: "zeta",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{Name: "zeta"},
	})
	reg.Register(&fixture.Plugin{
		NameValue: "alpha",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{Name: "alpha"},
	})
	h := newStructureAPI(t, nil, nil, reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/structure", nil))
	got := decodeStructure(t, rec)
	require.Len(t, got.Plugins, 2)
	assert.Equal(t, "alpha", got.Plugins[0].Name)
	assert.Equal(t, "zeta", got.Plugins[1].Name)
}

// Direct unit test on the version-hash helper. Same inputs → same
// hash; different inputs → different hash. Pins the determinism
// invariant against accidental non-deterministic serialization.
func TestComputeStructureVersion_Determinism(t *testing.T) {
	t.Parallel()
	regA := map[string]config.CanonicalKindConfig{
		"a": {Gaps: config.GapsFromMap(map[string]string{"x": "1"})},
		"b": {Gaps: config.GapsFromMap(map[string]string{"y": "2"})},
	}
	regB := map[string]config.CanonicalKindConfig{
		"b": {Gaps: config.GapsFromMap(map[string]string{"y": "2"})},
		"a": {Gaps: config.GapsFromMap(map[string]string{"x": "1"})},
	}
	v1 := computeStructureVersion(regA, []string{"x", "y"}, nil)
	v2 := computeStructureVersion(regB, []string{"x", "y"}, nil)
	assert.Equal(t, v1, v2, "map-key insertion order must not affect hash")

	v3 := computeStructureVersion(regA, []string{"x", "y", "z"}, nil)
	assert.NotEqual(t, v1, v3, "edge-types change must bump hash")

	// Sanity: 16 hex chars per the truncation choice.
	assert.Len(t, v1, 16)
}
