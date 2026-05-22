package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/vault"
)

// seedEntityWithMixedNotes writes an entity whose vault file carries
// three notes spanning the closed kind set: one default (legacy
// empty-kind), one explicit `note`, one `annotation`. Cut 3 tests
// pin the `notes_kind` filter scopes the returned array correctly
// across this mix.
func seedEntityWithMixedNotes(t *testing.T) (http.Handler, string) {
	t.Helper()
	h, st, w, _ := newAPIWithVaultIO(t)
	d := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	seedEntityWithVaultBody(t, st, w, &vault.Entity{
		ID:     "wikipedia:cut3",
		Kind:   "wikipedia-article",
		Plugin: "wikipedia",
		Data:   map[string]any{"title": "Cut 3 Fixture"},
		Notes: []vault.Note{
			// Legacy-shape: no kind set, no field set.
			{Date: d, Text: "operator-side observation", Author: "alice"},
			// Explicit `note`.
			{Date: d.Add(time.Minute), Text: "checked the page", Author: "alice", Kind: vault.NoteKindNote},
			// Annotation: agent-feedback shape carrying a field scope.
			{Date: d.Add(2 * time.Minute), Text: "birth_date disagrees with body text",
				Author: "agent:forge", Field: "birth_date", Kind: vault.NoteKindAnnotation},
		},
	})
	return h, "wikipedia:cut3"
}

// TestGetEntity_NotesKindFilter_Annotation pins the agent-feedback
// path: `?notes_kind=annotation` returns only the annotation entry.
func TestGetEntity_NotesKindFilter_Annotation(t *testing.T) {
	t.Parallel()
	h, id := seedEntityWithMixedNotes(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"?notes_kind=annotation", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := decodeEntityResponse(t, rec)
	require.Len(t, got.Notes, 1)
	assert.Equal(t, vault.NoteKindAnnotation, got.Notes[0].Kind)
	assert.Equal(t, "birth_date", got.Notes[0].Field)
}

// TestGetEntity_NotesKindFilter_Note pins the legacy-inclusive
// behavior: `?notes_kind=note` includes empty-kind entries (treated
// as the default) AND explicit-note entries, but drops annotations.
func TestGetEntity_NotesKindFilter_Note(t *testing.T) {
	t.Parallel()
	h, id := seedEntityWithMixedNotes(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"?notes_kind=note", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := decodeEntityResponse(t, rec)
	require.Len(t, got.Notes, 2)
	for _, n := range got.Notes {
		assert.NotEqual(t, vault.NoteKindAnnotation, n.Kind)
	}
}

// TestGetEntity_NotesKindFilter_OmittedReturnsAll pins backwards-
// compat: legacy callers (no query param) get every note unchanged.
func TestGetEntity_NotesKindFilter_OmittedReturnsAll(t *testing.T) {
	t.Parallel()
	h, id := seedEntityWithMixedNotes(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := decodeEntityResponse(t, rec)
	assert.Len(t, got.Notes, 3, "no filter ⇒ all notes")
}

// TestGetEntity_NotesKindFilter_RejectsUnknown pins the closed-set
// rule: an unknown kind returns 400 invalid_argument with the
// `notes_kind` field carrier.
func TestGetEntity_NotesKindFilter_RejectsUnknown(t *testing.T) {
	t.Parallel()
	h, id := seedEntityWithMixedNotes(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id+"?notes_kind=warning", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_argument")
	assert.Contains(t, rec.Body.String(), "notes_kind")
}

// TestGetEntityContext_NotesKindFilter_ScopesRoot pins the same
// filter shape on /context: the root's notes array is scoped.
func TestGetEntityContext_NotesKindFilter_ScopesRoot(t *testing.T) {
	t.Parallel()
	h, id := seedEntityWithMixedNotes(t)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/"+id+"/context?depth=0&notes_kind=annotation", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := decodeContextResponse(t, rec.Body.Bytes())
	require.Len(t, got.Root.Notes, 1)
	assert.Equal(t, vault.NoteKindAnnotation, got.Root.Notes[0].Kind)
}

// TestGetEntityContext_NotesKindFilter_RejectsUnknown pins
// validation parity with the entity endpoint.
func TestGetEntityContext_NotesKindFilter_RejectsUnknown(t *testing.T) {
	t.Parallel()
	h, id := seedEntityWithMixedNotes(t)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/"+id+"/context?depth=0&notes_kind=bogus", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "notes_kind")
}
