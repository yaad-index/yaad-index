package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
)

// stubRegistry adapts a list of *fixture.Plugin to the
// pluginRegistry interface the federation handler expects. Used
// in place of *plugins.Registry so tests don't have to spin up the
// full registration plumbing.
type stubRegistry struct {
	plugins []plugins.Plugin
}

func (r *stubRegistry) Plugins() []plugins.Plugin { return r.plugins }
func (r *stubRegistry) LookupByName(name string) (plugins.Plugin, bool) {
	for _, p := range r.plugins {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// fixtureWithSearch builds a fixture plugin that opts into search
// + returns a canned candidate list.
func fixtureWithSearch(name string, candidates []plugins.SearchCandidate) *fixture.Plugin {
	return &fixture.Plugin{
		NameValue: name,
		CapabilitiesValue: plugins.Capabilities{
			Name:           name,
			SupportsSearch: true,
		},
		SearchValue: candidates,
	}
}

// fixtureNoSearch builds a fixture plugin that doesn't opt in to
// search (SupportsSearch=false). The federation handler must skip
// it on no-allowlist requests and 422 on explicit-target requests.
func fixtureNoSearch(name string) *fixture.Plugin {
	return &fixture.Plugin{
		NameValue: name,
		CapabilitiesValue: plugins.Capabilities{
			Name:           name,
			SupportsSearch: false,
		},
	}
}

// doUpstreamSearch is the canonical test driver: marshals the
// request body + invokes the handler with a discarding logger.
func doUpstreamSearch(t *testing.T, registry pluginRegistry, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/search/upstream", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handleSearchUpstream(logger, registry).ServeHTTP(rec, req)
	return rec
}

// TestSearchUpstream_NoAllowlistFederatesOnlyOptedIn pins that
// when the caller doesn't specify a plugin allowlist, the daemon
// dispatches only to plugins with SupportsSearch=true and skips
// the rest silently.
func TestSearchUpstream_NoAllowlistFederatesOnlyOptedIn(t *testing.T) {
	t.Parallel()

	yes := fixtureWithSearch("wikipedia", []plugins.SearchCandidate{
		{ID: "Brass", Label: "Brass (board game)"},
	})
	no := fixtureNoSearch("gmail")

	reg := &stubRegistry{plugins: []plugins.Plugin{yes, no}}
	rec := doUpstreamSearch(t, reg, map[string]any{"query": "Brass"})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp searchUpstreamResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.PerPlugin, 1, "no-search plugin must be skipped from per_plugin_status")
	assert.Equal(t, "wikipedia", resp.PerPlugin[0].Plugin)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "wikipedia", resp.Results[0].Plugin)
	assert.Equal(t, "Brass", resp.Results[0].ID)
}

// TestSearchUpstream_ExplicitAllowlistFiltersToNamedPlugins pins
// that the request's plugins:[] field selects exactly the named
// plugins (when they're opted-in).
func TestSearchUpstream_ExplicitAllowlistFiltersToNamedPlugins(t *testing.T) {
	t.Parallel()

	wiki := fixtureWithSearch("wikipedia", []plugins.SearchCandidate{{ID: "Brass", Label: "Brass"}})
	bgg := fixtureWithSearch("bgg", []plugins.SearchCandidate{{ID: "28720", Label: "Brass: Birmingham"}})

	reg := &stubRegistry{plugins: []plugins.Plugin{wiki, bgg}}
	rec := doUpstreamSearch(t, reg, map[string]any{
		"query":   "Brass",
		"plugins": []string{"bgg"},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp searchUpstreamResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.PerPlugin, 1, "allowlist must filter dispatch to bgg only")
	assert.Equal(t, "bgg", resp.PerPlugin[0].Plugin)
}

// TestSearchUpstream_UnregisteredPluginIs400 pins the 400 path:
// caller names a plugin that doesn't exist in the registry.
func TestSearchUpstream_UnregisteredPluginIs400(t *testing.T) {
	t.Parallel()

	reg := &stubRegistry{plugins: []plugins.Plugin{
		fixtureWithSearch("wikipedia", nil),
	}}
	rec := doUpstreamSearch(t, reg, map[string]any{
		"query":   "x",
		"plugins": []string{"nonexistent-plugin"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not registered")
}

// TestSearchUpstream_UnsupportedPluginIs422 pins the 422 path per
// dispatch sub-question (d): caller names a registered plugin
// whose SupportsSearch=false → 422, NOT silent filter.
func TestSearchUpstream_UnsupportedPluginIs422(t *testing.T) {
	t.Parallel()

	reg := &stubRegistry{plugins: []plugins.Plugin{
		fixtureWithSearch("wikipedia", nil),
		fixtureNoSearch("gmail"),
	}}
	rec := doUpstreamSearch(t, reg, map[string]any{
		"query":   "x",
		"plugins": []string{"gmail"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"explicit target of non-search plugin must surface 422, not filter+warn")
	assert.Contains(t, rec.Body.String(), "does not support search")
}

// TestSearchUpstream_EmptyQueryIs400 pins the validation.
func TestSearchUpstream_EmptyQueryIs400(t *testing.T) {
	t.Parallel()

	reg := &stubRegistry{plugins: []plugins.Plugin{fixtureWithSearch("wikipedia", nil)}}
	rec := doUpstreamSearch(t, reg, map[string]any{"query": "   "})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "query is required")
}

// TestSearchUpstream_PluginErrorSurfacesInStatus pins the
// partial-results semantic: a plugin returning an error doesn't
// fail the call; the error message lands in per_plugin_status.
func TestSearchUpstream_PluginErrorSurfacesInStatus(t *testing.T) {
	t.Parallel()

	good := fixtureWithSearch("wikipedia", []plugins.SearchCandidate{{ID: "Brass", Label: "Brass"}})
	bad := &fixture.Plugin{
		NameValue: "broken",
		CapabilitiesValue: plugins.Capabilities{Name: "broken", SupportsSearch: true},
		SearchError:       errors.New("upstream is down"),
	}

	reg := &stubRegistry{plugins: []plugins.Plugin{good, bad}}
	rec := doUpstreamSearch(t, reg, map[string]any{"query": "Brass"})
	require.Equal(t, http.StatusOK, rec.Code, "partial-results semantic: bad plugin must NOT fail the call")

	var resp searchUpstreamResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.PerPlugin, 2)

	// Find the broken plugin's status.
	var brokenStatus *searchUpstreamPluginStatus
	for i := range resp.PerPlugin {
		if resp.PerPlugin[i].Plugin == "broken" {
			brokenStatus = &resp.PerPlugin[i]
		}
	}
	require.NotNil(t, brokenStatus)
	assert.False(t, brokenStatus.OK)
	assert.Contains(t, brokenStatus.ErrorMessage, "upstream is down")
	assert.Equal(t, 0, brokenStatus.Candidates)

	// Good plugin's results still surfaced.
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "wikipedia", resp.Results[0].Plugin)
}

// TestSearchUpstream_DeclarationOrderPreserved pins the merge
// semantic: results stream out in the registry's plugin-order,
// not in goroutine-completion order.
func TestSearchUpstream_DeclarationOrderPreserved(t *testing.T) {
	t.Parallel()

	slow := &fixture.Plugin{
		NameValue: "slow",
		CapabilitiesValue: plugins.Capabilities{Name: "slow", SupportsSearch: true},
		SearchFunc: func(ctx context.Context, _ string, _ int) ([]plugins.SearchCandidate, error) {
			time.Sleep(20 * time.Millisecond)
			return []plugins.SearchCandidate{{ID: "S", Label: "slow-result"}}, nil
		},
	}
	fast := fixtureWithSearch("fast", []plugins.SearchCandidate{{ID: "F", Label: "fast-result"}})

	// Registry order: slow first, fast second. Even though fast's
	// goroutine completes first, results must arrive slow→fast.
	reg := &stubRegistry{plugins: []plugins.Plugin{slow, fast}}
	rec := doUpstreamSearch(t, reg, map[string]any{"query": "x"})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp searchUpstreamResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Results, 2)
	assert.Equal(t, "slow", resp.Results[0].Plugin, "declaration order: slow before fast")
	assert.Equal(t, "fast", resp.Results[1].Plugin)
}

// TestSearchUpstream_PerPluginTimeoutBounds pins the timeout: a
// plugin that exceeds per_plugin_timeout_seconds gets cancelled
// and surfaces an error in per_plugin_status without affecting
// other plugins. Uses a 1s timeout + a plugin that sleeps 2s.
func TestSearchUpstream_PerPluginTimeoutBounds(t *testing.T) {
	t.Parallel()

	slow := &fixture.Plugin{
		NameValue:         "slow",
		CapabilitiesValue: plugins.Capabilities{Name: "slow", SupportsSearch: true},
		SearchFunc: func(ctx context.Context, _ string, _ int) ([]plugins.SearchCandidate, error) {
			select {
			case <-time.After(2 * time.Second):
				return []plugins.SearchCandidate{{ID: "should-not-arrive"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	fast := fixtureWithSearch("fast", []plugins.SearchCandidate{{ID: "F", Label: "ok"}})

	reg := &stubRegistry{plugins: []plugins.Plugin{slow, fast}}
	rec := doUpstreamSearch(t, reg, map[string]any{
		"query":                      "x",
		"per_plugin_timeout_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp searchUpstreamResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.PerPlugin, 2)

	// Slow plugin must have errored on context deadline.
	for _, s := range resp.PerPlugin {
		if s.Plugin == "slow" {
			assert.False(t, s.OK, "slow plugin must surface a context-deadline failure")
			assert.NotEmpty(t, s.ErrorMessage)
		}
	}

	// Fast plugin's result still in the merged list.
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "fast", resp.Results[0].Plugin)
}

// TestSearchUpstream_DefaultLimitApplied pins that an absent limit
// resolves to the default (no explicit failure on omission).
func TestSearchUpstream_DefaultLimitApplied(t *testing.T) {
	t.Parallel()

	reg := &stubRegistry{plugins: []plugins.Plugin{fixtureWithSearch("wikipedia", nil)}}
	rec := doUpstreamSearch(t, reg, map[string]any{"query": "x"})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp searchUpstreamResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, upstreamDefaultLimit, resp.Limit)
	assert.Equal(t, int(upstreamDefaultPerPluginTimeout/time.Second), resp.TimeoutSec)
}

// TestSearchUpstream_MalformedJSONIs400 pins the decoder error
// surface.
func TestSearchUpstream_MalformedJSONIs400(t *testing.T) {
	t.Parallel()

	reg := &stubRegistry{plugins: []plugins.Plugin{fixtureWithSearch("wikipedia", nil)}}
	req := httptest.NewRequest(http.MethodPost, "/v1/search/upstream",
		strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handleSearchUpstream(logger, reg).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "malformed JSON")
}
