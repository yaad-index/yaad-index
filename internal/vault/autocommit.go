// Auto-commit-on-write per yaad-index the source issue.
//
// When the vault root is a git working tree, vault.Writer can hand
// every successful atomic write to a Committer that records it as a
// git commit summarizing the operation. The vault becomes its own
// audit log: ingest, fill, and note writes show up as one commit
// each (or one batched commit per debounce window).
//
// Detection + opt-out semantics live in the operator config layer
// (`vault.auto_commit`, `vault.auto_commit_debounce_seconds`,
// `vault.auto_push`); the wiring in cmd/yaad-index/main.go
// constructs the right Committer flavor and passes it via
// WithCommitter. Tests + non-vault deploys leave Committer nil,
// in which case Writer skips the commit step entirely.

package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
)

// Committer hooks vault writes into a git auto-commit pipeline. The
// Writer calls OnWrite after every successful atomic rename; the
// committer is responsible for staging the file and producing a
// commit (per-operation or batched, depending on configuration).
//
// Implementations MUST be safe for concurrent OnWrite calls — the
// Writer does not serialize per-vault. Implementations that batch
// (debounce) carry their own queue + timer.
//
// Errors from OnWrite are logged but DO NOT fail the surrounding
// vault.Writer.Write — per ADR-0008 the vault file landed
// successfully; the audit-log commit is best-effort.
type Committer interface {
	// OnWrite records a successful write of relPath (relative to the
	// vault root) with a human-readable summary of the operation
	// that produced it. author is the calling agent's identity (e.g.
	// "agent:bob", "user:alice") or empty to fall back to the
	// committer identity.
	OnWrite(ctx context.Context, relPath, message, author string) error

	// Close flushes any pending debounced commits and releases
	// resources. Safe to call multiple times.
	Close() error
}

// NoopCommitter is the zero-cost default — no git, no commits.
// Tests, non-git vaults, and `auto_commit: false` deploys all use
// this implementation.
type NoopCommitter struct{}

// OnWrite discards the write notification.
func (NoopCommitter) OnWrite(_ context.Context, _, _, _ string) error { return nil }

// Close is a no-op.
func (NoopCommitter) Close() error { return nil }

// GitCommitterOptions configures a GitCommitter.
type GitCommitterOptions struct {
	// CommitterName / CommitterEmail are the git committer identity
	// stamped on every commit. Empty values fall back to
	// "yaad-index" / "yaad-index@localhost".
	CommitterName string
	CommitterEmail string

	// DebounceSeconds collapses bursty writes into batched commits.
	// 0 → per-operation commit (1:1 audit). >0 → collect writes
	// for N seconds, commit a single rollup with a summarized
	// message. Long debounce windows trade audit fidelity for
	// fewer process spawns.
	DebounceSeconds int

	// AutoPush runs `git push` after every commit (best-effort).
	// Push failures log but do not fail the vault write.
	AutoPush bool

	// Logger receives commit / push / retry events. Required.
	Logger *slog.Logger
}

// GitCommitter implements Committer by shelling out to `git` against
// a working tree. Construct via NewGitCommitter; the constructor
// validates that <vaultRoot>/.git exists.
type GitCommitter struct {
	root string
	committerName string
	committerEmail string
	debounce time.Duration
	autoPush bool
	logger *slog.Logger

	// mu serializes our own git invocations; concurrent OnWrite calls
	// queue here. External git invocations (operator hand-commits)
	// race on .git/index.lock and are handled by retry-with-backoff
	// inside runGit.
	mu sync.Mutex

	// debouncer state (only used when debounce > 0). flushTimer fires
	// fireFlush, which drains pending under pendingMu and commits
	// under mu. Close races against fireFlush by also draining pending
	// under pendingMu and committing under mu — whichever party sees
	// pending non-empty wins; the loser sees nil and no-ops.
	//
	// Close does NOT wait for an already-launched fireFlush goroutine
	// to finish its commitBatch — if fireFlush won the pendingMu race
	// before Close, Close finds c.pending == nil, skips its own
	// commitBatch (so it never acquires mu), and may return while the
	// fireFlush commit is still mid-flight. Auto-commit is best-effort
	// per the documented contract; an in-flight commit interrupted by
	// process exit is acceptable. If a future caller needs strong
	// flush-on-close, add a sync.WaitGroup around commitBatch.
	pending []pendingWrite
	pendingMu sync.Mutex
	flushTimer *time.Timer
	closed bool
}

type pendingWrite struct {
	relPath string
	message string
	author string
}

// NewGitCommitter constructs a GitCommitter for the given vault root.
// The root must contain a `.git/` directory (a git working tree);
// callers detect this at server startup before constructing.
func NewGitCommitter(vaultRoot string, opts GitCommitterOptions) (*GitCommitter, error) {
	if !filepath.IsAbs(vaultRoot) {
		return nil, fmt.Errorf("git committer: vault root must be absolute, got %q", vaultRoot)
	}
	if opts.Logger == nil {
		return nil, errors.New("git committer: logger is required")
	}
	if opts.DebounceSeconds < 0 {
		return nil, fmt.Errorf("git committer: debounce must be >= 0, got %d", opts.DebounceSeconds)
	}
	gitDir := filepath.Join(vaultRoot, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return nil, fmt.Errorf("git committer: %s missing or unreadable: %w", gitDir, err)
	}

	name := opts.CommitterName
	if name == "" {
		name = "yaad-index"
	}
	email := opts.CommitterEmail
	if email == "" {
		email = "yaad-index@localhost"
	}

	return &GitCommitter{
		root: vaultRoot,
		committerName: name,
		committerEmail: email,
		debounce: time.Duration(opts.DebounceSeconds) * time.Second,
		autoPush: opts.AutoPush,
		logger: opts.Logger,
	}, nil
}

// OnWrite stages the path and either commits immediately (debounce
// disabled) or appends to the pending batch and arms a flush timer.
func (c *GitCommitter) OnWrite(ctx context.Context, relPath, message, author string) error {
	if c.debounce == 0 {
		return c.commitSingle(ctx, relPath, message, author)
	}
	return c.appendPending(ctx, relPath, message, author)
}

// Close flushes any pending debounced commits and rejects further
// OnWrite calls. Idempotent.
func (c *GitCommitter) Close() error {
	c.pendingMu.Lock()
	if c.closed {
		c.pendingMu.Unlock()
		return nil
	}
	c.closed = true
	if c.flushTimer != nil {
		c.flushTimer.Stop()
		c.flushTimer = nil
	}
	pending := c.pending
	c.pending = nil
	c.pendingMu.Unlock()

	if len(pending) > 0 {
		if err := c.commitBatch(context.Background(), pending); err != nil {
			c.logger.Warn("auto-commit close: final flush failed", "error", err)
		}
	}
	return nil
}

func (c *GitCommitter) commitSingle(ctx context.Context, relPath, message, author string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// `git add -A -- <paths...>` (instead of plain `add`) so this same
	// code path stages create, modify, AND delete uniformly: a write
	// call after Writer.DeleteWithCommit removed the file would
	// otherwise fail with `pathspec did not match any files`. The
	// `-- <paths...>` argument scopes the -A to just the entity's
	// relPath + (per #314) its sibling subtree, so we don't sweep
	// unrelated working-tree changes into the commit.
	addArgs := append([]string{"add", "-A", "--"}, c.stagePathsFor(ctx, relPath)...)
	if err := c.runGit(ctx, addArgs...); err != nil {
		return fmt.Errorf("git add %s: %w", relPath, err)
	}
	args := []string{"commit", "-m", message}
	if author != "" {
		args = append(args, "--author", c.formatAuthor(author))
	}
	if err := c.runGit(ctx, args...); err != nil {
		// `git commit` exits 1 with "nothing to commit" when the staged
		// file is byte-identical to HEAD. A no-op write is not an error
		// for the audit log; the prior commit already records this state.
		if isNothingToCommit(err) {
			c.logger.Debug("auto-commit: nothing to commit (no-op write)", "path", relPath)
			return nil
		}
		return fmt.Errorf("git commit: %w", err)
	}
	c.logger.Debug("auto-commit: commit landed", "path", relPath, "message", message)
	if c.autoPush {
		c.tryPush(ctx)
	}
	return nil
}

func (c *GitCommitter) appendPending(_ context.Context, relPath, message, author string) error {
	c.pendingMu.Lock()
	if c.closed {
		c.pendingMu.Unlock()
		return errors.New("git committer: closed")
	}
	c.pending = append(c.pending, pendingWrite{relPath: relPath, message: message, author: author})
	if c.flushTimer == nil {
		c.flushTimer = time.AfterFunc(c.debounce, c.fireFlush)
	}
	c.pendingMu.Unlock()
	return nil
}

func (c *GitCommitter) fireFlush() {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = nil
	c.flushTimer = nil
	c.pendingMu.Unlock()
	if len(pending) == 0 {
		return
	}
	if err := c.commitBatch(context.Background(), pending); err != nil {
		c.logger.Warn("auto-commit batch flush failed", "error", err, "writes", len(pending))
	}
}

func (c *GitCommitter) commitBatch(ctx context.Context, pending []pendingWrite) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// `git add -A` so create / modify / delete all stage uniformly
	// (mirrors the change in commitSingle). Path scoping via `--`
	// keeps -A bounded to just the pending entries' relPaths +
	// (per #314) each entry's sibling subtree.
	addArgs := []string{"add", "-A", "--"}
	seen := make(map[string]struct{}, len(pending)*2)
	for _, p := range pending {
		for _, candidate := range c.stagePathsFor(ctx, p.relPath) {
			if _, dup := seen[candidate]; dup {
				continue
			}
			seen[candidate] = struct{}{}
			addArgs = append(addArgs, candidate)
		}
	}
	if err := c.runGit(ctx, addArgs...); err != nil {
		return fmt.Errorf("git add (batch %d): %w", len(pending), err)
	}

	message := summarizeBatch(pending)
	if err := c.runGit(ctx, "commit", "-m", message); err != nil {
		if isNothingToCommit(err) {
			c.logger.Debug("auto-commit batch: nothing to commit (all no-op)")
			return nil
		}
		return fmt.Errorf("git commit (batch): %w", err)
	}
	c.logger.Debug("auto-commit: batch landed", "writes", len(pending), "message", message)
	if c.autoPush {
		c.tryPush(ctx)
	}
	return nil
}

func (c *GitCommitter) tryPush(ctx context.Context) {
	if err := c.runGit(ctx, "push"); err != nil {
		c.logger.Warn("auto-push failed (non-fatal)", "error", err)
		return
	}
	c.logger.Debug("auto-push: pushed")
}

// runGit executes a git subcommand against the vault root with one
// retry on .git/index.lock contention (operator's external git
// invocation racing with ours). Identity flags are passed via env so
// the operator's user-level gitconfig doesn't override the per-server
// committer identity.
func (c *GitCommitter) runGit(ctx context.Context, args ...string) error {
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.execGit(ctx, args...)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isLockContended(err) {
			return err
		}
		c.logger.Debug("auto-commit: index lock contended, retrying", "attempt", attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return lastErr
}

func (c *GitCommitter) execGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.root}, args...)...)
	// TZ env propagates the operator-configured timezone (yaad-index
	// PR-C) into git's commit timestamp rendering. Default
	// time.UTC.String() = "UTC"; named locations like
	// "Europe/Berlin" resolve via /usr/share/zoneinfo. Per ADR-
	// 0008 audit-log commits then carry operator-TZ in the commit
	// log alongside the vault frontmatter's operator-TZ stamps.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+c.committerName,
		"GIT_AUTHOR_EMAIL="+c.committerEmail,
		"GIT_COMMITTER_NAME="+c.committerName,
		"GIT_COMMITTER_EMAIL="+c.committerEmail,
		"TZ="+clock.Location().String(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &gitError{
			args: args,
			err: err,
			stdout: stdout.String(),
			stderr: stderr.String(),
		}
	}
	return nil
}

// stagePathsFor returns the set of paths (relative to the vault
// root) that `git add` must include to capture every on-disk
// change associated with one logical entity write per #314. For
// an entity main file `<kind>/<slug>.md`, that's the .md path
// itself plus the sibling `<kind>/<slug>/` subtree where the
// daemon writes attachments + other sidecars. The subtree is
// only added when it exists on disk to avoid `pathspec did not
// match any files` failures on writes without sidecars.
//
// For relPaths that don't look like an entity main file (e.g.
// `tasks/<id>.md` from the workflow surface, or any non-.md
// path), only the relPath itself is returned — the entity-subtree
// convention is `<kind>/<slug>.md` paired with `<kind>/<slug>/`,
// and broadening the rule beyond that shape risks staging
// unrelated working-tree paths.
func (c *GitCommitter) stagePathsFor(ctx context.Context, relPath string) []string {
	paths := []string{relPath}
	subtree := entitySubtreeFor(relPath)
	if subtree == "" {
		return paths
	}
	// Stage the sibling subtree when it exists on disk — a write that
	// landed attachments alongside the main file (#314).
	if info, err := os.Stat(filepath.Join(c.root, subtree)); err == nil && info.IsDir() {
		return append(paths, subtree)
	}
	// The subtree is absent on disk. When the main file is ALSO absent
	// this is a delete (DeleteWithCommit removed both before this commit,
	// #444): the subtree may still be tracked in git, and those tracked
	// files' deletions must be staged so the single commit captures the
	// sidecar removal — otherwise the deletion is left unstaged and the
	// working tree is dirty. Probe tracked-status only in this case: it
	// keeps the common no-sidecar write path (main file present, subtree
	// never existed) free of an extra git call, and skips the `pathspec
	// did not match any files` failure a never-tracked subtree pathspec
	// would trigger.
	if _, err := os.Stat(filepath.Join(c.root, relPath)); os.IsNotExist(err) {
		if c.subtreeTracked(ctx, subtree) {
			return append(paths, subtree)
		}
	}
	return paths
}

// subtreeTracked reports whether any file under the given subtree path
// is tracked in the git index (i.e. was previously committed). Used by
// stagePathsFor to decide whether a now-deleted entity subtree must be
// added to the delete commit's pathspec. Best-effort: any error (git
// missing, nothing tracked) reports false, falling back to staging just
// the main file.
func (c *GitCommitter) subtreeTracked(ctx context.Context, subtree string) bool {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.root}, "ls-files", "--", subtree)...)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false
	}
	return stdout.Len() > 0
}

// entitySubtreeFor returns the entity-subtree path for an entity
// main file relPath, or "" when relPath isn't of the
// `<kind>/<slug>.md` shape. The pair `<kind>/<slug>.md` +
// `<kind>/<slug>/` is the vault layout convention per
// `internal/vault/entity.go` (KindDir + entity sidecar dirs).
func entitySubtreeFor(relPath string) string {
	const mdExt = ".md"
	if !strings.HasSuffix(relPath, mdExt) {
		return ""
	}
	base := strings.TrimSuffix(relPath, mdExt)
	// Exactly one path separator → `<kind>/<slug>.md` shape. Zero
	// separators (e.g. `README.md`) or two+ separators (e.g.
	// `_archive/<kind>/<slug>.md`) are not in scope of #314's
	// entity-attachments-sidecar rule. The _archive path mirrors
	// the same layout but is touched via archive_entity which
	// preserves the subtree via a directory-level mv, not a
	// flush-through write. Task-shape paths (`tasks/<id>.md`)
	// share the one-separator shape with entity main files, so
	// they pass this filter — but the caller (stagePathsFor) then
	// runs an `os.Stat` guard on `<base>/`, and a task file has
	// no sibling subtree on disk, so staging falls back to the
	// single-path behavior. Both guards are load-bearing.
	if strings.Count(base, "/") != 1 {
		return ""
	}
	return base
}

func (c *GitCommitter) formatAuthor(author string) string {
	// Author of the form "agent:bob" maps to "yaad <yaad@yaad-index>".
	// Bare strings ("yaad") use the same fallback. The format must
	// satisfy git's "Name <email>" parser.
	name := author
	if idx := strings.IndexByte(author, ':'); idx >= 0 && idx < len(author)-1 {
		name = author[idx+1:]
	}
	return fmt.Sprintf("%s <%s@yaad-index>", name, name)
}

// gitError captures `git`'s exit + output for clearer logging.
type gitError struct {
	args []string
	err error
	stdout string
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v (stderr=%q)", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.stderr))
}

func (e *gitError) Unwrap() error { return e.err }

func isNothingToCommit(err error) bool {
	var ge *gitError
	if !errors.As(err, &ge) {
		return false
	}
	return strings.Contains(ge.stdout, "nothing to commit") ||
		strings.Contains(ge.stderr, "nothing to commit") ||
		strings.Contains(ge.stdout, "nothing added to commit")
}

func isLockContended(err error) bool {
	var ge *gitError
	if !errors.As(err, &ge) {
		return false
	}
	return strings.Contains(ge.stderr, "index.lock") ||
		strings.Contains(ge.stderr, "Another git process")
}

// summarizeBatch produces a single commit message that names the
// per-operation counts in the batch ("bulk: ingest 12, fill 3,
// note 2"). Falls back to the single message when the batch is
// homogeneous of size 1.
func summarizeBatch(pending []pendingWrite) string {
	if len(pending) == 1 {
		return pending[0].message
	}
	counts := make(map[string]int)
	for _, p := range pending {
		op := operationOf(p.message)
		counts[op]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return "bulk: " + strings.Join(parts, ", ")
}

func operationOf(message string) string {
	idx := strings.IndexByte(message, ':')
	if idx <= 0 {
		return "other"
	}
	return strings.TrimSpace(message[:idx])
}
