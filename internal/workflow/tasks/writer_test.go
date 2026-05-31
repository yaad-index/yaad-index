package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWriter_Resolve_StampsAndArchives: the happy path —
// resolve writes resolved_at + moves the file to
// _archive/.
func TestWriter_Resolve_StampsAndArchives(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-s", "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n")
	w := NewWriter(vault)
	when := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	require.NoError(t, w.Resolve("wf-s", when, true))

	// Active file gone; archive file present with stamp.
	_, err := os.Stat(filepath.Join(vault, "tasks", "wf-s.md"))
	assert.True(t, os.IsNotExist(err), "active path empty after archive")
	body, err := os.ReadFile(filepath.Join(vault, "tasks", "_archive", "wf-s.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "resolved_at: 2026-05-16T12:00:00Z\n")
}

// TestWriter_Resolve_NoArchive_KeepsInPlace: autoArchive=
// false stamps resolved_at but leaves the file in the
// active dir.
func TestWriter_Resolve_NoArchive_KeepsInPlace(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-s", "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n")
	w := NewWriter(vault)
	when := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	require.NoError(t, w.Resolve("wf-s", when, false))

	body, err := os.ReadFile(filepath.Join(vault, "tasks", "wf-s.md"))
	require.NoError(t, err, "task stays in active dir")
	assert.Contains(t, string(body), "resolved_at: 2026-05-16T12:00:00Z\n")
	_, err = os.Stat(filepath.Join(vault, "tasks", "_archive", "wf-s.md"))
	assert.True(t, os.IsNotExist(err), "no archive file when autoArchive=false")
}

// TestWriter_Resolve_Idempotent: re-resolving an active
// task preserves the original resolved_at stamp.
func TestWriter_Resolve_Idempotent(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-s", "---\nkind: task\nworkflow: wf\ncreated_at: 2026-05-16T10:00:00Z\n---\n")
	w := NewWriter(vault)
	t1 := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

	require.NoError(t, w.Resolve("wf-s", t1, false))
	require.NoError(t, w.Resolve("wf-s", t2, false))

	body, _ := os.ReadFile(filepath.Join(vault, "tasks", "wf-s.md"))
	got := string(body)
	assert.Equal(t, 1, strings.Count(got, "resolved_at:"))
	assert.Contains(t, got, "resolved_at: 2026-05-16T12:00:00Z\n",
		"original stamp preserved")
}

// TestWriter_Resolve_AlreadyArchived_NoOp: resolving a task
// that already lives under _archive/ is a no-op success.
func TestWriter_Resolve_AlreadyArchived_NoOp(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	archiveDir := filepath.Join(vault, "tasks", "_archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, "wf-s.md"),
		[]byte("---\nresolved_at: 2026-01-01T00:00:00Z\n---\n"), 0o644))

	w := NewWriter(vault)
	require.NoError(t, w.Resolve("wf-s", time.Now(), true))
}

// TestWriter_Resolve_NotFound: missing task → ErrTaskNotFound.
func TestWriter_Resolve_NotFound(t *testing.T) {
	t.Parallel()
	w := NewWriter(t.TempDir())
	err := w.Resolve("absent", time.Now(), true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskNotFound)
}

// TestWriter_Resolve_PathTraversalRejected: id with `/` or
// `\` rejects with a clear error.
func TestWriter_Resolve_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	w := NewWriter(t.TempDir())
	err := w.Resolve("../etc/passwd", time.Now(), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path separator")
}

// recordingTasksCommitter captures Committer.OnWrite calls so
// tests can pin the #314 auto-commit signal contract for
// tasks.Writer.Resolve.
type recordingTasksCommitter struct {
	calls []recordingTasksCommitterCall
}

type recordingTasksCommitterCall struct {
	relPath string
	message string
}

func (c *recordingTasksCommitter) OnWrite(_ context.Context, relPath, message, _ string) error {
	c.calls = append(c.calls, recordingTasksCommitterCall{relPath: relPath, message: message})
	return nil
}

// TestWriter_Resolve_NotifiesCommitterOnArchive pins #314 for
// the resolve-and-archive path: the resolve-stamp write signals
// once, then the archive-move signals BOTH the source (deletion)
// + destination (creation) paths so the auto-committer captures
// the move in one staging pass. Per #374 the two archive-move
// signals carry distinct messages — the source unlink lands as
// "task: <id>: archive (unlink source)" and the dest lands as
// the primary "task: <id>: archive" line. The dest signal MUST
// fire last: `git log --oneline` is newest-first, so the LAST
// notifyCommit produces the top line; emitting the primary
// archive line last is what makes the batch-resolve log
// scannable (the scannability goal of #374).
func TestWriter_Resolve_NotifiesCommitterOnArchive(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-s", "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n")
	c := &recordingTasksCommitter{}
	w := NewWriter(vault, WithCommitter(c))
	require.NoError(t, w.Resolve("wf-s", time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC), true))
	require.Len(t, c.calls, 3, "stamp + archive-source-unlink + archive-dest")
	assert.Equal(t, "tasks/wf-s.md", c.calls[0].relPath)
	assert.Contains(t, c.calls[0].message, ": resolve-stamp")

	// #374 archive-source-unlink signal fires FIRST so the dest
	// commit lands newer + tops `git log --oneline`.
	assert.Equal(t, "tasks/wf-s.md", c.calls[1].relPath)
	assert.Equal(t, "task: wf-s: archive (unlink source)", c.calls[1].message,
		"source-unlink message is distinct from the dest archive message")

	// #374 archive-dest signal fires LAST — primary archive line
	// becomes the newest commit so `git log --oneline` after a
	// batch resolve reads as one scannable "archive" line per task.
	assert.Equal(t, filepath.Join("tasks", "_archive", "wf-s.md"), c.calls[2].relPath)
	assert.Equal(t, "task: wf-s: archive", c.calls[2].message,
		"archive-dest carries the primary archive message + lands newest")
}

// TestWriter_Resolve_NotifiesCommitterOnStampOnly pins that
// autoArchive=false still signals the resolved_at-stamp write
// (the only on-disk mutation in that branch). No phantom
// archive signal is emitted.
func TestWriter_Resolve_NotifiesCommitterOnStampOnly(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-s", "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n")
	c := &recordingTasksCommitter{}
	w := NewWriter(vault, WithCommitter(c))
	require.NoError(t, w.Resolve("wf-s", time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC), false))
	require.Len(t, c.calls, 1)
	assert.Contains(t, c.calls[0].message, ": resolve-stamp")
}

// TestWriter_Resolve_NilCommitterIsNoOp pins the back-compat
// path — no committer wired means no calls + no panic.
func TestWriter_Resolve_NilCommitterIsNoOp(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-s", "---\nkind: task\nworkflow: wf\nsubject: s\ncreated_at: 2026-05-16T10:00:00Z\n---\n\n## s\n\nx\n")
	w := NewWriter(vault) // no committer
	require.NoError(t, w.Resolve("wf-s", time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC), true))
}

// TestWriter_Resolve_UnlinksOriginal_NoStaleActiveFile pins #368:
// after Resolve(auto-archive=true) returns, the active path
// is gone and the archive path exists. Belt-and-suspenders
// confirmation that os.Rename's atomicity holds + the defensive
// cleanup catches any rename-as-copy fallback.
func TestWriter_Resolve_UnlinksOriginal_NoStaleActiveFile(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeTask(t, vault, "wf-x", "---\nkind: task\nworkflow: wf\nsubject: x\ncreated_at: 2026-05-31T10:00:00Z\n---\n\n## x\n\nbody\n")
	w := NewWriter(vault)

	require.NoError(t, w.Resolve("wf-x", time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), true))

	// Active path must NOT exist post-resolve.
	_, activeErr := os.Stat(filepath.Join(vault, "tasks", "wf-x.md"))
	assert.True(t, os.IsNotExist(activeErr),
		"active path must be unlinked after auto-archive resolve")

	// Archive path must exist with the resolved_at stamp.
	body, err := os.ReadFile(filepath.Join(vault, "tasks", "_archive", "wf-x.md"))
	require.NoError(t, err, "archive copy must be present")
	assert.Contains(t, string(body), "resolved_at: 2026-05-31T12:00:00Z\n")
}

// The defensive Stat + Remove branch inside Resolve (catches a
// stale active file when os.Rename succeeded but the source
// lingers — cross-fs rename-as-copy fallback or out-of-band
// rewrite between stamp and rename) isn't reachable from a unit
// test against a real filesystem: on POSIX rename is atomic, so
// the source is gone before Stat fires; the no-op idempotency
// guard at the top of Resolve catches a second invocation before
// the rename even runs. The branch remains as a belt-and-
// suspenders rollback path — the happy-path coverage above pins
// the invariant the caller depends on.
