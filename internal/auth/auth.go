// Package auth scaffolds alice2-index's pair-claim JWT authentication
// (per alice2-index a prior PR of the auth series). RS256 signing /
// verification + RS256 keypair generation + helpers used by the
// `alice2-index keygen` and `alice2-index issue-token` CLI subcommands.
//
// Pair-claim model (designed with the operator 2026-05-05): every token
// carries a `sub` (the agent — the actor that called the API)
// AND an `operator` (the human — the resource owner). The OAuth
// resource-owner / client split. Audit trail is operator+agent
// for every action; revocation can target an individual agent
// without rotating the operator-side trust.
//
// **Out of scope for a prior PR**: HTTP middleware that consumes these
// helpers (a prior PR =), comment author validation (a prior PR =),
// `/v1/jwks` public-key endpoint (a prior PR =). This package
// lands the building blocks; the wire-side integration follows.
//
// **Security note**: the private key MUST live alongside
// operational config (default `/etc/alice2-index/keys/`), never
// inside the vault. Per The operator: "the key should not be in vault.
// Then an agent can trick index to return it and then it is bad."

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Default key id for v1 — single shared kid per ADR-0013-style
// "default to a single shared kid in v1; rotation is a future
// follow-up." Per-pair kids land in a later PR.
const defaultKeyID = "alice2-index-default"

// Issuer is the `iss` claim stamped on every token. Constant
// across the cluster; verification rejects tokens carrying any
// other issuer.
const tokenIssuer = "alice2-index"

// Filenames within the configured keys directory. Locked so
// keygen + LoadSigner + LoadVerifier agree without a separate
// config field.
const (
	privateKeyFilename = "private.pem"
	publicKeyFilename = "public.pem"
)

// rsaKeySize is the RS256 modulus bits. 2048 is the OpenSSL
// default and meets current cryptographic guidance through 2030+.
// 4096 is overkill for v1; if a future operator needs it, lift
// this to a flag on `alice2-index keygen`.
const rsaKeySize = 2048

// Claim is the pair-claim payload (per alice2-index). A
// signed JWT carries this verbatim; verification returns it
// after signature + issuer + expiry checks pass.
type Claim struct {
	Subject string // agent (e.g. "bob")
	Operator string // human (e.g. "alice")
	IssuedAt time.Time // `iat`
	ExpiresAt time.Time // `exp`
	KeyID string // `kid` — for rotation
}

// IsOperatorOnly reports whether this claim represents an operator
// acting directly (not via an agent). Per alice2-index, CLI
// dispatch endpoints (command-shape input on /v1/ingest) require
// operator-only authority — the operator's command-issue surface
// is too privileged to delegate to an agent.
//
// "Operator-only" here means Subject == Operator: both halves of
// the pair-claim name the same identity, signaling the operator
// signed + issued their own token. Pair-claim tokens (Subject =
// agent, Operator = human, distinct values) represent
// agent-on-behalf-of-operator authority — sufficient for most
// daemon endpoints (operator-fill, comments, ingest URL-shape) but
// NOT for command-shape dispatch.
//
// The check is structural — no separate JWT field is added. An
// operator issuing a token with `--operator alice --agent alice`
// gets an operator-only claim; `--operator alice --agent bob`
// gets a pair-claim. The existing JWT format is preserved per
// ADR-0022 §6's "extend, don't fork" guidance.
//
// Returns false on:
// - nil receiver
// - empty Subject or Operator
// - Subject != Operator (agent-on-behalf pair-claim)
// - anonymous claims (caller branches on those separately
// via api.IsAnonymousClaim)
func (c *Claim) IsOperatorOnly() bool {
	if c == nil {
		return false
	}
	if c.Subject == "" || c.Operator == "" {
		return false
	}
	return c.Subject == c.Operator
}

// Signer signs a Claim into a JWT string.
type Signer interface {
	Sign(c Claim) (string, error)
}

// Verifier verifies a JWT string and returns the parsed Claim
// (or an error explaining why the token is invalid).
type Verifier interface {
	Verify(token string) (*Claim, error)
}

// rs256Signer wraps an RSA private key for signing. Constructed
// via LoadSigner; never holds raw key bytes longer than the
// constructor.
type rs256Signer struct {
	priv *rsa.PrivateKey
}

func (s *rs256Signer) Sign(c Claim) (string, error) {
	if c.Subject == "" {
		return "", errors.New("auth: empty Subject (agent)")
	}
	if c.Operator == "" {
		return "", errors.New("auth: empty Operator")
	}
	if c.IssuedAt.IsZero() {
		c.IssuedAt = time.Now().UTC()
	}
	if c.ExpiresAt.IsZero() {
		return "", errors.New("auth: empty ExpiresAt")
	}
	if c.KeyID == "" {
		c.KeyID = defaultKeyID
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": c.Subject,
		"operator": c.Operator,
		"iat": c.IssuedAt.Unix(),
		"exp": c.ExpiresAt.Unix(),
		"iss": tokenIssuer,
	})
	tok.Header["kid"] = c.KeyID
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		return "", fmt.Errorf("auth: sign: %w", err)
	}
	return signed, nil
}

// rs256Verifier wraps an RSA public key for verification.
type rs256Verifier struct {
	pub *rsa.PublicKey
}

func (v *rs256Verifier) Verify(raw string) (*Claim, error) {
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.pub, nil
	}, jwt.WithIssuer(tokenIssuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("auth: verify: %w", err)
	}
	if !tok.Valid {
		return nil, errors.New("auth: token marked invalid by parser")
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("auth: claims are not MapClaims")
	}
	out, err := claimFromMap(mc, tok.Header)
	if err != nil {
		return nil, fmt.Errorf("auth: extract claim: %w", err)
	}
	return out, nil
}

// claimFromMap pulls every required field out of the parsed
// claims map + JOSE header. The jwt library validates iss / exp
// for us via parser options; we just have to extract.
func claimFromMap(mc jwt.MapClaims, header map[string]any) (*Claim, error) {
	sub, err := stringField(mc, "sub")
	if err != nil {
		return nil, err
	}
	op, err := stringField(mc, "operator")
	if err != nil {
		return nil, err
	}
	iat, err := timeField(mc, "iat")
	if err != nil {
		return nil, err
	}
	exp, err := timeField(mc, "exp")
	if err != nil {
		return nil, err
	}
	kid, _ := header["kid"].(string)
	if kid == "" {
		kid = defaultKeyID
	}
	return &Claim{
		Subject: sub,
		Operator: op,
		IssuedAt: iat,
		ExpiresAt: exp,
		KeyID: kid,
	}, nil
}

func stringField(mc jwt.MapClaims, key string) (string, error) {
	v, ok := mc[key]
	if !ok {
		return "", fmt.Errorf("missing %q", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%q is not a non-empty string", key)
	}
	return s, nil
}

func timeField(mc jwt.MapClaims, key string) (time.Time, error) {
	v, ok := mc[key]
	if !ok {
		return time.Time{}, fmt.Errorf("missing %q", key)
	}
	switch n := v.(type) {
	case float64:
		return time.Unix(int64(n), 0).UTC(), nil
	case int64:
		return time.Unix(n, 0).UTC(), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return time.Time{}, fmt.Errorf("%q: %w", key, err)
		}
		return time.Unix(i, 0).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("%q has unsupported type %T", key, v)
	}
}

// LoadSigner reads private.pem from keysDir and returns a
// configured RS256 Signer.
func LoadSigner(keysDir string) (Signer, error) {
	if keysDir == "" {
		return nil, errors.New("auth.LoadSigner: empty keysDir")
	}
	path := filepath.Join(keysDir, privateKeyFilename)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth.LoadSigner: read %s: %w", path, err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("auth.LoadSigner: %s: PEM decode failed", path)
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Fall back to PKCS1 for keys generated by older tooling
		// (openssl genrsa pre-OpenSSL-3.0 emits PKCS1 by default).
		priv1, err1 := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err1 != nil {
			return nil, fmt.Errorf("auth.LoadSigner: %s: parse private key (PKCS8 + PKCS1 both failed): %v / %v", path, err, err1)
		}
		return &rs256Signer{priv: priv1}, nil
	}
	rsaPriv, ok := priv.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("auth.LoadSigner: %s: key is not RSA", path)
	}
	return &rs256Signer{priv: rsaPriv}, nil
}

// LoadVerifier reads public.pem from keysDir and returns a
// configured RS256 Verifier.
func LoadVerifier(keysDir string) (Verifier, error) {
	if keysDir == "" {
		return nil, errors.New("auth.LoadVerifier: empty keysDir")
	}
	path := filepath.Join(keysDir, publicKeyFilename)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth.LoadVerifier: read %s: %w", path, err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("auth.LoadVerifier: %s: PEM decode failed", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth.LoadVerifier: %s: %w", path, err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("auth.LoadVerifier: %s: key is not RSA", path)
	}
	return &rs256Verifier{pub: rsaPub}, nil
}

// GenerateKeypair writes RS256 private + public keys to keysDir.
// Refuses to overwrite existing files unless force is true.
//
// Files land at:
//
//	<keysDir>/private.pem (PKCS PEM, mode 0600)
//	<keysDir>/public.pem (PKIX PEM, mode 0644)
//
// Mode 0600 on the private key is load-bearing — the file should
// be readable only by the alice2-index process. The directory is
// expected to exist; GenerateKeypair does not create it (the
// operator's deployment manifest mounts it).
func GenerateKeypair(keysDir string, force bool) error {
	if keysDir == "" {
		return errors.New("auth.GenerateKeypair: empty keysDir")
	}
	info, err := os.Stat(keysDir)
	if err != nil {
		return fmt.Errorf("auth.GenerateKeypair: stat %s: %w", keysDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("auth.GenerateKeypair: %s is not a directory", keysDir)
	}
	privPath := filepath.Join(keysDir, privateKeyFilename)
	pubPath := filepath.Join(keysDir, publicKeyFilename)
	if !force {
		for _, p := range []string{privPath, pubPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("auth.GenerateKeypair: %s already exists; pass force=true to overwrite", p)
			}
		}
	}

	priv, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return fmt.Errorf("auth.GenerateKeypair: rsa.GenerateKey: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("auth.GenerateKeypair: marshal PKCS8: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("auth.GenerateKeypair: write %s: %w", privPath, err)
	}
	// os.WriteFile preserves existing perms when the file already
	// existed — explicit Chmod forces 0600 even when --force is
	// re-running over a pre-existing wrong-mode private.pem.
	if err := os.Chmod(privPath, 0o600); err != nil {
		return fmt.Errorf("auth.GenerateKeypair: chmod %s: %w", privPath, err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return fmt.Errorf("auth.GenerateKeypair: marshal PKIX: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return fmt.Errorf("auth.GenerateKeypair: write %s: %w", pubPath, err)
	}
	return nil
}
