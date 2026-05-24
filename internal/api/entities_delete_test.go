package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// seedEntityForDelete writes a minimal entity to vault + DB so the
// DELETE endpoint has something to remove. Returns the absolute
// vault root for assertions.
func seedEntityForDelete(t *testing.T, st store.Store, w *vault.Writer, id, kind string) {
	t.Helper()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: kind,
		Data: map[string]any{"title": "Soon-Gone Game"},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: kind,
		Source: []string{"fixture/default"},
		Data: map[string]any{"title": "Soon-Gone Game"},
	}))
}

// TestEntityDelete_HappyPath exercises the full destroy chain per
// ADR-0018 step 4: archive first (lifecycle gate), then DELETE
// removes the archived row from DB + vault `_archive/`. The two-
// step path is the only path; the state-machine IS the safety
// property.
func TestEntityDelete_HappyPath(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	const id = "boardgame:soon-gone-2024"
	seedEntityForDelete(t, st, w, id, "boardgame")

	// Archive first (lifecycle prerequisite per ADR-0018 step 4).
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/archive", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "archive prerequisite: body=%s", rec.Body.String())

	// Now DELETE on archived entity → hard destroy.
	rec = ugcReq(t, h, http.MethodDelete, "/v1/entities/"+id, tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got entityDeleteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, id, got.ID)
	assert.True(t, got.Deleted)

	// DB row gone (cascade).
	_, err = st.GetEntity(context.Background(), id)
	require.Error(t, err, "entity must be removed from store after DELETE")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Vault file gone — neither active nor archive layout.
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	_, err = r.ReadByID("boardgame", id)
	require.Error(t, err, "vault file must be removed")
	assert.True(t, vault.IsNotExist(err), "want IsNotExist, got %v", err)
}

// TestEntityDelete_ConflictOnActive pins the ADR-0018 step 4
// state-machine: DELETE on an active (non-archived) entity returns
// 409 Conflict with the specific error code + a hint pointing at
// the archive-first path. The entity stays untouched.
func TestEntityDelete_ConflictOnActive(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	const id = "boardgame:still-active-2024"
	seedEntityForDelete(t, st, w, id, "boardgame")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/entities/"+id, tok, nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "must archive before delete", "wire error code")
	assert.Contains(t, body, "/archive first", "hint points at archive path")

	// Entity untouched: still in DB, still active.
	got, err := st.GetEntity(context.Background(), id)
	require.NoError(t, err, "entity must still be in store after rejected DELETE")
	assert.Nil(t, got.ArchivedAt, "still active")

	// Vault file still in active layout.
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	_, err = r.ReadByID("boardgame", id)
	require.NoError(t, err, "vault file must still be present")
}

// TestEntityDelete_NotFound returns 404 when the entity ID doesn't
// resolve to a stored row. Distinct from a vault-DB drift case
// (handled separately): this is the clean "id never existed" path.
func TestEntityDelete_NotFound(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/entities/boardgame:never-existed-2024", tok, nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not_found")
}

// TestEntityDelete_RequiresAuth pins the protect() middleware
// coverage — DELETE without a Bearer token is rejected before the
// handler runs.
func TestEntityDelete_RequiresAuth(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newAuthedUGCFixture(t)

	rec := ugcReq(t, h, http.MethodDelete, "/v1/entities/boardgame:any-id-2024", "", nil, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
}

// TestEntityDelete_VaultRequired returns 503 when the daemon was
// constructed without a vault (operator running pre-a prior PR DB-only
// fallback). Surfaces the requirement clearly so agents see why
// the call failed instead of a confusing 500.
func TestEntityDelete_VaultRequired(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// No WithVaultIO — handler should return 503 on DELETE.
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed())

	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "boardgame:vaultless-2024",
		Kind: "boardgame",
		Data: map[string]any{"title": "Vaultless"},
	}))

	req := httptest.NewRequest(http.MethodDelete, "/v1/entities/boardgame:vaultless-2024", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "vault_required")
}
