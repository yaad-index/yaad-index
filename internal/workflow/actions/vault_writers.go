// Vault-backed NoteWriter + GapWriter implementations
// (Phase 4.B.2). Replace the Stub*Writer production defaults
// once the daemon wires a vault + writelock manager. Tests
// + dev binaries without a vault continue to use the stubs.
//
// **Pattern.** Each impl acquires a per-entity write-lock
// before touching the vault file (concurrency contract per
// ADR-0024 §"Concurrent writes" — every mutation surface other
// than notes/edges takes the lock; notes are append-only
// and skip the lock in the operator-handler path, but the
// workflow-author path goes through the lock anyway because
// add_gap *does* mutate Gaps + GapState which are NOT
// append-only at the storage layer). The lock holder string
// names the originating workflow so 409 conflict messages
// surface "workflow:<name>" as the active holder.
//
// **Workflow attribution.** Each write stamps the
// Note.Author / commit author as `workflow:<name>` per the
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

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/slug"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// DefaultActionWriteLockTimeout is the bounded wait the
// workflow-action vault writers apply on per-entity write-lock
// acquisition per #152. Workflows firing on edge_created during
// ingest race the ingest path's per-envelope write-lock hold
// (typically tens of ms); the old fail-fast Acquire failed
// every fire deterministically. 5 seconds is generous against
// the typical hold window AND short enough that a
// genuinely-wedged lock surfaces as an err-task inside one
// workflow tick rather than blocking the action runner
// indefinitely. HTTP-side writers keep the fail-fast Acquire
// shape — they have an operator / agent to retry on 409.
const DefaultActionWriteLockTimeout = 5 * time.Second

// EntityStore is the narrow subset of *store.Store the vault-
// backed writers need. Production wires *store.Store directly;
// tests substitute fakes that record calls. ArchiveEntity is
// the #150 archive_entity surface; ErrNotFound from this method
// is the soft-skip signal (entity may have been archived by
// another path) and the runner forwards success rather than
// failing the workflow chain.
type EntityStore interface {
	GetEntity(ctx context.Context, id string) (*store.Entity, error)
	UpsertEntity(ctx context.Context, e *store.Entity) error
	ArchiveEntity(ctx context.Context, id string) error
	// RestoreEntity is the #196 restore_entity surface — the
	// inverse of ArchiveEntity. ErrNotFound here is the same
	// soft-skip signal (entity may have been removed between
	// trigger and fire) and the runner forwards success.
	RestoreEntity(ctx context.Context, id string) error
	// CreateEdge is the #221 (ADR-0025 cut 2) hook: the
	// set_property runner consults the canonical day-refs
	// shape-scan after writing fields, and the scan upserts
	// canonical edges to any `day:YYYY-MM-DD`-shaped values.
	// Mirrors the production *sqliteStore signature so the
	// existing wiring satisfies the interface without a wrapper.
	CreateEdge(ctx context.Context, e *store.Edge) error
}

// VaultEntityReader is the narrow subset of *vault.Reader.
type VaultEntityReader interface {
	ReadByID(kind, id string) (*vault.Entity, error)
}

// VaultEntityWriter is the narrow subset of *vault.Writer.
// ArchiveWithCommit is the #150 archive_entity surface
// (move active → _archive). The production *vault.Writer
// idempotently treats already-at-destination as a no-op.
type VaultEntityWriter interface {
	WriteWithCommit(ctx context.Context, e *vault.Entity, message, author string) error
	ArchiveWithCommit(ctx context.Context, kind, id, message, author string) error
	// RestoreWithCommit is the #196 restore_entity surface
	// (move _archive → active). The production *vault.Writer
	// idempotently treats already-at-destination as a no-op.
	RestoreWithCommit(ctx context.Context, kind, id, message, author string) error
}

// VaultWriterBackend bundles the dependencies the vault-backed
// NoteWriter + GapWriter share. The two writers wire
// against identical state — splitting the backend in two
// would just duplicate every field.
type VaultWriterBackend struct {
	Store       EntityStore
	// EdgeWriter routes the workflow-side fill paths' day-ref
	// edge creates through the centralized edge-write service
	// per #304 Cut C1. Nil-safe — when unset, EmitDayRefs falls
	// back to Store.CreateEdge (preserving legacy behavior for
	// any test fixture that doesn't wire this).
	EdgeWriter  canonical.DayRefEdgeWriter
	VaultReader VaultEntityReader
	VaultWriter VaultEntityWriter
	WriteLocks  *writelocks.Manager
	Logger      *slog.Logger
	// Kinds is the operator's canonical-kinds registry,
	// threaded into maybeWrapEntity by writers that emit
	// vault content referencing entities (add_note body +
	// set_property values per #166). Nil-safe: an unset /
	// empty registry disables wikilink wrapping (every
	// string passes through unchanged).
	Kinds map[string]config.CanonicalKindConfig
	// Clock supplies the timestamp stamped on each appended
	// Note. nil → time.Now. Test-only knob; production
	// leaves it unset.
	Clock func() time.Time
	// LockTimeout caps the per-entity write-lock acquisition
	// wait per #152. Zero → DefaultActionWriteLockTimeout
	// (production). Tests that exercise the conflict-after-
	// timeout path set this to a small value to keep the
	// suite fast.
	LockTimeout time.Duration
}

// lockTimeout returns the configured timeout or the package
// default.
func (b *VaultWriterBackend) lockTimeout() time.Duration {
	if b.LockTimeout > 0 {
		return b.LockTimeout
	}
	return DefaultActionWriteLockTimeout
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

// VaultNoteWriter is the production-default NoteWriter
// for Phase 4.B.2+. See package docstring for the pattern.
type VaultNoteWriter struct {
	backend *VaultWriterBackend
}

// NewVaultNoteWriter constructs a NoteWriter backed by
// the given backend. Backend must be non-nil with Store +
// VaultReader + VaultWriter + WriteLocks set; missing fields
// panic at first call (caller bug — the daemon's main wiring
// is the only construction site and ships fully populated).
func NewVaultNoteWriter(b *VaultWriterBackend) *VaultNoteWriter {
	return &VaultNoteWriter{backend: b}
}

// AppendNote implements NoteWriter. Acquires the per-
// entity write-lock, reads the vault file, appends a
// vault.Note with Author=`workflow:<workflow>`, writes
// back with a commit author of `workflow:<workflow>`, mirrors
// the notes-text update into the store via UpsertEntity.
func (w *VaultNoteWriter) AppendNote(ctx context.Context, workflow, entityID, body, field, kind string) error {
	if w.backend == nil {
		return fmt.Errorf("VaultNoteWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "add_note")
	release, err := w.backend.WriteLocks.AcquireWithTimeout(ctx, entityID, holder, w.backend.lockTimeout())
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

	// Per #166, wrap a body that renders to a `<kind>:<id>`
	// entity-shape into `[[ ]]` so Obsidian surfaces the
	// backlink. Non-matching bodies pass through unchanged.
	// Whole-string wrap matches the task_append + Via-section
	// semantics from #163 — prose with embedded entity refs
	// isn't auto-scanned; workflow authors who want a wikilink
	// in mid-text render the bare id and let the helper wrap.
	body = maybeWrapEntity(body, w.backend.Kinds)

	// Truncate to second precision so the YAML frontmatter
	// + body `## Notes` section header round-trip cleanly
	// (mirrors handleNotes — without this the body loses
	// nanos and the next read-modify-write cycle would
	// see a "new" note on dedup).
	now := w.backend.clock().Truncate(time.Second)
	ve.Notes = append(ve.Notes, vault.Note{
		Date:   now,
		Text:   body,
		Author: workflowAuthor(workflow),
		Field:  field,
		Kind:   kind,
	})

	commitMsg := fmt.Sprintf("workflow note on %s by %s", ve.ID, workflowAuthor(workflow))
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
			"workflow add_note: store.UpsertEntity failed (vault already written)",
			"entity_id", entityID, "err", err)
		// Mirror handleNotes' shape: the vault write is the
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

// NewVaultGapWriter mirrors NewVaultNoteWriter.
func NewVaultGapWriter(b *VaultWriterBackend) *VaultGapWriter {
	return &VaultGapWriter{backend: b}
}

// AddGap implements GapWriter. Acquires the per-entity
// write-lock, reads the vault file, appends the gap to
// vault.Entity.Gaps (idempotent — adding a gap that's already
// present + no new injection is a no-op success), initializes
// a zero-value GapState entry (pending — not filled, not
// deferred) on first add, then merges any non-empty inj fields
// onto the entry. Writes back; mirrors to the store. Empty /
// zero injection fields preserve any pre-existing GapStateEntry
// values (lets a subsequent workflow refresh one aspect — e.g.
// extend an earlier `add_gap` with `data_schema` — without
// clobbering others).
func (w *VaultGapWriter) AddGap(ctx context.Context, workflow, entityID, gap string, inj GapInjection) error {
	if w.backend == nil {
		return fmt.Errorf("VaultGapWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "add_gap")
	release, err := w.backend.WriteLocks.AcquireWithTimeout(ctx, entityID, holder, w.backend.lockTimeout())
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

	gapPresent := false
	for _, g := range ve.Gaps {
		if g == gap {
			gapPresent = true
			break
		}
	}
	// Idempotent: gap already present + no inline metadata at
	// all → no-op. Empty injection means the workflow added the
	// bare gap (no shape, no schema); re-firing is a no-op.
	if gapPresent && injectionEmpty(inj) {
		return nil
	}
	if ve.GapState == nil {
		ve.GapState = make(map[string]vault.GapStateEntry)
	}
	if !gapPresent {
		ve.Gaps = append(ve.Gaps, gap)
	}
	entry := ve.GapState[gap]
	mergeGapInjection(&entry, inj)
	ve.GapState[gap] = entry

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

// VaultPropertyWriter is the production-default PropertyWriter
// for set_property. Acquires the per-entity write-lock, reads
// the vault file, merges the given fields into vault.Entity.Data
// (per-key overwrite), writes back with workflow:<name>
// commit-author + mirrors into the store. Bus emission of
// fill.completed is the runner's responsibility (set_property.go)
// — the writer reports success/failure only.
type VaultPropertyWriter struct {
	backend *VaultWriterBackend
}

// NewVaultPropertyWriter mirrors NewVaultNoteWriter.
func NewVaultPropertyWriter(b *VaultWriterBackend) *VaultPropertyWriter {
	return &VaultPropertyWriter{backend: b}
}

// SetProperties implements PropertyWriter. Read-merge-write
// loop with per-entity lock. The Data map is initialized when
// the entity has no prior data.
func (w *VaultPropertyWriter) SetProperties(ctx context.Context, workflow, entityID string, fields map[string]any) error {
	if w.backend == nil {
		return fmt.Errorf("VaultPropertyWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "set_property")
	release, err := w.backend.WriteLocks.AcquireWithTimeout(ctx, entityID, holder, w.backend.lockTimeout())
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

	if ve.Data == nil {
		ve.Data = make(map[string]any, len(fields))
	}
	for k, v := range fields {
		ve.Data[k] = wrapEntityValue(v, w.backend.Kinds)
	}

	commitMsg := fmt.Sprintf("workflow set_property on %s by %s", ve.ID, workflowAuthor(workflow))
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
			"workflow set_property: store.UpsertEntity failed (vault already written)",
			"entity_id", entityID, "err", err)
	}
	// ADR-0025 cut 2 (#221) day-reference shape-scan: when a
	// workflow set_property writes a `day:YYYY-MM-DD` value into
	// an entity field, ensure the target day entity exists and
	// emit a `references_day` edge. Workflow actions have no live
	// plugin attribution (the writing workflow names itself, not
	// the originating plugin), so DateFields is nil and every
	// day-shaped value gets the baseline edge type. Matches the
	// fill + reindex shape.
	canonical.EmitDayRefs(ctx, w.backend.Store, w.backend.EdgeWriter, ve.ID,
		vaultEntityDataForDB(ve), nil, w.backend.logger())
	return nil
}

// workflowAuthor returns the canonical `workflow:<name>`
// attribution string used for Note.Author + commit-author
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

// VaultArchiveWriter is the production-default ArchiveWriter
// per #150. Combines `vault.Writer.ArchiveWithCommit` (move
// active → _archive) with `store.Store.ArchiveEntity` (DB
// toggle) behind a per-entity write-lock acquired via
// AcquireWithTimeout — same async-side contention shape as
// VaultNoteWriter / VaultGapWriter / VaultPropertyWriter per
// PR-153.
//
// Idempotence: the vault layer's already-at-destination
// short-circuit (returns nil without a commit when src is
// missing AND dst exists) and the store's
// `COALESCE(archived_at, ?)` clause both preserve the original
// archive timestamp, so a re-fire is a clean no-op.
//
// Soft-skip semantics: when the store's GetEntity reports
// ErrNotFound for the resolved id, the writer logs at debug
// and returns nil (success). The entity may have been archived
// by another path between the trigger event and this action;
// failing the workflow chain over that would surface noise
// without giving the operator anything actionable.
type VaultArchiveWriter struct {
	backend *VaultWriterBackend
}

// NewVaultArchiveWriter mirrors the other writer constructors.
// Backend must be non-nil with Store + VaultWriter + WriteLocks
// set; missing fields panic at first call.
func NewVaultArchiveWriter(b *VaultWriterBackend) *VaultArchiveWriter {
	return &VaultArchiveWriter{backend: b}
}

// ArchiveEntity implements ArchiveWriter. Reads the entity to
// learn its kind for the vault move, acquires the per-entity
// write-lock via AcquireWithTimeout, runs the vault move, then
// flips the DB row. Vault-first per ADR-0008 vault-as-source-
// of-truth — a vault failure aborts before the DB toggle; a
// DB failure after the vault move logs loudly (reindex
// reconciles future state from the vault side).
//
// `reason` is folded into the commit message so the audit
// trail names the workflow's stated reason inline with the
// archive event.
func (w *VaultArchiveWriter) ArchiveEntity(ctx context.Context, workflow, entityID, reason string) error {
	if w.backend == nil {
		return fmt.Errorf("VaultArchiveWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "archive_entity")
	release, err := w.backend.WriteLocks.AcquireWithTimeout(ctx, entityID, holder, w.backend.lockTimeout())
	if err != nil {
		return fmt.Errorf("acquire write-lock on %s: %w", entityID, err)
	}
	defer release()

	got, err := w.backend.Store.GetEntity(ctx, entityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Soft skip per #150 — the entity may have been
			// archived (and the row dropped) by another path
			// between trigger and fire.
			w.backend.logger().DebugContext(ctx,
				"workflow archive_entity: entity not found in store; soft-skipping",
				"workflow", workflow, "entity_id", entityID)
			return nil
		}
		return fmt.Errorf("store.GetEntity %s: %w", entityID, err)
	}

	commitMsg := fmt.Sprintf("archive: %s", entityID)
	if reason != "" {
		commitMsg = fmt.Sprintf("archive: %s (%s)", entityID, reason)
	}
	author := workflowAuthor(workflow)

	// Vault move first. Already-archived (source missing AND
	// destination present) returns nil from the vault layer
	// without a commit — the writer treats that as success +
	// proceeds to ensure the DB toggle is also set. Other
	// vault errors surface verbatim.
	if err := w.backend.VaultWriter.ArchiveWithCommit(ctx, got.Kind, entityID, commitMsg, author); err != nil {
		return fmt.Errorf("vault archive %s: %w", entityID, err)
	}

	// DB toggle. Already-archived rows keep their original
	// archived_at via the store's COALESCE clause —
	// idempotent. ErrNotFound here means the row was dropped
	// between GetEntity and ArchiveEntity (rare race); same
	// soft-skip shape as the GetEntity branch above.
	if err := w.backend.Store.ArchiveEntity(ctx, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.backend.logger().DebugContext(ctx,
				"workflow archive_entity: store row vanished between GetEntity and ArchiveEntity; vault already moved",
				"workflow", workflow, "entity_id", entityID)
			return nil
		}
		// Vault move already succeeded; reindex will reconcile.
		return fmt.Errorf("store.ArchiveEntity %s (vault already moved): %w", entityID, err)
	}
	return nil
}

// VaultRestoreWriter is the production-default RestoreWriter
// per #196 — the mirror of VaultArchiveWriter. Combines
// `vault.Writer.RestoreWithCommit` (move _archive → active)
// with `store.Store.RestoreEntity` (DB toggle clears
// archived_at) behind a per-entity write-lock acquired via
// AcquireWithTimeout — same shape as VaultArchiveWriter.
//
// Idempotence: store.RestoreEntity is a no-op on rows whose
// archived_at is already NULL; vault.RestoreWithCommit
// short-circuits when source-in-archive is missing AND
// destination-in-active is present. Re-firing on an already-
// active entity is a clean no-op.
//
// Soft-skip semantics: when the store's GetEntity reports
// ErrNotFound for the resolved id, the writer logs at debug
// and returns nil (success). The entity may have been removed
// by another path between the trigger event and this action;
// failing the chain over that adds noise without giving the
// operator anything actionable.
type VaultRestoreWriter struct {
	backend *VaultWriterBackend
}

// NewVaultRestoreWriter mirrors NewVaultArchiveWriter. Backend
// must be non-nil with Store + VaultWriter + WriteLocks set;
// missing fields panic at first call.
func NewVaultRestoreWriter(b *VaultWriterBackend) *VaultRestoreWriter {
	return &VaultRestoreWriter{backend: b}
}

// RestoreEntity implements RestoreWriter. Reads the entity to
// learn its kind for the vault move, acquires the per-entity
// write-lock via AcquireWithTimeout, runs the vault move, then
// flips the DB row's archived_at to NULL. Vault-first per
// ADR-0008 vault-as-source-of-truth — a vault failure aborts
// before the DB toggle; a DB failure after the vault move logs
// loudly (reindex reconciles future state from the vault side).
//
// `reason` is folded into the commit message so the audit trail
// names the workflow's stated reason inline with the restore
// event.
func (w *VaultRestoreWriter) RestoreEntity(ctx context.Context, workflow, entityID, reason string) error {
	if w.backend == nil {
		return fmt.Errorf("VaultRestoreWriter: backend not wired")
	}
	holder := workflowHolder(workflow, "restore_entity")
	release, err := w.backend.WriteLocks.AcquireWithTimeout(ctx, entityID, holder, w.backend.lockTimeout())
	if err != nil {
		return fmt.Errorf("acquire write-lock on %s: %w", entityID, err)
	}
	defer release()

	got, err := w.backend.Store.GetEntity(ctx, entityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.backend.logger().DebugContext(ctx,
				"workflow restore_entity: entity not found in store; soft-skipping",
				"workflow", workflow, "entity_id", entityID)
			return nil
		}
		return fmt.Errorf("store.GetEntity %s: %w", entityID, err)
	}

	commitMsg := fmt.Sprintf("restore: %s", entityID)
	if reason != "" {
		commitMsg = fmt.Sprintf("restore: %s (%s)", entityID, reason)
	}
	author := workflowAuthor(workflow)

	// Vault move first. Already-active (source missing AND
	// destination present) returns nil from the vault layer
	// without a commit — the writer treats that as success +
	// proceeds to ensure the DB toggle is also cleared.
	if err := w.backend.VaultWriter.RestoreWithCommit(ctx, got.Kind, entityID, commitMsg, author); err != nil {
		return fmt.Errorf("vault restore %s: %w", entityID, err)
	}

	// DB toggle. Already-active rows (archived_at NULL) are a
	// no-op at the store layer. ErrNotFound here means the row
	// was dropped between GetEntity and RestoreEntity (rare race).
	if err := w.backend.Store.RestoreEntity(ctx, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.backend.logger().DebugContext(ctx,
				"workflow restore_entity: store row vanished between GetEntity and RestoreEntity; vault already moved",
				"workflow", workflow, "entity_id", entityID)
			return nil
		}
		// Vault move already succeeded; reindex will reconcile.
		return fmt.Errorf("store.RestoreEntity %s (vault already moved): %w", entityID, err)
	}
	return nil
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
	if len(e.Notes) > 0 {
		parts := make([]string, 0, len(e.Notes))
		for _, c := range e.Notes {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			out["notes_text"] = strings.Join(parts, "\n")
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
			Source:       v.Source,
			FilledAt:     v.FilledAt,
			Deferred:     v.Deferred,
			DeferredAt:   v.DeferredAt,
			DataSchema:   cloneStringMap(v.DataSchema),
			Type:         v.Type,
			Description:  v.Description,
			FillStrategy: v.FillStrategy,
			Range:        append([]int(nil), v.Range...),
			MaxLength:    v.MaxLength,
			Values:       append([]string(nil), v.Values...),
			Kinds:        append([]string(nil), v.Kinds...),
		}
	}
	return out
}

// injectionEmpty reports whether a GapInjection carries no
// fields at all. Used to short-circuit the no-op-add path: a
// bare `add_gap: { gap: <name> }` re-fire stays a no-op when
// the gap is already present and no inline metadata is
// supplied.
func injectionEmpty(inj GapInjection) bool {
	return len(inj.DataSchema) == 0 &&
		inj.Type == "" &&
		inj.Description == "" &&
		inj.FillStrategy == "" &&
		len(inj.Range) == 0 &&
		inj.MaxLength == 0 &&
		len(inj.Values) == 0 &&
		len(inj.Kinds) == 0
}

// mergeGapInjection layers the non-empty fields from inj onto
// the existing GapStateEntry. Empty / zero fields preserve the
// entry's prior value so a subsequent add_gap can refresh one
// aspect (e.g. add data_schema to a previously bare gap) without
// clobbering other workflow-injected metadata.
func mergeGapInjection(entry *vault.GapStateEntry, inj GapInjection) {
	if len(inj.DataSchema) > 0 {
		entry.DataSchema = cloneStringMap(inj.DataSchema)
	}
	if inj.Type != "" {
		entry.Type = inj.Type
	}
	if inj.Description != "" {
		entry.Description = inj.Description
	}
	if inj.FillStrategy != "" {
		entry.FillStrategy = inj.FillStrategy
	}
	if len(inj.Range) > 0 {
		entry.Range = append([]int(nil), inj.Range...)
	}
	if inj.MaxLength != 0 {
		entry.MaxLength = inj.MaxLength
	}
	if len(inj.Values) > 0 {
		entry.Values = append([]string(nil), inj.Values...)
	}
	if len(inj.Kinds) > 0 {
		entry.Kinds = append([]string(nil), inj.Kinds...)
	}
}

// cloneStringMap copies a string→string map so the caller can
// mutate the source without aliasing into the persisted entry
// (and vice versa). Returns nil for nil/empty input so the
// `omitempty` shape stays clean on the wire.
func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// VaultEdgeWriter is the production-default EdgeWriter for the
// add_canonical_edge primitive (#132). Slugifies the target name
// via the daemon's clean-slug rule, ensures a thin DB row for
// the target canonical-label exists, creates the source→target
// edge (idempotent via the (type, from, to) upsert in
// store.CreateEdge), and — when data is non-empty — appends a
// dataview-inline paragraph to the target canonical entity's
// body via canonical.AppendDataviewParagraph (auto-materializes
// the target vault file when missing per ADR-0021 §3).
//
// EdgeStore is the full store.Store interface here (rather than
// the narrow EntityStore used by Note/Gap/Property writers)
// because the canonical-edge + dataview-append paths need
// CreateEdge alongside the GetEntity / UpsertEntity surface
// EntityStore exposes.
type VaultEdgeWriter struct {
	store       store.Store
	edgeWriter  edgewrite.EdgeWriter
	vaultReader *vault.Reader
	vaultWriter *vault.Writer
	writeLocks  *writelocks.Manager
	kindReg     map[string]config.CanonicalKindConfig
	bus         eventbus.Bus
	logger      *slog.Logger
}

// NewVaultEdgeWriter constructs an EdgeWriter from the daemon's
// full vault + store wiring. All non-bus/non-logger fields are
// required; nil bus / nil logger fall through to no-op /
// discarding behaviour (mirrors the other vault-backed writers'
// optional-deps convention).
func NewVaultEdgeWriter(
	st store.Store,
	edgeWriter edgewrite.EdgeWriter,
	vaultReader *vault.Reader,
	vaultWriter *vault.Writer,
	writeLocks *writelocks.Manager,
	kindReg map[string]config.CanonicalKindConfig,
	bus eventbus.Bus,
	logger *slog.Logger,
) *VaultEdgeWriter {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if edgeWriter == nil {
		// Default to a passthrough Service per #304 Cut C1; the
		// VaultEdgeWriter's CreateEdge calls flow through it
		// so future cuts surface caller-mode + resolver routing
		// uniformly across all edge-creation paths.
		svc, err := edgewrite.New(st, nil)
		if err != nil {
			// Same surfacing as api/api.go's default path —
			// only fires if a future edgewrite.New grows a
			// new validation gate that the nil-resolver call
			// trips.
			panic(fmt.Sprintf("NewVaultEdgeWriter: default edgewrite.Service construction failed: %v", err))
		}
		edgeWriter = svc
	}
	return &VaultEdgeWriter{
		store:       st,
		edgeWriter:  edgeWriter,
		vaultReader: vaultReader,
		vaultWriter: vaultWriter,
		writeLocks:  writeLocks,
		kindReg:     kindReg,
		bus:         bus,
		logger:      logger,
	}
}

// AddCanonicalEdge implements EdgeWriter. Slugifies targetName
// via slug.Slug to produce the canonical-label id
// (`<targetKind>:<slug>`), ensures the thin DB row exists,
// creates the source→target edge (CreateEdge is upsert-keyed on
// (type, from, to) — re-fires for the same tuple are idempotent
// no-ops at the SQL layer), then optionally fires the dataview-
// append.
//
// Event emission:
//
//   - entity.created fires once when EnsureLabelRow materializes
//     a new target thin row (gated on the `created` return so
//     existing rows don't re-emit).
//   - entity.edge_added fires per CreateEdge (idempotent at the
//     event layer too: the bus subscriber's de-dup behaviour
//     decides whether duplicate edge events propagate).
//   - fill.completed fires when canonical.AppendDataviewParagraph
//     actually lands a new paragraph (gated on appended=true so
//     a content-hash-dedup skip doesn't fire the event).
//
// The SourceTag on every event is `workflow:<workflow>` to
// match the ADR-0024 vocabulary; downstream workflows can branch
// on the source to skip self-loops.
func (w *VaultEdgeWriter) AddCanonicalEdge(
	ctx context.Context,
	workflow, sourceID, edgeType, targetKind, targetName string,
	data map[string]string,
) error {
	// ADR-0027 cut 1 kind-prefix strip: when targetName is the
	// canonical-ID form `<targetKind>:<slug>` (e.g. operator
	// passes today() which returns "day:2026-11-11"), strip the
	// leading "<targetKind>:" before slugifying so the slug.Slug
	// pass doesn't mangle the colon into a hyphen. Bare
	// targetName values (e.g. "My Daily Note") pass through
	// unchanged. Conservative: only strip when the prefix
	// exactly matches the declared target.kind — any other text
	// is left alone.
	stripped := strings.TrimPrefix(targetName, targetKind+":")
	targetSlug := slug.Slug(stripped)
	if targetSlug == "" {
		return fmt.Errorf("slugify target name %q produced empty slug", targetName)
	}
	targetID := targetKind + ":" + targetSlug

	source := eventbus.WorkflowSource(workflow)
	chain := eventbus.WorkflowChainFromContext(ctx)

	created, err := canonical.EnsureLabelRow(ctx, w.store, targetID, w.logger)
	if err != nil {
		return fmt.Errorf("ensure label row %q: %w", targetID, err)
	}
	if created && w.bus != nil {
		// CausedByEntityID = sourceID: the entity the workflow
		// action operated on drove this thin-row materialization.
		w.bus.Publish(ctx, eventbus.EntityCreatedEvent{
			ID:               targetID,
			Kind:             targetKind,
			SourceTag:        source,
			At:               time.Now().UTC(),
			Chain:            chain,
			CausedByEntityID: sourceID,
		})
	}

	if err := w.edgeWriter.CreateEdge(ctx, &store.Edge{
		Type: edgeType,
		From: sourceID,
		To:   targetID,
	}); err != nil {
		return fmt.Errorf("create edge %s -[%s]-> %s: %w", sourceID, edgeType, targetID, err)
	}
	if w.bus != nil {
		w.bus.Publish(ctx, eventbus.EntityEdgeAddedEvent{
			FromID:           sourceID,
			ToID:             targetID,
			EdgeType:         edgeType,
			SourceTag:        source,
			At:               time.Now().UTC(),
			Chain:            chain,
			CausedByEntityID: sourceID,
		})
	}

	if len(data) == 0 {
		return nil
	}

	deps := canonical.DataviewAppendDeps{
		Store:       w.store,
		VaultReader: w.vaultReader,
		VaultWriter: w.vaultWriter,
		WriteLocks:  w.writeLocks,
		KindReg:     w.kindReg,
		Bus:         w.bus,
		Logger:      w.logger,
	}
	appended, err := canonical.AppendDataviewParagraph(ctx, deps, targetID, data, edgeType, workflow)
	if err != nil {
		return fmt.Errorf("append dataview paragraph on %s: %w", targetID, err)
	}
	if appended && w.bus != nil {
		w.bus.Publish(ctx, eventbus.FillCompletedEvent{
			EntityID:  targetID,
			Gap:       edgeType,
			SourceTag: source,
			At:        time.Now().UTC(),
			Chain:     chain,
		})
	}
	return nil
}
