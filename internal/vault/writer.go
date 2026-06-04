package vault

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidEntityID is returned by Writer.Write when the entity's ID
// doesn't match the `<prefix>:<slug>` shape required for the
// folder-per-kind layout. Slug derivation is plugin-owned per ADR-0008;
// the writer just splits on the first colon.
var ErrInvalidEntityID = errors.New("invalid entity id")

// Writer persists Entity values to a vault root directory using the
// folder-per-kind layout: `<root>/<kind>/<slug>.md`. Writes are atomic
// (temp file + rename in the same directory) so a crash mid-write
// cannot leave a half-written destination file. The temp file name
// uses a leading `.` so Obsidian-style file explorers hide it if a
// crash leaves one stranded.
//
// Writer is safe for concurrent use by multiple goroutines writing
// different entities; concurrent writes to the same entity (same
// destination path) race on os.Rename's last-writer-wins semantics —
// the file is still complete and well-formed, but which writer's
// content lands is undefined. Callers serialize per-entity if order
// matters.
type Writer struct {
	root string
	canonicalKinds []string
	committer Committer
	logger *slog.Logger
}

// WriterOption configures a Writer at construction.
type WriterOption func(*Writer)

// WithCanonicalKinds names the operator-enabled canonical kinds (per
// ADR-0008's CanonicalGuard). The Writer threads them into Marshal
// so ADR-0011's alias synthesis picks `data.name` for canonical-
// shape entities and `data.title` for source-shape entities. Pass
// `guard.EnabledKinds()` from the caller. Empty/unset = source-
// shape-only behavior (acceptable for tests + source-only deploys).
func WithCanonicalKinds(kinds []string) WriterOption {
	return func(w *Writer) { w.canonicalKinds = kinds }
}

// WithCommitter wires an auto-commit Committer (per yaad-index issue
//) so successful Writer.WriteWithCommit calls produce git
// commits summarizing the operation. nil committer = no auto-commit
// (the implicit default; tests + non-git vaults pass through). The
// plain Writer.Write call NEVER commits regardless of this option —
// only WriteWithCommit invokes the committer.
func WithCommitter(c Committer) WriterOption {
	return func(w *Writer) { w.committer = c }
}

// WithLogger wires the operator logger so committer.OnWrite errors
// land at WARN level (per yaad-index). Without this, OnWrite
// failures (git missing, .git permissions, push auth) are invisible
// to the operator since the audit-commit error must NOT propagate
// to the API caller per ADR-0008. Pass the same logger threaded
// through the rest of the server. Unset = silent fallback to
// slog.Default(); tests typically leave it unset.
func WithLogger(l *slog.Logger) WriterOption {
	return func(w *Writer) { w.logger = l }
}

// Root returns the absolute vault root the writer was constructed
// with. Used by callers (the attachments dispatcher per ADR-0014)
// that need the same on-disk anchor without re-threading the path
// from config separately. The string is treated as immutable.
func (w *Writer) Root() string { return w.root }

// NewWriter constructs a Writer rooted at vaultRoot. The root must be
// an absolute path — vault.path config per ADR-0008 is required absolute
// (no `~` expansion or relative resolution at the writer layer). The
// root directory must already exist; callers (server startup) are
// responsible for ensuring it.
func NewWriter(vaultRoot string, opts ...WriterOption) (*Writer, error) {
	if !filepath.IsAbs(vaultRoot) {
		return nil, fmt.Errorf("vault root must be absolute, got %q", vaultRoot)
	}
	info, err := os.Stat(vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("stat vault root %q: %w", vaultRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("vault root %q is not a directory", vaultRoot)
	}
	w := &Writer{root: vaultRoot}
	for _, o := range opts {
		o(w)
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	return w, nil
}

// Write serializes e and persists it atomically to the vault. The
// destination path is `<root>/<kind>/<slug>.md`; the kind directory is
// created if missing. On any error before rename, the temp file is
// best-effort removed; the destination is never partially written.
//
// Write does NOT auto-commit. Callers wanting an audit-log commit
// (per yaad-index the source issue) call WriteWithCommit instead.
func (w *Writer) Write(e *Entity) error {
	_, err := w.writeAtomic(e)
	return err
}

// WriteWithCommit performs the same atomic write as Write, then —
// when a Committer was wired via WithCommitter — hands the write to
// the committer to be recorded as a git commit. The commit message
// summarizes the operation (e.g. "ingest: wikipedia:susanna-clarke",
// "fill: ... [summary, tags]") and author identifies the calling
// agent (or empty for the committer's identity).
//
// Commit failures log via the committer's logger but DO NOT fail the
// surrounding write: per ADR-0008 the vault file landed; the audit
// commit is best-effort. The returned error reflects only the write
// itself.
func (w *Writer) WriteWithCommit(ctx context.Context, e *Entity, message, author string) error {
	relPath, err := w.writeAtomic(e)
	if err != nil {
		return err
	}
	if w.committer == nil || message == "" {
		return nil
	}
	// Best-effort: a commit-side error is not a write-side error
	// (per ADR-0008 the vault file landed; the audit-log commit is
	// best-effort). But silently dropping the error left auto-commit
	// failures invisible — see yaad-index — so log at WARN with
	// the relPath + err for the operator to investigate (git missing,
	// .git permissions, push auth fail, etc.). The error does NOT
	// propagate to the caller.
	if err := w.committer.OnWrite(ctx, relPath, message, author); err != nil {
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", relPath,
			"err", err)
	}
	return nil
}

// WriteCanonicalLabelWithCommit is the auto-materialize-on-first-
// fill path per ADR-0021's amendment (yaad-index phase D).
// Writes the entity to `<root>/ct/<kind>/<slug>.md` rather than
// `<root>/<kind>/<slug>.md` — the `ct/` prefix marks the file as
// a canonical-label metadata file, distinct from source-shape
// vault files.
//
// Used by operator-fill when an operator fills a canonical-label
// entity that had only a thin DB row (no vault file). Reader.ReadByID's
// chained-fallback probe (active → canonical-label → archive)
// finds the file at this path on subsequent reads.
//
// Same atomic + best-effort-commit semantics as WriteWithCommit:
// vault write succeeds before any commit work; commit failures
// log at WARN but don't fail the call.
func (w *Writer) WriteCanonicalLabelWithCommit(ctx context.Context, e *Entity, message, author string) error {
	if e == nil {
		return fmt.Errorf("%w: entity", ErrMissingRequiredField)
	}
	if e.Kind == "" {
		return fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(e.ID)
	if err != nil {
		return err
	}
	dir := filepath.Join(w.root, "ct", e.Kind)
	relPath, err := w.writeAtomicAt(e, dir, slug)
	if err != nil {
		return err
	}
	if w.committer == nil || message == "" {
		return nil
	}
	if err := w.committer.OnWrite(ctx, relPath, message, author); err != nil {
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", relPath,
			"err", err)
	}
	return nil
}

// WriteRawWithCommit writes verbatim bytes to `<root>/<relPath>`
// atomically (temp + rename in the destination dir), then best-effort
// commits — the same atomic + commit semantics as WriteWithCommit, but
// bypassing the Entity Marshal round-trip. For callers (#343: the notes
// endpoint's task path) that own a body whose section structure the
// Entity model would not preserve. relPath must be vault-root-relative;
// the parent dir is created if missing. An empty message skips the
// commit (consistent with the other Write* methods).
func (w *Writer) WriteRawWithCommit(ctx context.Context, relPath string, body []byte, message, author string) error {
	if strings.TrimSpace(relPath) == "" {
		return fmt.Errorf("%w: relPath", ErrMissingRequiredField)
	}
	dst := filepath.Join(w.root, relPath)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".raw.md.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return fmt.Errorf("rename to destination: %w", err)
	}
	if w.committer == nil || message == "" {
		return nil
	}
	if err := w.committer.OnWrite(ctx, relPath, message, author); err != nil {
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", relPath,
			"err", err)
	}
	return nil
}

func (w *Writer) writeAtomic(e *Entity) (string, error) {
	dir, slug, err := w.pathFor(e)
	if err != nil {
		return "", err
	}
	// #415 location preservation — user-content only: when the flat
	// `user-content/<slug>.md` doesn't exist but a same-slug file lives
	// one level deep in a subfolder, write back to that subfolder so an
	// edit doesn't orphan the file to the flat path. New entities and
	// normal flat files fall through to the flat dir (the flat file
	// exists, or no subfolder match). Scoped to user-content so other
	// kinds never glob nested markdown.
	if e.Kind == kindUserContent {
		if subDir, ok := w.existingSubfolderDir(dir, slug); ok {
			dir = subDir
		}
	}
	return w.writeAtomicAt(e, dir, slug)
}

// existingSubfolderDir returns the directory of an existing
// `<flatDir>/<subfolder>/<slug>.md` when the flat `<flatDir>/<slug>.md`
// is absent and exactly one single-level-subfolder match exists (#415).
// Returns ("", false) when the flat file exists or there is no unique
// subfolder match — the caller then writes to the flat dir.
func (w *Writer) existingSubfolderDir(flatDir, slug string) (string, bool) {
	if _, err := os.Stat(filepath.Join(flatDir, slug+".md")); err == nil {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(flatDir, "*", slug+".md"))
	if len(matches) == 1 {
		return filepath.Dir(matches[0]), true
	}
	return "", false
}

// WriteWithCommitInSubfolder writes the entity to
// `<root>/<kind>/<subfolder>/<slug>.md` instead of the flat
// `<root>/<kind>/<slug>.md`, for #415 operator-organized user-content.
// The entity id stays flat (`<kind>:<slug>`); only the on-disk path
// carries the subfolder. Same atomic + best-effort-commit semantics as
// WriteWithCommit. An empty subfolder writes to the flat path.
func (w *Writer) WriteWithCommitInSubfolder(ctx context.Context, e *Entity, subfolder, message, author string) error {
	if e == nil {
		return fmt.Errorf("%w: entity", ErrMissingRequiredField)
	}
	if e.Kind == "" {
		return fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(e.ID)
	if err != nil {
		return err
	}
	dir := filepath.Join(w.root, KindDir(e.Kind), subfolder)
	relPath, err := w.writeAtomicAt(e, dir, slug)
	if err != nil {
		return err
	}
	if w.committer == nil || message == "" {
		return nil
	}
	if err := w.committer.OnWrite(ctx, relPath, message, author); err != nil {
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", relPath,
			"err", err)
	}
	return nil
}

// writeAtomicAt is the shared atomic-write path used by both
// pathFor-driven writes (`<root>/<kind>/<slug>.md`) and the
// canonical-label override (`<root>/ct/<kind>/<slug>.md`).
// Caller passes the destination directory + slug; this helper
// runs MkdirAll, marshals + writes the temp file, fsyncs, and
// renames into place. Returns the vault-relative path of the
// written file.
func (w *Writer) writeAtomicAt(e *Entity, dir, slug string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create kind dir: %w", err)
	}

	body, err := Marshal(e, w.canonicalKinds)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, "."+slug+".md.tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close temp file: %w", err)
	}

	dst := filepath.Join(dir, slug+".md")
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return "", fmt.Errorf("rename to destination: %w", err)
	}
	relPath, err := filepath.Rel(w.root, dst)
	if err != nil {
		// Should never happen — dst is constructed under w.root. Fall
		// back to absolute on the off chance.
		relPath = dst
	}
	return relPath, nil
}

func (w *Writer) pathFor(e *Entity) (dir, slug string, err error) {
	slug, err = slugFromID(e.ID)
	if err != nil {
		return "", "", err
	}
	if e.Kind == "" {
		return "", "", fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	dir = filepath.Join(w.root, KindDir(e.Kind))
	return dir, slug, nil
}

// DeleteWithCommit removes the entity's vault file and hands a
// deletion notification to the wired Committer (if any). Mirrors
// WriteWithCommit's best-effort commit semantics: a commit-side
// error logs but does NOT fail the surrounding delete — the file
// is gone from disk; the audit-log commit catches up next time
// or stays missing per ADR-0008.
//
// Returns an error wrapping os.ErrNotExist when the file isn't
// present. nil committer = no auto-commit (same default as
// WriteWithCommit).
//
// Per ADR-0018 step 4 the active path delete via this method is
// no longer the operator-facing destroy path — that lives in
// DestroyArchivedWithCommit. DeleteWithCommit stays for any
// internal caller that legitimately needs to remove an active
// vault file (test fixtures, admin-cleanup paths). The handler
// layer's DELETE /v1/entities/{id} now gates on archived-state
// and routes to DestroyArchivedWithCommit instead.
// MoveToSubfolder relocates an existing entity's vault file to the
// given subfolder (empty = flat) via an atomic os.Rename, then
// best-effort commits — same move+commit shape as the archive moves.
// The id / kind / file content are unchanged; only the on-disk path
// moves (#425 Cut 1 — the subfolder is path-only per #415, so the
// entity row, provenance, and edges, all keyed by the flat id, are
// untouched by construction). The current file is located flat first,
// then via a single-level subfolder glob. Returns moved=false when the
// file is already at the target path (the idempotent same-subfolder
// no-op). The attachment sidecar subtree, if present, moves alongside.
func (w *Writer) MoveToSubfolder(ctx context.Context, kind, id, newSubfolder, message, author string) (moved bool, err error) {
	if kind == "" {
		return false, fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return false, err
	}
	flatDir := filepath.Join(w.root, KindDir(kind))

	// Resolve the current on-disk file: flat path, else a unique
	// single-level subfolder match (mirrors the #415 read fallback).
	srcFull := filepath.Join(flatDir, slug+".md")
	if _, statErr := os.Stat(srcFull); statErr != nil {
		if !os.IsNotExist(statErr) {
			return false, fmt.Errorf("stat source: %w", statErr)
		}
		matches, _ := filepath.Glob(filepath.Join(flatDir, "*", slug+".md"))
		if len(matches) != 1 {
			return false, fmt.Errorf("vault file for %s: %w", id, os.ErrNotExist)
		}
		srcFull = matches[0]
	}

	dstDir := flatDir
	if newSubfolder != "" {
		dstDir = filepath.Join(flatDir, newSubfolder)
	}
	dstFull := filepath.Join(dstDir, slug+".md")
	if srcFull == dstFull {
		// Already at the target path — idempotent no-op, no commit.
		return false, nil
	}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return false, fmt.Errorf("create destination dir: %w", err)
	}
	if err := os.Rename(srcFull, dstFull); err != nil {
		return false, fmt.Errorf("move %s -> %s: %w", srcFull, dstFull, err)
	}

	// Attachment sidecar subtree (`<dir>/<slug>/`) MUST ride along with
	// the .md: manifest attachment paths resolve relative to the .md's
	// location, so a moved .md with a stranded sidecar breaks every
	// attachment read. Unlike the archive move (where the entity leaves
	// active queries anyway), an in-place move keeps the entity live, so
	// the sidecar move is part of the move contract — on failure, roll
	// the .md back to its origin and fail the whole operation (#425
	// review). Sidecar absence is a no-op.
	srcSub := strings.TrimSuffix(srcFull, ".md")
	dstSub := strings.TrimSuffix(dstFull, ".md")
	if _, statErr := os.Stat(srcSub); statErr == nil {
		if renErr := os.Rename(srcSub, dstSub); renErr != nil {
			if rb := os.Rename(dstFull, srcFull); rb != nil {
				w.logger.Error("sidecar move failed AND .md rollback failed — vault left inconsistent; reindex required",
					"md_at", dstFull, "md_should_be", srcFull, "sidecar_err", renErr, "rollback_err", rb)
			}
			return false, fmt.Errorf("move attachment sidecar %s -> %s: %w", srcSub, dstSub, renErr)
		}
	} else if !os.IsNotExist(statErr) {
		if rb := os.Rename(dstFull, srcFull); rb != nil {
			w.logger.Error("sidecar stat failed AND .md rollback failed — vault left inconsistent; reindex required",
				"md_at", dstFull, "md_should_be", srcFull, "stat_err", statErr, "rollback_err", rb)
		}
		return false, fmt.Errorf("stat attachment sidecar %s: %w", srcSub, statErr)
	}

	if w.committer == nil || message == "" {
		return true, nil
	}
	dstRel, relErr := filepath.Rel(w.root, dstFull)
	if relErr != nil {
		dstRel = dstFull
	}
	// Commit the destination path; the committer's `git add -A` covers
	// the source's removal automatically (same as moveBetweenArchive).
	if err := w.committer.OnWrite(ctx, dstRel, message, author); err != nil {
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", dstRel, "op", "move", "err", err)
	}
	return true, nil
}

func (w *Writer) DeleteWithCommit(ctx context.Context, kind, id, message, author string) error {
	if kind == "" {
		return fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return err
	}
	relPath := filepath.Join(KindDir(kind), slug+".md")
	return w.removeAtRelPath(ctx, relPath, "delete", message, author)
}

// DestroyArchivedWithCommit removes the entity's vault file from
// the archive subtree per ADR-0018 step 4. Same shape as
// DeleteWithCommit — atomic os.Remove + best-effort commit — but
// the file path is `<root>/_archive/<kind>/<slug>.md` rather than
// the active path.
//
// Used by the DELETE /v1/entities/{id} handler AFTER the entity
// has been verified-archived via the store flag. Hard delete: the
// file (and the DB row, via the cascade in the handler) is gone
// after this returns.
//
// Auto-commit prefix per ADR-0018 + handler convention: callers
// pass `destroy: <id>` as message. Same WARN-on-error contract.
func (w *Writer) DestroyArchivedWithCommit(ctx context.Context, kind, id, message, author string) error {
	if kind == "" {
		return fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return err
	}
	relPath := filepath.Join(ArchiveDir, KindDir(kind), slug+".md")
	if err := w.removeAtRelPath(ctx, relPath, "destroy", message, author); err != nil {
		return err
	}

	// ADR-0018 step 6 ownership cascade: the entity's attachment
	// subdir lives at `_archive/<kind>/<slug>/` mirroring the .md
	// path. Remove it if present. Non-existence is a no-op (entities
	// without attachments have no subdir to clean up). Failures here
	// log at WARN — the .md is already gone and the DB cascade has
	// run; reindex will reconcile residual files on next walk.
	subdirRel := filepath.Join(ArchiveDir, KindDir(kind), slug)
	if err := os.RemoveAll(filepath.Join(w.root, subdirRel)); err != nil {
		w.logger.Warn("destroy attachment subdir cleanup failed (manifest entries are already orphaned)",
			"rel_path", subdirRel, "err", err)
	}
	return nil
}

// removeAtRelPath is the shared body for DeleteWithCommit +
// DestroyArchivedWithCommit. Removes <root>/<relPath>; on success
// + non-empty commit message + non-nil committer, hands the
// notification to the auto-commit chain. Same best-effort + WARN-
// on-error contract as the other Writer methods.
func (w *Writer) removeAtRelPath(ctx context.Context, relPath, op, message, author string) error {
	fullPath := filepath.Join(w.root, relPath)
	if err := os.Remove(fullPath); err != nil {
		return fmt.Errorf("remove vault file %s: %w", relPath, err)
	}
	if w.committer == nil || message == "" {
		return nil
	}
	if err := w.committer.OnWrite(ctx, relPath, message, author); err != nil {
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", relPath,
			"op", op,
			"err", err)
	}
	return nil
}

// ArchiveWithCommit moves the entity's vault file from
// `<root>/<kind>/<slug>.md` to `<root>/_archive/<kind>/<slug>.md`
// per ADR-0018 step 2. The destination kind directory is created
// under `_archive/` if missing. Move is os.Rename (atomic on the
// same filesystem); the source path is gone after success.
//
// Idempotent on already-archived rows: when the active source path
// is missing AND the archive destination already exists, returns
// nil (no-op). When the active source is missing AND the archive
// destination is also missing, returns os.ErrNotExist via the
// wrapped error.
//
// Auto-commit prefix per ADR-0018 + the existing convention
// (`ingest:`, `fill:`, `delete:`): callers pass `archive: <id>` as
// `message`. Commit failures log at WARN but do NOT fail the
// surrounding move (per ADR-0008's vault-as-source-of-truth +
// best-effort-audit-log contract; same shape as WriteWithCommit
// and DeleteWithCommit).
func (w *Writer) ArchiveWithCommit(ctx context.Context, kind, id, message, author string) error {
	return w.moveBetweenArchive(ctx, kind, id, message, author, true)
}

// RestoreWithCommit is the inverse of ArchiveWithCommit: moves the
// vault file from `<root>/_archive/<kind>/<slug>.md` back to
// `<root>/<kind>/<slug>.md`. Same atomicity, same idempotence-on-
// already-restored, same best-effort-commit semantics.
func (w *Writer) RestoreWithCommit(ctx context.Context, kind, id, message, author string) error {
	return w.moveBetweenArchive(ctx, kind, id, message, author, false)
}

// moveBetweenArchive handles both directions of the archive move.
// `archiving=true` goes active→archive; `archiving=false` goes
// archive→active. Idempotence rule: if the source is missing AND
// the destination already exists, return nil (the move is already
// in the desired state). If both source and destination are
// missing, return os.ErrNotExist.
func (w *Writer) moveBetweenArchive(ctx context.Context, kind, id, message, author string, archiving bool) error {
	if kind == "" {
		return fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return err
	}

	activeRel := filepath.Join(KindDir(kind), slug+".md")
	archiveRel := filepath.Join(ArchiveDir, KindDir(kind), slug+".md")
	// #415: when archiving a user-content file, the active file may live
	// one level deep in a subfolder. Resolve the real source so the move
	// finds it; the archive destination stays flat
	// (`_archive/<kind>/<slug>.md`). A restore therefore returns the file
	// to the flat active path — the subfolder organization is not
	// preserved across an archive round-trip in v1. Scoped to
	// user-content so other kinds never glob nested markdown.
	if archiving && kind == kindUserContent {
		if _, statErr := os.Stat(filepath.Join(w.root, activeRel)); os.IsNotExist(statErr) {
			if matches, _ := filepath.Glob(filepath.Join(w.root, KindDir(kind), "*", slug+".md")); len(matches) == 1 {
				if rel, relErr := filepath.Rel(w.root, matches[0]); relErr == nil {
					activeRel = rel
				}
			}
		}
	}
	var srcRel, dstRel string
	if archiving {
		srcRel, dstRel = activeRel, archiveRel
	} else {
		srcRel, dstRel = archiveRel, activeRel
	}
	srcFull := filepath.Join(w.root, srcRel)
	dstFull := filepath.Join(w.root, dstRel)

	// Idempotence check: source missing → either already-moved
	// (destination exists) or genuinely-not-found.
	if _, err := os.Stat(srcFull); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat source %s: %w", srcRel, err)
		}
		if _, dstErr := os.Stat(dstFull); dstErr == nil {
			// Already in the desired state — no move, no commit.
			return nil
		}
		return fmt.Errorf("vault file %s: %w", srcRel, os.ErrNotExist)
	}

	// Ensure destination kind directory exists. For archive moves
	// that's `<root>/_archive/<kind>/`; for restore that's
	// `<root>/<kind>/`. Both share the same MkdirAll shape.
	if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
		return fmt.Errorf("create destination dir: %w", err)
	}
	if err := os.Rename(srcFull, dstFull); err != nil {
		return fmt.Errorf("move %s → %s: %w", srcRel, dstRel, err)
	}

	// ADR-0018 step 6 ownership cascade: the attachment subdir
	// (`<kind>/<slug>/` active or `_archive/<kind>/<slug>/` archive)
	// rides alongside the .md file. Move it in the same direction.
	// Subdir absence is a no-op (entities without attachments have
	// no subdir). The destination kind dir was just created by
	// MkdirAll above, so a sibling subdir-rename only needs the
	// source to exist.
	var srcSubRel, dstSubRel string
	if archiving {
		srcSubRel = filepath.Join(KindDir(kind), slug)
		dstSubRel = filepath.Join(ArchiveDir, KindDir(kind), slug)
	} else {
		srcSubRel = filepath.Join(ArchiveDir, KindDir(kind), slug)
		dstSubRel = filepath.Join(KindDir(kind), slug)
	}
	srcSubFull := filepath.Join(w.root, srcSubRel)
	dstSubFull := filepath.Join(w.root, dstSubRel)
	if _, err := os.Stat(srcSubFull); err == nil {
		if err := os.Rename(srcSubFull, dstSubFull); err != nil {
			// The .md already moved — log loudly so the operator
			// can investigate, but don't fail the surrounding op.
			// Reindex's incremental walk reconciles attachments via
			// the manifest; partial state is recoverable.
			w.logger.Warn("attachment subdir cascade move failed (manifest entries may be orphaned)",
				"rel_path", srcSubRel, "dst", dstSubRel, "err", err)
		}
	} else if !os.IsNotExist(err) {
		w.logger.Warn("stat attachment subdir during cascade",
			"rel_path", srcSubRel, "err", err)
	}

	if w.committer == nil || message == "" {
		return nil
	}
	// Best-effort commit — same WARN-on-error contract as
	// WriteWithCommit / DeleteWithCommit. Commit the destination
	// path; the committer's `git add -A` covers the source's
	// removal automatically.
	if err := w.committer.OnWrite(ctx, dstRel, message, author); err != nil {
		op := "archive"
		if !archiving {
			op = "restore"
		}
		w.logger.Warn("auto-commit OnWrite failed",
			"rel_path", dstRel,
			"op", op,
			"err", err)
	}
	return nil
}

// SlugFromTitle derives a vault-filename-safe slug from a UGC title
// (yaad-index). Lowercase, ASCII alphanumeric runs separated by
// single hyphens, leading/trailing hyphens trimmed. Markdown
// formatting (`*`, `_`, “ ` “) is dropped before the alphanumeric
// pass so `My **Bold** Note` and `My Bold Note` collapse to the same
// slug. Returns ErrInvalidEntityID when the title slugifies to the
// empty string (e.g. all whitespace, all formatting chars). Slug
// collisions on the writer side surface as 409 Conflict at the API
// layer; this function does not auto-suffix or dedupe.
func SlugFromTitle(title string) (string, error) {
	out := slugifyHeading(title)
	out = strings.TrimLeft(out, "-")
	if out == "" {
		return "", fmt.Errorf("%w: title %q slugifies to empty", ErrInvalidEntityID, title)
	}
	return out, nil
}

// slugFromID extracts the slug portion of an entity ID for use as a
// filename. ID shape per ADR-0008 is `<prefix>:<slug>`; the slug must
// be non-empty and free of path separators (the writer relies on this
// to keep entities inside their kind folder).
func slugFromID(id string) (string, error) {
	idx := strings.IndexByte(id, ':')
	if idx < 0 || idx == len(id)-1 {
		return "", fmt.Errorf("%w: %q must be `<prefix>:<slug>`", ErrInvalidEntityID, id)
	}
	slug := id[idx+1:]
	if strings.ContainsAny(slug, `/\`) {
		return "", fmt.Errorf("%w: slug %q contains path separators", ErrInvalidEntityID, slug)
	}
	return slug, nil
}
