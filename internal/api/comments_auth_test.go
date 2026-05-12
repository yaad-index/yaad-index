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
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Author-validation tests for POST /v1/entities/{id}/comments per
// yaad-index a prior PR. Production runs through RequireAuth — these
// tests wire that path explicitly so the strict identity check fires.
// The dev-mode (AnonymousAuth) path is exercised by the existing
// comments_test.go corpus, which keeps passing because IsAnonymousClaim
// short-circuits the enforcement.

const commentsAuthTestEntityID = "boardgame:auth-comment-test"

// newAuthedCommentsFixture builds a vault-wired handler protected by
// RequireAuth, plus the signer the test uses to mint Bearer tokens.
// Mirrors newCommentsFixture's seed pattern but adds an auth keypair
// and threads the verifier through NewHandlerWithRegistry.
func newAuthedCommentsFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
	)

	seedEntity(t, st, commentsAuthTestEntityID, "boardgame")
	require.NoError(t, w.Write(&vault.Entity{
		ID: commentsAuthTestEntityID,
		Kind: "boardgame",
		Plugin: "test-fixture",
		Data: map[string]any{"id": commentsAuthTestEntityID, "title": "Auth Comment Test"},
	}))
	return h, st, root, signer
}

func mintToken(t *testing.T, signer auth.Signer, agent, operator string) string {
	t.Helper()
	now := time.Now().UTC()
	tok, err := signer.Sign(auth.Claim{
		Subject: agent,
		Operator: operator,
		IssuedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	return tok
}

func postAuthedComment(t *testing.T, h http.Handler, id, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/entities/" + id + "/comments"
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCommentsAuth_EmptyAuthor_FilledFromClaim(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedCommentsFixture(t)
	tok := mintToken(t, signer, "bob", "alice")

	rec := postAuthedComment(t, h, commentsAuthTestEntityID, tok, map[string]any{
		"text": "no author specified — middleware fills it",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "bob", got.Comment.Author, "empty author defaulted to JWT sub")
	assert.Equal(t, "alice", got.Comment.Operator, "operator stamped from JWT operator claim")

	// Vault file mirrors the stamps.
	v := readVaultByID(t, root, "boardgame", commentsAuthTestEntityID)
	require.Len(t, v.Comments, 1)
	assert.Equal(t, "bob", v.Comments[0].Author)
	assert.Equal(t, "alice", v.Comments[0].Operator)
}

func TestCommentsAuth_MatchingAuthor_StoresBoth(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedCommentsFixture(t)
	tok := mintToken(t, signer, "bob", "alice")

	rec := postAuthedComment(t, h, commentsAuthTestEntityID, tok, map[string]any{
		"text": "explicit author matching the JWT",
		"author": "bob",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "bob", got.Comment.Author)
	assert.Equal(t, "alice", got.Comment.Operator)

	v := readVaultByID(t, root, "boardgame", commentsAuthTestEntityID)
	require.Len(t, v.Comments, 1)
	assert.Equal(t, "bob", v.Comments[0].Author)
	assert.Equal(t, "alice", v.Comments[0].Operator)
}

func TestCommentsAuth_MismatchedAuthor_403(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedCommentsFixture(t)
	tok := mintToken(t, signer, "bob", "alice")

	rec := postAuthedComment(t, h, commentsAuthTestEntityID, tok, map[string]any{
		"text": "alice2 trying to post as bob",
		"author": "alice2", // disagrees with the JWT sub
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	var er errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&er))
	assert.Equal(t, "author_mismatch", er.Error)

	// Vault file must NOT have grown.
	v := readVaultByID(t, root, "boardgame", commentsAuthTestEntityID)
	assert.Empty(t, v.Comments, "rejected comment must not land in vault")
}

func TestCommentsAuth_MissingToken_401(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newAuthedCommentsFixture(t)

	target := "/v1/entities/" + commentsAuthTestEntityID + "/comments"
	body := strings.NewReader(`{"text":"no token"}`)
	req := httptest.NewRequest(http.MethodPost, target, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	var er errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&er))
	assert.Equal(t, "missing_authorization", er.Error)
}

// Regression: even when the client matches the JWT sub, an expired
// token must still 401 — the middleware rejects before reaching the
// handler's author check.
func TestCommentsAuth_ExpiredTokenWithMatchingAuthor_401(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedCommentsFixture(t)

	// Sign a token in the past so verification fails on `exp`.
	now := time.Now().UTC()
	expiredTok, err := signer.Sign(auth.Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	})
	require.NoError(t, err)

	rec := postAuthedComment(t, h, commentsAuthTestEntityID, expiredTok, map[string]any{
		"text": "valid author but token is expired",
		"author": "bob",
	})
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	var er errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&er))
	assert.Equal(t, "token_expired", er.Error)
}

// Sanity: ClaimFromContext returns the verified claim downstream of
// RequireAuth so a prior PR can read it without re-parsing the header.
func TestCommentsAuth_ClaimReachesHandler(t *testing.T) {
	t.Parallel()
	// Smaller in-process check on the middleware → handler chain.
	// Builds a verifier-backed handler that just reflects the claim
	// it found on context.
	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := ClaimFromContext(r.Context())
		require.True(t, ok)
		require.NotNil(t, c)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sub": c.Subject,
			"operator": c.Operator,
		})
	})
	h := RequireAuth(logger, verifier)(inner)

	tok := mintToken(t, signer, "bob", "alice")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "bob", got["sub"])
	assert.Equal(t, "alice", got["operator"])
}

// Compile-time guard: storing the claim on context survives request
// derivation (WithContext) so handlers don't have to re-parse the
// Authorization header. The wire-test corpus pins the API; this
// helper-level test pins the type signature.
var _ = func() *auth.Claim {
	c, _ := ClaimFromContext(context.Background())
	return c
}
