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

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// TestGetPlugins_EmptyRegistry pins the zero-state shape: no
// plugins registered → ok=true + empty plugins array.
func TestGetPlugins_EmptyRegistry(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, plugins.NewRegistry())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Empty(t, resp.Plugins, "empty registry must produce empty plugins array")
}

// TestGetPlugins_FullCapabilitiesShape pins the per-plugin
// serialization: every Capabilities field this endpoint surfaces
// makes it onto the wire with the right JSON shape. Uses a
// fixture plugin populated with every relevant field.
func TestGetPlugins_FullCapabilitiesShape(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "gmail",
			Version: "0.4.0",
			URLPatterns: []string{},
			SourceNamespace: "gmail",
			Commands: []plugins.CommandSpec{{Name: "fetch"}},
			EntityKinds: []plugins.KindSpec{
				{Name: "source", Description: "gmail-source-kind"},
			},
			EdgeKinds: []plugins.KindSpec{},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Len(t, resp.Plugins, 1)
	got := resp.Plugins[0]
	assert.Equal(t, "gmail", got.Name)
	assert.Equal(t, "0.4.0", got.Version)
	assert.Equal(t, "gmail", got.SourceNamespace)
	assert.Equal(t, []string{}, got.URLPatterns,
		"poll-driven plugin must surface url_patterns as [] (not null)")
	assert.Equal(t, []plugins.CommandSpec{{Name: "fetch"}}, got.Commands)
	require.Len(t, got.EntityKinds, 1)
	assert.Equal(t, "source", got.EntityKinds[0].Name)
	assert.Equal(t, "gmail-source-kind", got.EntityKinds[0].Description)
	assert.Empty(t, got.EdgeKinds, "no edges declared")
}

// TestGetPlugins_PreservesRegistryOrder pins the dispatch-order
// contract: plugins on /v1/plugins surface in registration order,
// matching the first-match-wins order /v1/ingest walks. Consumers
// (yaad-mcp SKILL.md generators) can show the dispatch precedence
// directly.
func TestGetPlugins_PreservesRegistryOrder(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{NameValue: "first", MatchFunc: func(string) bool { return false }, CapabilitiesValue: plugins.Capabilities{Name: "first"}})
	registry.Register(&fixture.Plugin{NameValue: "second", MatchFunc: func(string) bool { return false }, CapabilitiesValue: plugins.Capabilities{Name: "second"}})
	registry.Register(&fixture.Plugin{NameValue: "third", MatchFunc: func(string) bool { return false }, CapabilitiesValue: plugins.Capabilities{Name: "third"}})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Len(t, resp.Plugins, 3)
	assert.Equal(t, "first", resp.Plugins[0].Name)
	assert.Equal(t, "second", resp.Plugins[1].Name)
	assert.Equal(t, "third", resp.Plugins[2].Name)
}

// TestGetPlugins_FallsBackToPluginName pins the defensive name
// resolution: when Capabilities.Name is empty (e.g. a fixture that
// didn't bother to set it), the endpoint uses Plugin.Name() instead.
// Production plugins should always set Capabilities.Name, but
// defensive fallback prevents an empty `name` field on the wire.
func TestGetPlugins_FallsBackToPluginName(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "plugin-name-only",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{}, // no Name field
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Plugins, 1)
	assert.Equal(t, "plugin-name-only", resp.Plugins[0].Name,
		"empty Capabilities.Name should fall back to Plugin.Name()")
}

// TestGetPlugins_EdgesSurfaceFromKindToKind pins the edge metadata
// pass-through: from_kind + to_kind on plugins.KindSpec reach the
// pluginEdgeEntry wire shape.
func TestGetPlugins_EdgesSurfaceFromKindToKind(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "bgg",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "bgg",
			EdgeKinds: []plugins.KindSpec{
				{Name: "designed_by", Description: "designed-by", FromKind: "boardgame", ToKind: "person"},
			},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Plugins, 1)
	require.Len(t, resp.Plugins[0].EdgeKinds, 1)
	assert.Equal(t, "designed_by", resp.Plugins[0].EdgeKinds[0].Name)
	assert.Equal(t, "boardgame", resp.Plugins[0].EdgeKinds[0].FromKind)
	assert.Equal(t, "person", resp.Plugins[0].EdgeKinds[0].ToKind)
}

// TestGetPlugins_KindsAreSortedByName pins the per-plugin
// stable-diff contract: entity_kinds and edge_kinds within each
// plugin sort alphabetically by name. Successive calls produce
// byte-identical responses for a stable plugin set.
func TestGetPlugins_KindsAreSortedByName(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "messy",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "messy",
			EntityKinds: []plugins.KindSpec{
				{Name: "zebra"},
				{Name: "apple"},
				{Name: "mango"},
			},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Plugins, 1)
	names := []string{}
	for _, k := range resp.Plugins[0].EntityKinds {
		names = append(names, k.Name)
	}
	assert.Equal(t, []string{"apple", "mango", "zebra"}, names,
		"entity_kinds within a plugin must be alphabetically sorted")
}

// --- ADR-0028 Cut 2: /v1/plugins surfaces per-plugin instances ---

func TestGetPlugins_InstancesSingleImplicit_SurfacesDefault(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "wikipedia",
			URLPatterns: []string{`^https?://en\.wikipedia\.org/.*`},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// No WithPluginInstances → handler falls back to synthesized
	// `[{name: "default", enabled: true}]` per the ADR-0028 §1
	// implicit-instance contract.
	h := NewHandlerWithRegistry(logger, st, registry)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Plugins, 1)
	assert.Equal(t, []pluginInstanceEntry{{Name: "default", Enabled: true}},
		resp.Plugins[0].Instances,
		"single-implicit plugin must surface the synthesized default instance")
}

func TestGetPlugins_InstancesMultiInstance_SurfacesOperatorList(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "github",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "github",
			URLPatterns: []string{`^https?://github\.com/.*`},
			SupportsInstances: true,
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Operator declared two instances; the handler must surface
	// the full list in declaration order per ADR-0028 §1 + §4
	// (fan-out semantics depend on this order).
	h := NewHandlerWithRegistry(logger, st, registry,
		WithPluginInstances(map[string][]string{
			"github": {"personal", "work"},
		}),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Plugins, 1)
	assert.Equal(t,
		[]pluginInstanceEntry{
			{Name: "personal", Enabled: true},
			{Name: "work", Enabled: true},
		},
		resp.Plugins[0].Instances,
		"multi-instance plugin must surface operator's list in declaration order")
}
