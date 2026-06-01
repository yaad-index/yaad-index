// edit_note / delete_note handlers per ADR-0015 §Note identity (#390
// Cut 2). Both target a note by its stable note_id (Cut 1), are
// author-gated (only the note's author may mutate it), and reuse the
// vault note-id back-compat stamping so a legacy id-less note in the
// same block is regularised on the write.
//
// - edit_note replaces text/field/kind in place and stamps
//   last_edited_at alongside the original created date.
// - delete_note hard-deletes the note (no tombstone — the vault's git
//   history + last_edited_at cover audit).

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// editNoteRequest is the PUT /v1/entities/{id}/notes/{note_id} body.
// text is required; field + kind replace the existing values (a note
// edit is a whole-note replace of the mutable fields, not a patch).
type editNoteRequest struct {
	Text  string `json:"text"`
	Field string `json:"field,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

type deleteNoteResponse struct {
	OK      bool   `json:"ok"`
	ID      string `json:"id"`
	NoteID  string `json:"note_id"`
	Deleted bool   `json:"deleted"`
}

// formatLastEdited renders a note's LastEditedAt for the wire: RFC3339
// UTC, or empty when the note has never been edited.
func formatLastEdited(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// canEditNote authorizes an edit/delete on a note per ADR-0015 §Note
// identity (#390): only the note's author may mutate it. Anonymous
// (dev-mode) claims bypass, mirroring the rest of the note surface. A
// note with no stored author cannot be edited by anyone (no author to
// match) outside dev-mode — a conservative default for legacy notes.
func canEditNote(claim *auth.Claim, note vault.Note) bool {
	if IsAnonymousClaim(claim) {
		return true
	}
	return note.Author != "" && claim.Subject == note.Author
}

// loadEntityForNoteMutation loads the store row + vault entity for an
// edit/delete. Unlike add_note it never auto-materializes — editing or
// deleting a note requires the entity (and its vault file) to exist.
// Returns the store row too so the caller can preserve CreatedAt and
// re-mirror GapState on the write-back (the UpsertEntity UPSERT path
// nulls any column it isn't handed).
func loadEntityForNoteMutation(logger *slog.Logger, r *http.Request, st store.Store, vaultReader *vault.Reader, id string) (*vault.Entity, *store.Entity, int, string, string) {
	if vaultReader == nil {
		return nil, nil, http.StatusServiceUnavailable, "vault_required",
			"note endpoints require vault.path configuration; notes live in vault files"
	}
	got, err := st.GetEntity(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, http.StatusNotFound, "not_found", fmt.Sprintf("no entity with id %s", id)
	}
	if err != nil {
		logger.ErrorContext(r.Context(), "store.GetEntity from note mutation", "err", err, "id", id)
		return nil, nil, http.StatusInternalServerError, "internal_error", "failed to look up entity"
	}
	ve, err := vaultReader.ReadByID(got.Kind, id)
	if err != nil {
		if vault.IsNotExist(err) {
			return nil, nil, http.StatusNotFound, "not_found", fmt.Sprintf("no vault file for id %s", id)
		}
		logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from note mutation", "err", err, "id", id)
		return nil, nil, http.StatusInternalServerError, "internal_error", "failed to read vault file"
	}
	return ve, got, 0, "", ""
}

// findNoteByID returns the index of the note with the given id, or -1.
func findNoteByID(notes []vault.Note, noteID string) int {
	for i := range notes {
		if notes[i].ID == noteID {
			return i
		}
	}
	return -1
}

// handleEditNote implements PUT /v1/entities/{id}/notes/{note_id}.
func handleEditNote(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		noteID := r.PathValue("note_id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"note endpoints require vault.path configuration; notes live in vault files")
			return
		}

		var req editNoteRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"text is required and must be non-empty after whitespace trim")
			return
		}
		field := strings.TrimSpace(req.Field)
		kind := strings.TrimSpace(req.Kind)
		switch kind {
		case "", vault.NoteKindNote, vault.NoteKindAnnotation:
		default:
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("kind=%q is not recognised (want %q or %q)", kind, vault.NoteKindNote, vault.NoteKindAnnotation))
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(), "edit_note reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}

		release, lockOK := acquireWriteLock(w, r, writeLocks, id)
		if !lockOK {
			return
		}
		defer release()

		ve, got, status, errCode, errMsg := loadEntityForNoteMutation(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		idx := findNoteByID(ve.Notes, noteID)
		if idx == -1 {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no note with note_id %s on %s", noteID, id))
			return
		}
		if !canEditNote(claim, ve.Notes[idx]) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the note's author may edit it")
			return
		}

		ve.Notes[idx].Text = text
		ve.Notes[idx].Field = field
		ve.Notes[idx].Kind = kind
		ve.Notes[idx].LastEditedAt = clock.Now().UTC().Truncate(time.Second)
		if err := vault.EnsureNoteIDs(ve.Notes); err != nil {
			logger.ErrorContext(r.Context(), "ensureNoteIDs from edit_note", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to stamp note ids")
			return
		}
		edited := ve.Notes[idx]

		var commitAuthor string
		if !IsAnonymousClaim(claim) {
			commitAuthor = claim.Subject
		}
		commitMsg := fmt.Sprintf("edit note %s on %s", noteID, ve.ID)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, agentAuthorRef(commitAuthor)); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from edit_note", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to write vault file")
			return
		}
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID:        ve.ID,
			Kind:      ve.Kind,
			Data:      vaultEntityDataForDB(ve),
			GapState:  vaultGapStateToStore(ve.GapState),
			CreatedAt: got.CreatedAt,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from edit_note", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to mirror note to DB")
			return
		}

		fresh, err := st.GetEntity(r.Context(), id)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity post-edit reread", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to reload entity")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(commentsResponse{
			OK: true,
			Note: noteEntry{
				ID:           edited.ID,
				Date:         edited.Date.UTC().Format(time.RFC3339),
				LastEditedAt: formatLastEdited(edited.LastEditedAt),
				Text:         edited.Text,
				Author:       edited.Author,
				Operator:     edited.Operator,
				Field:        edited.Field,
				Kind:         edited.Kind,
			},
			Entity: toAPIEntity(fresh),
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode edit_note response", "err", err, "id", id)
		}
	}
}

// handleDeleteNote implements DELETE /v1/entities/{id}/notes/{note_id}.
func handleDeleteNote(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		noteID := r.PathValue("note_id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"note endpoints require vault.path configuration; notes live in vault files")
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(), "delete_note reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}

		release, lockOK := acquireWriteLock(w, r, writeLocks, id)
		if !lockOK {
			return
		}
		defer release()

		ve, got, status, errCode, errMsg := loadEntityForNoteMutation(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		idx := findNoteByID(ve.Notes, noteID)
		if idx == -1 {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no note with note_id %s on %s", noteID, id))
			return
		}
		if !canEditNote(claim, ve.Notes[idx]) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the note's author may delete it")
			return
		}

		// Hard-delete: drop the note from the slice (no tombstone).
		ve.Notes = append(ve.Notes[:idx], ve.Notes[idx+1:]...)
		if err := vault.EnsureNoteIDs(ve.Notes); err != nil {
			logger.ErrorContext(r.Context(), "ensureNoteIDs from delete_note", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to stamp note ids")
			return
		}

		var commitAuthor string
		if !IsAnonymousClaim(claim) {
			commitAuthor = claim.Subject
		}
		commitMsg := fmt.Sprintf("delete note %s on %s", noteID, ve.ID)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, agentAuthorRef(commitAuthor)); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from delete_note", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to write vault file")
			return
		}
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID:        ve.ID,
			Kind:      ve.Kind,
			Data:      vaultEntityDataForDB(ve),
			GapState:  vaultGapStateToStore(ve.GapState),
			CreatedAt: got.CreatedAt,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from delete_note", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to mirror deletion to DB")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(deleteNoteResponse{
			OK: true, ID: ve.ID, NoteID: noteID, Deleted: true,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode delete_note response", "err", err, "id", id)
		}
	}
}
