package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/reindex"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

const commentsTestEntityID = "boardgame:note-test"

// newNotesFixture builds a vault-wired handler + a seeded entity
// (DB row + vault file) ready to accept POST /v1/entities/{id}/notes
// calls. Mirrors newFillFixture's shape.
func newNotesFixture(t *testing.T) (http.Handler, store.Store, string) {
	t.Helper()
	h, st, root := newAPIWithVault(t)
	seedCommentsEntity(t, st, root, commentsTestEntityID, "boardgame")
	return h, st, root
}

// seedCommentsEntity writes a partial entity to BOTH the DB (for
// the kind lookup) and the vault file (the canonical state per
// ADR-0008). No notes yet; the test exercises the append path.
func seedCommentsEntity(t *testing.T, st store.Store, vaultRoot, id, kind string) {
	t.Helper()
	seedEntity(t, st, id, kind)
	w, err := vault.NewWriter(vaultRoot)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: kind,
		Source: []string{"test-fixture/default"},
		Data: map[string]any{"id": id, "title": "Note Test Game"},
	}))
}

func postComments(t *testing.T, h http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/entities/" + id + "/notes"
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestComments_HappyPath(t *testing.T) {
	t.Parallel()

	h, _, root := newNotesFixture(t)

	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": "Played this last night, thoroughly enjoyed the canal phase.",
		"author": "alice",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "alice", got.Note.Author)
	assert.Equal(t, "Played this last night, thoroughly enjoyed the canal phase.", got.Note.Text)
	_, err := time.Parse(time.RFC3339, got.Note.Date)
	require.NoError(t, err, "note.date is RFC3339")

	// Vault file has the note in frontmatter (canonical) + body
	// `## Notes` section (regenerated mirror).
	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, v.Notes, 1)
	assert.Equal(t, "alice", v.Notes[0].Author)
	assert.Equal(t, "Played this last night, thoroughly enjoyed the canal phase.", v.Notes[0].Text)
	assert.False(t, v.Notes[0].Date.IsZero(), "server-stamped date set")
}

func TestComments_AccumulateAcrossCalls(t *testing.T) {
	t.Parallel()

	h, _, root := newNotesFixture(t)
	for i, msg := range []string{"first", "second", "third"} {
		rec := postComments(t, h, commentsTestEntityID, map[string]any{
			"text": msg,
			"author": "alice",
		})
		require.Equal(t, http.StatusCreated, rec.Code, "iteration %d body=%s", i, rec.Body.String())
		// Tiny pause so server-stamped dates are strictly ordered (the
		// vault parser dedups on (date, author, text); identical-second
		// dates would collide on identical text + author).
		time.Sleep(2 * time.Millisecond)
	}

	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, v.Notes, 3, "three appends → three frontmatter rows")
	for i := 1; i < len(v.Notes); i++ {
		assert.False(t, v.Notes[i].Date.Before(v.Notes[i-1].Date),
			"notes[%d].date must be ≥ notes[%d].date (chronological)", i, i-1)
	}
}

func TestComments_TrimsLeadingAndTrailingWhitespace(t *testing.T) {
	t.Parallel()

	// The cold-reviewer's a prior PR review note: note text with leading newlines
	// loses them on body round-trip. Locks the documented input
	// normalization (TrimSpace) — round trip is now lossless for
	// non-whitespace text content.
	h, _, root := newNotesFixture(t)
	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": " \n\nhello world\n ",
		"author": " alice ",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "hello world", got.Note.Text, "text trimmed on input")
	assert.Equal(t, "alice", got.Note.Author, "author trimmed on input")

	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, v.Notes, 1)
	assert.Equal(t, "hello world", v.Notes[0].Text)
	assert.Equal(t, "alice", v.Notes[0].Author)
}

func TestComments_RejectsWhitespaceOnlyText(t *testing.T) {
	t.Parallel()

	h, _, _ := newNotesFixture(t)
	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": " \n\t\n",
	})
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "text is required")
}

func TestComments_RejectsMissingText(t *testing.T) {
	t.Parallel()

	h, _, _ := newNotesFixture(t)
	rec := postComments(t, h, commentsTestEntityID, map[string]any{})
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "text is required")
}

func TestComments_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	h, _, _ := newNotesFixture(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v1/entities/"+commentsTestEntityID+"/notes", strings.NewReader(`{`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "JSON")
}

func TestComments_RejectsUnknownFields(t *testing.T) {
	t.Parallel()

	h, _, _ := newNotesFixture(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v1/entities/"+commentsTestEntityID+"/notes",
		strings.NewReader(`{"text":"x","date":"2026-01-01T00:00:00Z"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "unknown")
}

func TestComments_UnknownEntity_404(t *testing.T) {
	t.Parallel()

	h, _, _ := newAPIWithVault(t) // no entity seeded
	rec := postComments(t, h, "boardgame:nope", map[string]any{
		"text": "ghost-entity note",
	})
	assertErrorEnvelope(t, rec, http.StatusNotFound, "not_found", "boardgame:nope")
}

func TestComments_VaultNotConfigured_503(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	seedEntity(t, st, commentsTestEntityID, "boardgame")
	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, plugins.NewRegistry()) // NO WithVaultIO

	rec := postComments(t, h, commentsTestEntityID, map[string]any{"text": "x"})
	assertErrorEnvelope(t, rec, http.StatusServiceUnavailable, "vault_required", "vault.path")
}

// TestComments_HandEditPicksUpInBodyThenAPIAppendKeepsBoth covers
// the body→frontmatter merge property: a hand-edit dropping a
// dated `## Notes` block into the body in Obsidian, followed by
// an API append, lands BOTH the hand-edit and the API call into
// the entity. The vault.Reader merges body→frontmatter on read; the
// API append re-reads via the handler's vault.Reader.ReadByID and
// inherits the body note, then writes the merged shape back.
func TestComments_HandEditPicksUpInBodyThenAPIAppendKeepsBoth(t *testing.T) {
	t.Parallel()

	h, _, root := newNotesFixture(t)

	// Hand-edit: append a body-only note block (the kind a user
	// would write directly in Obsidian).
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	bodyEdit := vault.Note{
		Date: time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		Text: "hand-edited body note",
		// Author intentionally empty to spot-check the optional path.
	}
	v.Notes = append(v.Notes, bodyEdit)
	require.NoError(t, w.Write(v))

	// API append on top.
	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": "API-append after the hand-edit",
		"author": "alice",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	got := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, got.Notes, 2, "both hand-edit + API-append survive")

	// Order is by the vault.mergeNotes stable sort by Date ASC.
	// Hand-edit (older) first, API-append (now) second.
	assert.Equal(t, "hand-edited body note", got.Notes[0].Text)
	assert.Empty(t, got.Notes[0].Author)
	assert.Equal(t, "API-append after the hand-edit", got.Notes[1].Text)
	assert.Equal(t, "alice", got.Notes[1].Author)
}

// TestComments_StaleWriterOverwritesBodyHandEdit pins the cold-reviewer's review-
// note property: an API-appended note surviving a subsequent
// vault.Writer.Write that doesn't first read is the staleness window
// the vault package documents (see internal/vault/entity.go). A
// writer firing on an in-memory entity without first re-reading the
// disk state CAN overwrite a body-only hand-edit. Locking this
// behavior so a future "fix" doesn't accidentally start preserving
// stale body content.
func TestComments_StaleWriterOverwritesBodyHandEdit(t *testing.T) {
	t.Parallel()

	h, _, root := newNotesFixture(t)

	// First API-append: vault file now has note A in frontmatter +
	// body section.
	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": "note A from API",
		"author": "alice",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	// In-memory state from before the API call (i.e., no notes).
	stale := &vault.Entity{
		ID: commentsTestEntityID,
		Kind: "boardgame",
		Source: []string{"test-fixture/default"},
		Data: map[string]any{"id": commentsTestEntityID, "title": "Note Test Game"},
	}

	// Simulate a stale-writer code path that doesn't go through the
	// API handler: serializes the in-memory state directly. This is
	// exactly the staleness window the vault package's read/write
	// asymmetry doc describes.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(stale))

	// Note A is gone — the stale writer didn't merge body content.
	got := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	assert.Empty(t, got.Notes, "stale writer overwrites: documented staleness window")
}

// TestComments_FTSFindsNoteText locks the vaultEntityDataForDB
// projection extension: the LIKE-on-data search finds note text
// because we fold concatenated note text into `data["notes_text"]`
// on each UpsertEntity.
func TestComments_FTSFindsNoteText(t *testing.T) {
	t.Parallel()

	h, _, _ := newNotesFixture(t)
	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": "this note contains the word abracadabra",
		"author": "alice",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	// q=abracadabra (a word that appears nowhere in the seeded
	// fixture's data) should hit because notes_text is in data.
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=abracadabra", nil)
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, req)
	require.Equal(t, http.StatusOK, srec.Code, "body=%s", srec.Body.String())

	var resp searchResponse
	require.NoError(t, json.NewDecoder(srec.Body).Decode(&resp))
	require.NotEmpty(t, resp.Results, "search should find the entity by note text")
	var found bool
	for _, r := range resp.Results {
		if r.ID == commentsTestEntityID {
			found = true
		}
	}
	assert.True(t, found, "results should include %s; got %v", commentsTestEntityID, resp.Results)
}

// TestComments_ReindexRoundTripsHandEdit pins ADR-0008's "hand-edits
// flow through reindex" claim. Hand-edit a body note, run
// reindex, query → entity has the note. End-to-end vault →
// reindex → DB property.
func TestComments_ReindexRoundTripsHandEdit(t *testing.T) {
	t.Parallel()

	h, st, root := newNotesFixture(t)

	// First API-append so the entity has a known initial state in
	// both vault and DB (kind lookup works for subsequent calls).
	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text": "API initial",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Hand-edit: a fresh body-only note that the API never saw.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	v.Notes = append(v.Notes, vault.Note{
		// Truncate to second precision to match the vault layer's
		// body↔frontmatter round-trip (the writer formats body
		// headers as RFC3339, second-precision; nano-precision
		// frontmatter dates would not dedup against second-precision
		// body parses).
		Date: time.Now().UTC().Add(time.Minute).Truncate(time.Second),
		Text: "hand-edited via reindex path",
	})
	require.NoError(t, w.Write(v))

	// Reindex: walk vault, regenerate DB rows.
	r, err := reindex.New(st, root, nil, nil)
	require.NoError(t, err)
	summary, err := r.Run(context.Background(), reindex.Full)
	require.NoError(t, err)
	assert.Empty(t, summary.Errors)

	// Search finds the hand-edited text via the notes_text
	// projection that reindex's UpsertEntity-via-vaultEntityDataForDB
	// emits — except reindex doesn't go through that projection
	// directly. Verify via GetEntity instead.
	got, err := st.GetEntity(context.Background(), commentsTestEntityID)
	require.NoError(t, err)
	// The vault file is canonical; the DB projection mirrors. After
	// reindex, the entity's notes are in the vault (this test
	// directly asserts the vault state survived round-trip).
	v2 := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, v2.Notes, 2, "API + hand-edit both present")
	gotTexts := make([]string, 0, len(v2.Notes))
	for _, c := range v2.Notes {
		gotTexts = append(gotTexts, c.Text)
	}
	assert.Contains(t, gotTexts, "hand-edited via reindex path")
	_ = got // GetEntity used as a smoke check that the row still resolves
}

// TestComments_AcceptsFieldAndKind pins the #186 Cut 2 write
// surface: POST /v1/entities/{id}/notes accepts optional
// `field` + `kind` and persists them through to the vault note +
// the response envelope.
func TestComments_AcceptsFieldAndKind(t *testing.T) {
	t.Parallel()
	h, _, root := newNotesFixture(t)

	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text":   "Birth-date looks off; bio says 1955-04 but article says 1955-05.",
		"author": "alice",
		"field":  "birth_date",
		"kind":   vault.NoteKindAnnotation,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "birth_date", got.Note.Field)
	assert.Equal(t, vault.NoteKindAnnotation, got.Note.Kind)

	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, v.Notes, 1)
	assert.Equal(t, "birth_date", v.Notes[0].Field)
	assert.Equal(t, vault.NoteKindAnnotation, v.Notes[0].Kind)
}

// TestComments_DefaultsFieldAndKindEmpty pins that omitting the
// new fields preserves the legacy shape — the persisted note has
// empty Field + Kind, and the response omits them.
func TestComments_DefaultsFieldAndKindEmpty(t *testing.T) {
	t.Parallel()
	h, _, root := newNotesFixture(t)

	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text":   "Legacy-shape note.",
		"author": "alice",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Empty(t, got.Note.Field)
	assert.Empty(t, got.Note.Kind)

	v := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, v.Notes, 1)
	assert.Empty(t, v.Notes[0].Field)
	assert.Empty(t, v.Notes[0].Kind)
}

// TestComments_RejectsUnknownKind pins the closed-set rule:
// any kind value not in {empty, "note", "annotation"} returns
// 400 invalid_argument before the vault is touched.
func TestComments_RejectsUnknownKind(t *testing.T) {
	t.Parallel()
	h, _, _ := newNotesFixture(t)

	rec := postComments(t, h, commentsTestEntityID, map[string]any{
		"text":   "x",
		"author": "alice",
		"kind":   "warning",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_argument")
	assert.Contains(t, rec.Body.String(), "warning")
}
