package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestUGC_Rename_EndToEnd pins #425 Cut 2 through the endpoint: a rename
// relocates the vault file to the new slug, the new id resolves, and the
// OLD id still resolves via the back-compat alias.
func TestUGC_Rename_EndToEnd(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	id, slug := createUGCForMove(t, h, tok, "Rename Me")
	require.FileExists(t, filepath.Join(root, "user-content", slug+".md"))

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/rename", tok,
		map[string]any{"new_title": "Brand New Title"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	newID := "user-content:brand-new-title"
	require.FileExists(t, filepath.Join(root, "user-content", "brand-new-title.md"))
	assert.NoFileExists(t, filepath.Join(root, "user-content", slug+".md"))

	// GET by the new id works.
	get := ugcReq(t, h, http.MethodGet, "/v1/user-content/"+newID, tok, nil, nil)
	require.Equal(t, http.StatusOK, get.Code, "get-new body=%s", get.Body.String())

	// GET by the OLD id still resolves via the back-compat alias.
	getOld := ugcReq(t, h, http.MethodGet, "/v1/user-content/"+id, tok, nil, nil)
	require.Equal(t, http.StatusOK, getOld.Code, "get-old (alias) body=%s", getOld.Body.String())
}

// TestUGC_Rename_PreservesInboundEdge pins that an inbound edge (some
// other entity -> user-content:old) re-points to the new id on rename,
// and the old id still resolves via the alias.
func TestUGC_Rename_PreservesInboundEdge(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	id, _ := createUGCForMove(t, h, tok, "Linked Note")
	ctx := context.Background()

	// A neighbor that points AT the UGC note.
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID: "x:linker", Kind: "x", Data: map[string]any{"id": "x:linker"},
	}))
	require.NoError(t, st.CreateEdge(ctx, &store.Edge{Type: "mentions", From: "x:linker", To: id}))

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/rename", tok,
		map[string]any{"new_title": "Linked Note Renamed"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	newID := "user-content:linked-note-renamed"

	// The inbound edge now points at the new id.
	inbound, err := st.GetEdgesTo(ctx, newID, nil)
	require.NoError(t, err)
	require.Len(t, inbound, 1)
	assert.Equal(t, "x:linker", inbound[0].From)

	// The old id still resolves (alias).
	resolved, err := st.ResolveAlias(ctx, "linked-note", userContentKind)
	require.NoError(t, err)
	assert.Equal(t, newID, resolved)
}

// TestUGC_Rename_ForeignOldSlugAlias_409 pins the #425 Cut 2 review fix:
// when the entity's current bare slug is already an alias owned by a
// different entity, the rename is refused (409) before anything moves on
// disk — completing it would orphan the old reference to the foreign
// entity.
func TestUGC_Rename_ForeignOldSlugAlias_409(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	idA, slugA := createUGCForMove(t, h, tok, "Target Note")
	idB, _ := createUGCForMove(t, h, tok, "Other Note")
	ctx := context.Background()

	// Re-point A's bare slug to B (steals A's self-alias), so A's current
	// slug is now a foreign-owned alias.
	require.NoError(t, st.ReplaceAliases(ctx, idB, []store.Alias{{Alias: slugA, EntityID: idB, Kind: store.AliasKindBare}}))

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+idA+"/rename", tok,
		map[string]any{"new_title": "Target Renamed"}, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())

	// Nothing moved on disk.
	require.FileExists(t, filepath.Join(root, "user-content", slugA+".md"))
	assert.NoFileExists(t, filepath.Join(root, "user-content", "target-renamed.md"))
}

// TestUGC_Rename_NoOp_SameSlug pins that a new title slugifying to the
// current slug is a 200 no-op (file + id unchanged).
func TestUGC_Rename_NoOp_SameSlug(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	id, slug := createUGCForMove(t, h, tok, "Same Slug")

	// "Same  Slug" (different spacing/case) slugifies back to "same-slug".
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/rename", tok,
		map[string]any{"new_title": "SAME slug"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.FileExists(t, filepath.Join(root, "user-content", slug+".md"))
}

// TestUGC_Rename_Collision_409 pins that renaming onto a slug already
// taken by another entity is a 409, and neither file moves.
func TestUGC_Rename_Collision_409(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	idA, slugA := createUGCForMove(t, h, tok, "Note A")
	_, slugB := createUGCForMove(t, h, tok, "Note B")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+idA+"/rename", tok,
		map[string]any{"new_title": "Note B"}, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.FileExists(t, filepath.Join(root, "user-content", slugA+".md"))
	require.FileExists(t, filepath.Join(root, "user-content", slugB+".md"))
}

// TestUGC_Rename_CrossOperator_403 pins that a caller whose operator does
// not match the entity's operator cannot rename it, and the file stays.
func TestUGC_Rename_CrossOperator_403(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	alice := mintToken(t, signer, "alice-agent", "alice")
	bob := mintToken(t, signer, "bob-agent", "bob")

	id, slug := createUGCForMove(t, h, alice, "Alice Note") // operator=alice

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/rename", bob,
		map[string]any{"new_title": "Bob Took It"}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
	require.FileExists(t, filepath.Join(root, "user-content", slug+".md"))
	assert.NoFileExists(t, filepath.Join(root, "user-content", "bob-took-it.md"))
}

// TestUGC_Rename_BadAndUnknown pins the 400 (empty title) + 404 (unknown
// entity) paths.
func TestUGC_Rename_BadAndUnknown(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	id, _ := createUGCForMove(t, h, tok, "Real Note")

	bad := ugcReq(t, h, http.MethodPost, "/v1/user-content/"+id+"/rename", tok,
		map[string]any{"new_title": "   "}, nil)
	require.Equal(t, http.StatusBadRequest, bad.Code, "body=%s", bad.Body.String())

	missing := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:nope/rename", tok,
		map[string]any{"new_title": "Whatever"}, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, "body=%s", missing.Body.String())
}
