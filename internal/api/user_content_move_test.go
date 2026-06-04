package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createUGCForMove creates a flat UGC entity via the endpoint and
// returns its id + slug.
func createUGCForMove(t *testing.T, h http.Handler, tok, title string) (id, slug string) {
	t.Helper()
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok,
		map[string]any{"title": title, "tags": []string{"x"}}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())
	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.NotEmpty(t, body.ID)
	return body.ID, body.ID[len("user-content:"):]
}

// TestUGC_Move_EndToEnd pins #425 Cut 1 through the endpoint: a move
// relocates the vault file, keeps the entity id/identity stable, is an
// idempotent no-op on the same subfolder, and round-trips back to flat.
func TestUGC_Move_EndToEnd(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	id, slug := createUGCForMove(t, h, tok, "Move Me")

	flat := filepath.Join(root, "user-content", slug+".md")
	notes := filepath.Join(root, "user-content", "notes", slug+".md")
	require.FileExists(t, flat)

	// flat -> subfolder
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/move", tok,
		map[string]any{"subfolder": "notes"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.NoFileExists(t, flat)
	require.FileExists(t, notes)

	// entity identity persists — GET by the same id resolves.
	get := ugcReq(t, h, http.MethodGet, "/v1/user-content/"+id, tok, nil, nil)
	require.Equal(t, http.StatusOK, get.Code, "get body=%s", get.Body.String())

	// same subfolder -> idempotent no-op (still 200, file unchanged).
	again := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/move", tok,
		map[string]any{"subfolder": "notes"}, nil)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	require.FileExists(t, notes)

	// subfolder -> flat (empty subfolder).
	back := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/move", tok,
		map[string]any{}, nil)
	require.Equal(t, http.StatusOK, back.Code, "body=%s", back.Body.String())
	require.FileExists(t, flat)
	assert.NoFileExists(t, notes)
}

// TestUGC_Move_BadSubfolder_400 pins the validation: a malformed
// subfolder rejects with the same 400 shape as create, and does not move
// the file.
func TestUGC_Move_BadSubfolder_400(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	id, slug := createUGCForMove(t, h, tok, "Stay Put")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/move", tok,
		map[string]any{"subfolder": "Bad Slug"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.FileExists(t, filepath.Join(root, "user-content", slug+".md"))
}

// TestUGC_Move_CrossOperator_403 pins the #425 review auth fix: a caller
// whose operator does not match the entity's operator cannot move it
// (closes the #377 cross-operator bypass class for this endpoint), and
// the file stays put.
func TestUGC_Move_CrossOperator_403(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	alice := mintToken(t, signer, "alice-agent", "alice")
	bob := mintToken(t, signer, "bob-agent", "bob")

	id, slug := createUGCForMove(t, h, alice, "Alice Note") // operator=alice
	flat := filepath.Join(root, "user-content", slug+".md")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/move", bob,
		map[string]any{"subfolder": "notes"}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")

	// The file is untouched — no cross-operator move.
	require.FileExists(t, flat)
	assert.NoFileExists(t, filepath.Join(root, "user-content", "notes", slug+".md"))
}

// TestUGC_Move_UnknownEntity_404 pins that a move on a non-existent UGC
// id is a 404.
func TestUGC_Move_UnknownEntity_404(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:nope/move", tok,
		map[string]any{"subfolder": "notes"}, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}
