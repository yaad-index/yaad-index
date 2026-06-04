package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// userContentMoveRequest is the POST /v1/user-content/{id}/move body.
// An empty / omitted subfolder moves the file to the flat
// `user-content/<slug>.md` path.
type userContentMoveRequest struct {
	Subfolder string `json:"subfolder,omitempty"`
}

// handleUserContentMove implements POST /v1/user-content/{id}/move (#425
// Cut 1): relocate a UGC entity's vault file to a different subfolder
// in place, without the archive -> delete -> recreate dance. The entity
// id stays flat (#415 — subfolder is path-only), so provenance, edges,
// and the DB row (all keyed by the flat id) are preserved by
// construction; only the on-disk path moves. Same subfolder is an
// idempotent no-op; a bad subfolder pattern rejects with the same 400
// shape as create.
func handleUserContentMove(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if vaultReader == nil || vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content move requires vault.path configuration")
			return
		}

		id := r.PathValue("id")
		id, rerr := resolveEntityID(r.Context(), st, id)
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to resolve entity reference")
			return
		}

		var req userContentMoveRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		subfolder := strings.TrimSpace(req.Subfolder)
		if subfolder != "" && !userContentSubfolderPattern.MatchString(subfolder) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"subfolder must be a single path segment of lowercase alphanumerics and hyphens (e.g. notes, drafts, projects)")
			return
		}

		// The entity must exist and be user-content. A missing row is a
		// 404; a non-UGC kind is rejected (this endpoint is UGC-only).
		got, err := st.GetEntity(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no user-content entity with id %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "store.GetEntity from user-content move", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to look up entity")
			return
		}
		if got.Kind != userContentKind {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("id %s is kind %q, not user-content; move is a user-content operation", id, got.Kind))
			return
		}

		release, lockOK := acquireWriteLock(w, r, writeLocks, id)
		if !lockOK {
			return
		}
		defer release()

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content move reached without an auth claim — middleware misconfigured", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}

		// Load the entity under the lock and apply the UGC edit-permission
		// gate BEFORE mutating — the same operator-match check every other
		// UGC mutation (section / frontmatter / delete / archive) enforces,
		// so move can't relocate another operator's file (closes the #377
		// cross-operator bypass class for this new endpoint). A move leaves
		// the body unchanged, so this pre-move read also serves the
		// response.
		ve, err := vaultReader.ReadByID(userContentKind, id)
		if err != nil {
			if vault.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for user-content entity %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Reader.ReadByID (pre-move auth) from user-content move", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to read vault file")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "operator_mismatch",
				"caller's operator does not match this user-content entity's operator")
			return
		}

		commitMsg := userContentMoveCommitMessage(id, subfolder, author)
		if _, err := vaultWriter.MoveToSubfolder(r.Context(), userContentKind, id, subfolder, commitMsg, agentAuthorRef(author)); err != nil {
			if vault.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for user-content entity %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Writer.MoveToSubfolder from user-content move", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to move vault file")
			return
		}

		sections := vault.ParseSections(ve.CleanContent)
		out := userContentEntityResponse{
			OK: true,
			ID: ve.ID,
			Kind: ve.Kind,
			Data: ve.Data,
			Tags: ve.Tags,
			Provenance: vaultProvenanceToAPI(ve.Provenance),
			Sections: buildSectionsPage(sections, 0, sectionsDefaultLimit),
		}
		w.Header().Set("Content-Type", "application/json")
		// The etag is content-derived; a move leaves the body unchanged,
		// so the etag is stable across a move (an in-flight section-edit
		// If-Match still validates) — the file location moved, the body
		// did not.
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.Header().Set("Location", "/v1/user-content/"+id)
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content/{id}/move response", "err", err, "id", id)
		}
	}
}

// userContentMoveCommitMessage builds the auto-commit message for a move
// (`move:` prefix per the autocommit convention). Names the destination
// subfolder, or "(flat)" when moving to the flat path.
func userContentMoveCommitMessage(id, subfolder, author string) string {
	dst := "(flat)"
	if subfolder != "" {
		dst = subfolder + "/"
	}
	msg := fmt.Sprintf("move: %s -> %s", id, dst)
	if author != "" {
		msg += " by " + author
	}
	return msg
}
