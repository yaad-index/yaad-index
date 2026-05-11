package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/golang-jwt/jwt/v5"
)

// Per alice2-index a prior PR: the auth scaffold is the building
// blocks for a prior PR's HTTP middleware. Tests cover keypair
// generation + Sign-then-Verify round trip + the failure modes
// reviewers will reasonably ask about (expiry rejection,
// wrong-key rejection, missing-key rejection).

func newKeysDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))
	return d
}

func TestGenerateKeypair_WritesPrivateAndPublic(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))

	priv, err := os.ReadFile(filepath.Join(d, "private.pem"))
	require.NoError(t, err)
	assert.Contains(t, string(priv), "-----BEGIN PRIVATE KEY-----")
	pub, err := os.ReadFile(filepath.Join(d, "public.pem"))
	require.NoError(t, err)
	assert.Contains(t, string(pub), "-----BEGIN PUBLIC KEY-----")

	// Private key should be 0600 (owner-readable only).
	info, err := os.Stat(filepath.Join(d, "private.pem"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"private.pem must be 0600 — load-bearing for security")
}

func TestGenerateKeypair_RefusesOverwriteWithoutForce(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))
	err := GenerateKeypair(d, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestGenerateKeypair_OverwritesWithForce(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))
	first, err := os.ReadFile(filepath.Join(d, "private.pem"))
	require.NoError(t, err)

	require.NoError(t, GenerateKeypair(d, true))
	second, err := os.ReadFile(filepath.Join(d, "private.pem"))
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "force overwrite must produce a new key")
}

// Regression: os.WriteFile keeps the existing mode when overwriting
// an existing file. If a prior run left private.pem at 0644 (or an
// operator chmod'd it by accident), --force must restore 0600 — the
// security guarantee the keygen subcommand advertises.
func TestGenerateKeypair_ForceCorrectsBadPrivateKeyMode(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, GenerateKeypair(d, false))
	privPath := filepath.Join(d, "private.pem")
	require.NoError(t, os.Chmod(privPath, 0o644))

	require.NoError(t, GenerateKeypair(d, true))
	info, err := os.Stat(privPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"--force must restore 0600 on a pre-existing private.pem")
}

func TestGenerateKeypair_RejectsNonexistentDir(t *testing.T) {
	t.Parallel()
	err := GenerateKeypair(filepath.Join(t.TempDir(), "does-not-exist"), false)
	require.Error(t, err)
}

func TestSignVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	d := newKeysDir(t)
	signer, err := LoadSigner(d)
	require.NoError(t, err)
	verifier, err := LoadVerifier(d)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	in := Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}
	tok, err := signer.Sign(in)
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	out, err := verifier.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "bob", out.Subject)
	assert.Equal(t, "alice", out.Operator)
	assert.Equal(t, in.IssuedAt.Unix(), out.IssuedAt.Unix())
	assert.Equal(t, in.ExpiresAt.Unix(), out.ExpiresAt.Unix())
	assert.Equal(t, defaultKeyID, out.KeyID, "default kid stamped when caller leaves it empty")
}

func TestSign_RejectsEmptyFields(t *testing.T) {
	t.Parallel()
	signer, err := LoadSigner(newKeysDir(t))
	require.NoError(t, err)

	_, err = signer.Sign(Claim{Operator: "alice", ExpiresAt: time.Now().Add(time.Hour)})
	require.Error(t, err)
	_, err = signer.Sign(Claim{Subject: "bob", ExpiresAt: time.Now().Add(time.Hour)})
	require.Error(t, err)
	_, err = signer.Sign(Claim{Subject: "bob", Operator: "alice"})
	require.Error(t, err) // empty ExpiresAt
}

func TestVerify_RejectsExpiredToken(t *testing.T) {
	t.Parallel()
	d := newKeysDir(t)
	signer, err := LoadSigner(d)
	require.NoError(t, err)
	verifier, err := LoadVerifier(d)
	require.NoError(t, err)

	past := time.Now().UTC().Add(-time.Hour)
	tok, err := signer.Sign(Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: past.Add(-time.Hour),
		ExpiresAt: past, // expired one hour ago
	})
	require.NoError(t, err)

	_, err = verifier.Verify(tok)
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, jwt.ErrTokenExpired) || errString(err, "expired"),
		"expired token must be rejected with a clear error; got %v", err)
}

func TestVerify_RejectsWrongKeypair(t *testing.T) {
	t.Parallel()
	dA := newKeysDir(t)
	dB := newKeysDir(t)

	signerA, err := LoadSigner(dA)
	require.NoError(t, err)
	verifierB, err := LoadVerifier(dB)
	require.NoError(t, err)

	tok, err := signerA.Sign(Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	_, err = verifierB.Verify(tok)
	require.Error(t, err, "token signed by A must not verify with B's public key")
}

func TestVerify_RejectsTokenWithWrongIssuer(t *testing.T) {
	t.Parallel()
	d := newKeysDir(t)
	verifier, err := LoadVerifier(d)
	require.NoError(t, err)

	// Hand-roll a token with iss != "alice2-index" using the same
	// private key — the verifier must reject on the issuer check
	// even though the signature is valid.
	priv := loadRawPrivateKey(t, d)
	bad := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "bob",
		"operator": "alice",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
		"iss": "definitely-not-alice2-index",
	})
	bad.Header["kid"] = "test"
	tok, err := bad.SignedString(priv)
	require.NoError(t, err)

	_, err = verifier.Verify(tok)
	require.Error(t, err, "token with wrong iss must be rejected")
}

func TestLoadSigner_MissingKeyError(t *testing.T) {
	t.Parallel()
	_, err := LoadSigner(t.TempDir())
	require.Error(t, err)
}

func TestLoadVerifier_MissingKeyError(t *testing.T) {
	t.Parallel()
	_, err := LoadVerifier(t.TempDir())
	require.Error(t, err)
}

func TestLoadSigner_RejectsBadPEM(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(d, "private.pem"),
		[]byte("not a pem file"), 0o600))
	_, err := LoadSigner(d)
	require.Error(t, err)
}

func TestSign_StampsKeyIDOnHeader(t *testing.T) {
	t.Parallel()
	d := newKeysDir(t)
	signer, err := LoadSigner(d)
	require.NoError(t, err)
	verifier, err := LoadVerifier(d)
	require.NoError(t, err)

	tok, err := signer.Sign(Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
		KeyID: "rotated-key-2",
	})
	require.NoError(t, err)

	out, err := verifier.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "rotated-key-2", out.KeyID,
		"caller-supplied kid round-trips on the JOSE header")
}

// loadRawPrivateKey is a test-only helper for hand-rolling
// unusual tokens (wrong-issuer, etc.).
func loadRawPrivateKey(t *testing.T, dir string) *rsa.PrivateKey {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "private.pem"))
	require.NoError(t, err)
	block, _ := pem.Decode(body)
	require.NotNil(t, block)
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	rsaPriv, ok := priv.(*rsa.PrivateKey)
	require.True(t, ok)
	return rsaPriv
}

func errString(err error, sub string) bool {
	return err != nil && containsFold(err.Error(), sub)
}

func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a := s[i+j]
			b := sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
