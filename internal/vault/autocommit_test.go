package vault_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/vault"
)

// errCommitter is a minimal Committer stub whose OnWrite always
// returns the configured error — used to drive the
// commit-error-logging path without spinning up a real git tree.
type errCommitter struct{ err error }

func (c errCommitter) OnWrite(_ context.Context, _, _, _ string) error { return c.err }
func (c errCommitter) Close() error { return nil }

// requireGit skips when /usr/bin/git or equivalent isn't on PATH.
// CI containers should have git but personal-scale dev sandboxes
// might not — skip rather than fail.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
}

// initGitVault creates a temp-dir vault rooted as a git working tree
// with a baseline empty commit so subsequent vault writes have an
// existing HEAD to commit against.
func initGitVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "test@yaad-index.localhost"},
		{"config", "user.name", "test-yaad-index"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@yaad-index.localhost",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@yaad-index.localhost",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func gitLog(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "log", "--pretty=%s")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git log: %s", out)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mkEntity(id, kind string) *vault.Entity {
	return &vault.Entity{
		ID: id,
		Kind: kind,
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "test"},
	}
}

// Plain Write never auto-commits even when a Committer is wired.
// Only WriteWithCommit invokes the committer.
func TestWriter_Write_NoCommit(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{Logger: newSilentLogger()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committer.Close() })

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	require.NoError(t, w.Write(mkEntity("wikipedia:susanna-clarke", "wikipedia")))
	require.Equal(t, []string{"init"}, gitLog(t, root), "plain Write must not commit")
}

// WriteWithCommit on a git-rooted vault produces one commit with the
// supplied message.
func TestWriter_WriteWithCommit_PerOperation(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{Logger: newSilentLogger()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committer.Close() })

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, w.WriteWithCommit(ctx, mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))
	require.NoError(t, w.WriteWithCommit(ctx, mkEntity("wikipedia:b", "wikipedia"), "ingest: wikipedia:b", "agent:bob"))

	log := gitLog(t, root)
	require.Equal(t, []string{"ingest: wikipedia:b", "ingest: wikipedia:a", "init"}, log)
}

// A vault without `.git/` produces a write-side success and no git
// invocation. NewGitCommitter rejects the missing-dir; the calling
// path (main.go's buildAutoCommitter) is responsible for falling back
// to a nil committer. Here we verify NewGitCommitter's contract.
func TestNewGitCommitter_RejectsNonGitVault(t *testing.T) {
	requireGit(t)
	root := t.TempDir() // not a git repo
	_, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{Logger: newSilentLogger()})
	require.Error(t, err)
	require.Contains(t, err.Error(), ".git")
}

// With nil committer (the default), WriteWithCommit reduces to plain
// Write — no error, no git invocation, and crucially: does not panic
// trying to call OnWrite on nil.
func TestWriter_WriteWithCommit_NilCommitter(t *testing.T) {
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.WriteWithCommit(context.Background(), mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))
}

// NoopCommitter explicitly wired (e.g. by `auto_commit: false`)
// behaves like nil — write succeeds, no commits.
func TestWriter_WriteWithCommit_NoopCommitter(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	w, err := vault.NewWriter(root, vault.WithCommitter(vault.NoopCommitter{}))
	require.NoError(t, err)
	require.NoError(t, w.WriteWithCommit(context.Background(), mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))
	require.Equal(t, []string{"init"}, gitLog(t, root), "noop committer must not commit")
}

// Debounce: N writes within the debounce window collapse to a single
// rollup commit; writes after the window flush separately.
func TestGitCommitter_Debounce(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{
		Logger: newSilentLogger(),
		DebounceSeconds: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committer.Close() })

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, w.WriteWithCommit(ctx, mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))
	require.NoError(t, w.WriteWithCommit(ctx, mkEntity("wikipedia:b", "wikipedia"), "ingest: wikipedia:b", ""))
	require.NoError(t, w.WriteWithCommit(ctx, mkEntity("person:c", "person"), "fill: person:c [summary]", ""))

	// Within window: still no commit landed.
	require.Equal(t, []string{"init"}, gitLog(t, root), "writes inside debounce window should not commit yet")

	// Wait for the timer to fire + the commit to land.
	require.Eventually(t, func() bool {
		return len(gitLog(t, root)) == 2 // init + rollup
	}, 4*time.Second, 50*time.Millisecond, "debounce flush did not land")

	log := gitLog(t, root)
	require.Equal(t, "init", log[1])
	require.Equal(t, "bulk: fill 1, ingest 2", log[0])
}

// Close flushes any pending debounced commits even if the timer
// hasn't fired yet.
func TestGitCommitter_CloseFlushesPending(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{
		Logger: newSilentLogger(),
		DebounceSeconds: 60, // long window — Close must flush before the timer
	})
	require.NoError(t, err)

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	require.NoError(t, w.WriteWithCommit(context.Background(),
		mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))
	require.Equal(t, []string{"init"}, gitLog(t, root))

	require.NoError(t, committer.Close())

	log := gitLog(t, root)
	require.Equal(t, []string{"ingest: wikipedia:a", "init"}, log)

	// Close is idempotent.
	require.NoError(t, committer.Close())
}

// External git lock contention causes a single retry; the second
// attempt succeeds once we release the lock. We simulate by holding
// .git/index.lock during the first retry attempt.
func TestGitCommitter_RetriesOnIndexLock(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{Logger: newSilentLogger()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committer.Close() })

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	// Hold the lock briefly so the first git invocation hits
	// "Another git process" but the retry (after our 150ms backoff)
	// finds it released.
	lockPath := filepath.Join(root, ".git", "index.lock")
	f, err := os.Create(lockPath)
	require.NoError(t, err)
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = f.Close()
		_ = os.Remove(lockPath)
	}()

	require.NoError(t, w.WriteWithCommit(context.Background(),
		mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))
	require.Eventually(t, func() bool {
		log := gitLog(t, root)
		return len(log) == 2 && log[0] == "ingest: wikipedia:a"
	}, 2*time.Second, 50*time.Millisecond, "retry did not eventually commit")
}

// A no-op write (entity content byte-identical to HEAD) is logged as
// debug but does not error. The audit log already records this state
// via the prior commit.
func TestGitCommitter_NoOpWriteSucceeds(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{Logger: newSilentLogger()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committer.Close() })

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	ctx := context.Background()
	e := mkEntity("wikipedia:a", "wikipedia")
	require.NoError(t, w.WriteWithCommit(ctx, e, "ingest: wikipedia:a", ""))
	// Same entity, same body — second commit is a no-op.
	require.NoError(t, w.WriteWithCommit(ctx, e, "ingest: wikipedia:a", ""))

	log := gitLog(t, root)
	require.Equal(t, 2, len(log), "no-op write should not land a second commit; got %v", log)
}

// formatAuthor's "agent:X" handling produces a parseable git author.
func TestGitCommitter_AuthorThreaded(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{Logger: newSilentLogger()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committer.Close() })

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	require.NoError(t, w.WriteWithCommit(context.Background(),
		mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", "agent:bob"))

	cmd := exec.Command("git", "-C", root, "log", "-1", "--pretty=%an <%ae>")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	got := strings.TrimSpace(string(out))
	require.Equal(t, "bob <bob@yaad-index>", got)
}

// commit-side error must NOT fail the surrounding write — the vault
// file is the source of truth, the commit is best-effort. We force a
// commit failure by closing the committer first and verifying the
// next WriteWithCommit still returns nil (the write itself succeeded).
func TestWriter_WriteWithCommit_BestEffortOnCommitError(t *testing.T) {
	requireGit(t)
	root := initGitVault(t)
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{
		Logger: newSilentLogger(),
		DebounceSeconds: 60,
	})
	require.NoError(t, err)
	require.NoError(t, committer.Close())

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)

	// Closed committer rejects appendPending; WriteWithCommit must
	// still report success on the write itself.
	require.NoError(t, w.WriteWithCommit(context.Background(),
		mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))

	// The file landed even though the commit failed.
	dst := filepath.Join(root, "wikipedia", "a.md")
	_, err = os.Stat(dst)
	require.NoError(t, err, "vault file should exist despite commit failure")
}

// OnWrite errors must land in the operator's logs at WARN level so
// auto-commit failures aren't silent (yaad-index — git missing
// in the docker image was undetectable because writer.go discarded
// the error with `_ =`). The write itself MUST still succeed; the
// audit-commit is best-effort per ADR-0008.
func TestWriter_WriteWithCommit_LogsOnWriteError(t *testing.T) {
	root := t.TempDir() // no .git/ needed — committer is stubbed
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	sentinel := errors.New(`exec: "git": executable file not found in $PATH`)

	w, err := vault.NewWriter(root,
		vault.WithCommitter(errCommitter{err: sentinel}),
		vault.WithLogger(logger),
	)
	require.NoError(t, err)

	require.NoError(t, w.WriteWithCommit(context.Background(),
		mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""),
		"OnWrite error must NOT propagate to caller")

	out := buf.String()
	require.Contains(t, out, "level=WARN", "OnWrite error must land at WARN, got %q", out)
	require.Contains(t, out, "auto-commit OnWrite failed", "log should name the operation, got %q", out)
	require.Contains(t, out, filepath.Join("wikipedia", "a.md"), "log should include rel_path, got %q", out)
	require.Contains(t, out, "executable file not found", "log should include the underlying err text, got %q", out)
}

// Sentinel: ensure error-test for IsLockContended classifier doesn't
// silently regress (false positives would make every error a retry).
func TestIsLockContended_NotContendedOnGenericError(t *testing.T) {
	// Direct test of the unexported helper would require an internal
	// test; here we verify behavior end-to-end: a non-lock error from
	// runGit still surfaces (no infinite retry).
	requireGit(t)
	root := initGitVault(t)

	// Corrupt .git so any git invocation errors with a non-lock
	// failure, then verify Close doesn't hang or panic.
	committer, err := vault.NewGitCommitter(root, vault.GitCommitterOptions{
		Logger: newSilentLogger(),
		DebounceSeconds: 60,
	})
	require.NoError(t, err)

	w, err := vault.NewWriter(root, vault.WithCommitter(committer))
	require.NoError(t, err)
	require.NoError(t, w.WriteWithCommit(context.Background(),
		mkEntity("wikipedia:a", "wikipedia"), "ingest: wikipedia:a", ""))

	// Break .git so the flush errors. The Close error path must log
	// and return; not hang.
	require.NoError(t, os.RemoveAll(filepath.Join(root, ".git")))

	closeErr := committer.Close()
	// Close swallows the inner flush error and logs; returning nil is
	// the documented contract. We just confirm no hang and no panic.
	_ = closeErr
	if errors.Is(closeErr, context.Canceled) {
		t.Fatalf("close should not propagate context.Canceled, got %v", closeErr)
	}
}
