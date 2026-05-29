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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
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
//
// #268 mirrors the err-task into the store on first-create
// (kind=task, errored=true in the data map) so the same
// /v1/entities lookup + workflow-set_property surface works
// for err tasks alongside regular tasks. No triggered_by
// edge — err tasks aren't entity-scoped.
type FileErrTaskWriter struct {
	vaultRoot string
	// store mirrors first-create err-task files into entity
	// rows (kind=task, errored=true). Nil-safe; see
	// NewFileErrTaskWriter for semantics.
	store store.Store
	// committer hooks err-task writes into the vault auto-
	// committer per #314 so workflow-driven append-to-existing-
	// err-task mutations land in git alongside entity writes.
	// Nil-safe — the on-disk write still lands.
	committer vault.Committer
	logger    *slog.Logger
	mu        sync.Mutex
}

// NewFileErrTaskWriter constructs a writer rooted at the
// vault path. Mirrors NewFileTaskWriter's signature. st may
// be nil for test fixtures that don't wire a backing store
// (the on-disk file shape stays the same); production wires
// a real store so err tasks land as first-class entities.
// committer may be nil for test fixtures; production wires the
// same vault.Committer the vault.Writer uses so every err-task
// append lands a git commit.
// logger may be nil; falls back to slog.Default().
func NewFileErrTaskWriter(vaultRoot string, st store.Store, committer vault.Committer, logger *slog.Logger) *FileErrTaskWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &FileErrTaskWriter{vaultRoot: vaultRoot, store: st, committer: committer, logger: logger}
}

// AppendErrTask implements ErrTaskWriter.
func (w *FileErrTaskWriter) AppendErrTask(ctx context.Context, workflow string, when time.Time, entityID, errMsg string) error {
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
		if err := w.writeAtomic(path, freshErrTaskBody(workflow, when, line)); err != nil {
			return err
		}
		w.notifyCommit(ctx, path, taskCommitMessage(workflow, "", "err-create"), workflowAuthor(workflow))
		w.materializeErrTaskEntity(ctx, workflow, when)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing err task %q: %w", path, err)
	}

	// #337 Cut 1: append the new failure line to the err-task's
	// notes section. The 5-section schema replaces the legacy
	// `## Failures` markdown section; every failure still lands
	// (no dedup — each is distinct by timestamp). Cut 2's
	// add_note bounded primitive will replace this inline
	// parse+render with a method call.
	body, err := appendErrFailureLine(existing, line)
	if err != nil {
		return err
	}
	if err := w.writeAtomic(path, body); err != nil {
		return err
	}
	w.notifyCommit(ctx, path, taskCommitMessage(workflow, "", "err-append"), workflowAuthor(workflow))
	return nil
}

// notifyCommit mirrors FileTaskWriter.notifyCommit for the
// err-task surface — best-effort auto-commit signal after
// every successful write. Per #314.
func (w *FileErrTaskWriter) notifyCommit(ctx context.Context, path, message, author string) {
	if w.committer == nil {
		return
	}
	rel, err := filepath.Rel(w.vaultRoot, path)
	if err != nil {
		w.logger.WarnContext(ctx, "err-task auto-commit relPath compute failed", "path", path, "err", err)
		return
	}
	if err := w.committer.OnWrite(ctx, rel, message, author); err != nil {
		w.logger.WarnContext(ctx, "err-task auto-commit OnWrite failed", "path", rel, "err", err)
	}
}

// materializeErrTaskEntity mirrors the err-task into the
// store. Best-effort — store failures log at WARN and the
// on-disk file remains authoritative. No automatic backfill
// in v1.x; a row that failed to upsert here stays absent
// until the err task is recreated.
func (w *FileErrTaskWriter) materializeErrTaskEntity(ctx context.Context, workflow string, when time.Time) {
	if w.store == nil {
		return
	}
	wfSlug := slugify(workflow)
	if wfSlug == "" {
		return
	}
	taskID := canonical.TaskKind + ":" + wfSlug + "-err"
	if err := w.store.UpsertEntity(ctx, &store.Entity{
		ID:   taskID,
		Kind: canonical.TaskKind,
		Data: map[string]any{
			"workflow":   workflow,
			"errored":    true,
			"created_at": when.UTC().Format(time.RFC3339),
		},
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		w.logger.WarnContext(ctx, "err-task entity store upsert failed (vault file landed)",
			"task_id", taskID, "err", err)
	}
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
	return filepath.Join(w.vaultRoot, vault.KindDir(canonical.TaskKind), wfSlug+"-err.md")
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

// errTaskPromptTemplate is the prompt-section body for an
// err-task. Operators reading the task see the instruction
// describing what failed and how to handle it; agents can
// route on the workflow name in frontmatter for automated
// triage. The prompt itself is stable across err appends —
// the per-failure detail lands in the notes section per #337
// Cut 1.
const errTaskPromptTemplate = "Workflow %q produced one or more execution failures. " +
	"Review the failure entries in the notes section, address the underlying causes, and " +
	"resolve or archive this err-task once the workflow runs cleanly."

// freshErrTaskBody renders the initial err-task file body —
// frontmatter (`kind: task` + `errored: true`) + the 5-section
// schema with the prompt populated and the first failure line
// landing in the notes section per #337 Cut 1.
func freshErrTaskBody(workflow string, when time.Time, line string) []byte {
	prompt := fmt.Sprintf(errTaskPromptTemplate, workflow)
	body, err := RenderTaskSections(TaskSections{
		Prompt: prompt,
		Notes:  line,
	})
	if err != nil {
		// Renderer only errors on empty prompt; we always supply
		// one above. Treat as programmer-bug — fall back to a
		// minimal body so the caller still gets a writable file.
		body = "<!-- yaad-index prompt -->\n" + prompt + "\n<!-- /yaad-index prompt -->\n"
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("kind: task\n")
	b.WriteString("errored: true\n")
	b.WriteString("workflow: " + workflow + "\n")
	b.WriteString("created_at: " + when.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("---\n\n")
	b.WriteString(body)
	return []byte(b.String())
}

// appendErrFailureLine parses an existing err-task body, adds
// the new failure line to the notes section, and renders the
// 5-section schema back. Cut 2's bounded add_note primitive
// will absorb this logic; Cut 1 uses it inline so the append
// branch stays semantically aligned with the schema.
func appendErrFailureLine(existing []byte, line string) ([]byte, error) {
	frontmatter, sectionsBody, err := splitFrontmatter(string(existing))
	if err != nil {
		return nil, fmt.Errorf("err-task append: %w", err)
	}
	sections, err := ParseTaskSections(sectionsBody)
	if err != nil {
		return nil, fmt.Errorf("err-task append: %w", err)
	}
	if sections.Notes == "" {
		sections.Notes = line
	} else {
		sections.Notes = sections.Notes + "\n" + line
	}
	body, err := RenderTaskSections(sections)
	if err != nil {
		return nil, fmt.Errorf("err-task append: %w", err)
	}
	return []byte(frontmatter + body), nil
}

// splitFrontmatter splits a task body into (frontmatterBlock,
// remainder) at the second `---` line. The frontmatterBlock
// retains its leading `---` / yaml / trailing `---` and the
// double newline that follows so the caller can concatenate
// back without re-shaping. When no frontmatter is present
// (no leading `---` line) returns ("", body) so callers can
// always concat.
func splitFrontmatter(body string) (string, string, error) {
	if !strings.HasPrefix(body, "---\n") {
		return "", body, nil
	}
	// Find the closing `---` on its own line.
	closeIdx := strings.Index(body[4:], "\n---\n")
	if closeIdx < 0 {
		return "", "", fmt.Errorf("malformed frontmatter: missing closing ---")
	}
	end := 4 + closeIdx + len("\n---\n")
	// Consume the blank line the renderer emits between the
	// frontmatter and the section markers (so re-render
	// produces byte-stable output).
	if strings.HasPrefix(body[end:], "\n") {
		end++
	}
	return body[:end], body[end:], nil
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
