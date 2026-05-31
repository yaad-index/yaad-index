package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
)

// createUserContentForEdit POSTs a fresh UGC entity with the
// supplied initial frontmatter so the subsequent PUT /frontmatter
// edit can re-derive against a known-good baseline. Returns the
// minted token + the entity id.
func createUserContentForEdit(
	t *testing.T,
	h http.Handler,
	signer auth.Signer,
	title string,
	initialData map[string]any,
) (tok string, id string) {
	t.Helper()
	tok = mintToken(t, signer, "the implementer", "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": title,
		"body": "",
		"tags": []string{"games"},
		"data": initialData,
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())
	id = "user-content:" + slugifyForTest(title)
	return tok, id
}

// slugifyForTest mirrors vault.SlugFromTitle's transforms enough
// for the test fixtures' titles. Uses the helper inline rather
// than importing vault — we control the test inputs so this stays
// simple.
func slugifyForTest(title string) string {
	out := []byte{}
	prev := byte('-')
	for i := 0; i < len(title); i++ {
		c := title[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c = c + ('a' - 'A')
			out = append(out, c)
			prev = c
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
			prev = c
		default:
			if prev != '-' {
				out = append(out, '-')
				prev = '-'
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// TestUserContentFrontmatterEdit_HappyPath_ReplacesEdges pins the
// load-bearing contract: an edit replaces prior edges with
// the new fill's edges. Distinct value between create + edit so
// the replacement is observable.
func TestUserContentFrontmatterEdit_HappyPath_ReplacesEdges(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Edit Replaces Test", map[string]any{
		"about": map[string]any{"name": "Caverna", "kind": "boardgame"},
	})

	// First-fill edges land.
	first, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "boardgame:caverna", first[0].To)

	// PUT /frontmatter with a different value.
	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	second, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, second, 1, "edit must replace, not append")
	assert.Equal(t, "boardgame:brass-birmingham", second[0].To,
		"new fill's edge replaces the prior one")
}

// TestUserContentFrontmatterEdit_OmittedFieldClearsEdges covers
// the "edit by omission clears" semantic: when the agent omits a
// previously-set declared field from the edit request, the prior
// edges of that type wipe (DeleteEdgesByTypeFrom fires; no new
// edges created).
func TestUserContentFrontmatterEdit_OmittedFieldClearsEdges(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Omitted Field Test", map[string]any{
		"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
		"publisher": map[string]any{"name": "Roxley", "kind": "company"},
	})

	first, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, first, 2)

	// Edit with only `about` set; `publisher` omitted → its edges
	// should clear.
	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	second, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, second, 1, "publisher edge cleared by omission")
	assert.Equal(t, "is_about", second[0].Type)
}

// TestUserContentFrontmatterEdit_EmptyListClearsEdges covers the
// explicit empty-list clear shape: setting a declared field to
// `[]` clears its edges (same outcome as omission, but explicit).
func TestUserContentFrontmatterEdit_EmptyListClearsEdges(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Empty List Edit Test", map[string]any{
		"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
	})

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": []any{},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	assert.Empty(t, edges, "empty-list edit clears all edges of that type")
}

// TestUserContentFrontmatterEdit_KindMismatchRejects covers the
// validation gate: an edit whose value's kind doesn't match the
// declared mapping's target_kind rejects with 400; prior edges
// are NOT wiped (the parse failure short-circuits before the
// vault/db write).
func TestUserContentFrontmatterEdit_KindMismatchRejects(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Kind Mismatch Edit Test", map[string]any{
		"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
	})

	// `about` mapping declares target_kind=boardgame; a person-kind
	// value rejects.
	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": map[string]any{"name": "Martin Wallace", "kind": "person"},
			},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "edit body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")

	// Prior edges survive — the parse-error path rejects before
	// any vault/db mutation.
	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "prior edges must survive a rejected edit")
	assert.Equal(t, "boardgame:brass-birmingham", edges[0].To)
}

// TestUserContentFrontmatterEdit_MalformedPreformedLabelRejects
// covers pre-formed-label parse failures: a string without a
// `:` / empty kind / empty slug rejects with 400.
func TestUserContentFrontmatterEdit_MalformedPreformedLabelRejects(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Malformed Edit Test", map[string]any{})

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": "no-colon-here",
			},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "edit body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_canonical_label")

	// No edges created on rejection — empty initial state stays
	// empty.
	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	assert.Empty(t, edges)
}

// TestUserContentFrontmatterEdit_PreformedLabelsAccepted covers
// the dual-shape acceptance: pre-formed `<kind>:<slug>` strings
// flow through unchanged (UGC operator-authored, same shape as
// operator-fill).
func TestUserContentFrontmatterEdit_PreformedLabelsAccepted(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Preformed Edit Test", map[string]any{})

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": "boardgame:agricola",
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "boardgame:agricola", edges[0].To,
		"pre-formed label slug round-trips as-is on edit")
}

// TestUserContentFrontmatterEdit_UndeclaredFieldsLandInData pins
// the back-compat surface: edit body's `data` fields not declared
// in the mapping flow through to `vault.Entity.Data` verbatim,
// with no edge derivation.
func TestUserContentFrontmatterEdit_UndeclaredFieldsLandInData(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Undeclared Edit Test", map[string]any{})

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
				"year": 2018,
				"my_note": "freeform operator note",
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	ve := readVaultByID(t, root, "user-content", id)
	assert.EqualValues(t, 2018, ve.Data["year"])
	assert.Equal(t, "freeform operator note", ve.Data["my_note"])
}

// TestUserContentFrontmatterEdit_RejectsTitleMutation covers the
// identity-bearing-field gate: edit attempts to mutate `title` /
// `id` / `author` / `operator` reject with 400. Title is set-once
// at create time (drives the slug → entity ID); id is derived
// from title; author/operator stamp the create-time identity.
func TestUserContentFrontmatterEdit_RejectsTitleMutation(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Title Mutation Test", map[string]any{})

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"title": "Different Title",
			},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "edit body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "set-once at create time")
}

// TestUserContentFrontmatterEdit_OperatorMismatchForbidden covers
// the auth gate per #377: only a JWT whose operator pair-claim
// matches the entity's stored operator may edit. A token with a
// different operator rejects with 403 operator_mismatch.
func TestUserContentFrontmatterEdit_OperatorMismatchForbidden(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())

	tok, id := createUserContentForEdit(t, h, signer, "Auth Mismatch Test", map[string]any{})

	// A token with a different subject + operator pair (different
	// operator-suite altogether).
	otherTok := mintToken(t, signer, "stranger", "stranger")
	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", otherTok,
		map[string]any{
			"data": map[string]any{
				"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "edit body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")

	_ = tok // suppress "declared but not used" if go vet complains
}

// TestUserContentFrontmatterEdit_NoMappings_NoOpEdgesUntouched
// pins the graceful-degradation surface: when the operator hasn't
// declared any frontmatter-edge mappings, the edit accepts the
// data replacement but no edge work happens (no edges created or
// deleted). The dead-config-field regression's mirror property —
// derivation is opt-in.
func TestUserContentFrontmatterEdit_NoMappings_NoOpEdgesUntouched(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, nil)

	tok, id := createUserContentForEdit(t, h, signer, "No Mappings Edit Test", map[string]any{
		"freeform": "anything goes",
	})

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/"+id+"/frontmatter", tok,
		map[string]any{
			"data": map[string]any{
				"freeform": "still anything",
				"new_key": "fresh value",
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "edit body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	assert.Empty(t, edges, "no mappings → no edge work either way")
}

// TestUserContentFrontmatterEdit_NotFound covers the standard
// 404 surface: PUT against a non-existent user-content id rejects.
func TestUserContentFrontmatterEdit_NotFound(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:does-not-exist/frontmatter", tok,
		map[string]any{
			"data": map[string]any{},
		}, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// _ ensures config import stays even if all the explicit
// references migrate; mirrors operator_fill_test.go's pattern.
var _ = config.CanonicalTypeName
