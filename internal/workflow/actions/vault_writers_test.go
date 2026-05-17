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
	mu       sync.Mutex
	writes   []vaultWrite
	writeErr error
}

type vaultWrite struct {
	entity  *vault.Entity
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
// holder of the entity's write-lock returns a ConflictError
// the workflow surfaces verbatim.
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
	// Acquire the lock first; the writer's Acquire should
	// then 409.
	release, err := b.WriteLocks.Acquire("pr:1", "test-holder")
	require.NoError(t, err)
	defer release()

	w := NewVaultNoteWriter(b)
	err = w.AppendNote(context.Background(), "wf", "pr:1", "hi")
	require.Error(t, err)
	assert.True(t, writelocks.IsConflict(unwrapToConflict(err)),
		"writelocks.IsConflict on the unwrapped writer error")
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
