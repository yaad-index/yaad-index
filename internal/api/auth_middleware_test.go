package api

import (
	"context"
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
)

// authTestKeys returns a freshly-generated keys dir for one test —
// avoids pulling in test fixtures that would couple the middleware
// to the keygen surface area beyond what a prior PR already covers.
func authTestKeys(t *testing.T) (auth.Signer, auth.Verifier) {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(d, false))
	signer, err := auth.LoadSigner(d)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(d)
	require.NoError(t, err)
	return signer, verifier
}

// quietLogger drops middleware DEBUG log noise from test output but
// keeps the slog handler shape so the middleware's structured logging
// doesn't crash on a nil handler.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// claimEcho is the inner handler for middleware tests — emits a small
// JSON payload that surfaces what was attached to context, so tests
// can assert both 200/401 status AND the claim plumbing without
// needing a full handler.
func claimEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := ClaimFromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if !ok || c == nil {
			_, _ = w.Write([]byte(`{"claim":null}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"claim": map[string]string{
				"sub": c.Subject,
				"operator": c.Operator,
				"kid": c.KeyID,
			},
		})
	})
}

func signTestToken(t *testing.T, s auth.Signer, ttl time.Duration) string {
	t.Helper()
	now := time.Now().UTC()
	tok, err := s.Sign(auth.Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now,
		ExpiresAt: now.Add(ttl),
	})
	require.NoError(t, err)
	return tok
}

func decodeError(t *testing.T, body io.Reader) errorResponse {
	t.Helper()
	var er errorResponse
	require.NoError(t, json.NewDecoder(body).Decode(&er))
	return er
}

func TestRequireAuth_RejectsMissingHeader(t *testing.T) {
	t.Parallel()
	_, verifier := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifier)(claimEcho())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	er := decodeError(t, w.Body)
	assert.False(t, er.OK)
	assert.Equal(t, "missing_authorization", er.Error)
}

func TestRequireAuth_RejectsMalformedHeader(t *testing.T) {
	t.Parallel()
	_, verifier := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifier)(claimEcho())

	cases := []string{
		"NotBearer abc",
		"Bearer", // no token after the scheme
		"Bearer ", // empty token
		"Basic dXNlcjpwd", // wrong scheme
	}
	for _, hv := range cases {
		t.Run(hv, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", hv)
			h.ServeHTTP(w, r)
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Equal(t, "missing_authorization", decodeError(t, w.Body).Error)
		})
	}
}

func TestRequireAuth_AcceptsValidToken(t *testing.T) {
	t.Parallel()
	signer, verifier := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifier)(claimEcho())

	tok := signTestToken(t, signer, time.Hour)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
	var got struct {
		Claim map[string]string `json:"claim"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&got))
	assert.Equal(t, "bob", got.Claim["sub"])
	assert.Equal(t, "alice", got.Claim["operator"])
}

func TestRequireAuth_BearerSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	signer, verifier := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifier)(claimEcho())
	tok := signTestToken(t, signer, time.Hour)

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", scheme+" "+tok)
			h.ServeHTTP(w, r)
			assert.Equal(t, http.StatusOK, w.Code, scheme)
		})
	}
}

func TestRequireAuth_RejectsExpiredToken(t *testing.T) {
	t.Parallel()
	signer, verifier := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifier)(claimEcho())

	tok := signTestToken(t, signer, -time.Hour) // already expired

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "token_expired", decodeError(t, w.Body).Error)
}

func TestRequireAuth_RejectsTokenSignedByDifferentKeypair(t *testing.T) {
	t.Parallel()
	// Two independent keypairs; sign with A, verify with B.
	signerA, _ := authTestKeys(t)
	_, verifierB := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifierB)(claimEcho())

	tok := signTestToken(t, signerA, time.Hour)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	// jwt/v5 returns ErrTokenSignatureInvalid for cross-keypair mismatch;
	// the middleware maps it to wrong_key for operator clarity.
	got := decodeError(t, w.Body)
	assert.Contains(t, []string{"wrong_key", "invalid_token"}, got.Error,
		"signature mismatch must produce wrong_key (or fall through to invalid_token)")
}

func TestRequireAuth_RejectsMalformedToken(t *testing.T) {
	t.Parallel()
	_, verifier := authTestKeys(t)
	h := RequireAuth(quietLogger(), verifier)(claimEcho())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not-a-real-jwt")
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "invalid_token", decodeError(t, w.Body).Error)
}

func TestAnonymousAuth_AttachesSyntheticClaim(t *testing.T) {
	t.Parallel()
	h := AnonymousAuth()(claimEcho())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil) // no header
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code,
		"AnonymousAuth must pass through unauthenticated requests")
	var got struct {
		Claim map[string]string `json:"claim"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&got))
	assert.Equal(t, "anonymous", got.Claim["sub"])
	assert.Equal(t, "none", got.Claim["operator"])
}

// The cold-reviewer a prior PR review note 1: AnonymousAuth must construct a fresh
// Claim per request — a shared pointer would race once a prior PR adds
// per-request identity fields. This pins the per-request-copy
// invariant by capturing the *Claim from two requests through a
// custom inner handler and asserting they are distinct allocations.
func TestAnonymousAuth_FreshClaimPerRequest(t *testing.T) {
	t.Parallel()
	var seen []*auth.Claim
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := ClaimFromContext(r.Context())
		seen = append(seen, c)
		w.WriteHeader(http.StatusOK)
	})
	h := AnonymousAuth()(capture)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		h.ServeHTTP(rec, req)
	}
	require.Len(t, seen, 2)
	require.NotNil(t, seen[0])
	require.NotNil(t, seen[1])
	assert.NotSame(t, seen[0], seen[1],
		"AnonymousAuth must allocate a fresh *Claim per request — "+
			"shared pointer would race in a prior PR once per-request fields land")
	// Sanity: the synthetic shape is identical even though pointers differ.
	assert.Equal(t, seen[0].Subject, seen[1].Subject)
	assert.Equal(t, seen[0].Operator, seen[1].Operator)
}

func TestClaimFromContext_NoMiddleware_ReturnsFalse(t *testing.T) {
	t.Parallel()
	c, ok := ClaimFromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, c)
}

// extractBearer is exercised indirectly by TestRequireAuth_*Header — the
// table here pins behavior on edge cases that don't naturally appear in
// the middleware-level tests (whitespace handling).
func TestExtractBearer_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	tok, ok := extractBearer("Bearer abc.def.ghi ")
	assert.True(t, ok)
	assert.Equal(t, "abc.def.ghi", tok)

	tok, ok = extractBearer(strings.Repeat(" ", 5))
	assert.False(t, ok)
	assert.Equal(t, "", tok)
}
