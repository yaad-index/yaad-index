package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// TestKindsHandler_ServesSeededTestRegistryPayload exercises the
// happy path against the test-registry seed (boardgame/book/person +
// designed_by/authored_by). Previously asserted bootstrapKinds; now
// the same shape comes from the bgg + books fixture plugins.
func TestKindsHandler_ServesSeededTestRegistryPayload(t *testing.T) {
	t.Parallel()

	h := newAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/kinds", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body kindsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode response body")

	assert.True(t, body.OK)

	ents := indexEntityKinds(body.EntityKinds)
	bg, ok := ents["boardgame"]
	require.True(t, ok, "entity_kinds missing 'boardgame'; got names %v", entityNames(body.EntityKinds))
	assert.NotEmpty(t, bg.Description, "entity_kinds[boardgame].description")
	assert.Equal(t, []string{"bgg"}, bg.SourcePlugins, "entity_kinds[boardgame].source_plugins")

	// `person` is advertised by BOTH bgg and books — exercises the
	// source_plugins union + dedup logic in aggregateKinds.
	person, ok := ents["person"]
	require.True(t, ok, "entity_kinds missing 'person'")
	assert.Equal(t, []string{"bgg", "books"}, person.SourcePlugins,
		"entity_kinds[person].source_plugins: want sorted union")

	edges := indexEdgeKinds(body.EdgeKinds)
	designed, ok := edges["designed_by"]
	require.True(t, ok, "edge_kinds missing 'designed_by'; got names %v", edgeNames(body.EdgeKinds))
	assert.Equal(t, "boardgame", designed.FromKind)
	assert.Equal(t, "person", designed.ToKind)
}

// TestKindsHandler_EmptyRegistry_ReturnsOnlyDaemonBuiltins pins the
// zero-plugin shape post-ADR-0025: `day` plus the five canonical
// edge type names (due_on, occurred_on, is_about_day, references_day,
// ingested_on) surface as daemon-managed entries even with no
// plugins registered. Source_plugins on those entries names the
// synthetic "yaad-index" producer so consumers can distinguish
// daemon-built-in kinds from plugin-emitted ones.
func TestKindsHandler_EmptyRegistry_ReturnsOnlyDaemonBuiltins(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New")
	t.Cleanup(func() { _ = st.Close() })
	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, plugins.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/v1/kinds", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body kindsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode")
	assert.True(t, body.OK)

	require.Len(t, body.EntityKinds, 1, "only the daemon-built-in `day` kind surfaces with empty registry")
	assert.Equal(t, canonical.DayKind, body.EntityKinds[0].Name)
	assert.Equal(t, []string{daemonSourcePlugin}, body.EntityKinds[0].SourcePlugins)

	gotEdgeNames := edgeNames(body.EdgeKinds)
	assert.ElementsMatch(t, canonical.DaemonEdgeTypes(), gotEdgeNames,
		"all five canonical edge type names surface even when no plugin advertises them")
	for _, e := range body.EdgeKinds {
		assert.Equal(t, canonical.DayKind, e.ToKind,
			"canonical edge %q must terminate at the `day` kind", e.Name)
		assert.Equal(t, []string{daemonSourcePlugin}, e.SourcePlugins,
			"canonical edge %q source_plugins must name the daemon", e.Name)
	}
}

// TestKindsHandler_AlphabeticalOrder asserts entity_kinds + edge_kinds
// are sorted alphabetically by name. Stable order matters: clients
// compare the response across runs (e.g. as a deploy-validation
// snapshot), and a non-deterministic order would make every restart
// look like a config change.
func TestKindsHandler_AlphabeticalOrder(t *testing.T) {
	t.Parallel()

	// Register plugins in REVERSE alphabetical order to confirm the
	// handler sorts rather than echoing registration order.
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "z-plugin",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "z-plugin",
			EntityKinds: []plugins.KindSpec{
				{Name: "zebra"},
				{Name: "apple"},
			},
			EdgeKinds: []plugins.KindSpec{
				{Name: "zips_to", FromKind: "zebra", ToKind: "apple"},
				{Name: "annotates", FromKind: "apple", ToKind: "zebra"},
			},
		},
	})
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New")
	t.Cleanup(func() { _ = st.Close() })
	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), st, registry)

	req := httptest.NewRequest(http.MethodGet, "/v1/kinds", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body kindsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode")

	entityNamesGot := entityNames(body.EntityKinds)
	assert.True(t, sort.StringsAreSorted(entityNamesGot),
		"entity_kinds names: want sorted alphabetically, got %v", entityNamesGot)
	edgeNamesGot := edgeNames(body.EdgeKinds)
	assert.True(t, sort.StringsAreSorted(edgeNamesGot),
		"edge_kinds names: want sorted alphabetically, got %v", edgeNamesGot)
}

// TestKindsHandler_EdgeEndpointsAreDeclaredEntities locks in the closure
// invariant of the bootstrap payload: every edge_kind's from_kind and
// to_kind must appear in entity_kinds. A future kind added on one side
// without the other will trip this test instead of shipping an internally
// inconsistent /v1/kinds response.
//
// Daemon-built-in canonical edges per ADR-0025 leave FromKind empty
// because the source side is open (any entity can carry a day
// reference); the closure check skips edges with empty FromKind to
// honor that "any source" semantic.
func TestKindsHandler_EdgeEndpointsAreDeclaredEntities(t *testing.T) {
	t.Parallel()

	h := newAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/kinds", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body kindsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode response body")

	declared := indexEntityKinds(body.EntityKinds)
	for _, e := range body.EdgeKinds {
		if e.FromKind == "" {
			// Open-source edge (daemon-built-in canonical day edges):
			// any entity may carry the reference, so the closure
			// invariant doesn't apply to the from side.
			_, toOK := declared[e.ToKind]
			assert.True(t, toOK,
				"edge_kinds[%s].to_kind=%q not in entity_kinds %v",
				e.Name, e.ToKind, entityNames(body.EntityKinds))
			continue
		}
		_, fromOK := declared[e.FromKind]
		assert.True(t, fromOK,
			"edge_kinds[%s].from_kind=%q not in entity_kinds %v",
			e.Name, e.FromKind, entityNames(body.EntityKinds))
		_, toOK := declared[e.ToKind]
		assert.True(t, toOK,
			"edge_kinds[%s].to_kind=%q not in entity_kinds %v",
			e.Name, e.ToKind, entityNames(body.EntityKinds))
	}
}

func TestKindsHandler_RejectsNonGet(t *testing.T) {
	t.Parallel()

	h := newAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/kinds", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"method-aware mux should reject POST on a GET route")
}

func indexEntityKinds(in []entityKind) map[string]entityKind {
	out := make(map[string]entityKind, len(in))
	for _, k := range in {
		out[k.Name] = k
	}
	return out
}

func indexEdgeKinds(in []edgeKind) map[string]edgeKind {
	out := make(map[string]edgeKind, len(in))
	for _, k := range in {
		out[k.Name] = k
	}
	return out
}

func entityNames(in []entityKind) []string {
	out := make([]string, 0, len(in))
	for _, k := range in {
		out = append(out, k.Name)
	}
	return out
}

func edgeNames(in []edgeKind) []string {
	out := make([]string, 0, len(in))
	for _, k := range in {
		out = append(out, k.Name)
	}
	return out
}
