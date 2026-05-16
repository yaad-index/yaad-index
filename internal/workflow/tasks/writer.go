// Task writer — mutates task frontmatter + handles the
// resolve-and-archive flow per ADR-0024 §"Task" close
// semantics. Read-side is in reader.go; the two sit in the
// same package so they share the on-disk format
// definitions + the path-traversal defense.

package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Writer mutates tasks in the configured vault root. v1
// surface: Resolve marks a task done + optionally archives
// per ADR-0024 §"Task". Concurrent-safe (an internal mutex
// serializes the read-modify-write).
type Writer struct {
	tasksDir   string
	archiveDir string
	mu         sync.Mutex
}

// NewWriter returns a Writer rooted at `<vaultRoot>/tasks/`.
// The archive sub-directory `<vaultRoot>/tasks/_archive/`
// is created on first archived resolve.
func NewWriter(vaultRoot string) *Writer {
	return &Writer{
		tasksDir:   filepath.Join(vaultRoot, "tasks"),
		archiveDir: filepath.Join(vaultRoot, "tasks", "_archive"),
	}
}

// Resolve marks the task done by stamping
// `resolved_at: <now>` into the frontmatter. When
// autoArchive is true (default for normal tasks unless
// the originating workflow declared `auto_archive_on_done:
// false`; always true for err-tasks per ADR-0024 §"Runtime
// errors") the file is moved to `tasks/_archive/<id>.md`
// after the write. Idempotent: re-resolving an already-
// resolved task is a no-op success (the stamp stays put);
// resolving an archived task is also a no-op success.
func (w *Writer) Resolve(id string, now time.Time, autoArchive bool) error {
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("tasks: id %q contains path separator", id)
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	activePath := filepath.Join(w.tasksDir, id+".md")
	archivePath := filepath.Join(w.archiveDir, id+".md")

	// Re-resolving an already-archived task is a no-op.
	if _, err := os.Stat(archivePath); err == nil {
		return nil
	}

	body, err := os.ReadFile(activePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, id)
		}
		return fmt.Errorf("read task %q: %w", activePath, err)
	}
	updated, err := stampResolvedAt(string(body), now)
	if err != nil {
		return err
	}
	if updated != string(body) {
		if err := writeAtomic(activePath, []byte(updated)); err != nil {
			return err
		}
	}
	if !autoArchive {
		return nil
	}
	if err := os.MkdirAll(w.archiveDir, 0o755); err != nil {
		return fmt.Errorf("mkdir archive dir: %w", err)
	}
	if err := os.Rename(activePath, archivePath); err != nil {
		return fmt.Errorf("archive task %q → %q: %w", activePath, archivePath, err)
	}
	return nil
}

// stampResolvedAt rewrites the task body's frontmatter to
// include `resolved_at: <RFC3339>`. When the field already
// exists (re-resolve), preserves the existing timestamp —
// resolve is idempotent. When the file has no frontmatter
// at all, returns the input unchanged (no resolved_at to
// stamp).
func stampResolvedAt(body string, now time.Time) (string, error) {
	const opener = "---\n"
	const closer = "\n---\n"
	if !strings.HasPrefix(body, opener) {
		return body, nil
	}
	rest := body[len(opener):]
	idx := strings.Index(rest, closer)
	if idx < 0 {
		return body, nil
	}
	fmBlock := rest[:idx]
	postBody := rest[idx+len(closer):]

	// Already-resolved? Preserve.
	for _, line := range strings.Split(fmBlock, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "resolved_at:") {
			return body, nil
		}
	}
	stamp := "resolved_at: " + now.UTC().Format(time.RFC3339)
	newFM := strings.TrimRight(fmBlock, "\n") + "\n" + stamp
	return opener + newFM + closer + postBody, nil
}

// writeAtomic mirrors the writer pattern from FileTaskWriter
// — temp file + rename. Permissions match a standard
// markdown file.
func writeAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write temp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q → %q: %w", tmp, path, err)
	}
	return nil
}
