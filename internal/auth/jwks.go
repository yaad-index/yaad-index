package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// JSON Web Key Set support per alice2-index (a prior PR of the auth
// series). Serves the verifier's public key to peer agents so they
// can verify alice2-index-issued tokens without out-of-band key sharing.
//
// Single-key v1: LoadJWKS always returns a one-entry slice. The
// `keys` array shape (per RFC 7517) is forward-compatible with key
// rotation — a future multi-key PR can return additional entries
// without changing the wire shape.

// JWK is one entry in a JSON Web Key Set per RFC 7517 §4. Field
// names match the spec exactly; JSON encoding is implicit via the
// json tags so the api handler can encode the slice directly.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N string `json:"n"`
	E string `json:"e"`
}

// LoadJWKS reads public.pem from keysDir and returns the JWKS shape
// suitable for /v1/jwks. Single-key v1: the returned slice always
// contains exactly one JWK and the `kid` field matches the constant
// a prior PR stamps on issued tokens (`alice2-index-default`).
//
// Caller-side caching is expected — main.go calls LoadJWKS once at
// startup and passes the slice to api.WithJWKS. The handler does not
// re-read the disk per request.
func LoadJWKS(keysDir string) ([]JWK, error) {
	pub, err := loadPublicKey(keysDir)
	if err != nil {
		return nil, err
	}
	return []JWK{publicKeyToJWK(pub, defaultKeyID)}, nil
}

// loadPublicKey reads public.pem from keysDir and returns the parsed
// *rsa.PublicKey. Same parse path as LoadVerifier (PKIX-encoded);
// extracted so JWKS construction can share the loader without
// allocating a Verifier wrapper it doesn't need.
func loadPublicKey(keysDir string) (*rsa.PublicKey, error) {
	if keysDir == "" {
		return nil, errors.New("auth.loadPublicKey: empty keysDir")
	}
	path := filepath.Join(keysDir, publicKeyFilename)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth.loadPublicKey: read %s: %w", path, err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("auth.loadPublicKey: %s: PEM decode failed", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth.loadPublicKey: %s: %w", path, err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("auth.loadPublicKey: %s: key is not RSA", path)
	}
	return rsaPub, nil
}

// publicKeyToJWK converts an *rsa.PublicKey to a JWK per RFC 7518 §6.3
// (RSA key parameter encoding):
//
// - `n` is the modulus, base64url-encoded big-endian unsigned integer.
// - `e` is the public exponent, same encoding.
//
// `kty=RSA`, `alg=RS256`, `use=sig` are constants for the v1 single-
// key configuration; rotation / multiple-use will revisit these.
func publicKeyToJWK(pub *rsa.PublicKey, kid string) JWK {
	return JWK{
		Kty: "RSA",
		Use: "sig",
		Kid: kid,
		Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
