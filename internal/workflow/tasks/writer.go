// Task writer — mutates task frontmatter + handles the
// resolve-and-archive flow per ADR-0024 §"Task" close
// semantics. Read-side is in reader.go; the two sit in the
// same package so they share the on-disk format
// definitions + the path-traversal defense.

package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Committer is the narrow auto-commit interface tasks.Writer
// signals after every successful on-disk mutation per #314.
// Structurally identical to internal/vault.Committer so
// main.go can pass the same instance into both.
type Committer interface {
	OnWrite(ctx context.Context, relPath, message, author string) error
}

// Writer mutates tasks in the configured vault root. v1
// surface: Resolve marks a task done + optionally archives
// per ADR-0024 §"Task". Concurrent-safe (an internal mutex
// serializes the read-modify-write).
type Writer struct {
	vaultRoot  string
	tasksDir   string
	archiveDir string
	committer  Committer
	logger     *slog.Logger
	mu         sync.Mutex
}

// NewWriter returns a Writer rooted at `<vaultRoot>/tasks/`.
// The archive sub-directory `<vaultRoot>/tasks/_archive/`
// is created on first archived resolve.
//
// committer hooks resolve-time + archive-move writes into the
// vault auto-committer per #314. Nil disables the signal —
// on-disk write still lands, no git commit follows. Production
// wires the same vault.Committer the vault.Writer uses.
// logger may be nil; non-nil enables WARN logs on best-effort
// committer failures.
func NewWriter(vaultRoot string, opts ...WriterOption) *Writer {
	w := &Writer{
		vaultRoot:  vaultRoot,
		tasksDir:   filepath.Join(vaultRoot, "tasks"),
		archiveDir: filepath.Join(vaultRoot, "tasks", "_archive"),
		logger:     slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// WriterOption configures a Writer at construction time.
type WriterOption func(*Writer)

// WithCommitter wires the auto-commit signal per #314.
func WithCommitter(c Committer) WriterOption {
	return func(w *Writer) {
		if c != nil {
			w.committer = c
		}
	}
}

// WithLogger wires the operator logger so committer-OnWrite
// failures land at WARN.
func WithLogger(l *slog.Logger) WriterOption {
	return func(w *Writer) {
		if l != nil {
			w.logger = l
		}
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
		w.notifyCommit(context.Background(), activePath, fmt.Sprintf("task: %s: resolve-stamp", id), "")
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
	// #368 defensive unlink: os.Rename on POSIX atomically
	// unlinks the source, but explicit Stat + Remove catches
	// the edge cases where the rename succeeded but a stale
	// active file lingers (cross-fs rename-as-copy fallback on
	// some filesystems; out-of-band workflow trigger that
	// re-wrote the active path between the stamp and the
	// rename). Atomic-or-fail: if the cleanup unlink fails, we
	// also remove the archive copy so the caller doesn't see
	// `auto_archived: true` while both files coexist.
	if _, statErr := os.Stat(activePath); statErr == nil {
		if rmErr := os.Remove(activePath); rmErr != nil {
			_ = os.Remove(archivePath)
			return fmt.Errorf("unlink original task %q after archive: %w (archive copy rolled back)", activePath, rmErr)
		}
	}
	// Auto-commit the archive move per #314. Both the old (deleted)
	// path and the new (created) path need staging; the auto-
	// committer's `git add -A -- <path>` shape stages a deletion at
	// activePath when we signal that path AFTER the rename — so we
	// notify on both so the commit captures the move.
	w.notifyCommit(context.Background(), activePath, fmt.Sprintf("task: %s: archive", id), "")
	w.notifyCommit(context.Background(), archivePath, fmt.Sprintf("task: %s: archive", id), "")
	return nil
}

// notifyCommit signals the auto-committer (when wired) that
// the path was just mutated. Best-effort per the #314 design.
func (w *Writer) notifyCommit(ctx context.Context, path, message, author string) {
	if w.committer == nil {
		return
	}
	rel, err := filepath.Rel(w.vaultRoot, path)
	if err != nil {
		w.logger.WarnContext(ctx, "task-writer auto-commit relPath compute failed", "path", path, "err", err)
		return
	}
	if err := w.committer.OnWrite(ctx, rel, message, author); err != nil {
		w.logger.WarnContext(ctx, "task-writer auto-commit OnWrite failed", "path", rel, "err", err)
	}
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
