package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// handleEntityAttachment implements GET /v1/entities/{id}/attachments/{name}
// per ADR-0018 step 6 §Attachments. The file lives under the entity's
// own subdir (active or archive); only manifest-listed attachments are
// reachable — the manifest IS the contract surface.
//
// The handler resolves through `vault.Reader.OpenAttachment`, which:
// - Validates `name` (no path separators, no leading dots, no
// traversal segments).
// - Reads the entity .md (active path with archive fallback) and
// finds the manifest entry matching `name`.
// - Validates the manifest's `Path` (no `..` escape, no absolute
// paths).
// - Joins onto the entity dir and opens the file.
//
// Wire shapes:
//
//	200 OK + binary stream (Content-Type from manifest.Kind, falls
//	 back to extension-based detection by http.DetectContentType
//	 on the first 512 bytes when manifest.Kind is empty).
//	400 invalid_attachment_name — name segment failed validation.
//	404 not_found — entity, manifest entry, or backing file missing.
//	503 vault_required — daemon running without vault.path configured.
func handleEntityAttachment(logger *slog.Logger, st store.Store, vaultReader *vault.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if vaultReader == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"GET /v1/entities/{id}/attachments/{name} requires vault.path configuration; the file lives in the vault subdir")
			return
		}
		id := r.PathValue("id")
		id, rerr := resolveEntityID(r.Context(), st, id)
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to resolve entity reference")
			return
		}
		name := r.PathValue("name")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing_id", "path missing entity id")
			return
		}
		if name == "" {
			writeError(w, http.StatusBadRequest, "missing_name", "path missing attachment name")
			return
		}

		// Resolve the entity's kind via the store — needed to construct
		// the vault path. Avoids re-walking the filesystem.
		got, err := st.GetEntity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity from attachment-stream", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load entity")
			return
		}

		f, manifest, info, err := vaultReader.OpenAttachment(got.Kind, id, name)
		if err != nil {
			switch {
			case errors.Is(err, vault.ErrInvalidAttachmentName):
				writeError(w, http.StatusBadRequest, "invalid_attachment_name", err.Error())
			case errors.Is(err, vault.ErrAttachmentNotInManifest):
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("entity %s has no attachment named %q", id, name))
			case vault.IsNotExist(err):
				// Either the .md file or the backing file is missing.
				// Manifest says it should be there; surface as 404 with
				// a drift hint so the operator knows reindex may help.
				logger.WarnContext(r.Context(),
					"attachment-stream: manifest entry exists but file missing (manifest-disk drift)",
					"id", id, "kind", got.Kind, "name", name, "err", err)
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("attachment %q for %s missing on disk; reindex may reconcile", name, id))
			default:
				logger.ErrorContext(r.Context(), "vault.OpenAttachment", "err", err,
					"id", id, "kind", got.Kind, "name", name)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to open attachment")
			}
			return
		}
		defer func() { _ = f.Close() }()

		if manifest.Kind != "" {
			w.Header().Set("Content-Type", manifest.Kind)
		}
		// http.ServeContent handles Range requests, Last-Modified, and
		// Content-Length. It also auto-detects Content-Type from the
		// first 512 bytes when the response header is empty — so the
		// extension-fallback shape just works when manifest.Kind isn't
		// set. Requires an io.ReadSeeker; *os.File satisfies that.
		seeker, ok := f.(io.ReadSeeker)
		if !ok {
			// Defensive — vault.Reader.OpenAttachment returns *os.File,
			// which is a ReadSeeker. If a future refactor swaps in a
			// non-seekable implementation, fall back to plain copy
			// (loses Range support but keeps the response valid).
			logger.WarnContext(r.Context(),
				"attachment-stream: file impl is not seekable; falling back to io.Copy",
				"id", id, "name", name)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
			w.WriteHeader(http.StatusOK)
			if _, err := io.Copy(w, f); err != nil {
				logger.WarnContext(r.Context(), "attachment-stream: copy errored mid-write",
					"err", err, "id", id, "name", name)
			}
			return
		}
		// Use the URL-decoded name as ServeContent's first arg — it
		// drives extension-based Content-Type detection when the
		// header isn't already set.
		http.ServeContent(w, r, name, info.ModTime(), seeker)
	}
}
