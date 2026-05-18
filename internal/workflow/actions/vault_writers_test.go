package actions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// fakeEntityStore is an in-memory EntityStore for the vault-
// writer tests. GetEntity returns store.ErrNotFound for ids
// not in the seeded map; UpsertEntity records the upsert.
type fakeEntityStore struct {
	mu        sync.Mutex
	entities  map[string]*store.Entity
	upserts   []*store.Entity
	upsertErr error
	// archives counts ArchiveEntity calls per id (per #150).
	archives map[string]int
}

func newFakeEntityStore(seed map[string]*store.Entity) *fakeEntityStore {
	if seed == nil {
		seed = map[string]*store.Entity{}
	}
	return &fakeEntityStore{entities: seed}
}

func (f *fakeEntityStore) GetEntity(_ context.Context, id string) (*store.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entities[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e, nil
}

func (f *fakeEntityStore) UpsertEntity(_ context.Context, e *store.Entity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	cp := *e
	f.upserts = append(f.upserts, &cp)
	return nil
}

func (f *fakeEntityStore) ArchiveEntity(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entities[id]
	if !ok {
		return store.ErrNotFound
	}
	if f.archives == nil {
		f.archives = map[string]int{}
	}
	f.archives[id]++
	if e.ArchivedAt == nil {
		now := time.Now().UTC()
		e.ArchivedAt = &now
	}
	return nil
}

func (f *fakeEntityStore) archiveCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.archives[id]
}

func (f *fakeEntityStore) upsertSnapshot() []*store.Entity {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.Entity, len(f.upserts))
	copy(out, f.upserts)
	return out
}

// fakeVaultReader returns the seeded entity per (kind, id),
// or fs.ErrNotExist for unknowns (matching vault.IsNotExist).
type fakeVaultReader struct {
	mu       sync.Mutex
	entities map[string]*vault.Entity // keyed by id
}

func newFakeVaultReader(seed map[string]*vault.Entity) *fakeVaultReader {
	if seed == nil {
		seed = map[string]*vault.Entity{}
	}
	return &fakeVaultReader{entities: seed}
}

func (f *fakeVaultReader) ReadByID(_ string, id string) (*vault.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entities[id]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: id, Err: fs.ErrNotExist}
	}
	// Return a deep-ish copy of the slices so the writer can
	// append without leaking back into the seeded shape.
	cp := *e
	cp.Notes = append([]vault.Note(nil), e.Notes...)
	cp.Gaps = append([]string(nil), e.Gaps...)
	if e.GapState != nil {
		cp.GapState = make(map[string]vault.GapStateEntry, len(e.GapState))
		for k, v := range e.GapState {
			cp.GapState[k] = v
		}
	}
	return &cp, nil
}

// fakeVaultWriter records the latest WriteWithCommit call.
type fakeVaultWriter struct {
	mu          sync.Mutex
	writes      []vaultWrite
	writeErr    error
	archives    []archiveVaultCall
	archiveErr  error
}

type vaultWrite struct {
	entity  *vault.Entity
	message string
	author  string
}

type archiveVaultCall struct {
	kind    string
	id      string
	message string
	author  string
}

func (f *fakeVaultWriter) WriteWithCommit(_ context.Context, e *vault.Entity, message, author string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	cp := *e
	cp.Notes = append([]vault.Note(nil), e.Notes...)
	cp.Gaps = append([]string(nil), e.Gaps...)
	if e.GapState != nil {
		cp.GapState = make(map[string]vault.GapStateEntry, len(e.GapState))
		for k, v := range e.GapState {
			cp.GapState[k] = v
		}
	}
	f.writes = append(f.writes, vaultWrite{entity: &cp, message: message, author: author})
	return nil
}

func (f *fakeVaultWriter) snapshot() []vaultWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vaultWrite, len(f.writes))
	copy(out, f.writes)
	return out
}

func (f *fakeVaultWriter) ArchiveWithCommit(_ context.Context, kind, id, message, author string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.archiveErr != nil {
		return f.archiveErr
	}
	f.archives = append(f.archives, archiveVaultCall{
		kind: kind, id: id, message: message, author: author,
	})
	return nil
}

func (f *fakeVaultWriter) archiveSnapshot() []archiveVaultCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]archiveVaultCall, len(f.archives))
	copy(out, f.archives)
	return out
}

// newVaultWriterBackend assembles a backend for the tests
// with seeded fakes + a fresh writelocks.Manager + a
// fixed-clock so timestamps are deterministic.
func newVaultWriterBackend(t *testing.T, storeSeed map[string]*store.Entity, vaultSeed map[string]*vault.Entity, fixedTime time.Time) (*VaultWriterBackend, *fakeEntityStore, *fakeVaultReader, *fakeVaultWriter) {
	t.Helper()
	es := newFakeEntityStore(storeSeed)
	vr := newFakeVaultReader(vaultSeed)
	vw := &fakeVaultWriter{}
	b := &VaultWriterBackend{
		Store:       es,
		VaultReader: vr,
		VaultWriter: vw,
		WriteLocks:  writelocks.New(),
		Clock:       func() time.Time { return fixedTime },
	}
	return b, es, vr, vw
}

// TestVaultNoteWriter_HappyPath: a note lands as
// vault.Note with workflow:<name> author + commit author
// + DB upsert reflecting the notes_text column.
func TestVaultNoteWriter_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr", CreatedAt: now},
		},
		map[string]*vault.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr", Data: map[string]any{"title": "fix things"}},
		},
		now,
	)
	w := NewVaultNoteWriter(b)
	err := w.AppendNote(context.Background(), "bgg-news", "pr:1", "found a related entity")
	require.NoError(t, err)

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	require.Len(t, writes[0].entity.Notes, 1)
	assert.Equal(t, "found a related entity", writes[0].entity.Notes[0].Text)
	assert.Equal(t, "workflow:bgg-news", writes[0].entity.Notes[0].Author)
	assert.Equal(t, now, writes[0].entity.Notes[0].Date)
	assert.Equal(t, "workflow:bgg-news", writes[0].author, "commit author")
	assert.Contains(t, writes[0].message, "workflow note on pr:1")

	upserts := es.upsertSnapshot()
	require.Len(t, upserts, 1)
	assert.Equal(t, "found a related entity", upserts[0].Data["notes_text"])
	assert.Equal(t, "pr:1", upserts[0].ID)
}

// TestVaultNoteWriter_EntityNotFound: workflows targeting
// an unknown entity surface ErrEntityNotFound (the runner's
// caller propagates it through ActionResult.Err).
func TestVaultNoteWriter_EntityNotFound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, _, _, _ := newVaultWriterBackend(t, nil, nil, now)
	w := NewVaultNoteWriter(b)
	err := w.AppendNote(context.Background(), "wf", "pr:absent", "hi")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEntityNotFound)
}

// TestVaultNoteWriter_VaultFileMissing: store row exists
// but vault file doesn't — surfaces ErrEntityNotFound too
// (workflows don't auto-materialize per ADR-0021 amendment).
func TestVaultNoteWriter_VaultFileMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, _, _, _ := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"pr:thin": {ID: "pr:thin", Kind: "pr", CreatedAt: now},
		},
		nil, // no vault file
		now,
	)
	w := NewVaultNoteWriter(b)
	err := w.AppendNote(context.Background(), "wf", "pr:thin", "hi")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEntityNotFound)
	assert.Contains(t, err.Error(), "vault file missing")
}

// TestVaultNoteWriter_WriteLockConflict: a concurrent
// holder of the entity's write-lock that never releases
// returns a ConflictError after the action-side
// AcquireWithTimeout deadline (#152). The workflow surfaces
// the conflict verbatim.
func TestVaultNoteWriter_WriteLockConflict(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, _, _, _ := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr"},
		},
		map[string]*vault.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr"},
		},
		now,
	)
	// Shrink the per-backend timeout so this test doesn't
	// burn 5s waiting for the lock-that-never-releases.
	b.LockTimeout = 50 * time.Millisecond
	// Acquire the lock first; the writer's Acquire should
	// timeout after the shrunk AcquireWithTimeout deadline.
	release, err := b.WriteLocks.Acquire("pr:1", "test-holder")
	require.NoError(t, err)
	defer release()

	w := NewVaultNoteWriter(b)
	err = w.AppendNote(context.Background(), "wf", "pr:1", "hi")
	require.Error(t, err)
	assert.True(t, writelocks.IsConflict(unwrapToConflict(err)),
		"writelocks.IsConflict on the unwrapped writer error")
}

// TestVaultNoteWriter_WriteLockWaitsForReleaser pins the #152
// fix: when a concurrent holder releases the lock during the
// action runner's bounded wait, the writer acquires + completes
// successfully instead of failing with a 409 conflict.
func TestVaultNoteWriter_WriteLockWaitsForReleaser(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr"},
		},
		map[string]*vault.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr"},
		},
		now,
	)
	// Hold the lock briefly, release after 100ms — simulates
	// the ingest path's per-envelope hold during which a
	// workflow fires on edge_created.
	release, err := b.WriteLocks.Acquire("pr:1", "ingest:gmail")
	require.NoError(t, err)
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()

	w := NewVaultNoteWriter(b)
	err = w.AppendNote(context.Background(), "wf", "pr:1", "post-ingest note")
	require.NoError(t, err, "writer should wait for the ingest hold to release, not error")

	writes := vw.snapshot()
	require.Len(t, writes, 1, "write proceeds after the ingest hold releases")
}

// TestVaultNoteWriter_UpsertErrorDegradesGracefully: a
// store.UpsertEntity failure logs a Warn but the write itself
// still returns nil (vault is source of truth per ADR-0008;
// DB is a search-mirror).
func TestVaultNoteWriter_UpsertErrorDegradesGracefully(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr"},
		},
		map[string]*vault.Entity{
			"pr:1": {ID: "pr:1", Kind: "pr"},
		},
		now,
	)
	es.upsertErr = errors.New("db down")
	w := NewVaultNoteWriter(b)
	err := w.AppendNote(context.Background(), "wf", "pr:1", "hi")
	assert.NoError(t, err, "vault write succeeded; DB-mirror failure doesn't fail the call")
	require.Len(t, vw.snapshot(), 1, "vault write still landed")
}

// TestVaultGapWriter_HappyPath: a fresh gap lands in Gaps +
// GapState (zero-value entry → pending), commit + DB upsert
// fire.
func TestVaultGapWriter_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "classify", "email:m1", "is_interesting_to_me", GapInjection{})
	require.NoError(t, err)

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	assert.Equal(t, []string{"is_interesting_to_me"}, writes[0].entity.Gaps)
	require.Contains(t, writes[0].entity.GapState, "is_interesting_to_me")
	// Zero-value entry: not filled, not deferred (pending).
	entry := writes[0].entity.GapState["is_interesting_to_me"]
	assert.Empty(t, entry.Source)
	assert.Nil(t, entry.FilledAt)
	assert.False(t, entry.Deferred)
	assert.Equal(t, "workflow:classify", writes[0].author)
	assert.Contains(t, writes[0].message, "workflow add_gap is_interesting_to_me")

	upserts := es.upsertSnapshot()
	require.Len(t, upserts, 1)
	require.Contains(t, upserts[0].GapState, "is_interesting_to_me")
}

// TestVaultGapWriter_Idempotent: adding a gap already present
// in Gaps is a no-op success (no vault write, no upsert).
func TestVaultGapWriter_Idempotent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {
				ID:   "email:m1",
				Kind: "email",
				Gaps: []string{"is_interesting_to_me"},
				GapState: map[string]vault.GapStateEntry{
					"is_interesting_to_me": {},
				},
			},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "classify", "email:m1", "is_interesting_to_me", GapInjection{})
	assert.NoError(t, err)
	assert.Empty(t, vw.snapshot(), "no vault write on idempotent add")
	assert.Empty(t, es.upsertSnapshot(), "no DB upsert on idempotent add")
}

// TestVaultGapWriter_EntityNotFound: same shape as the
// note-writer not-found path.
func TestVaultGapWriter_EntityNotFound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, _, _, _ := newVaultWriterBackend(t, nil, nil, now)
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "wf", "pr:absent", "g", GapInjection{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEntityNotFound)
}

// TestVaultGapWriter_PreservesExistingGapState: an existing
// GapState entry for an unrelated gap is preserved on a new
// gap add.
func TestVaultGapWriter_PreservesExistingGapState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	filledAt := time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {
				ID:   "email:m1",
				Kind: "email",
				Gaps: []string{},
				GapState: map[string]vault.GapStateEntry{
					"existing_gap": {Source: "operator", FilledAt: &filledAt},
				},
			},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "wf", "email:m1", "new_gap", GapInjection{})
	require.NoError(t, err)

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	require.Contains(t, writes[0].entity.GapState, "existing_gap")
	require.Contains(t, writes[0].entity.GapState, "new_gap")
	existing := writes[0].entity.GapState["existing_gap"]
	assert.Equal(t, "operator", existing.Source, "operator-filled gap retained")
}

// TestVaultGapWriter_NewGapWithDataSchema: a new gap injected
// alongside a workflow-supplied data_schema persists the
// schema on the GapStateEntry so /v1/needs-fill can surface
// the per-key extraction guidance (#117).
func TestVaultGapWriter_NewGapWithDataSchema(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	schema := map[string]string{
		"role":      "the role title in the hiring alert",
		"salary":    "salary range if mentioned, else omit",
		"work_mode": "remote / hybrid / onsite if mentioned, else omit",
	}
	err := w.AddGap(context.Background(), "linkedin-classify", "email:m1", "hiring_alert_for", GapInjection{DataSchema: schema})
	require.NoError(t, err)

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	entry := writes[0].entity.GapState["hiring_alert_for"]
	assert.Equal(t, schema, entry.DataSchema)

	upserts := es.upsertSnapshot()
	require.Len(t, upserts, 1)
	assert.Equal(t, schema, upserts[0].GapState["hiring_alert_for"].DataSchema,
		"store mirror carries the schema for /v1/needs-fill to surface")
}

// TestVaultGapWriter_DataSchemaCloned: mutating the caller's
// dataSchema map after AddGap returns must not bleed into the
// persisted entry. The writer clones at insert time.
func TestVaultGapWriter_DataSchemaCloned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	schema := map[string]string{"role": "extract role"}
	err := w.AddGap(context.Background(), "wf", "email:m1", "g", GapInjection{DataSchema: schema})
	require.NoError(t, err)

	schema["role"] = "MUTATED"
	schema["new_key"] = "should not appear"

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	persisted := writes[0].entity.GapState["g"].DataSchema
	assert.Equal(t, "extract role", persisted["role"], "caller mutation must not leak in")
	_, hasNew := persisted["new_key"]
	assert.False(t, hasNew, "caller-added key must not appear in persisted schema")
}

// TestVaultGapWriter_IdempotentSkipPreservesSchema: re-adding
// an existing gap with no new schema is the no-op-success path
// the original idempotent contract guarantees. The earlier
// schema on the entry survives untouched (no vault write so
// nothing is rewritten).
func TestVaultGapWriter_IdempotentSkipPreservesSchema(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {
				ID:   "email:m1",
				Kind: "email",
				Gaps: []string{"g"},
				GapState: map[string]vault.GapStateEntry{
					"g": {DataSchema: map[string]string{"k": "earlier instruction"}},
				},
			},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "wf", "email:m1", "g", GapInjection{})
	require.NoError(t, err)
	assert.Empty(t, vw.snapshot(), "no vault write on idempotent re-add with no new schema")
	assert.Empty(t, es.upsertSnapshot(), "no DB upsert on idempotent re-add")
}

// TestVaultGapWriter_GapPresentReplaceSchema: re-adding an
// existing gap WITH a new schema overwrites the entry's
// schema (workflows can refresh the extraction instructions
// without going through a manual operator path).
func TestVaultGapWriter_GapPresentReplaceSchema(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {
				ID:   "email:m1",
				Kind: "email",
				Gaps: []string{"g"},
				GapState: map[string]vault.GapStateEntry{
					"g": {DataSchema: map[string]string{"k": "old"}},
				},
			},
		},
		now,
	)
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "wf", "email:m1", "g",
		GapInjection{DataSchema: map[string]string{"k": "new", "k2": "added"}})
	require.NoError(t, err)
	writes := vw.snapshot()
	require.Len(t, writes, 1, "schema refresh triggers a write even when the gap was present")
	persisted := writes[0].entity.GapState["g"].DataSchema
	assert.Equal(t, "new", persisted["k"])
	assert.Equal(t, "added", persisted["k2"])
}

// TestVaultGapWriter_VaultWriteError: vault write failure
// propagates with the entity-id context wrapped.
func TestVaultGapWriter_VaultWriteError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		map[string]*vault.Entity{
			"email:m1": {ID: "email:m1", Kind: "email"},
		},
		now,
	)
	vw.writeErr = errors.New("disk full")
	w := NewVaultGapWriter(b)
	err := w.AddGap(context.Background(), "wf", "email:m1", "g", GapInjection{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk full")
}

// TestWorkflowAuthor_EmptyWorkflowName: defensive — an empty
// workflow name falls back to "workflow:unknown" rather than
// stamping an unattributed "workflow:" prefix.
func TestWorkflowAuthor_EmptyWorkflowName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "workflow:unknown", workflowAuthor(""))
	assert.Equal(t, "workflow:unknown", workflowAuthor("   "))
	assert.Equal(t, "workflow:foo", workflowAuthor("foo"))
}

// unwrapToConflict walks the err chain to find a writelocks
// ConflictError so the test can assert via IsConflict.
func unwrapToConflict(err error) error {
	for err != nil {
		var ce *writelocks.ConflictError
		if errors.As(err, &ce) {
			return ce
		}
		err = errors.Unwrap(err)
		if err == nil {
			break
		}
	}
	return fmt.Errorf("no conflict in chain")
}

// TestVaultArchiveWriter_HappyPath: archive_entity flows through
// vault move → store toggle behind the per-entity write-lock,
// with the workflow name in the commit author + commit msg.
func TestVaultArchiveWriter_HappyPath(t *testing.T) {
	t.Parallel()
	const id = "gmail:msg-1"
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			id: {ID: id, Kind: "gmail", Data: map[string]any{"id": id}},
		},
		map[string]*vault.Entity{
			id: {ID: id, Kind: "gmail", Plugin: "gmail"},
		},
		now,
	)
	w := NewVaultArchiveWriter(b)
	err := w.ArchiveEntity(context.Background(),
		"classify-and-archive", id, "classified-into-canonical-edge")
	require.NoError(t, err)

	vaults := vw.archiveSnapshot()
	require.Len(t, vaults, 1)
	assert.Equal(t, "gmail", vaults[0].kind)
	assert.Equal(t, id, vaults[0].id)
	assert.Equal(t, "archive: gmail:msg-1 (classified-into-canonical-edge)", vaults[0].message)
	assert.Equal(t, "workflow:classify-and-archive", vaults[0].author)
	assert.Equal(t, 1, es.archiveCount(id))
}

// TestVaultArchiveWriter_ReasonOptional: empty reason yields a
// bare commit message without the parenthesized suffix.
func TestVaultArchiveWriter_ReasonOptional(t *testing.T) {
	t.Parallel()
	const id = "gmail:msg-no-reason"
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			id: {ID: id, Kind: "gmail", Data: map[string]any{"id": id}},
		},
		map[string]*vault.Entity{
			id: {ID: id, Kind: "gmail", Plugin: "gmail"},
		},
		now,
	)
	w := NewVaultArchiveWriter(b)
	require.NoError(t, w.ArchiveEntity(context.Background(), "wf", id, ""))

	vaults := vw.archiveSnapshot()
	require.Len(t, vaults, 1)
	assert.Equal(t, "archive: gmail:msg-no-reason", vaults[0].message,
		"empty reason → bare commit message without parenthesized suffix")
}

// TestVaultArchiveWriter_NotFound_SoftSkip: ErrNotFound from
// store.GetEntity returns nil (success) per #150 — the entity
// may have been archived by another path. No vault move runs;
// no store toggle runs.
func TestVaultArchiveWriter_NotFound_SoftSkip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{}, // empty store → ErrNotFound
		map[string]*vault.Entity{},
		now,
	)
	w := NewVaultArchiveWriter(b)
	err := w.ArchiveEntity(context.Background(),
		"wf", "gmail:missing", "any-reason")
	require.NoError(t, err, "not-found is a soft-skip success per #150")
	assert.Empty(t, vw.archiveSnapshot(), "no vault move on soft-skip")
	assert.Equal(t, 0, es.archiveCount("gmail:missing"), "no store toggle on soft-skip")
}

// TestVaultArchiveWriter_Idempotent: re-firing on an already-
// archived row succeeds (vault layer + store both idempotent).
// The fake's archive counter increments since the fake doesn't
// model COALESCE — but production *store.Store does. The test
// pins the runner-side success contract: re-fire returns nil.
func TestVaultArchiveWriter_Idempotent(t *testing.T) {
	t.Parallel()
	const id = "gmail:msg-idem"
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, _ := newVaultWriterBackend(t,
		map[string]*store.Entity{
			id: {ID: id, Kind: "gmail", Data: map[string]any{"id": id}},
		},
		map[string]*vault.Entity{
			id: {ID: id, Kind: "gmail", Plugin: "gmail"},
		},
		now,
	)
	w := NewVaultArchiveWriter(b)
	require.NoError(t, w.ArchiveEntity(context.Background(), "wf", id, ""))
	require.NoError(t, w.ArchiveEntity(context.Background(), "wf", id, ""),
		"re-fire on already-archived returns nil — workflow chain continues")
}

// TestVaultArchiveWriter_BackendNotWired: a writer constructed
// with a nil backend surfaces a clear configuration error
// rather than panicking.
func TestVaultArchiveWriter_BackendNotWired(t *testing.T) {
	t.Parallel()
	w := &VaultArchiveWriter{}
	err := w.ArchiveEntity(context.Background(), "wf", "gmail:x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backend not wired")
}

// TestVaultArchiveWriter_VaultError_NoDBToggle: a vault-move
// failure aborts before the DB is touched (vault-first per
// ADR-0008).
func TestVaultArchiveWriter_VaultError_NoDBToggle(t *testing.T) {
	t.Parallel()
	const id = "gmail:msg-vault-err"
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, es, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{
			id: {ID: id, Kind: "gmail", Data: map[string]any{"id": id}},
		},
		map[string]*vault.Entity{
			id: {ID: id, Kind: "gmail", Plugin: "gmail"},
		},
		now,
	)
	vw.archiveErr = errors.New("disk full")
	w := NewVaultArchiveWriter(b)
	err := w.ArchiveEntity(context.Background(), "wf", id, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk full")
	assert.Equal(t, 0, es.archiveCount(id),
		"DB toggle skipped when vault move fails")
}

// TestVaultArchiveWriter_LockConflictTimesOut: when another
// holder grabs the per-entity lock first, AcquireWithTimeout
// surfaces a ConflictError after the per-backend timeout.
func TestVaultArchiveWriter_LockConflictTimesOut(t *testing.T) {
	t.Parallel()
	const id = "gmail:msg-locked"
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, _ := newVaultWriterBackend(t,
		map[string]*store.Entity{
			id: {ID: id, Kind: "gmail"},
		},
		map[string]*vault.Entity{
			id: {ID: id, Kind: "gmail", Plugin: "gmail"},
		},
		now,
	)
	b.LockTimeout = 50 * time.Millisecond
	release, err := b.WriteLocks.Acquire(id, "intruder")
	require.NoError(t, err)
	defer release()

	w := NewVaultArchiveWriter(b)
	err = w.ArchiveEntity(context.Background(), "wf", id, "")
	require.Error(t, err)
	assert.True(t, writelocks.IsConflict(unwrapToConflict(err)),
		"err chain carries the writelocks.ConflictError")
}

// withKindsRegistry returns a fresh backend wired to the
// canonical-kinds registry the wikilink tests use. Mirrors
// the canonicalKindsForTest helper used by wikilinks_test.go
// — three known kinds (boardgame, person, gmail) plus the
// company kind that the linkedin workflow chain references.
func withKindsRegistry(b *VaultWriterBackend) {
	b.Kinds = map[string]config.CanonicalKindConfig{
		"boardgame": {},
		"person":    {},
		"gmail":     {},
		"company":   {},
	}
}

// TestVaultNoteWriter_WrapsEntityShapedBody (#166): when the
// add_note CEL template renders to a `<kind>:<id>` string and
// the kind is in the registry, the body lands as a `[[ ]]`
// wikilink so Obsidian surfaces the backlink.
func TestVaultNoteWriter_WrapsEntityShapedBody(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"pr:1": {ID: "pr:1", Kind: "pr"}},
		map[string]*vault.Entity{"pr:1": {ID: "pr:1", Kind: "pr"}},
		now,
	)
	withKindsRegistry(b)
	w := NewVaultNoteWriter(b)
	require.NoError(t, w.AppendNote(context.Background(),
		"link-watcher", "pr:1", "gmail:msg-abc"))

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	require.Len(t, writes[0].entity.Notes, 1)
	assert.Equal(t, "[[gmail:msg-abc]]", writes[0].entity.Notes[0].Text,
		"entity-shaped body wraps via maybeWrapEntity per #166")
}

// TestVaultNoteWriter_PassesThroughProseBody (#166): a body
// that isn't a single `<kind>:<id>` (prose with spaces /
// multiple colons / unknown kind) passes through unchanged.
func TestVaultNoteWriter_PassesThroughProseBody(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"pr:1": {ID: "pr:1", Kind: "pr"}},
		map[string]*vault.Entity{"pr:1": {ID: "pr:1", Kind: "pr"}},
		now,
	)
	withKindsRegistry(b)
	w := NewVaultNoteWriter(b)

	cases := []string{
		"see gmail:msg-1 for context", // prose with embedded ref → no wrap (kind portion isn't bare)
		"2026-05-18T17:30:00Z",        // multiple colons (timestamp)
		"package:foo-bar",             // single-colon shape but kind not in registry
		"plain text no colon",
	}
	for _, body := range cases {
		require.NoError(t, w.AppendNote(context.Background(),
			"wf", "pr:1", body))
	}

	writes := vw.snapshot()
	require.Len(t, writes, len(cases))
	for i, body := range cases {
		// fakeVaultReader.ReadByID returns the seeded entity
		// fresh each call (empty Notes), so each write's
		// entity.Notes contains exactly the just-appended note.
		require.NotEmpty(t, writes[i].entity.Notes, "write %d should carry a note", i)
		got := writes[i].entity.Notes[0].Text
		assert.NotContains(t, got, "[[",
			"prose body %d should not be wrapped: %q", i, body)
		assert.Equal(t, body, got,
			"prose body %d passes through unchanged", i)
	}
}

// TestVaultNoteWriter_NoRegistryNoWrap (#166): a backend with
// no Kinds registry leaves the body untouched even when the
// shape matches `<kind>:<id>`. Guards the test-friendly nil-
// kinds path; production sets backend.Kinds = mergedRegistry.
func TestVaultNoteWriter_NoRegistryNoWrap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"pr:1": {ID: "pr:1", Kind: "pr"}},
		map[string]*vault.Entity{"pr:1": {ID: "pr:1", Kind: "pr"}},
		now,
	)
	// b.Kinds intentionally unset.
	w := NewVaultNoteWriter(b)
	require.NoError(t, w.AppendNote(context.Background(),
		"wf", "pr:1", "gmail:msg-1"))

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	assert.Equal(t, "gmail:msg-1", writes[0].entity.Notes[0].Text,
		"nil registry → no wrap; body passes through")
}

// TestVaultPropertyWriter_WrapsStringValue (#166): a string
// field with `<kind>:<id>` shape lands as `[[<kind>:<id>]]`
// in vault.Entity.Data.
func TestVaultPropertyWriter_WrapsStringValue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		map[string]*vault.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		now,
	)
	withKindsRegistry(b)
	w := NewVaultPropertyWriter(b)
	require.NoError(t, w.SetProperties(context.Background(),
		"linkedin-classify", "gmail:msg-1",
		map[string]any{
			"hiring_alert_for": "company:acme-corp",
			"summary":          "plain text passes through",
		}))

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	assert.Equal(t, "[[company:acme-corp]]",
		writes[0].entity.Data["hiring_alert_for"],
		"entity-shaped string value wraps")
	assert.Equal(t, "plain text passes through",
		writes[0].entity.Data["summary"],
		"non-matching value passes through unchanged")
}

// TestVaultPropertyWriter_WrapsArrayElements (#166): the
// spec's mixed-array case. A `[]any` field where some
// elements match the entity shape + others don't — each
// element is wrapped independently.
func TestVaultPropertyWriter_WrapsArrayElements(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		map[string]*vault.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		now,
	)
	withKindsRegistry(b)
	w := NewVaultPropertyWriter(b)
	require.NoError(t, w.SetProperties(context.Background(),
		"wf", "gmail:msg-1",
		map[string]any{
			"refs": []any{
				"boardgame:acme-game",
				"plain string",
				"company:foo-corp",
				"unknown-kind:value",
			},
		}))

	writes := vw.snapshot()
	require.Len(t, writes, 1)
	refs, ok := writes[0].entity.Data["refs"].([]any)
	require.True(t, ok, "refs preserved as []any after wrap")
	require.Len(t, refs, 4)
	assert.Equal(t, "[[boardgame:acme-game]]", refs[0])
	assert.Equal(t, "plain string", refs[1])
	assert.Equal(t, "[[company:foo-corp]]", refs[2])
	assert.Equal(t, "unknown-kind:value", refs[3])
}

// TestVaultPropertyWriter_WrapsStringSliceShape (#166): a
// `[]string` field (the CEL-template homogeneous-slice
// surface) wraps each element + returns the same `[]string`
// shape so the YAML emission stays a list of strings rather
// than `[]any` round-trip.
func TestVaultPropertyWriter_WrapsStringSliceShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		map[string]*vault.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		now,
	)
	withKindsRegistry(b)
	w := NewVaultPropertyWriter(b)
	require.NoError(t, w.SetProperties(context.Background(),
		"wf", "gmail:msg-1",
		map[string]any{
			"tags": []string{"person:alex-example", "plain-tag"},
		}))

	writes := vw.snapshot()
	tags, ok := writes[0].entity.Data["tags"].([]string)
	require.True(t, ok, "tags stays []string after element-wise wrap")
	assert.Equal(t, []string{"[[person:alex-example]]", "plain-tag"}, tags)
}

// TestVaultPropertyWriter_NonStringTypesPassThrough (#166):
// values whose runtime type isn't string / []any / []string
// (booleans, integers, nested maps) can't be entity refs by
// shape — pass through unchanged regardless of registry state.
func TestVaultPropertyWriter_NonStringTypesPassThrough(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	b, _, _, vw := newVaultWriterBackend(t,
		map[string]*store.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		map[string]*vault.Entity{"gmail:msg-1": {ID: "gmail:msg-1", Kind: "gmail"}},
		now,
	)
	withKindsRegistry(b)
	w := NewVaultPropertyWriter(b)
	require.NoError(t, w.SetProperties(context.Background(),
		"wf", "gmail:msg-1",
		map[string]any{
			"is_archived": true,
			"priority":    int64(3),
			"metadata":    map[string]any{"nested": "string"},
		}))

	writes := vw.snapshot()
	assert.Equal(t, true, writes[0].entity.Data["is_archived"])
	assert.Equal(t, int64(3), writes[0].entity.Data["priority"])
	m, ok := writes[0].entity.Data["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", m["nested"],
		"nested map values aren't recursed — pass through verbatim")
}
