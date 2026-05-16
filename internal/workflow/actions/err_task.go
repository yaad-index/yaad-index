// Err-task pattern (Phase 5.B). Per ADR-0024 §"Runtime
// errors — the err-task pattern":
//
//   - ONE err task per workflow, ever — not per-failure.
//   - Frontmatter: `kind: task` + `errored: true` (NOT a
//     separate kind; the errored flag is the surface
//     filter).
//   - First failure CREATES the err task; subsequent
//     failures APPEND failure details (timestamp + source
//     entity + error message) to the body.
//   - Operator-resolve CLOSES the err task; the next
//     failure spawns a fresh one. v1 close-mechanism is
//     operator-deletes-the-file — the engine treats a
//     missing err-task file as "no open err task," so the
//     next AppendErrTask creates a new one.
//   - Err tasks always auto-archive on operator-resolve;
//     the `auto_archive_on_done: false` flag for normal
//     tasks does NOT apply (the task-resolution side
//     enforces this).
//   - Surfaced alongside normal tasks via the
//     `errored: true` filter; first-class in `task.list`
//     (Phase 6 agent surface).

package actions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrTaskWriter is the err-task surface the engine invokes
// when a workflow run fails systemically (condition-eval
// error, subject-render error, action-runner non-MissingRef
// error). Production wires a vault-backed implementation
// that finds-or-creates the canonical err-task file at
// `tasks/<workflow>-err.md`; tests substitute fakes.
type ErrTaskWriter interface {
	// AppendErrTask appends a failure entry to the
	// workflow's err task, creating the file if absent.
	// when stamps the failure timestamp; entityID names
	// the source entity (empty for target-less failures
	// or pre-resolve errors); errMsg is the wrapped
	// failure message.
	AppendErrTask(ctx context.Context, workflow string, when time.Time, entityID, errMsg string) error
}

// FileErrTaskWriter is the production-default ErrTaskWriter
// — writes/appends to `<vault>/tasks/<workflow>-err.md`
// using the same atomic-write + slugified-path pattern as
// FileTaskWriter. Safe for concurrent use; an internal
// mutex serializes the read-modify-write cycle.
type FileErrTaskWriter struct {
	vaultRoot string
	mu        sync.Mutex
}

// NewFileErrTaskWriter constructs a writer rooted at the
// vault path. Mirrors NewFileTaskWriter's signature.
func NewFileErrTaskWriter(vaultRoot string) *FileErrTaskWriter {
	return &FileErrTaskWriter{vaultRoot: vaultRoot}
}

// AppendErrTask implements ErrTaskWriter.
func (w *FileErrTaskWriter) AppendErrTask(_ context.Context, workflow string, when time.Time, entityID, errMsg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if strings.TrimSpace(workflow) == "" {
		return fmt.Errorf("FileErrTaskWriter: workflow is empty")
	}
	path := w.errTaskPath(workflow)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir tasks dir: %w", err)
	}

	line := formatFailureLine(when, entityID, errMsg)

	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// First failure since the last resolution — create
		// the err task file with frontmatter + Failures
		// section + the first entry.
		return w.writeAtomic(path, freshErrTaskBody(workflow, when, line))
	}
	if err != nil {
		return fmt.Errorf("read existing err task %q: %w", path, err)
	}

	// Append the new failure line to the existing
	// Failures section. Re-uses the mergeSection helper
	// (Phase 4) with append-anyway so every failure lands
	// (no skip-dedup on err entries — each failure is
	// distinct by timestamp).
	body, err := mergeSection(string(existing), errTaskSectionName, line, "append-anyway")
	if err != nil {
		return err
	}
	return w.writeAtomic(path, []byte(body))
}

// errTaskPath returns the canonical err-task file path:
// `<vault>/tasks/<workflow-slug>-err.md`. The `-err` suffix
// distinguishes the err task from regular workflow tasks
// (which use `<workflow>-<subject>` paths) without colliding
// when subject="err" (operators authoring a literal `subject:
// err` workflow would land at `<workflow>-err.md` too; in
// practice subjects rendered from CEL like `entity.id` won't
// produce "err", and the operator-side filename collision is
// a rare-and-fixable surface).
func (w *FileErrTaskWriter) errTaskPath(workflow string) string {
	wfSlug := slugify(workflow)
	return filepath.Join(w.vaultRoot, "tasks", wfSlug+"-err.md")
}

// writeAtomic mirrors FileTaskWriter.writeFile — temp file +
// rename. Kept inline rather than extracted to share the
// shape across siblings.
func (w *FileErrTaskWriter) writeAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write temp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q → %q: %w", tmp, path, err)
	}
	return nil
}

// errTaskSectionName is the body section every failure line
// appends to. Kept distinct from regular task sections so a
// future shared-vocabulary surface (Phase 6 task.* MCP) can
// branch on it.
const errTaskSectionName = "Failures"

// freshErrTaskBody renders the initial err-task file body —
// frontmatter (`kind: task` + `errored: true`) + the
// Failures section header + the first failure line.
func freshErrTaskBody(workflow string, when time.Time, line string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("kind: task\n")
	b.WriteString("errored: true\n")
	b.WriteString("workflow: " + workflow + "\n")
	b.WriteString("created_at: " + when.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("## " + errTaskSectionName + "\n\n")
	b.WriteString(line + "\n")
	return []byte(b.String())
}

// formatFailureLine renders one entry for the Failures
// section. Single-line shape (no embedded newlines) so the
// mergeSection helper's line-based dedup model works cleanly:
//
//	- 2026-05-16T18:00:00Z (boardgame:b): condition: cel-go error: ...
//
// entityID is omitted when empty (target-less manual fires
// or pre-resolve errors).
func formatFailureLine(when time.Time, entityID, errMsg string) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(when.UTC().Format(time.RFC3339))
	if entityID != "" {
		b.WriteString(" (")
		b.WriteString(entityID)
		b.WriteString(")")
	}
	b.WriteString(": ")
	// Collapse internal newlines so the line stays single-
	// line per the mergeSection contract.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(errMsg, "\n", " "), "\r", " "))
	return b.String()
}

// StubErrTaskWriter is the zero-config default for tests +
// dev binaries running without a vault. Discards every
// AppendErrTask call. Production wires FileErrTaskWriter via
// main.go when the vault path is configured.
type StubErrTaskWriter struct{}

// AppendErrTask on StubErrTaskWriter is a no-op success.
func (StubErrTaskWriter) AppendErrTask(_ context.Context, _ string, _ time.Time, _, _ string) error {
	return nil
}
