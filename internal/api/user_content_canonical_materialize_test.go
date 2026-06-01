package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newCanonicalUGCFixture is newAuthedUGCFixture plus a canonical-kind
// registry (boardgame / person) so the section handlers can auto-
// materialize a never-filled thin-edge per ADR-0031 §4.
func newCanonicalUGCFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	rd, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {},
		"person":    {},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, rd),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)
	return h, st, root, signer
}

// TestCanonicalBody_AutoMaterialize_BodyOnEmptyThinEdge pins the
// ADR-0031 §4 + #388 test case: a canonical thin-edge with only a DB
// row (no vault file yet) accepts an operator-authored body. The read
// path returns an empty section set (not 404); the first section write
// materializes the vault file (ugc:true) and claims ownership.
func TestCanonicalBody_AutoMaterialize_BodyOnEmptyThinEdge(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalUGCFixture(t)

	// Thin DB row only — no vault file (the EnsureLabelRow shape, the
	// `data: null` boardgame the issue describes).
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   "boardgame:moon-colony-bloodbath",
		Kind: "boardgame",
	}))

	tok := mintToken(t, signer, "alice-agent", "alice")
	const id = "/v1/user-content/boardgame:moon-colony-bloodbath"

	// Read: empty section set, NOT a 404 (no body yet, no file made).
	rec := ugcReq(t, h, http.MethodGet, id+"/sections", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"read on a never-materialized canonical must be empty, not 404; body=%s", rec.Body.String())
	var page userContentSectionsPage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	// Empty body parses to just the implicit pre-heading section 0
	// (ADR-0012 containment model) — no authored/headed sections yet.
	for _, e := range page.Entries {
		assert.Empty(t, e.Heading, "no authored section yet on an unmaterialized canonical")
	}
	// The read must NOT have created a vault file on disk.
	rdr, err := vault.NewReader(root)
	require.NoError(t, err)
	_, statErr := rdr.ReadByID("boardgame", "boardgame:moon-colony-bloodbath")
	assert.True(t, vault.IsNotExist(statErr), "a read must not materialize the vault file")

	// First write materializes the vault file + claims ownership. The
	// empty body's etag is the If-Match for the first add.
	rec = ugcReq(t, h, http.MethodPost, id+"/sections", tok,
		map[string]any{"heading": "My take", "body": "Best worst game.\n"},
		map[string]string{"If-Match": userContentEtag("")},
	)
	require.Equal(t, http.StatusCreated, rec.Code,
		"first body write on an empty thin-edge must materialize + succeed; body=%s", rec.Body.String())

	// Vault file now exists, ugc:true, owned by the first writer.
	v := readVaultByID(t, root, "boardgame", "boardgame:moon-colony-bloodbath")
	assert.True(t, v.UGC, "materialized canonical carries the ugc flag")
	assert.Equal(t, "alice", v.Data["operator"], "first writer claims ownership")
	assert.Contains(t, v.CleanContent, "My take")

	dbe, err := st.GetEntity(context.Background(), "boardgame:moon-colony-bloodbath")
	require.NoError(t, err)
	assert.Equal(t, "boardgame", dbe.Kind)
}

// TestCanonicalBody_NonCanonicalNoVault_Stays404 pins that the auto-
// materialize path is canonical-kind-only: a row whose kind is NOT in
// the canonical registry and has no vault file stays a 404 — the
// daemon never fabricates a body for it.
func TestCanonicalBody_NonCanonicalNoVault_Stays404(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newCanonicalUGCFixture(t)

	// A plugin-source-shaped row with no vault file; "gmail" is not in
	// the canonical registry.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   "gmail:msg-1",
		Kind: "gmail",
	}))

	tok := mintToken(t, signer, "alice-agent", "alice")
	rec := ugcReq(t, h, http.MethodGet, "/v1/user-content/gmail:msg-1/sections", tok, nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"non-canonical no-vault must stay 404, not auto-materialize; body=%s", rec.Body.String())
}
