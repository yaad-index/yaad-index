package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JWKS round-trip tests (per yaad-index a prior PR). The defining
// invariant: a peer that fetches /v1/jwks must be able to reconstruct
// an *rsa.PublicKey from `n` + `e` and use it to verify a token
// signed by the corresponding private key. These tests exercise that
// without going through the HTTP layer (the handler tests in
// internal/api/jwks_test.go cover the wire shape).

func TestLoadJWKS_ShapeFields(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))

	keys, err := LoadJWKS(d)
	require.NoError(t, err)
	require.Len(t, keys, 1, "v1 single-key — slice has exactly one entry")

	jwk := keys[0]
	assert.Equal(t, "RSA", jwk.Kty)
	assert.Equal(t, "sig", jwk.Use)
	assert.Equal(t, defaultKeyID, jwk.Kid,
		"kid must match the constant a prior PR stamps on issued tokens")
	assert.Equal(t, "RS256", jwk.Alg)
	assert.NotEmpty(t, jwk.N, "modulus base64url-encoded")
	assert.NotEmpty(t, jwk.E, "exponent base64url-encoded")
}

func TestLoadJWKS_RoundTripsToVerifyingPublicKey(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))

	signer, err := LoadSigner(d)
	require.NoError(t, err)
	keys, err := LoadJWKS(d)
	require.NoError(t, err)

	// Reconstruct *rsa.PublicKey from JWK.
	pub := jwkToPublicKey(t, keys[0])

	// Sign a token with the private key, verify with a Verifier
	// constructed from the reconstructed public key — proves the JWK
	// fields encode the same key bits as a prior PR's verifier loads.
	now := time.Now().UTC()
	tok, err := signer.Sign(Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)

	v := &rs256Verifier{pub: pub}
	out, err := v.Verify(tok)
	require.NoError(t, err, "JWK-reconstructed public key must verify tokens signed with the matching private key")
	assert.Equal(t, "bob", out.Subject)
	assert.Equal(t, "alice", out.Operator)
}

// jwkToPublicKey is a test-only helper that mirrors what a JWKS
// consumer (peer agent) would do: base64url-decode `n` and `e`, then
// construct the RSA public key from those big-endian bytes. If a
// future change drifts from RFC 7518 §6.3 encoding, this helper
// breaks loudly.
func jwkToPublicKey(t *testing.T, jwk JWK) *rsa.PublicKey {
	t.Helper()
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	require.NoError(t, err)
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	require.NoError(t, err)
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}
}

func TestLoadJWKS_MissingKeysDir(t *testing.T) {
	t.Parallel()
	_, err := LoadJWKS(filepath.Join(t.TempDir(), "no-such-dir"))
	require.Error(t, err)
}

func TestLoadJWKS_EmptyKeysDir(t *testing.T) {
	t.Parallel()
	_, err := LoadJWKS("")
	require.Error(t, err)
}

func TestLoadJWKS_BadPEM(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(d, "public.pem"),
		[]byte("not a pem file"), 0o644))
	_, err := LoadJWKS(d)
	require.Error(t, err)
}
