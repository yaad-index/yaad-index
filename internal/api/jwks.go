package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/auth"
)

// JWKS endpoint. Serves the verifier's public key so peer agents can
// verify yaad-index-issued tokens without out-of-band key sharing.
// Public route — no auth middleware (the public-key-serving endpoint
// MUST be reachable without a token, otherwise the bootstrap is
// circular).
//
// Single-key v1: the JWKS document carries exactly one JWK whose
// `kid` matches what is stamped on issued tokens. Multi-key rotation
// is forward-compatible (the `keys` array allows it) but not
// implemented in v1.

// jwksDocument is the wire shape of /v1/jwks per RFC 7517 §5. The
// `keys` array carries one JWK per published verification key; the
// shape is forward-compatible with key rotation.
type jwksDocument struct {
	Keys []auth.JWK `json:"keys"`
}

// jwksCacheControl bounds how long peers cache the JWKS document.
// One hour matches the RFC 7517 example and is acceptable for our
// scale: rotation requires a redeploy (out of scope for v1) so the
// cache window doesn't conflict with a hot-rotation flow that doesn't
// exist yet.
const jwksCacheControl = "public, max-age=3600"

// handleJWKS returns the /v1/jwks handler. The slice is cached at
// startup by main.go (auth.LoadJWKS); the handler does not re-read
// disk per request. A nil/empty slice is treated as a server-side
// configuration error and surfaces as 503 — registering the route
// without keys would be a confusing 200 with `{"keys":[]}`.
func handleJWKS(logger *slog.Logger, keys []auth.JWK) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(keys) == 0 {
			writeError(w, http.StatusServiceUnavailable, "jwks_unavailable",
				"server is configured without verification keys; /v1/jwks has nothing to serve")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", jwksCacheControl)
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(jwksDocument{Keys: keys}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/jwks response", "err", err)
		}
	}
}
