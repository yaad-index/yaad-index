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

	"github.com/yaad-index/yaad-index/internal/buildinfo"
)

// withBuildinfoVersion swaps buildinfo.Version for the duration of a
// test, restoring the prior value on cleanup. Lets the precedence
// tests below exercise the injected-Version branch without re-linking.
// Tests using this helper can't safely run with t.Parallel — package-
// level vars are shared.
func withBuildinfoVersion(t *testing.T, v string) {
	t.Helper()
	prev := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = prev })
}

// Test_Health_Returns200WithOK is the load-bearing assertion: the
// endpoint must serve a 200 with `ok: true`. Monitors and CI smoke
// tests treat a missing or non-200 /v1/health as "service is broken,"
// so this test exists to make a regression on the wiring or response
// shape trip loudly.
func Test_Health_Returns200WithOK(t *testing.T) {
	t.Parallel()

	h := newAPI(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body healthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode response")
	assert.True(t, body.OK)
	// version is best-effort (debug.ReadBuildInfo returns empty under
	// some `go test` paths); we only assert it's a string. Asserting a
	// concrete value would be flaky.
}

// Test_Health_NoStoreOrRegistryDependency proves the handler is a
// pure liveness probe — works without any store or plugin wiring. A
// /v1/health that requires a healthy store would force operators
// using alice2-index in degraded modes (e.g. read-only filesystem)
// into false-negative alerts.
func Test_Health_NoStoreOrRegistryDependency(t *testing.T) {
	t.Parallel()

	// Construct the handler directly without going through newAPI's
	// store wiring — proves the route + handler don't reach for a
	// store handle. (newAPI does construct a store; this test
	// constructs a handler that explicitly cannot use it.)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	handleHealth(logger).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// Test_readBuildVersion_PrefersInjectedWhenSet exercises the primary
// precedence branch: the Makefile's -ldflags injection sets
// buildinfo.Version to a real `<git-describe>+<short-hash>` string, and
// readBuildVersion must surface that verbatim instead of falling
// through to debug.ReadBuildInfo. Sequential (mutates a package-level
// var); the helper restores on cleanup.
func Test_readBuildVersion_PrefersInjectedWhenSet(t *testing.T) {
	withBuildinfoVersion(t, "v0.1.0+abc1234")

	assert.Equal(t, "v0.1.0+abc1234", readBuildVersion())
}

// Test_readBuildVersion_FallsBackWhenUnknown asserts the secondary
// branch: when buildinfo.Version is the sentinel "unknown" (no
// -ldflags injection happened), readBuildVersion does NOT surface
// "unknown" to clients — it falls through to debug.ReadBuildInfo so
// `go install` paths still get a meaningful version. The
// debug.ReadBuildInfo result is environment-dependent (returns "" in
// some `go test` paths, the module version in others), so we only
// assert that the sentinel doesn't leak.
func Test_readBuildVersion_FallsBackWhenUnknown(t *testing.T) {
	withBuildinfoVersion(t, buildinfo.Unknown)

	assert.NotEqual(t, buildinfo.Unknown, readBuildVersion(),
		"sentinel %q leaked to wire — fallback to debug.ReadBuildInfo failed", buildinfo.Unknown)
}

// Test_readBuildVersion_FallsBackOnUnknownPlusUnknown is the
// regression guard for the cold-reviewer's a prior PR finding: the original Makefile
// concat `$(GIT_TAG)+$(GIT_HASH)` with shell `|| echo "unknown"`
// fallbacks produced `"unknown+unknown"` when git was unavailable
// (release tarball, minimal container). That string is neither empty
// nor the bare Unknown sentinel, so it slipped past readBuildVersion's
// precedence guard and landed verbatim on /v1/health — strictly worse
// than the pre-fix state (where debug.ReadBuildInfo's pseudo-version
// was emitted). The Makefile now degrades to the bare "unknown" via
// `ifeq` so the existing guard handles it; this test pins that
// readBuildVersion does NOT surface "unknown+unknown" if it ever
// regresses back into buildinfo.Version.
func Test_readBuildVersion_FallsBackOnUnknownPlusUnknown(t *testing.T) {
	withBuildinfoVersion(t, "unknown+unknown")

	assert.NotEqual(t, "unknown+unknown", readBuildVersion(),
		"%q leaked to wire — Makefile concat regressed past the no-git guard",
		"unknown+unknown")
}
