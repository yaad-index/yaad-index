// Cut-1 end-to-end test for the MCP-over-HTTP route per
// #101. Exercises the JWT auth gate + the bridge round-trip:
// a Bearer-authed MCP `tools/call` for `get_entity` should
// return the entity's JSON shape; an unauthenticated call
// should be rejected by the auth middleware.

package api_test

import (
	"bytes"
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

	"github.com/yaad-index/yaad-index/internal/api"
	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/store"
)

// newAuthedMCPHandler stands up a daemon mux with auth.required
// enabled + a real RS256 signer so the test can mint a Bearer
// JWT that the /mcp route accepts. Returns the handler + the
// signer + the temp store so callers can seed entities.
func newAuthedMCPHandler(t *testing.T) (http.Handler, auth.Signer, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	h := api.NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st,
		nil,
		api.WithAuthVerifier(verifier),
		api.WithAuthRequired(true),
		api.WithMCPServerVersion("test-0.0.0"),
	)
	return h, signer, st
}

// mintMCPToken signs an agent/operator pair-claim JWT for
// the MCP test fixture. Mirrors the existing mintToken
// pattern from notes_auth_test.go but lives in the MCP test
// file so the package-test boundary is clean.
func mintMCPToken(t *testing.T, signer auth.Signer, agent, operator string) string {
	t.Helper()
	now := time.Now().UTC()
	tok, err := signer.Sign(auth.Claim{
		Subject:   agent,
		Operator:  operator,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	return tok
}

// mcpCall sends an MCP JSON-RPC request to /mcp with an
// optional Bearer token. Returns the recorded HTTP response
// for the test to inspect.
func mcpCall(t *testing.T, h http.Handler, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMCP_GetEntity_JWTAuthSucceeds (#101 Cut 1): the
// bedrock JWT-path-end-to-end test the pre-implementation
// review required — Cut 1 must exercise the auth path on
// a real authenticated tool, not a dummy one. Seeds an
// entity, mints a Bearer token, fires `tools/call` for
// `get_entity`, asserts the recorded entity surfaces in the
// MCP result.
func TestMCP_GetEntity_JWTAuthSucceeds(t *testing.T) {
	t.Parallel()
	h, signer, st := newAuthedMCPHandler(t)

	const id = "wikipedia:test-entity"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   id,
		Kind: "wikipedia",
		Data: map[string]any{
			"id":    id,
			"title": "Test Entity",
		},
	}))

	token := mintMCPToken(t, signer, "agent:alice", "alice")

	rec := mcpCall(t, h, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_entity",
			"arguments": map[string]any{"id": id},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "MCP /mcp returns 200; body=%s", rec.Body.String())

	// The MCP library wraps the tool result in JSON-RPC
	// envelope shape; the daemon's JSON response body lands
	// in the first content entry's text field.
	var env struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env),
		"decode MCP response; body=%s", rec.Body.String())
	require.Nil(t, env.Error, "MCP result is not an error envelope")
	require.False(t, env.Result.IsError, "tool result is not flagged as error; body=%s", rec.Body.String())
	require.NotEmpty(t, env.Result.Content)
	body := env.Result.Content[0].Text
	assert.Contains(t, body, id,
		"tool result carries the entity id (bridge round-tripped the JSON)")
	assert.Contains(t, body, "Test Entity",
		"tool result carries the entity data")
}

// TestMCP_GetEntity_NoAuthRejected (#101 Cut 1): a request
// to /mcp without a Bearer token is rejected by the auth
// middleware BEFORE the MCP server sees the JSON-RPC body.
// Verifies the protect() chain applies to /mcp.
func TestMCP_GetEntity_NoAuthRejected(t *testing.T) {
	t.Parallel()
	h, _, _ := newAuthedMCPHandler(t)

	rec := mcpCall(t, h, "" /*no token*/, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "get_entity", "arguments": map[string]any{"id": "x"}},
	})
	assert.GreaterOrEqual(t, rec.Code, 400,
		"unauthenticated MCP request rejected; got %d body=%s", rec.Code, rec.Body.String())
	assert.Less(t, rec.Code, 500)
}

// TestMCP_GetEntity_NotFound_SurfacesAsErrorResult
// (#101 Cut 1): a tool call against a non-existent entity
// surfaces the inner HTTP 404 as an MCP error result (not
// a JSON-RPC error — tool errors live inside the result
// envelope per MCP spec).
func TestMCP_GetEntity_NotFound_SurfacesAsErrorResult(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	rec := mcpCall(t, h, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_entity",
			"arguments": map[string]any{"id": "wikipedia:never-existed"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var env struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.True(t, env.Result.IsError,
		"tool result flagged isError for inner 404; body=%s", rec.Body.String())
	require.NotEmpty(t, env.Result.Content)
	assert.True(t, strings.Contains(env.Result.Content[0].Text, "404") ||
		strings.Contains(env.Result.Content[0].Text, "not_found"),
		"error text mentions 404 / not_found; got %q", env.Result.Content[0].Text)
}

// TestMCP_ToolsList_ReturnsGetEntity (#101 Cut 1):
// `tools/list` enumerates the registered tools — should
// surface get_entity. Confirms the registration call from
// internal/mcp landed.
func TestMCP_ToolsList_ReturnsGetEntity(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	rec := mcpCall(t, h, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var env struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	names := make([]string, len(env.Result.Tools))
	for i, t := range env.Result.Tools {
		names[i] = t.Name
	}
	assert.Contains(t, names, "get_entity",
		"tools/list returns the registered get_entity tool; got %v", names)
}

// TestMCP_ExpiredJWTRejected (#173): a JWT whose
// ExpiresAt is in the past is rejected by the outer
// protect() middleware before the MCP server is reached.
// Pairs with TestMCP_GetEntity_NoAuthRejected — together
// they prove the auth gate fires on both the missing-
// token AND the expired-token paths.
func TestMCP_ExpiredJWTRejected(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)

	now := time.Now().UTC()
	expiredTok, err := signer.Sign(auth.Claim{
		Subject:   "agent:alice",
		Operator:  "alice",
		IssuedAt:  now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	})
	require.NoError(t, err)

	rec := mcpCall(t, h, expiredTok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_entity",
			"arguments": map[string]any{"id": "x"},
		},
	})
	assert.GreaterOrEqual(t, rec.Code, 400,
		"expired-token MCP request rejected; got %d body=%s", rec.Code, rec.Body.String())
	assert.Less(t, rec.Code, 500)
}

// TestMCP_TamperedJWTRejected (#173): a JWT signed by a
// SECOND keypair the daemon doesn't trust is rejected.
// The wire shape is a valid JWT in every respect except
// the signature key — proves the verifier checks the
// signature against its loaded public key, not just the
// claim shape. A regression that swapped the verifier for
// a no-op (or matched on `iss` instead of signature) would
// be caught here.
func TestMCP_TamperedJWTRejected(t *testing.T) {
	t.Parallel()
	h, _, _ := newAuthedMCPHandler(t)

	// Generate a SECOND keypair the daemon has never seen;
	// sign a structurally-valid claim with it.
	otherKeyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(otherKeyDir, false))
	otherSigner, err := auth.LoadSigner(otherKeyDir)
	require.NoError(t, err)

	now := time.Now().UTC()
	tamperedTok, err := otherSigner.Sign(auth.Claim{
		Subject:   "agent:alice",
		Operator:  "alice",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)

	rec := mcpCall(t, h, tamperedTok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_entity",
			"arguments": map[string]any{"id": "x"},
		},
	})
	assert.GreaterOrEqual(t, rec.Code, 400,
		"wrong-key MCP request rejected; got %d body=%s", rec.Code, rec.Body.String())
	assert.Less(t, rec.Code, 500)
}
