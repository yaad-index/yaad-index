package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUGC_Create_Subfolder pins #415: a create with a subfolder lands
// the vault file under user-content/<subfolder>/<slug>.md, the DB row
// keeps the flat id, and the entity reads back by that flat id.
func TestUGC_Create_Subfolder(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title":     "My Note",
		"tags":      []string{"personal"},
		"subfolder": "notes",
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// File under the subfolder, not the flat path.
	assert.FileExists(t, filepath.Join(root, "user-content", "notes", "my-note.md"))
	assert.NoFileExists(t, filepath.Join(root, "user-content", "my-note.md"))

	// DB row keeps the flat id.
	dbe, err := st.GetEntity(context.Background(), "user-content:my-note")
	require.NoError(t, err)
	assert.Equal(t, "user-content:my-note", dbe.ID)

	// Reads back by flat id (ReadByID subfolder glob).
	get := ugcReq(t, h, http.MethodGet, "/v1/user-content/user-content:my-note", tok, nil, nil)
	require.Equal(t, http.StatusOK, get.Code, "GET by flat id resolves the subfoldered file; body=%s", get.Body.String())
	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(get.Body).Decode(&body))
	assert.Equal(t, "user-content:my-note", body.ID)
}

// TestUGC_Create_Subfolder_CollisionAcrossSubfolders pins that the flat
// id stays globally unique within the kind: the same slug in a second
// subfolder is rejected.
func TestUGC_Create_Subfolder_CollisionAcrossSubfolders(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	first := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Shared Slug", "tags": []string{"x"}, "subfolder": "notes",
	}, nil)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())

	dup := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Shared Slug", "tags": []string{"x"}, "subfolder": "drafts",
	}, nil)
	assert.Equal(t, http.StatusConflict, dup.Code,
		"same slug in a different subfolder collides on the flat id; body=%s", dup.Body.String())
}

// TestUGC_Create_Subfolder_InvalidRejected pins the validation: a
// subfolder with a path separator or traversal is a 400.
func TestUGC_Create_Subfolder_InvalidRejected(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	for _, bad := range []string{"a/b", "..", "../escape", "Notes", "with space", "/abs"} {
		rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
			"title": "T " + bad, "tags": []string{"x"}, "subfolder": bad,
		}, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "subfolder %q must be rejected; body=%s", bad, rec.Body.String())
	}
}

// TestUGC_Create_DuplicateSubfolderFiles_Collision pins the
// multi-subfolder collision case (#415): when two same-slug files
// already exist in different subfolders
// (hand-authored before reindex), ReadByID can't pick a unique match —
// the create must still 409 on the any-match subfolder probe rather than
// writing a third flat file and breaking the flat id's uniqueness.
func TestUGC_Create_DuplicateSubfolderFiles_Collision(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	for _, sub := range []string{"notes", "drafts"} {
		dir := filepath.Join(root, "user-content", sub)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dup-note.md"),
			[]byte("---\nid: user-content:dup-note\n---\n"), 0o644))
	}

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Dup Note", "tags": []string{"x"},
	}, nil)
	assert.Equal(t, http.StatusConflict, rec.Code,
		"create must 409 even when ReadByID can't pick a unique match; body=%s", rec.Body.String())
	assert.NoFileExists(t, filepath.Join(root, "user-content", "dup-note.md"),
		"create must not write a third flat file")
}

// TestUGC_Create_NoSubfolder_Flat pins the regression: omitting the
// subfolder writes the flat path, unchanged.
func TestUGC_Create_NoSubfolder_Flat(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Flat Note", "tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.FileExists(t, filepath.Join(root, "user-content", "flat-note.md"))
}
