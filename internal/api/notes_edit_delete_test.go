package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// assertGapStateSurvives fails if the entity's DB-side gap_state lost
// the `rating` entry OR its workflow-injected spec metadata — the
// regression the #390 review flagged (a note write nulling gap_state,
// and the lossy vault→store translator stripping the GapSpec fields
// /v1/needs-fill reads).
func assertGapStateSurvives(t *testing.T, st store.Store, id string) {
	t.Helper()
	got, err := st.GetEntity(context.Background(), id)
	require.NoError(t, err)
	require.Contains(t, got.GapState, "rating", "gap_state entry must survive note mutation")
	e := got.GapState["rating"]
	assert.Equal(t, "int", e.Type, "gap spec Type must survive")
	assert.Equal(t, "the operator's 1-10 rating", e.Description, "gap spec Description must survive")
	assert.Equal(t, "operator", e.FillStrategy, "gap spec FillStrategy must survive")
	assert.Equal(t, []int{1, 10}, e.Range, "gap spec Range must survive")
}

// addNoteAs posts a note authored by the given token's subject and
// returns its note_id.
func addNoteAs(t *testing.T, h http.Handler, id, tok, text string) string {
	t.Helper()
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/notes", tok,
		map[string]any{"text": text}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "add_note body=%s", rec.Body.String())
	var added commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&added))
	require.NotEmpty(t, added.Note.ID)
	return added.Note.ID
}

// TestEditNote_AuthorGated pins #390 Cut 2 edit_note: only the note's
// author may edit; the edit replaces text/kind in place, stamps
// last_edited_at, and keeps the same note_id; unknown ids 404.
func TestEditNote_AuthorGated(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	const id = "boardgame:edit-note-test"
	seedCommentsEntity(t, st, root, id, "boardgame")
	alice := mintToken(t, signer, "alice", "op")
	bob := mintToken(t, signer, "bob", "op")

	noteID := addNoteAs(t, h, id, alice, "original text")

	// bob may not edit alice's note.
	rec := ugcReq(t, h, http.MethodPut, "/v1/entities/"+id+"/notes/"+noteID, bob,
		map[string]any{"text": "hijacked"}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "author_mismatch")

	// alice edits: 200, in-place, last_edited_at stamped, id unchanged.
	rec = ugcReq(t, h, http.MethodPut, "/v1/entities/"+id+"/notes/"+noteID, alice,
		map[string]any{"text": "corrected text", "kind": "annotation"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var edited commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&edited))
	assert.Equal(t, "corrected text", edited.Note.Text)
	assert.Equal(t, "annotation", edited.Note.Kind)
	assert.Equal(t, noteID, edited.Note.ID, "note_id is stable across edit")
	assert.NotEmpty(t, edited.Note.LastEditedAt, "edit stamps last_edited_at")

	// 404 on an unknown note_id.
	rec = ugcReq(t, h, http.MethodPut, "/v1/entities/"+id+"/notes/deadbeef", alice,
		map[string]any{"text": "x"}, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// TestDeleteNote_AuthorGated pins #390 Cut 2 delete_note: only the
// author may delete; the note is hard-removed.
func TestDeleteNote_AuthorGated(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	const id = "boardgame:delete-note-test"
	seedCommentsEntity(t, st, root, id, "boardgame")
	alice := mintToken(t, signer, "alice", "op")
	bob := mintToken(t, signer, "bob", "op")

	noteID := addNoteAs(t, h, id, alice, "note to remove")

	// bob may not delete alice's note.
	rec := ugcReq(t, h, http.MethodDelete, "/v1/entities/"+id+"/notes/"+noteID, bob, nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "author_mismatch")

	// alice deletes: 200 deleted.
	rec = ugcReq(t, h, http.MethodDelete, "/v1/entities/"+id+"/notes/"+noteID, alice, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var del deleteNoteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&del))
	assert.True(t, del.Deleted)
	assert.Equal(t, noteID, del.NoteID)

	// The note is gone from the entity.
	v := readVaultByID(t, root, "boardgame", id)
	for _, n := range v.Notes {
		assert.NotEqual(t, noteID, n.ID, "deleted note must not remain in the vault")
	}

	// Second delete → 404 (already gone).
	rec = ugcReq(t, h, http.MethodDelete, "/v1/entities/"+id+"/notes/"+noteID, alice, nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// TestNoteMutation_PreservesGapState pins the #390 review fix: add /
// edit / delete note must NOT erase the entity's DB-side gap_state (the
// UpsertEntity UPSERT path nulls any column it isn't handed).
func TestNoteMutation_PreservesGapState(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	const id = "boardgame:gapstate-note-test"
	now := time.Now().UTC().Truncate(time.Second)

	// A workflow-injected gap (full GapSpec metadata, the #142 shape).
	storeGap := store.GapStateEntry{
		Source: "operator", Type: "int", Description: "the operator's 1-10 rating",
		FillStrategy: "operator", Range: []int{1, 10},
	}
	vaultGap := vault.GapStateEntry{
		Source: "operator", Type: "int", Description: "the operator's 1-10 rating",
		FillStrategy: "operator", Range: []int{1, 10},
	}
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:        id,
		Kind:      "boardgame",
		CreatedAt: now,
		Data:      map[string]any{"title": "Gap Game"},
		GapState:  map[string]store.GapStateEntry{"rating": storeGap},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID:       id,
		Kind:     "boardgame",
		Source:   []string{"fixture/default"},
		Data:     map[string]any{"title": "Gap Game"},
		Gaps:     []string{"rating"},
		GapState: map[string]vault.GapStateEntry{"rating": vaultGap},
	}))

	alice := mintToken(t, signer, "alice", "op")

	// add_note keeps gap_state.
	noteID := addNoteAs(t, h, id, alice, "first note")
	assertGapStateSurvives(t, st, id)

	// edit_note keeps gap_state.
	rec := ugcReq(t, h, http.MethodPut, "/v1/entities/"+id+"/notes/"+noteID, alice,
		map[string]any{"text": "edited note"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assertGapStateSurvives(t, st, id)

	// delete_note keeps gap_state.
	rec = ugcReq(t, h, http.MethodDelete, "/v1/entities/"+id+"/notes/"+noteID, alice, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assertGapStateSurvives(t, st, id)
}
