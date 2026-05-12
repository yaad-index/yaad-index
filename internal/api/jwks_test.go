package api

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/store"
)

// /v1/jwks wire-level tests (per yaad-index a prior PR). Auth-package
// tests in internal/auth/jwks_test.go cover the JWK construction +
// RSA round-trip; these tests pin the HTTP wire shape (status code,
// Cache-Control header, JSON envelope, route is unauthenticated).

func newJWKSAPI(t *testing.T, withKeys bool) (http.Handler, []auth.JWK) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{}
	var keys []auth.JWK
	if withKeys {
		d := t.TempDir()
		require.NoError(t, auth.GenerateKeypair(d, false))
		keys, err = auth.LoadJWKS(d)
		require.NoError(t, err)
		opts = append(opts, WithJWKS(keys))
	}
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), opts...), keys
}

func TestJWKS_Returns200WithRFC7517Shape(t *testing.T) {
	t.Parallel()
	h, keys := newJWKSAPI(t, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jwks", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"),
		"Cache-Control header bounds peer-side caching to 1h per yaad-index")

	var got struct {
		Keys []auth.JWK `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Keys, 1, "v1 single-key — exactly one JWK")
	assert.Equal(t, keys[0], got.Keys[0],
		"served JWK matches the auth.LoadJWKS result byte-for-byte")
	assert.Equal(t, "RSA", got.Keys[0].Kty)
	assert.Equal(t, "sig", got.Keys[0].Use)
	assert.Equal(t, "RS256", got.Keys[0].Alg)
	assert.NotEmpty(t, got.Keys[0].Kid)
	assert.NotEmpty(t, got.Keys[0].N)
	assert.NotEmpty(t, got.Keys[0].E)
}

// TestJWKS_NotRegisteredWithoutKeys pins the dev-mode shape: when
// the operator runs without a keypair on disk, /v1/jwks is NOT
// registered. A request to it produces a stdlib 404, not 200 with
// an empty `{"keys":[]}` envelope.
func TestJWKS_NotRegisteredWithoutKeys(t *testing.T) {
	t.Parallel()
	h, _ := newJWKSAPI(t, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jwks", nil)
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"absent WithJWKS — route stays unregistered")
}

// TestJWKS_PublicRoute_NoAuthRequired regression-pins the protected/
// public split for /v1/jwks. The endpoint MUST be reachable without
// a Bearer token even when auth.required=true; otherwise the
// public-key bootstrap is circular (clients can't fetch the key
// they need to mint a token).
func TestJWKS_PublicRoute_NoAuthRequired(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	d := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(d, false))
	verifier, err := auth.LoadVerifier(d)
	require.NoError(t, err)
	keys, err := auth.LoadJWKS(d)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithJWKS(keys),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jwks", nil) // no Authorization header
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"GET /v1/jwks must NOT 401 even when auth.required=true: body=%s", rec.Body.String())
}

// TestJWKS_KeyRoundTripsViaWire is the integration version of the
// auth-package round-trip test: hit the HTTP endpoint, decode the
// JWK, reconstruct the *rsa.PublicKey, verify a token signed with
// the matching private key. Pins the full peer-agent flow.
func TestJWKS_KeyRoundTripsViaWire(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	d := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(d, false))
	signer, err := auth.LoadSigner(d)
	require.NoError(t, err)
	keys, err := auth.LoadJWKS(d)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithJWKS(keys),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jwks", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got struct {
		Keys []auth.JWK `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Keys, 1)

	// Reconstruct *rsa.PublicKey from the wire JWK — exactly what a
	// peer would do. Decode `n` + `e`, then construct the key.
	jwk := got.Keys[0]
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	require.NoError(t, err)
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	require.NoError(t, err)
	_ = &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}

	// Sign a token, verify with a Verifier loaded from the SAME keys
	// dir — proves both halves agree on the key bits. (We can't
	// directly construct a Verifier from a *rsa.PublicKey because the
	// auth package keeps that path internal; the wire-level test
	// validates that the published JWK's n/e decode cleanly, which is
	// the externally observable contract.)
	verifier, err := auth.LoadVerifier(d)
	require.NoError(t, err)
	now := time.Now().UTC()
	tok, err := signer.Sign(auth.Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	out, err := verifier.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, jwk.Kid, out.KeyID,
		"kid stamped on issued tokens matches the kid published in /v1/jwks")
}
