package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yaad-index/yaad-index/internal/auth"
)

// Bearer-token authentication middleware (per yaad-index, a prior PR of
// the auth series). RequireAuth extracts `Authorization: Bearer <token>`,
// verifies via the auth.Verifier from a prior PR, attaches the parsed Claim to
// the request context, and emits a canonical 401 envelope on any failure.
//
// AnonymousAuth is the dev-mode bypass wired when `auth.required=false`:
// it attaches a synthetic anonymous Claim and skips verification entirely.
// The operator opts in explicitly; the server logs a startup warning so
// running with auth disabled is never silent.
//
// Public routes (health, structure, cv-status, jwks-future) are wired
// without either middleware in api.go and stay accessible without a token.

const (
	authzHeader = "Authorization"
	bearerScheme = "Bearer "
)

// claimKeyType keeps the context key collision-free without exporting a
// string. ClaimFromContext is the only reader.
type claimKeyType struct{}

var claimKey claimKeyType

// ClaimFromContext returns the pair-claim attached by RequireAuth (or the
// synthetic anonymous claim from AnonymousAuth in dev mode), and a bool
// indicating presence. Public routes that bypass both middlewares return
// (nil, false).
func ClaimFromContext(ctx context.Context) (*auth.Claim, bool) {
	v, ok := ctx.Value(claimKey).(*auth.Claim)
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// withClaim attaches a Claim to the context. Internal — handlers read
// via ClaimFromContext.
func withClaim(ctx context.Context, c *auth.Claim) context.Context {
	return context.WithValue(ctx, claimKey, c)
}

// RequireAuth is the production middleware: every request must carry a
// valid Bearer JWT. On success the parsed Claim is attached to the
// request context. On failure the canonical 401 envelope is emitted with
// one of the codes:
//
// - missing_authorization — header absent or not "Bearer <token>"
// - token_expired — exp claim in the past
// - wrong_key — signature doesn't verify against public.pem
// - invalid_token — anything else (malformed, wrong issuer, …)
//
// Verification failures are logged at DEBUG with the request id so
// operators can correlate without leaking token contents at INFO/WARN.
func RequireAuth(logger *slog.Logger, verifier auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := extractBearer(r.Header.Get(authzHeader))
			if !ok {
				writeError(w, http.StatusUnauthorized, "missing_authorization",
					"missing or malformed Authorization: Bearer <token> header")
				return
			}
			claim, err := verifier.Verify(tok)
			if err != nil {
				code, msg := classifyVerifyError(err)
				logger.DebugContext(r.Context(), "auth: verify failed",
					"err", err.Error(),
					"code", code,
					"request_id", RequestIDFromContext(r.Context()),
				)
				writeError(w, http.StatusUnauthorized, code, msg)
				return
			}
			next.ServeHTTP(w, r.WithContext(withClaim(r.Context(), claim)))
		})
	}
}

// AnonymousAuth is the dev-mode bypass middleware. Wired when the
// operator sets `auth.required=false` (or `--auth-required=false` /
// `YAAD_INDEX_AUTH_REQUIRED=false`). Attaches a synthetic anonymous
// Claim (sub=anonymous, operator=none) to the context so downstream
// handlers that dereference ClaimFromContext continue to work, and
// passes through unconditionally.
//
// The synthetic claim's IssuedAt / ExpiresAt are deliberately the zero
// time so handlers can detect dev-mode shape if they need to (a prior PR
// will surface the agent identity on comment writes).
//
// A fresh *Claim is constructed per request — the cold-reviewer's a prior PR review
// note 1: a prior PR may extend the Claim with per-request identity fields,
// and a shared pointer would race or leak across requests once that
// happens.
func AnonymousAuth() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(withClaim(r.Context(), newAnonymousClaim())))
		})
	}
}

// newAnonymousClaim returns a fresh anonymous-shape Claim. Callers
// own the returned pointer — no aliasing across requests.
func newAnonymousClaim() *auth.Claim {
	return &auth.Claim{
		Subject: anonymousSubject,
		Operator: anonymousOperator,
		KeyID: "anonymous",
	}
}

const (
	anonymousSubject = "anonymous"
	anonymousOperator = "none"
)

// IsAnonymousClaim reports whether the claim is the synthetic shape
// stamped by AnonymousAuth in dev-mode (auth.required=false). Handlers
// that derive enforcement from the claim's identity (e.g. comments
// author validation per yaad-index) branch on this so dev-mode
// preserves pre-auth behavior — there is no real identity to enforce
// against in the bypass path.
//
// Production RequireAuth never produces this shape: the verifier
// rejects tokens whose `iss` is not "yaad-index", and signed tokens
// carry the operator's real keypair-stamped subject + operator.
func IsAnonymousClaim(c *auth.Claim) bool {
	return c != nil && c.Subject == anonymousSubject && c.Operator == anonymousOperator
}

// ClaimHasOperatorAuthority reports whether the claim represents an
// identity authorized to take operator-attributed actions on behalf
// of a real human operator. The pair-claim model (yaad-index):
// every authenticated request carries (Subject, Operator). Two valid
// shapes hold operator authority:
//
// - Operator acting directly: Subject == Operator (the operator
// issued + signed their own token).
// - Agent acting on behalf of an operator: Subject is the agent
// identity; Operator names the human the agent is conduit for.
//
// Both shapes route through this helper; the only thing the gate
// rejects is a claim with no operator at all (agent-only token, or
// missing operator field). Anonymous dev-mode claims also fail the
// gate — there's no real operator to attribute the action to.
//
// Replaces the brittle Subject==Operator check that existed at the
// operator-fill + comments call sites legacy. Those endpoints used
// to reject all agent-on-behalf-of-operator JWTs even though the
// operator authority was structurally present on the pair-claim.
func ClaimHasOperatorAuthority(c *auth.Claim) bool {
	if c == nil {
		return false
	}
	if IsAnonymousClaim(c) {
		return false
	}
	return c.Operator != ""
}

// ClaimIsOperatorOnly reports whether the claim is the operator-only
// shape required for CLI dispatch (command-shape input on
// /v1/ingest, per yaad-index + ADR-0022 §6).
//
// Operator-only = Subject == Operator: the operator issued and
// signed their own token, no agent intermediary. Pair-claim tokens
// (Subject = agent, Operator = human, distinct values) are
// REJECTED at this gate even though they hold pair-claim operator
// authority — command-shape dispatch is too privileged to delegate.
//
// Anonymous dev-mode claims pass this gate (treated as
// operator-equivalent for permissive dev-mode behavior — operators
// running with `auth.required=false` shouldn't suddenly need real
// tokens to invoke command-shape inputs that worked under
// AnonymousAuth before).
//
// Replaces no prior check — this is the new gate introduces.
// Production /v1/ingest's command-shape dispatch path calls this;
// URL-shape continues to gate on ClaimHasOperatorAuthority.
func ClaimIsOperatorOnly(c *auth.Claim) bool {
	if c == nil {
		return false
	}
	if IsAnonymousClaim(c) {
		// Dev-mode bypass: the operator running with auth.required
		// =false is the only actor; no real subject to enforce
		// against. Permit command-shape under the same shape that
		// permitted everything else.
		return true
	}
	return c.IsOperatorOnly()
}

// extractBearer parses an Authorization header value of the form
// `Bearer <token>`. The scheme match is case-insensitive (RFC 6750
// §2.1); the token portion is taken as-is after a single space, with
// surrounding whitespace trimmed. Returns ("", false) on any
// malformation — empty header, wrong scheme, missing token.
func extractBearer(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if len(value) < len(bearerScheme) {
		return "", false
	}
	if !strings.EqualFold(value[:len(bearerScheme)], bearerScheme) {
		return "", false
	}
	tok := strings.TrimSpace(value[len(bearerScheme):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// classifyVerifyError maps a wrapped verifier error onto the 401
// envelope's error code + human message. The verifier wraps every
// underlying jwt error with an "auth: verify:" prefix; errors.Is
// unwraps cleanly so we can branch on the canonical sentinel set.
//
// Anything we don't recognize falls through to invalid_token — we
// don't surface internal error text to clients, only the classified
// code + a short generic message.
func classifyVerifyError(err error) (code, message string) {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "token_expired", "token has expired"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "wrong_key", "token signature does not verify"
	case errors.Is(err, jwt.ErrTokenMalformed),
		errors.Is(err, jwt.ErrTokenInvalidIssuer),
		errors.Is(err, jwt.ErrTokenNotValidYet),
		errors.Is(err, jwt.ErrTokenUnverifiable):
		return "invalid_token", "token is not valid"
	default:
		return "invalid_token", "token is not valid"
	}
}
