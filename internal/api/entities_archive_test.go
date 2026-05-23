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

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// TestEntityArchive_RoundTrip pins ADR-0018 step 2's full contract
// at the API layer: archive flips DB flag + moves vault file;
// restore flips them back. Vault active path goes empty after
// archive, populated after restore. _archive path is the inverse.
func TestEntityArchive_RoundTrip(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	const id = "boardgame:archive-test-2024"
	const kind = "boardgame"
	seedEntityForDelete(t, st, w, id, kind)

	activePath := filepath.Join(root, kind, "archive-test-2024.md")
	archivePath := filepath.Join(root, vault.ArchiveDir, kind, "archive-test-2024.md")

	// Baseline: active file present, archive path absent.
	_, err = os.Stat(activePath)
	require.NoError(t, err, "active vault path missing pre-archive")
	_, err = os.Stat(archivePath)
	require.True(t, os.IsNotExist(err), "archive path should not exist pre-archive")

	// POST /archive: 200, response.archived=true, vault file moves,
	// DB row's archived_at is non-NULL.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/archive", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp archiveResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, id, resp.ID)
	assert.True(t, resp.Archived, "post-archive: response.archived=true")

	_, err = os.Stat(activePath)
	require.True(t, os.IsNotExist(err), "active path should be gone post-archive")
	_, err = os.Stat(archivePath)
	require.NoError(t, err, "archive path should exist post-archive")

	got, err := st.GetEntity(context.Background(), id)
	require.NoError(t, err, "GetEntity exempt from archive default-hide")
	require.NotNil(t, got.ArchivedAt, "archived_at non-NULL post-archive")

	// Default search excludes archived rows.
	hits, total, err := st.Search(context.Background(), "Soon-Gone", "", 50, 0, store.ArchivedExclude, false)
	require.NoError(t, err)
	assert.Equal(t, 0, total, "default-filter search hides archived")
	assert.Empty(t, hits)

	// Include-archived returns it.
	hits, total, err = st.Search(context.Background(), "Soon-Gone", "", 50, 0, store.ArchivedInclude, false)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, hits, 1)
	assert.Equal(t, id, hits[0].ID)

	// POST /restore: round-trip back to active.
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/restore", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.False(t, resp.Archived, "post-restore: response.archived=false")

	_, err = os.Stat(activePath)
	require.NoError(t, err, "active path should exist post-restore")
	_, err = os.Stat(archivePath)
	require.True(t, os.IsNotExist(err), "archive path should be gone post-restore")

	got, err = st.GetEntity(context.Background(), id)
	require.NoError(t, err)
	assert.Nil(t, got.ArchivedAt, "archived_at NULL post-restore")
}

// TestEntityArchive_NotFound returns 404 for an id that doesn't
// resolve to a stored entity. Mirrors handleEntityDelete's
// not-found path so the agent-side error envelope is uniform.
func TestEntityArchive_NotFound(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/person:never-existed/archive", tok, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/person:never-existed/restore", tok, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}
