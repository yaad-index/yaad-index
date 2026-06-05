package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUGC_Create_MirrorsTagsToDB pins #438: UGC create folds tags into the
// DB `data` column (via vaultEntityDataForDB), so UGC is tag-searchable —
// not silently dropped as it was when the handler upserted raw ve.Data.
func TestUGC_Create_MirrorsTagsToDB(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok,
		map[string]any{"title": "Tagged Note", "tags": []string{"alpha", "beta"}}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))

	ent, err := st.GetEntity(context.Background(), body.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, dataTags(t, ent.Data),
		"UGC tags must reach the DB data column")
}

// TestUGC_SectionAdd_PreservesTagsInDB pins that a section mutation re-mirrors
// through vaultEntityDataForDB too — tags (a derived field) survive a section
// add, where the upsert previously wrote raw ve.Data and dropped them.
func TestUGC_SectionAdd_PreservesTagsInDB(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok,
		map[string]any{"title": "Sectioned", "tags": []string{"alpha"}, "body": "## One\n\nhi\n"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag, "create returns an ETag for If-Match")
	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))

	add := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+body.ID+"/sections", tok,
		map[string]any{"heading": "Two", "body": "world\n"},
		map[string]string{"If-Match": etag})
	require.Equal(t, http.StatusCreated, add.Code, "section add body=%s", add.Body.String())

	ent, err := st.GetEntity(context.Background(), body.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha"}, dataTags(t, ent.Data),
		"tags must survive the section-add DB re-mirror")
}

// dataTags extracts the JSON-round-tripped tags array from a store entity's
// data map as []string.
func dataTags(t *testing.T, data map[string]any) []string {
	t.Helper()
	raw, ok := data["tags"].([]any)
	require.True(t, ok, "data.tags present as array, got %T (%v)", data["tags"], data["tags"])
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		require.True(t, ok, "tag entry is a string")
		out = append(out, s)
	}
	return out
}
