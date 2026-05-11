package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// entityDeleteResponse is the wire shape returned on a successful
// DELETE /v1/entities/{id} call. Mirrors the user-content delete
// response shape; agents reading the result key on `ok` + `id`.
type entityDeleteResponse struct {
	OK bool `json:"ok"`
	ID string `json:"id"`
	Deleted bool `json:"deleted"`
}

// handleEntityDelete implements DELETE /v1/entities/{id} per
// alice2-index.
//
// **Destructive.** The entity's vault file is removed (with auto-
// commit producing the audit trail); the DB row + its inbound +
// outbound edges + provenance entries cascade through
// `store.DeleteEntityCascade`. There is no soft-delete + undo today.
//
// **Audit:**
//
// - WARN-level slog on every successful delete, naming the
// calling agent (claim.Subject), the entity id, and the kind.
// Lands in the operator's logs (docker logs / journald) for
// after-the-fact visibility.
// - The auto-commit chain (vault.Writer.DeleteWithCommit's
// Committer hook) produces a git commit recording the delete
// event. THAT commit is the durable provenance trail —
// alice2-index spec'd "provenance entry on every delete";
// since the entity is being cascade-deleted, a per-entity
// provenance row would itself be deleted. The audit-commit
// message survives in git history regardless.
//
// **Auth:** same Bearer-JWT gate as ingest. Anonymous bypass (when
// auth.required=false) is permitted by the middleware; the WARN
// audit-log records the synthetic claim's subject in that case.
func handleEntityDelete(logger *slog.Logger, st store.Store, vaultWriter *vault.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing_id", "path missing entity id")
			return
		}
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"DELETE /v1/entities/{id} requires vault.path configuration; the entity body lives in vault files")
			return
		}

		// Load the entity first so we can 404 cleanly when it
		// doesn't exist AND learn the kind for the vault delete.
		// vault.Writer.DeleteWithCommit takes (kind, full-id) and
		// derives the local-id internally — but we want a 404 BEFORE
		// trying the vault remove (a vault-only file with no DB row
		// would otherwise return 500 from the DB-cascade step).
		got, err := st.GetEntity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity from delete-entity", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load entity")
			return
		}

		// ADR-0018 step 4 state-machine: DELETE is only valid on
		// archived entities. An active entity (archived_at IS NULL)
		// is rejected with 409 + a hint pointing at the
		// archive-first path. Operators with a "I really mean it"
		// intent confirm via two explicit calls (archive + delete);
		// no `?confirm=permanent` flag, no opt-in skip — the
		// state-machine IS the safety property.
		if got.ArchivedAt == nil {
			writeError(w, http.StatusConflict, "must archive before delete",
				fmt.Sprintf("POST /v1/entities/%s/archive first; DELETE only destroys archived entities (ADR-0018)", id))
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"delete-entity reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}

		// Pre-destroy audit log Per the prior design, + ADR-0018 step 4. WARN
		// level so the destructive op surfaces above the default
		// INFO threshold — operators tailing logs see destroys
		// immediately even when log_level is at its default.
		logger.WarnContext(r.Context(), "destroy-entity",
			"id", id,
			"kind", got.Kind,
			"agent", author,
			"anonymous", IsAnonymousClaim(claim))

		commitMsg := entityDestroyCommitMessage(id, got.Kind, author)
		commitAuthor := agentAuthorRef(author)

		// Vault first (per ADR-0008): a vault-write failure aborts
		// the request and the DB stays intact. After the archive-
		// path file is gone, the DB cascade tidies up. The vault
		// path is `_archive/<kind>/<slug>.md` — DestroyArchivedWithCommit
		// removes from the archive subtree per ADR-0018 step 4.
		if err := vaultWriter.DestroyArchivedWithCommit(r.Context(), got.Kind, id, commitMsg, commitAuthor); err != nil {
			if errors.Is(err, vault.ErrInvalidEntityID) {
				writeError(w, http.StatusBadRequest, "invalid_id",
					fmt.Sprintf("entity id %q is not the `<kind>:<local-id>` shape required for vault placement", id))
				return
			}
			if errors.Is(err, os.ErrNotExist) {
				logger.WarnContext(r.Context(),
					"destroy-entity: vault file missing for archived DB-present entity (vault-DB drift)",
					"id", id, "kind", got.Kind, "err", err)
				// Don't 404 here — the DB row IS present and
				// archived; the cascade should still run to keep
				// state consistent.
			} else {
				logger.ErrorContext(r.Context(), "vault.Writer.DestroyArchivedWithCommit from destroy-entity",
					"err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to remove archived vault file")
				return
			}
		}

		if err := st.DeleteEntityCascade(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
			// Vault file is gone but DB cascade failed. Reindex's
			// disappear-pass will reconcile on next walk; flag it
			// loudly so the operator can investigate.
			logger.ErrorContext(r.Context(), "store.DeleteEntityCascade from destroy-entity (vault file already removed)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to drop entity from DB; vault file is already removed (reindex will reconcile)")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(entityDeleteResponse{
			OK: true, ID: id, Deleted: true,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities (delete) response", "err", err, "id", id)
		}
	}
}
