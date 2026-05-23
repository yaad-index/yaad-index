package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// newRoutingValidationFixture builds a handler with a pair of
// fixture plugins whose Capabilities + Match closures pin the
// shapes the routing-validation tests need:
//
// - "bgg" plugin: url_patterns require `^bgg:\s*\d+$` (digits only).
// A `bgg: https://wikipedia.com/foo` input should reject — the
// pattern doesn't match.
// - "gmail" plugin: declares commands `["fetch"]`. A `gmail: !sync`
// should reject; `gmail: !fetch` should pass.
//
// spawns counts subprocess invocations so tests can assert the
// reject path skipped the spawn.
func newRoutingValidationFixture(t *testing.T) (http.Handler, *atomic.Int32) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	var spawns atomic.Int32
	registry := plugins.NewRegistry()

	// "bgg" — strict shorthand-only matcher: bgg:<digits>.
	registry.Register(&fixture.Plugin{
		NameValue: "bgg",
		MatchFunc: func(rawURL string) bool {
			// Only `bgg:<whitespace>?<digits>` — digits-only id,
			// no URL-shaped suffixes.
			s := strings.TrimSpace(strings.TrimPrefix(rawURL, "bgg:"))
			if s == rawURL {
				return false // no bgg: prefix
			}
			if s == "" {
				return false
			}
			for _, r := range s {
				if r < '0' || r > '9' {
					return false
				}
			}
			return true
		},
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			spawns.Add(1)
			return onEnvelope(&plugins.FetchResult{
				Entity: &store.Entity{
					ID: "bgg:42",
					Kind: "source",
					Data: map[string]any{"name": "Test"},
				},
				Provenance: []store.ProvenanceEntry{{Source: "bgg:fetch", OK: true}},
			})
		},
		CapabilitiesValue: plugins.Capabilities{
			Name: "bgg",
			SourceNamespace: "bgg",
			EntityKinds: []plugins.KindSpec{{Name: "source"}},
		},
	})

	// "gmail" — command-only plugin declaring commands: ["fetch"].
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			spawns.Add(1)
			return nil
		},
		CapabilitiesValue: plugins.Capabilities{
			Name: "gmail",
			SourceNamespace: "gmail",
			EntityKinds: []plugins.KindSpec{{Name: "source"}},
			Commands: []plugins.CommandSpec{{Name: "fetch"}},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry)
	return h, &spawns
}

// TestRoutingValidation_URLShapeMismatchRejects400 pins ADR-0022
// §4: an input naming a registered plugin whose url_patterns don't
// match returns 400 invalid_input, no subprocess spawned.
func TestRoutingValidation_URLShapeMismatchRejects400(t *testing.T) {
	t.Parallel()
	h, spawns := newRoutingValidationFixture(t)

	rec := postIngest(t, h, map[string]any{
		"url": "bgg: https://wikipedia.com/foo",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_input",
		"error envelope must use invalid_input code")
	assert.Contains(t, rec.Body.String(), "bgg",
		"error message must name the plugin")
	assert.Equal(t, int32(0), spawns.Load(),
		"routing-time rejection must skip the subprocess spawn")
}

// TestRoutingValidation_CommandShapeMismatchRejects400 pins the
// command-list contract: a command not in the plugin's declared
// list returns 400 invalid_input.
func TestRoutingValidation_CommandShapeMismatchRejects400(t *testing.T) {
	t.Parallel()
	h, spawns := newRoutingValidationFixture(t)

	rec := postIngest(t, h, map[string]any{
		"url": "gmail: !sync",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_input")
	assert.Contains(t, rec.Body.String(), "sync",
		"error message must name the rejected command")
	assert.Equal(t, int32(0), spawns.Load(),
		"routing-time rejection must skip the subprocess spawn")
}

// TestRoutingValidation_CommandShapeUnknownPluginRejects404 pins
// the plugin-not-found contract: command-shape input naming an
// unregistered plugin returns 404 plugin_not_found.
func TestRoutingValidation_CommandShapeUnknownPluginRejects404(t *testing.T) {
	t.Parallel()
	h, spawns := newRoutingValidationFixture(t)

	rec := postIngest(t, h, map[string]any{
		"url": "no-such-plugin: !whatever",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "plugin_not_found")
	assert.Contains(t, rec.Body.String(), "no-such-plugin")
	assert.Equal(t, int32(0), spawns.Load(),
		"unknown-plugin command-shape must skip spawn")
}

// TestRoutingValidation_ValidURLShapePassesThrough pins the
// happy-path: a URL-shape input whose plugin's url_patterns DO
// match passes validation. The input still has to make it through
// the rest of the dispatch path (notation cache, tracker, etc.) —
// here we just confirm validation didn't block.
func TestRoutingValidation_ValidURLShapePassesThrough(t *testing.T) {
	t.Parallel()
	h, spawns := newRoutingValidationFixture(t)

	// `bgg: 42` matches the fixture's `^bgg:<digits>$` pattern.
	rec := postIngest(t, h, map[string]any{
		"url": "bgg: 42",
		"wait_seconds": 2,
	})
	// Whatever the dispatch produces (Complete, fetch_failed, etc.)
	// is fine — we just want NOT a 400/404 from routing validation.
	assert.NotEqual(t, http.StatusBadRequest, rec.Code,
		"valid URL-shape must not reject at routing-time (body=%s)", rec.Body.String())
	assert.NotEqual(t, http.StatusNotFound, rec.Code,
		"valid URL-shape must not reject at routing-time (body=%s)", rec.Body.String())
	assert.GreaterOrEqual(t, spawns.Load(), int32(1),
		"valid URL-shape must reach subprocess spawn")
}

// TestRoutingValidation_ValidCommandShapePassesThrough pins the
// happy-path for command-shape: `gmail: !fetch` against a plugin
// declaring commands=["fetch"] passes validation. Post-#52 the
// dispatch fork also routes the input to the named plugin via
// LookupByName, so the call reaches the gmail fixture. The fixture
// emits no envelopes, so the tracker surfaces 404 not_found — a
// DIFFERENT 404 than validateRouting's "plugin_not_found"
// rejection. This test pins that distinction by checking the
// error code in the body, not the bare HTTP status.
func TestRoutingValidation_ValidCommandShapePassesThrough(t *testing.T) {
	t.Parallel()
	h, _ := newRoutingValidationFixture(t)

	rec := postIngest(t, h, map[string]any{
		"url": "gmail: !fetch",
		"wait_seconds": 1,
	})
	// validateRouting must not reject. Both validator-side codes
	// (invalid_input @ 400, plugin_not_found @ 404) would mean the
	// validator wrongly rejected a valid command-shape. The dispatch
	// fork's downstream behavior (here: 404 not_found because the
	// fixture emits zero envelopes) is unrelated to validation.
	assert.NotContains(t, rec.Body.String(), "invalid_input",
		"valid command-shape must not reject at validation (body=%s)", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "plugin_not_found",
		"valid command-shape must not reject at validation (body=%s)", rec.Body.String())
}

// TestRoutingValidation_PlainURLPassesThrough pins that inputs
// without a recognized plugin namespace (raw URLs like
// `https://example.com/foo`) skip validation and fall through to
// the existing first-match-wins registry walk. The "https"
// pseudo-namespace from ParseInvocation isn't a registered plugin
// name so validateRouting returns nil.
func TestRoutingValidation_PlainURLPassesThrough(t *testing.T) {
	t.Parallel()
	h, _ := newRoutingValidationFixture(t)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.com/foo",
		"wait_seconds": 1,
	})
	// Neither bgg nor gmail's MatchFunc accepts this URL → 422
	// unsupported_url from the existing code path. NOT 400/404.
	assert.NotEqual(t, http.StatusBadRequest, rec.Code,
		"plain URL must not reject at routing-time (body=%s)", rec.Body.String())
	assert.NotEqual(t, http.StatusNotFound, rec.Code,
		"plain URL must not reject at routing-time (body=%s)", rec.Body.String())
}

// TestValidateRouting_DirectShapes pins the routing-validator's
// behavior at the unit level (without driving the HTTP layer) so
// regressions on a corner case surface as a focused failure rather
// than a downstream end-to-end miss.
func TestValidateRouting_DirectShapes(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "p1",
		MatchFunc: func(s string) bool { return s == "p1: ok-input" },
		CapabilitiesValue: plugins.Capabilities{
			Name: "p1",
			Commands: []plugins.CommandSpec{{Name: "go"}, {Name: "stop"}},
		},
	})

	cases := []struct {
		name string
		input string
		wantNil bool
		wantCode string
		wantStat int
	}{
		{"command in list", "p1: !go", true, "", 0},
		{"command not in list", "p1: !run", false, "invalid_input", 400},
		{"unknown plugin command-shape", "ghost: !go", false, "plugin_not_found", 404},
		{"url-shape match", "p1: ok-input", true, "", 0},
		{"url-shape mismatch", "p1: bad-input", false, "invalid_input", 400},
		{"plain URL falls through", "https://example.com/foo", true, "", 0},
		{"no namespace falls through", "no-colon-here", true, "", 0},
		{"namespace not registered URL-shape falls through", "https: nothing", true, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRouting(registry, tc.input)
			if tc.wantNil {
				assert.Nil(t, err, "input=%q should pass validation", tc.input)
				return
			}
			require.NotNil(t, err, "input=%q should reject", tc.input)
			assert.Equal(t, tc.wantCode, err.Code)
			assert.Equal(t, tc.wantStat, err.Status)
		})
	}
}
