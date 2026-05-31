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
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// archiveResponse is the wire shape returned on a successful
// POST /v1/entities/{id}/archive or /restore call per ADR-0018
// step 2. The boolean tells the agent whether the row is now in
// the archived state — true post-archive, false post-restore.
type archiveResponse struct {
	OK bool `json:"ok"`
	ID string `json:"id"`
	Archived bool `json:"archived"`
}

// handleEntityArchive implements POST /v1/entities/{id}/archive
// per ADR-0018 step 2. Sets `archived_at = NOW()` on the entity
// row and moves the vault file from `<kind>/<slug>.md` to
// `_archive/<kind>/<slug>.md`. Idempotent: archiving an already-
// archived entity is a no-op + 200.
//
// Auth + audit shape mirrors handleEntityDelete (WARN audit log on
// every transition; auto-commit prefix `archive: <id>`). The
// transition is non-destructive — restore reverses it.
func handleEntityArchive(logger *slog.Logger, st store.Store, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return handleEntityArchiveTransition(logger, st, vaultWriter, writeLocks, true)
}

// handleEntityRestore implements POST /v1/entities/{id}/restore.
// Inverse of handleEntityArchive; same audit + commit shape.
func handleEntityRestore(logger *slog.Logger, st store.Store, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return handleEntityArchiveTransition(logger, st, vaultWriter, writeLocks, false)
}

// handleEntityArchiveTransition is the shared body for archive +
// restore. `archiving` selects the direction:
//
//	archiving=true → POST /archive: store.ArchiveEntity + vault move active→archive
//	archiving=false → POST /restore: store.RestoreEntity + vault move archive→active
//
// Order: vault move first (per ADR-0008 vault-as-source-of-truth),
// then DB toggle. A vault-move failure aborts the request before
// the DB is touched. A DB-toggle failure after a successful vault
// move logs at ERROR; reindex's incremental walk will reconcile on
// next pass (the `archived_at` flag is DB-only today, but the
// vault-side `_archive/` placement is the durable signal).
func handleEntityArchiveTransition(logger *slog.Logger, st store.Store, vaultWriter *vault.Writer, writeLocks *writelocks.Manager, archiving bool) http.HandlerFunc {
	op := "restore"
	if archiving {
		op = "archive"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing_id", "path missing entity id")
			return
		}
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				fmt.Sprintf("POST /v1/entities/{id}/%s requires vault.path configuration; the entity body lives in vault files", op))
			return
		}
		// Per-entity write-lock (yaad-index #23 + ADR-0024).
		release, ok := acquireWriteLock(w, r, writeLocks, id)
		if !ok {
			return
		}
		defer release()

		// Load the entity first so we can 404 cleanly when it
		// doesn't exist AND learn the kind for the vault move.
		got, err := st.GetEntity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity from "+op+"-entity", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load entity")
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				op+"-entity reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}

		// #377 nava-catch: UGC entities reach archive + restore via
		// this generic /v1/entities/{id}/(archive|restore) route in
		// addition to the UGC-specific delete path (which uses the
		// archive route directly). Without this gate a cross-operator
		// caller can archive a UGC entity through the generic surface
		// and chain it with the generic destroy to bypass the
		// canEditUserContent check entirely. Mirror the UGC permission
		// rule here keyed on the store row's operator.
		if got.Kind == "user-content" {
			storedOperator, _ := got.Data["operator"].(string)
			if !canEditByOperator(claim, storedOperator) {
				writeError(w, http.StatusForbidden, "operator_mismatch",
					"caller's operator does not match this user-content entity's operator")
				return
			}
		}

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}

		// Audit log per ADR-0018: archive/restore are non-
		// destructive but state-changing — log at INFO so they
		// surface in operator-side audit pipelines.
		logger.InfoContext(r.Context(), op+"-entity",
			"id", id,
			"kind", got.Kind,
			"agent", author,
			"anonymous", IsAnonymousClaim(claim))

		commitMsg := fmt.Sprintf("%s: %s", op, id)
		commitAuthor := agentAuthorRef(author)

		// Vault move first. Idempotence at the vault layer: when
		// the source path is missing AND the destination already
		// exists, ArchiveWithCommit / RestoreWithCommit treat that
		// as already-in-the-desired-state and return nil without a
		// commit.
		var vaultErr error
		if archiving {
			vaultErr = vaultWriter.ArchiveWithCommit(r.Context(), got.Kind, id, commitMsg, commitAuthor)
		} else {
			vaultErr = vaultWriter.RestoreWithCommit(r.Context(), got.Kind, id, commitMsg, commitAuthor)
		}
		if vaultErr != nil {
			if errors.Is(vaultErr, vault.ErrInvalidEntityID) {
				writeError(w, http.StatusBadRequest, "invalid_id",
					fmt.Sprintf("entity id %q is not the `<kind>:<local-id>` shape required for vault placement", id))
				return
			}
			if errors.Is(vaultErr, os.ErrNotExist) {
				// Vault-DB drift: DB row exists but vault file is in
				// neither location. Treat as 404 — operator should
				// reindex.
				logger.WarnContext(r.Context(),
					op+"-entity: vault file missing for DB-present entity (vault-DB drift)",
					"id", id, "kind", got.Kind, "err", vaultErr)
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("vault file for %s missing in both active and archive layouts", id))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Writer."+op+"WithCommit from "+op+"-entity",
				"err", vaultErr, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				fmt.Sprintf("failed to %s vault file", op))
			return
		}

		// DB toggle. ErrNotFound here means the row was deleted
		// between GetEntity and the toggle (race); treat as 404.
		var dbErr error
		if archiving {
			dbErr = st.ArchiveEntity(r.Context(), id)
		} else {
			dbErr = st.RestoreEntity(r.Context(), id)
		}
		if errors.Is(dbErr, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if dbErr != nil {
			// Vault move already succeeded; DB toggle failed. Log
			// loudly so the operator can investigate; reindex
			// reconciles future state from the vault side.
			logger.ErrorContext(r.Context(), "store."+op+"Entity from "+op+"-entity (vault already moved)",
				"err", dbErr, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to toggle archived_at; vault file already moved (reindex will reconcile)")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(archiveResponse{
			OK: true, ID: id, Archived: archiving,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities ("+op+") response", "err", err, "id", id)
		}
	}
}
