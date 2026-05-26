// Per-workflow CRUD surface per #277:
//
//   - GET    /v1/workflows/{name} — returns the on-disk markdown body
//   - PUT    /v1/workflows/{name} — pre-validates + atomic-writes
//   - DELETE /v1/workflows/{name} — idempotent file removal
//
// Vault-as-truth per ADR-0008: every mutation writes the file
// first; the workflow loader's mtime poll reconciles engine state
// on the next pass. PUT pre-validates via parser.Parse so an
// invalid body fails 422 WITHOUT a half-written file landing on
// disk + triggering a "file rejected" log line on the next poll.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// workflowNameRE pins the per-workflow URL path-segment shape:
// lowercase alphanumerics, hyphens, and underscores; no leading
// or trailing punctuation; no slashes (path-segment-safe). Same
// shape as the existing `instances[*].name` regex — keeps the
// operator-facing name vocabulary uniform across surfaces.
//
// Rejecting unsafe names at the handler entry point also closes
// the path-traversal vector: a name like "../../etc/passwd"
// doesn't even reach the filesystem resolver.
var workflowNameRE = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)

// workflowDefineMaxBytes caps the per-workflow file body size at
// 256 KiB. A normal workflow file is a few KiB of frontmatter +
// markdown + a YAML fence; 256 KiB is comfortably above any
// realistic operator-authored file without inviting a memory-
// exhaustion vector via the HTTP write surface.
const workflowDefineMaxBytes = 256 * 1024

// resolveWorkflowFilePath validates `name` against workflowNameRE
// + joins it to workflowDir as `<name>.md`. Returns the
// joined-and-cleaned absolute path on success; an error naming
// the invalid name otherwise.
//
// Cleaning + the regex pre-check together rule out path traversal
// (the joined path is always a direct child of workflowDir).
func resolveWorkflowFilePath(workflowDir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("workflow name is required")
	}
	if !workflowNameRE.MatchString(name) {
		return "", fmt.Errorf("workflow name %q is invalid (must match %s)",
			name, workflowNameRE.String())
	}
	path := filepath.Join(workflowDir, name+".md")
	cleaned := filepath.Clean(path)
	if filepath.Dir(cleaned) != filepath.Clean(workflowDir) {
		return "", fmt.Errorf("workflow path %q escapes workflow dir %q (refusing path-traversal)",
			cleaned, workflowDir)
	}
	return cleaned, nil
}

// handleWorkflowGet implements GET /v1/workflows/{name}. Returns
// the raw markdown body as `text/markdown; charset=utf-8`.
// Missing file → 404; invalid name → 400.
func handleWorkflowGet(logger *slog.Logger, workflowDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		path, err := resolveWorkflowFilePath(workflowDir, name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("workflow %q does not exist", name))
				return
			}
			logger.ErrorContext(r.Context(), "workflow get: read file",
				"err", err, "name", name, "path", path)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"workflow read failed")
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(data); err != nil {
			logger.ErrorContext(r.Context(), "workflow get: write response",
				"err", err, "name", name)
		}
	}
}

// handleWorkflowDefine implements PUT /v1/workflows/{name}. The
// request body is the full markdown file content (frontmatter +
// prose + YAML fence). Pre-validates via parser.Parse before
// touching the filesystem; if validation passes, writes atomically
// via a sibling tmp file + rename so the loader can't observe a
// partial-write state.
//
// Validation errors return 422 with the parser's error message —
// operator gets the structured rule violation without a half-
// written file on disk. The path-name vs frontmatter `name:`
// mismatch returns 400 (it's an addressing error, not a body-
// shape violation).
func handleWorkflowDefine(logger *slog.Logger, workflowDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		path, err := resolveWorkflowFilePath(workflowDir, name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, workflowDefineMaxBytes))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					fmt.Sprintf("request body exceeds %d bytes", workflowDefineMaxBytes))
				return
			}
			logger.ErrorContext(r.Context(), "workflow define: read body",
				"err", err, "name", name)
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("failed to read request body: %v", err))
			return
		}
		wf, err := parser.Parse(body)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_argument",
				fmt.Sprintf("workflow body failed validation: %v", err))
			return
		}
		if wf.Name != name {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("path name %q does not match frontmatter name %q "+
					"(per #277 — define the workflow at /v1/workflows/%s or "+
					"update the frontmatter `name:` field)",
					name, wf.Name, wf.Name))
			return
		}
		if err := atomicWriteFile(path, body, 0o644); err != nil {
			logger.ErrorContext(r.Context(), "workflow define: atomic write",
				"err", err, "name", name, "path", path)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"workflow write failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeOK(w, map[string]any{
			"name": name,
			"path": path,
		})
	}
}

// handleWorkflowDelete implements DELETE /v1/workflows/{name}.
// Idempotent: a missing file returns 200 (with `existed: false`)
// so callers can drive an end-state from any starting state.
// The next loader poll unregisters the workflow from the engine.
func handleWorkflowDelete(logger *slog.Logger, workflowDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		path, err := resolveWorkflowFilePath(workflowDir, name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		err = os.Remove(path)
		existed := true
		switch {
		case err == nil:
			// removed
		case os.IsNotExist(err):
			existed = false
		default:
			logger.ErrorContext(r.Context(), "workflow delete: remove file",
				"err", err, "name", name, "path", path)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"workflow delete failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeOK(w, map[string]any{
			"name":    name,
			"existed": existed,
		})
	}
}

// atomicWriteFile writes data to path via a sibling tmp file +
// rename. The rename is atomic at the filesystem level on the
// same volume — the loader's mtime poll sees either the old file
// or the new file, never a partial-write. tmp file lives in the
// same dir so the rename stays on the same filesystem.
//
// The parent dir MUST exist at call time. Provisioning happens
// at daemon boot in `cmd/yaad-index/main.go` via the
// `ensureWorkflowDir` helper — single source of dir creation,
// fail-fast at start if the path is uncreatable. Mirrors the
// #284 plugin data-dir pattern.
//
// On any error after the tmp file is created, the tmp file is
// removed; the target path is left untouched.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create tmpfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write tmpfile %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod tmpfile %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close tmpfile %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// writeOK encodes a 200 success envelope `{ok: true, ...fields}`.
// Used by define/delete; the get handler streams the raw markdown
// body so it doesn't go through this path.
func writeOK(w http.ResponseWriter, fields map[string]any) {
	out := map[string]any{"ok": true}
	for k, v := range fields {
		out[k] = v
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		// header already committed; best-effort
		_, _ = fmt.Fprintf(w, "encode ok response: %v", err)
	}
}
