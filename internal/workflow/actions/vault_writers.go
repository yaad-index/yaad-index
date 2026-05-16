// Vault-backed CommentWriter + GapWriter implementations
// (Phase 4.B.2). Replace the Stub*Writer production defaults
// once the daemon wires a vault + writelock manager. Tests
// + dev binaries without a vault continue to use the stubs.
//
// **Pattern.** Each impl acquires a per-entity write-lock
// before touching the vault file (concurrency contract per
// ADR-0024 §"Concurrent writes" — every mutation surface other
// than comments/edges takes the lock; comments are append-only
// and skip the lock in the operator-handler path, but the
// workflow-author path goes through the lock anyway because
// add_gap *does* mutate Gaps + GapState which are NOT
// append-only at the storage layer). The lock holder string
// names the originating workflow so 409 conflict messages
// surface "workflow:<name>" as the active holder.
//
// **Workflow attribution.** Each write stamps the
// Comment.Author / commit author as `workflow:<name>` per the
// ADR-0024 Source vocabulary (same vocabulary fill.completed
// events use). Operators reading the vault file or `git log`
// see which workflow added what.
//
// **Auto-materialize.** Out of scope. Workflows do NOT create
// canonical-label vault files from nothing; that surface is
// operator-fill's prerogative per ADR-0021's amendment. A
// workflow targeting a missing entity gets an
// ErrEntityNotFound (the runner translates that into the
// per-action ActionResult.Err — Phase 5's err-task pattern
// will surface it on a task).

package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// EntityStore is the narrow subset of *store.Store the vault-
// backed writers need. Production wires *store.Store directly;
// tests substitute fakes that record calls.
type EntityStore interface {
	GetEntity(ctx context.Context, id string) (*store.Entity, error)
	UpsertEntity(ctx context.Context, e *store.Entity) error
}

// VaultEntityReader is the narrow subset of *vault.Reader.
type VaultEntityReader interface {
	ReadByID(kind, id string) (*vault.Entity, error)
}

// VaultEntityWriter is the narrow subset of *vault.Writer.
type VaultEntityWriter interface {
	WriteWithCommit(ctx context.Context, e *vault.Entity, message, author string) error
}

// VaultWriterBackend bundles the dependencies the vault-backed
// CommentWriter + GapWriter share. The two writers wire
// against identical state — splitting the backend in two
// would just duplicate every field.
type VaultWriterBackend struct {
	Store       EntityStore
	VaultReader VaultEntityReader
	VaultWriter VaultEntityWriter
	WriteLocks  *writelocks.Manager
	Logger      *slog.Logger
	// Clock supplies the timestamp stamped on each appended
	// Comment. nil → time.Now. Test-only knob; production
	// leaves it unset.
	Clock func() time.Time
}

func (b *VaultWriterBackend) clock() time.Time {
	if b.Clock != nil {
		return b.Clock()
	}
	return time.Now()
}

func (b *VaultWriterBackend) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// ErrEntityNotFound is the sentinel the vault writers return
// when the target entity has no store row OR no vault file.
// The runner wraps it through ActionResult.Err; Phase 5's
// err-task pattern surfaces this on the resulting task.
var ErrEntityNotFound = errors.New("actions: entity not found")

// VaultCommentWriter is the production-default CommentWriter
// for Phase 4.B.2+. See package docstring for the pattern.
type VaultCommentWriter struct {
	backend *VaultWriterBackend
}

// NewVaultCommentWriter constructs a CommentWriter backed by
// the given backend. Backend must be non-nil with Store +
// VaultReader + VaultWriter + WriteLocks set; missing fields
// panic at first call (caller bug — the daemon's main wiring
// is the only construction site and ships fully populated).
func NewVaultCommentWriter(b *VaultWriterBackend) *VaultCommentWriter {
	return &VaultCommentWriter{backend: b}
}

// AppendComment implements CommentWriter. Acquires the per-
// entity write-lock, reads the vault file, appends a
// vault.Comment with Author=`workflow:<workflow>`, writes
// back with a commit author of `workflow:<workflow>`, mirrors
// the comments-text update into the store via UpsertEntity.
func (w *VaultCommentWriter) AppendComment(ctx context.Context, workflow, entityID, body string) error {
	if w.backend == nil {
		return fmt.Errorf("VaultCommentWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "add_comment")
	release, err := w.backend.WriteLocks.Acquire(entityID, holder)
	if err != nil {
		return fmt.Errorf("acquire write-lock on %s: %w", entityID, err)
	}
	defer release()

	got, err := w.backend.Store.GetEntity(ctx, entityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrEntityNotFound, entityID)
		}
		return fmt.Errorf("store.GetEntity %s: %w", entityID, err)
	}

	ve, err := w.backend.VaultReader.ReadByID(got.Kind, entityID)
	if err != nil {
		if vault.IsNotExist(err) {
			return fmt.Errorf("%w: %s (vault file missing)", ErrEntityNotFound, entityID)
		}
		return fmt.Errorf("vault.Reader.ReadByID %s: %w", entityID, err)
	}

	// Truncate to second precision so the YAML frontmatter
	// + body `## Comments` section header round-trip cleanly
	// (mirrors handleComments — without this the body loses
	// nanos and the next read-modify-write cycle would
	// see a "new" comment on dedup).
	now := w.backend.clock().Truncate(time.Second)
	ve.Comments = append(ve.Comments, vault.Comment{
		Date:   now,
		Text:   body,
		Author: workflowAuthor(workflow),
	})

	commitMsg := fmt.Sprintf("workflow comment on %s by %s", ve.ID, workflowAuthor(workflow))
	if err := w.backend.VaultWriter.WriteWithCommit(ctx, ve, commitMsg, workflowAuthor(workflow)); err != nil {
		return fmt.Errorf("vault.Writer.WriteWithCommit %s: %w", entityID, err)
	}

	if err := w.backend.Store.UpsertEntity(ctx, &store.Entity{
		ID:        ve.ID,
		Kind:      ve.Kind,
		Data:      vaultEntityDataForDB(ve),
		CreatedAt: got.CreatedAt,
		GapState:  vaultGapStateForDB(ve.GapState),
	}); err != nil {
		w.backend.logger().Warn(
			"workflow add_comment: store.UpsertEntity failed (vault already written)",
			"entity_id", entityID, "err", err)
		// Mirror handleComments' shape: the vault write is the
		// source of truth (ADR-0008). A failed DB mirror is a
		// degraded-search state, not a write-side failure.
	}
	return nil
}

// VaultGapWriter is the production-default GapWriter for
// Phase 4.B.2+. See package docstring for the pattern.
type VaultGapWriter struct {
	backend *VaultWriterBackend
}

// NewVaultGapWriter mirrors NewVaultCommentWriter.
func NewVaultGapWriter(b *VaultWriterBackend) *VaultGapWriter {
	return &VaultGapWriter{backend: b}
}

// AddGap implements GapWriter. Acquires the per-entity
// write-lock, reads the vault file, appends the gap to
// vault.Entity.Gaps (idempotent — adding a gap that's already
// present is a no-op success), initializes a zero-value
// GapState entry (pending — not filled, not deferred), writes
// back, mirrors to the store.
func (w *VaultGapWriter) AddGap(ctx context.Context, workflow, entityID, gap string) error {
	if w.backend == nil {
		return fmt.Errorf("VaultGapWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "add_gap")
	release, err := w.backend.WriteLocks.Acquire(entityID, holder)
	if err != nil {
		return fmt.Errorf("acquire write-lock on %s: %w", entityID, err)
	}
	defer release()

	got, err := w.backend.Store.GetEntity(ctx, entityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrEntityNotFound, entityID)
		}
		return fmt.Errorf("store.GetEntity %s: %w", entityID, err)
	}

	ve, err := w.backend.VaultReader.ReadByID(got.Kind, entityID)
	if err != nil {
		if vault.IsNotExist(err) {
			return fmt.Errorf("%w: %s (vault file missing)", ErrEntityNotFound, entityID)
		}
		return fmt.Errorf("vault.Reader.ReadByID %s: %w", entityID, err)
	}

	// Idempotent: gap already present → no-op success.
	for _, g := range ve.Gaps {
		if g == gap {
			return nil
		}
	}
	ve.Gaps = append(ve.Gaps, gap)
	if ve.GapState == nil {
		ve.GapState = make(map[string]vault.GapStateEntry)
	}
	if _, ok := ve.GapState[gap]; !ok {
		// Zero-value entry: not filled, not deferred. The
		// gap shows up in needs_fill until an operator /
		// agent fills it.
		ve.GapState[gap] = vault.GapStateEntry{}
	}

	commitMsg := fmt.Sprintf("workflow add_gap %s on %s by %s", gap, ve.ID, workflowAuthor(workflow))
	if err := w.backend.VaultWriter.WriteWithCommit(ctx, ve, commitMsg, workflowAuthor(workflow)); err != nil {
		return fmt.Errorf("vault.Writer.WriteWithCommit %s: %w", entityID, err)
	}

	if err := w.backend.Store.UpsertEntity(ctx, &store.Entity{
		ID:        ve.ID,
		Kind:      ve.Kind,
		Data:      vaultEntityDataForDB(ve),
		CreatedAt: got.CreatedAt,
		GapState:  vaultGapStateForDB(ve.GapState),
	}); err != nil {
		w.backend.logger().Warn(
			"workflow add_gap: store.UpsertEntity failed (vault already written)",
			"entity_id", entityID, "gap", gap, "err", err)
	}
	return nil
}

// workflowAuthor returns the canonical `workflow:<name>`
// attribution string used for Comment.Author + commit-author
// stamps per ADR-0024 Source vocabulary.
func workflowAuthor(workflow string) string {
	w := strings.TrimSpace(workflow)
	if w == "" {
		return "workflow:unknown"
	}
	return "workflow:" + w
}

// workflowHolder is the writelocks holder identifier — a
// human-readable string surfaced in 409 ConflictError so the
// next caller sees which workflow holds the lock.
func workflowHolder(workflow, action string) string {
	return fmt.Sprintf("%s [%s]", workflowAuthor(workflow), action)
}

// vaultEntityDataForDB extracts the searchable map the DB
// upsert mirrors. Mirrors the api-side helper of the same
// name; duplicated here to keep the actions package
// dependency-free against internal/api.
func vaultEntityDataForDB(e *vault.Entity) map[string]any {
	out := make(map[string]any, len(e.Data)+3)
	for k, v := range e.Data {
		out[k] = v
	}
	if e.Summary != "" {
		out["summary"] = e.Summary
	}
	if len(e.Tags) > 0 {
		out["tags"] = e.Tags
	}
	if len(e.Comments) > 0 {
		parts := make([]string, 0, len(e.Comments))
		for _, c := range e.Comments {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			out["comments_text"] = strings.Join(parts, "\n")
		}
	}
	return out
}

// vaultGapStateForDB translates vault.GapStateEntry → the
// store-side GapStateEntry shape. Each field maps one-to-one
// per ADR-0019 §Storage; the helper exists so the action
// package doesn't reach into api's per-field translator.
func vaultGapStateForDB(in map[string]vault.GapStateEntry) map[string]store.GapStateEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]store.GapStateEntry, len(in))
	for k, v := range in {
		out[k] = store.GapStateEntry{
			Source:     v.Source,
			FilledAt:   v.FilledAt,
			Deferred:   v.Deferred,
			DeferredAt: v.DeferredAt,
		}
	}
	return out
}
