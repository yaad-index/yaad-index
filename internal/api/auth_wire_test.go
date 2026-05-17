package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/store"
)

// Wire-level integration tests for the auth middleware (per
// yaad-index a prior PR). These exercise NewHandlerWithRegistry with
// WithAuthVerifier + WithAuthRequired so the route-level wiring (which
// routes are protected, which stay public) is pinned end-to-end —
// not just the standalone middleware unit tests.
//
// The protected/public split is documented in adr/0002-api-surface.md;
// this file is the regression guard against accidentally moving a route
// across that line.

func newAuthAPI(t *testing.T) (http.Handler, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	d := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(d, false))
	signer, err := auth.LoadSigner(d)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(d)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
	)
	return h, signer
}

func newDevModeAPI(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithAuthRequired(false),
	)
}

func validBearer(t *testing.T, s auth.Signer) string {
	t.Helper()
	now := time.Now().UTC()
	tok, err := s.Sign(auth.Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	return "Bearer " + tok
}

// publicRoutes are reachable without a token by design — system
// metadata, no vault data. Pinned here so adding a new public route
// requires updating this list AND the doc.
//
// /v1/jwks lives outside this list — registration depends on whether
// keys are loaded (per yaad-index a prior PR), so it can't be asserted
// against newAuthAPI which doesn't pass WithJWKS. The dedicated
// jwks_test.go suite covers the public-route invariant for it.
var publicRoutes = []struct {
	method string
	path string
}{
	{http.MethodGet, "/v1/health"},
	{http.MethodGet, "/v1/structure"},
	{http.MethodGet, "/v1/cv-status"},
}

// protectedRoutes pin every existing protected path so a future PR
// that accidentally drops protect() trips this test. Each entry's
// body is whatever survives the routing layer; the assertions only
// look at status codes (401 vs not-401) so handler-shape changes
// don't churn this list.
var protectedRoutes = []struct {
	method string
	path string
	body string // optional body for POSTs
}{
	{http.MethodGet, "/v1/kinds", ""},
	{http.MethodGet, "/v1/plugins", ""},
	{http.MethodPost, "/v1/entities/batch", `{"ids":["x"]}`},
	{http.MethodGet, "/v1/entities/batch", ""}, // 405 carve-out is also protected
	{http.MethodGet, "/v1/entities/some-id", ""},
	{http.MethodGet, "/v1/entities/some-id/context", ""},
	{http.MethodPost, "/v1/edges", `{"from":"a","to":"b","type":"x"}`},
	{http.MethodGet, "/v1/search?q=x", ""},
	{http.MethodPost, "/v1/ingest", `{"url":"http://x"}`},
	{http.MethodGet, "/v1/needs-fill", ""},
	{http.MethodPost, "/v1/entities/some-id/fill", `{}`},
	{http.MethodPost, "/v1/entities/some-id/notes", `{"author":"bob","body":"hi"}`},
}

func TestAuthWiring_PublicRoutes_AccessibleWithoutToken(t *testing.T) {
	t.Parallel()
	h, _ := newAuthAPI(t)

	for _, route := range publicRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"%s %s must NOT 401 without auth (it's public): body=%s",
				route.method, route.path, rec.Body.String())
		})
	}
}

func TestAuthWiring_ProtectedRoutes_401WithoutToken(t *testing.T) {
	t.Parallel()
	h, _ := newAuthAPI(t)

	for _, route := range protectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path,
				strings.NewReader(route.body))
			if route.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code,
				"%s %s must 401 without token: body=%s",
				route.method, route.path, rec.Body.String())
			var er errorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&er))
			assert.Equal(t, "missing_authorization", er.Error)
		})
	}
}

func TestAuthWiring_ProtectedRoutes_PassWithValidToken(t *testing.T) {
	t.Parallel()
	h, signer := newAuthAPI(t)
	bearer := validBearer(t, signer)

	// We don't assert 200 — most of these need fixtures (entities, edges,
	// stores) that don't exist in the bare in-memory test setup. We
	// assert "not 401" — proving the middleware let the request through
	// even when the downstream handler 404s or 400s.
	for _, route := range protectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path,
				strings.NewReader(route.body))
			if route.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Authorization", bearer)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"%s %s must NOT 401 with valid token (got %d): body=%s",
				route.method, route.path, rec.Code, rec.Body.String())
		})
	}
}

func TestAuthWiring_DevMode_ProtectedRoutesPassThrough(t *testing.T) {
	t.Parallel()
	// auth.required=false → AnonymousAuth bypass; no token needed even
	// on protected routes.
	h := newDevModeAPI(t)
	for _, route := range protectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path,
				strings.NewReader(route.body))
			if route.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"%s %s must NOT 401 in dev-mode (auth.required=false): body=%s",
				route.method, route.path, rec.Body.String())
		})
	}
}

// The cold-reviewer a prior PR review note 2: WithAuthRequired(true) without
// WithAuthVerifier(...) is a programmer error (bad test wiring or a
// missing main.go option). buildAuthMiddleware now panics rather
// than silently falling back to AnonymousAuth, which would have
// shipped an unauth'd server in the bad-config case.
func TestAuthWiring_RequiredWithoutVerifier_Panics(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	assert.Panics(t, func() {
		_ = NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
			WithAuthRequired(true), // no verifier — must panic
		)
	}, "construction must panic on (required=true, verifier=nil) — masking this would ship an unauth server")
}

func TestAuthWiring_ExpiredToken_Rejected(t *testing.T) {
	t.Parallel()
	h, signer := newAuthAPI(t)
	now := time.Now().UTC()
	expiredTok, err := signer.Sign(auth.Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	req.Header.Set("Authorization", "Bearer "+expiredTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	var er errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&er))
	assert.Equal(t, "token_expired", er.Error)
}
