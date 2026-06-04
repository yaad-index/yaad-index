package api

import (
	"context"
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

// userContentRenameRequest is the POST /v1/user-content/{id}/rename body.
// The new slug (and thus the new id) is derived from new_title
// server-side, the same way create derives the slug from title — so the
// caller renames by intent (the human title) rather than by computing a
// slug.
type userContentRenameRequest struct {
	NewTitle string `json:"new_title"`
}

// handleUserContentRename implements POST /v1/user-content/{id}/rename
// (#425 Cut 2): change a UGC entity's id from `user-content:<old-slug>`
// to `user-content:<new-slug>` by retitling it. Unlike move (which keeps
// the id and relocates the file), rename changes the identity:
//
//   - the vault file is renamed to the new slug (in its current location,
//     flat or subfolder), with the new id + title in its frontmatter and
//     the attachment sidecar carried along;
//   - the store row, every edge (inbound + outbound), provenance, and
//     aliases are re-keyed to the new id in one transaction
//     (store.RenameEntity);
//   - the bare old slug is aliased to the new id, so existing
//     `user-content:<old-slug>` references still resolve.
//
// Same operator-edit gate as move/section/delete. A new slug that
// collides with a live entity, a vault file, or an existing alias is a
// 409. A new title that slugifies to the current slug is an idempotent
// no-op (200, unchanged).
func handleUserContentRename(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if vaultReader == nil || vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content rename requires vault.path configuration")
			return
		}

		id := r.PathValue("id")
		id, rerr := resolveEntityID(r.Context(), st, id)
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to resolve entity reference")
			return
		}

		var req userContentRenameRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		newTitle := strings.TrimSpace(req.NewTitle)
		if newTitle == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"new_title is required")
			return
		}
		newSlug, err := vault.SlugFromTitle(newTitle)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("new_title slugifies to empty: %v", err))
			return
		}
		newID := userContentIDPrefix + newSlug

		// The entity must exist and be user-content. A missing row is a
		// 404; a non-UGC kind is rejected (this endpoint is UGC-only).
		got, err := st.GetEntity(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no user-content entity with id %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "store.GetEntity from user-content rename", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to look up entity")
			return
		}
		if got.Kind != userContentKind {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("id %s is kind %q, not user-content; rename is a user-content operation", id, got.Kind))
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
				"user-content rename reached without an auth claim — middleware misconfigured", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}

		// Load the entity under the lock and apply the UGC edit-permission
		// gate BEFORE mutating — same operator-match check every other UGC
		// mutation (move / section / delete) enforces, so rename can't
		// re-key another operator's entity.
		ve, err := vaultReader.ReadByID(userContentKind, id)
		if err != nil {
			if vault.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for user-content entity %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Reader.ReadByID (pre-rename auth) from user-content rename", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to read vault file")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "operator_mismatch",
				"caller's operator does not match this user-content entity's operator")
			return
		}

		// New title slugifies to the current slug → no identity change.
		// Idempotent no-op (return the entity unchanged), mirroring move's
		// same-subfolder no-op.
		if newID == id {
			writeUserContentEntity(w, logger, r, ve, http.StatusOK)
			return
		}

		// Collision probe — the new slug must be free across the store
		// row, the vault file, AND the alias resolver. An alias collision
		// matters because the bare new slug already resolving to some
		// other entity would be silently shadowed by the new id (and the
		// old->new back-reference we add would fight it).
		if code, msg, conflict := userContentSlugConflict(r.Context(), st, vaultReader, id, newID, newSlug); conflict {
			writeError(w, code, "conflict", msg)
			return
		}

		// Build the renamed entity: same body / tags / provenance, new id
		// + title. The body is unchanged, so the content-derived etag is
		// stable across the rename (an in-flight section-edit If-Match
		// still validates).
		newEntity := *ve
		newEntity.ID = newID
		newEntity.Data = cloneDataWithIDTitle(ve.Data, newID, newTitle)

		commitMsg := userContentRenameCommitMessage(id, newID, author)
		if err := vaultWriter.RenameUserContentSlug(r.Context(), userContentKind, id, &newEntity, commitMsg, agentAuthorRef(author)); err != nil {
			if vault.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for user-content entity %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Writer.RenameUserContentSlug from user-content rename", "err", err, "id", id, "new_id", newID)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to rename vault file")
			return
		}

		// Mirror the re-key to the store (vault already renamed). On
		// failure the vault is the source of truth — reindex reconciles
		// the store on the next walk — but surface it so the caller knows
		// the DB view lagged. Matches create's vault-first contract.
		if err := st.RenameEntity(r.Context(), id, newID, newEntity.Data); err != nil {
			logger.ErrorContext(r.Context(), "store.RenameEntity from user-content rename (vault already renamed)",
				"err", err, "id", id, "new_id", newID)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror rename to DB")
			return
		}

		writeUserContentEntity(w, logger, r, &newEntity, http.StatusOK)
	}
}

// writeUserContentEntity renders a UGC entity envelope (the same shape
// create / get / move return): the entity fields, a content-derived
// ETag, and a Location pointing at the entity's own id. Used by rename
// for both the no-op and the renamed responses.
func writeUserContentEntity(w http.ResponseWriter, logger *slog.Logger, r *http.Request, ve *vault.Entity, status int) {
	sections := vault.ParseSections(ve.CleanContent)
	out := userContentEntityResponse{
		OK:         true,
		ID:         ve.ID,
		Kind:       ve.Kind,
		Data:       ve.Data,
		Tags:       ve.Tags,
		Provenance: vaultProvenanceToAPI(ve.Provenance),
		Sections:   buildSectionsPage(sections, 0, sectionsDefaultLimit),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", userContentEtag(ve.CleanContent))
	w.Header().Set("Location", "/v1/user-content/"+ve.ID)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		logger.ErrorContext(r.Context(), "encode user-content entity response", "err", err, "id", ve.ID)
	}
}

// userContentSlugConflict reports whether newID / newSlug is already taken
// by a live entity row, a vault file, or an existing alias that points at
// some entity other than the one being renamed (selfID). Returns the HTTP
// status + message to write when it conflicts.
func userContentSlugConflict(ctx context.Context, st store.Store, vaultReader *vault.Reader, selfID, newID, newSlug string) (int, string, bool) {
	if _, err := st.GetEntity(ctx, newID); err == nil {
		return http.StatusConflict,
			fmt.Sprintf("a user-content entity with id %s already exists; pick a different title", newID), true
	} else if !errors.Is(err, store.ErrNotFound) {
		return http.StatusInternalServerError, "failed to check id availability", true
	}
	if _, err := vaultReader.ReadByID(userContentKind, newID); err == nil {
		return http.StatusConflict,
			fmt.Sprintf("a user-content file with slug %q already exists in the vault; pick a different title", newSlug), true
	}
	if exists, err := vaultReader.UserContentSlugInSubfolder(newSlug); err != nil {
		return http.StatusInternalServerError, "failed to check slug availability", true
	} else if exists {
		return http.StatusConflict,
			fmt.Sprintf("a user-content file with slug %q already exists in the vault; pick a different title", newSlug), true
	}
	if resolved, err := st.ResolveAlias(ctx, newSlug, userContentKind); err != nil {
		return http.StatusInternalServerError, "failed to check alias availability", true
	} else if resolved != "" && resolved != selfID {
		return http.StatusConflict,
			fmt.Sprintf("the slug %q is already an alias for %s; pick a different title", newSlug, resolved), true
	}
	return 0, "", false
}

// cloneDataWithIDTitle returns a shallow copy of the entity data map with
// id + title overridden — so re-keying does not mutate the source map the
// pre-rename read returned.
func cloneDataWithIDTitle(in map[string]any, newID, newTitle string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	out["id"] = newID
	out["title"] = newTitle
	return out
}

// userContentRenameCommitMessage builds the auto-commit message for a
// rename (`rename:` prefix per the autocommit convention).
func userContentRenameCommitMessage(oldID, newID, author string) string {
	msg := fmt.Sprintf("rename: %s -> %s", oldID, newID)
	if author != "" {
		msg += " by " + author
	}
	return msg
}
