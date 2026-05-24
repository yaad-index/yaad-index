package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// seedAttachmentEntity writes a vault entity with a one-attachment
// manifest plus the backing file at <vault>/<kind>/<slug>/attachments/
// <name>. Returns the absolute path of the attachment file so tests
// can poke at it post-write.
func seedAttachmentEntity(t *testing.T, st store.Store, root, id, kind, name, mime string, body []byte) string {
	t.Helper()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: kind,
		Data: map[string]any{"name": "Has-attach"},
	}))

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: kind,
		Source: []string{"fixture/default"},
		Data: map[string]any{"name": "Has-attach"},
		Attachments: []vault.Attachment{
			{
				Name: name,
				Kind: mime,
				Path: filepath.Join("attachments", name),
				Bytes: int64(len(body)),
			},
		},
	}))

	slug := localFromID(t, id)
	dir := filepath.Join(root, kind, slug, "attachments")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	filePath := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(filePath, body, 0o644))
	return filePath
}

func localFromID(t *testing.T, id string) string {
	t.Helper()
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return id[i+1:]
		}
	}
	t.Fatalf("id %q has no kind:slug shape", id)
	return ""
}

func TestEntityAttachment_HappyPath_StreamsBytes(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:has-thumb-2024"
	want := []byte("\xff\xd8\xffthis-is-a-jpeg-fixture")
	seedAttachmentEntity(t, st, root, id, "boardgame", "thumbnail.jpg", "image/jpeg", want)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"/attachments/thumbnail.jpg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Equal(t, want, rec.Body.Bytes())
}

func TestEntityAttachment_PathTraversal_OnNameSegment(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:has-thumb-2024"
	seedAttachmentEntity(t, st, root, id, "boardgame", "thumbnail.jpg", "image/jpeg", []byte("ok"))

	// `..%2Fsecret` → after URL decoding the path-value becomes
	// `../secret` which the validator must reject.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/"+id+"/attachments/..%2Fsecret", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_attachment_name")
}

func TestEntityAttachment_LeadingDot_Rejected(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:has-thumb-2024"
	seedAttachmentEntity(t, st, root, id, "boardgame", "thumbnail.jpg", "image/jpeg", []byte("ok"))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"/attachments/.hidden", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid_attachment_name")
}

func TestEntityAttachment_PathTraversal_OnManifestPath(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:poisoned-manifest-2024"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "boardgame",
		Data: map[string]any{"name": "Poisoned"},
	}))

	// Hand-craft a manifest whose Path tries to escape the entity dir.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "boardgame",
		Source: []string{"fixture/default"},
		Data: map[string]any{"name": "Poisoned"},
		Attachments: []vault.Attachment{
			{
				Name: "evil.txt",
				Kind: "text/plain",
				Path: "../../etc/passwd",
			},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"/attachments/evil.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_attachment_name",
		"manifest path traversal must reject; cold-reviewer carry-over from a prior PR")
}

func TestEntityAttachment_NotInManifest_404(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:has-thumb-2024"
	seedAttachmentEntity(t, st, root, id, "boardgame", "thumbnail.jpg", "image/jpeg", []byte("ok"))

	// Even if a file exists at the canonical path, an attachment NOT
	// in the manifest is unreachable — manifest is the contract.
	dir := filepath.Join(root, "boardgame", "has-thumb-2024", "attachments")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stowaway.bin"), []byte("haha"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"/attachments/stowaway.bin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not_found")
}

func TestEntityAttachment_FileMissingOnDisk_404Drift(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:drift-2024"
	filePath := seedAttachmentEntity(t, st, root, id, "boardgame", "thumbnail.jpg", "image/jpeg", []byte("ok"))
	// Manifest entry exists; file deleted (manifest-disk drift).
	require.NoError(t, os.Remove(filePath))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"/attachments/thumbnail.jpg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "reindex may reconcile",
		"drift-shape 404 hint")
}

func TestEntityAttachment_EntityNotFound_404(t *testing.T) {
	t.Parallel()
	h, _, _ := newAPIWithVault(t)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:never-existed-2024/attachments/foo.jpg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not_found")
}

func TestEntityAttachment_VaultRequired_503(t *testing.T) {
	t.Parallel()
	// Build a handler WITHOUT vault wiring.
	h := newAPI(t)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:any/attachments/foo.jpg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "vault_required")
}

// Range request support comes free via http.ServeContent — the
// handler hands an *os.File (io.ReadSeeker) so partial-content
// responses Just Work. Pin it so a future refactor that breaks
// seekability shows up here.
func TestEntityAttachment_RangeRequest_PartialContent(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "boardgame:rangey-2024"
	want := []byte("0123456789abcdef")
	seedAttachmentEntity(t, st, root, id, "boardgame", "data.bin", "application/octet-stream", want)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"/attachments/data.bin", nil)
	req.Header.Set("Range", "bytes=4-9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusPartialContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, []byte("456789"), rec.Body.Bytes())
}
