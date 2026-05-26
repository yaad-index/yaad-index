// Tests for the per-#48 slice 3 /v1/canonical_registry/effective
// + /v1/canonical_registry/available HTTP surface.

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
	"github.com/yaad-index/yaad-index/internal/store"
)

// newCanonicalRegistryFixture wires a Handler with a given
// merged registry + provenance map.
func newCanonicalRegistryFixture(t *testing.T, reg map[string]config.CanonicalKindConfig, prov config.RegistryProvenance) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewHandlerWithRegistry(logger, st, nil,
		WithCanonicalKindRegistry(reg),
		WithCanonicalKindProvenance(prov),
	)
}

// TestCanonicalRegistryEffective_HappyPath pins the basic
// surface: a merged registry with provenance returns kinds +
// per-gap source_layer.
func TestCanonicalRegistryEffective_HappyPath(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"name": {Type: "string", Description: "The name."},
				"rating": {Type: "int", Description: "Rate it.",
					Range: []int{1, 10}, FillStrategy: "operator"},
				"custom_field": {Type: "string", Description: "Operator-added."},
			},
			Instruction: &config.InstructionSpec{Enabled: true, Text: "Be brief."},
		},
	}
	prov := config.RegistryProvenance{
		"boardgame": {
			"name":         config.LayerUniversalDefaults,
			"rating":       config.LayerBuiltinKindGaps,
			"custom_field": config.LayerOperatorPerKind,
			config.InstructionProvenanceKey: config.LayerOperatorPerKind,
		},
	}
	h := newCanonicalRegistryFixture(t, reg, prov)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/canonical_registry/effective", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp canonicalRegistryEffectiveResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	require.Contains(t, resp.Kinds, "boardgame")
	bg := resp.Kinds["boardgame"]
	assert.Equal(t, "code_defaults", bg.Gaps["name"].SourceLayer)
	assert.Equal(t, "builtin_kind", bg.Gaps["rating"].SourceLayer)
	assert.Equal(t, "operator", bg.Gaps["custom_field"].SourceLayer)
	assert.Equal(t, "int", bg.Gaps["rating"].Type)
	assert.Equal(t, []int{1, 10}, bg.Gaps["rating"].Range)
	assert.Equal(t, "operator", bg.Gaps["rating"].FillStrategy)
	assert.True(t, bg.Instruction.Enabled)
	assert.Equal(t, "Be brief.", bg.Instruction.Text)
	assert.Equal(t, "operator", bg.Instruction.SourceLayer)
}

// TestCanonicalRegistryEffective_EmptyRegistry pins the no-kinds
// path: an empty merged registry returns `{ok, kinds: {}}` —
// not a 404, not an error.
func TestCanonicalRegistryEffective_EmptyRegistry(t *testing.T) {
	t.Parallel()
	h := newCanonicalRegistryFixture(t, map[string]config.CanonicalKindConfig{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/canonical_registry/effective", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp canonicalRegistryEffectiveResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Empty(t, resp.Kinds)
}

// TestCanonicalRegistryAvailable_ListsDormantBuiltins pins the
// available shape: kinds in BuiltinKindGapsList that are NOT in
// the active merged registry appear, with their daemon-shipped
// gap-set.
func TestCanonicalRegistryAvailable_ListsDormantBuiltins(t *testing.T) {
	t.Parallel()
	// Active registry has only boardgame. The other 5 built-in
	// kinds (article/book/person/place/recipe) are dormant.
	active := map[string]config.CanonicalKindConfig{
		"boardgame": {Gaps: map[string]config.GapSpec{}},
	}
	h := newCanonicalRegistryFixture(t, active, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/canonical_registry/available", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp canonicalRegistryAvailableResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.OK)

	for _, kind := range []string{"article", "book", "person", "place", "recipe"} {
		assert.Contains(t, resp.Kinds, kind, "dormant built-in %q must appear in available list", kind)
	}
	assert.NotContains(t, resp.Kinds, "boardgame",
		"active kind must NOT appear in available list (it's already activated)")

	// Names list is sorted lexicographically for stable
	// MCP / CLI output.
	assert.Equal(t,
		[]string{"article", "book", "person", "place", "recipe"},
		resp.Names,
		"names must be sorted")

	// Spot-check a specific kind's gap surface.
	require.Contains(t, resp.Kinds, "person")
	require.Contains(t, resp.Kinds["person"].Gaps, "birth_date")
	assert.Equal(t, "string", resp.Kinds["person"].Gaps["birth_date"].Type)
}

// TestCanonicalRegistryAvailable_AllActiveYieldsEmpty pins the
// inverse: when every Layer 1.5 built-in kind is already active
// in the merged registry, the available list is empty (operator
// has nothing more to opt into from the daemon-shipped starter
// pool).
func TestCanonicalRegistryAvailable_AllActiveYieldsEmpty(t *testing.T) {
	t.Parallel()
	active := map[string]config.CanonicalKindConfig{}
	for _, kind := range config.BuiltinKindGapsList() {
		active[kind] = config.CanonicalKindConfig{Gaps: map[string]config.GapSpec{}}
	}
	h := newCanonicalRegistryFixture(t, active, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/canonical_registry/available", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp canonicalRegistryAvailableResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Kinds)
	assert.Empty(t, resp.Names)
}
