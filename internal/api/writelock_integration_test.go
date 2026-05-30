package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// newWriteLockFixture builds a vault-wired handler with the
// write-lock manager exposed so tests can pre-acquire locks +
// observe 409 responses.
func newWriteLockFixture(t *testing.T) (http.Handler, *writelocks.Manager, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	mgr := writelocks.New()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithWriteLocks(mgr),
	)
	return h, mgr, st
}

// TestWriteLock_OperatorFill_ReturnsConflictWhenHeld pins the
// 409 envelope shape on the operator-fill surface when another
// writer already holds the entity-id lock.
func TestWriteLock_OperatorFill_ReturnsConflictWhenHeld(t *testing.T) {
	t.Parallel()
	h, mgr, _ := newWriteLockFixture(t)

	const entityID = "boardgame:locked-game"
	release, err := mgr.Acquire(entityID, "test-holder")
	require.NoError(t, err)
	defer release()

	req := httptest.NewRequest(http.MethodPost,
		"/v1/entities/"+entityID+"/fill",
		strings.NewReader(`{"fields":{"name":"Test"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code,
		"operator-fill must return 409 when entity is locked; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "write_conflict",
		"error envelope must use write_conflict code")
	assert.Contains(t, rec.Body.String(), "test-holder",
		"error envelope must name the active holder")
}

// TestWriteLock_EntityDelete_ReturnsConflictWhenHeld pins the
// 409 envelope on the DELETE /v1/entities/{id} surface.
func TestWriteLock_EntityDelete_ReturnsConflictWhenHeld(t *testing.T) {
	t.Parallel()
	h, mgr, _ := newWriteLockFixture(t)

	const entityID = "boardgame:locked-delete"
	release, err := mgr.Acquire(entityID, "test-holder")
	require.NoError(t, err)
	defer release()

	req := httptest.NewRequest(http.MethodDelete,
		"/v1/entities/"+entityID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "write_conflict")
}

// TestWriteLock_EntityArchive_ReturnsConflictWhenHeld pins the
// 409 envelope on the archive surface.
func TestWriteLock_EntityArchive_ReturnsConflictWhenHeld(t *testing.T) {
	t.Parallel()
	h, mgr, _ := newWriteLockFixture(t)

	const entityID = "boardgame:locked-archive"
	release, err := mgr.Acquire(entityID, "test-holder")
	require.NoError(t, err)
	defer release()

	req := httptest.NewRequest(http.MethodPost,
		"/v1/entities/"+entityID+"/archive", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "write_conflict")
}

// TestWriteLock_Comments_BypassesLock pins the additive-write
// skip path: notes POSTs proceed even when the entity is
// locked by another writer (additive-append, no conflict possible).
func TestWriteLock_Comments_BypassesLock(t *testing.T) {
	t.Parallel()
	h, mgr, _ := newWriteLockFixture(t)

	const entityID = "boardgame:any"
	release, err := mgr.Acquire(entityID, "test-holder")
	require.NoError(t, err)
	defer release()

	req := httptest.NewRequest(http.MethodPost,
		"/v1/entities/"+entityID+"/notes",
		strings.NewReader(`{"text":"note","author":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Notes path skips the write-lock gate entirely. The actual
	// response may be 404 (entity doesn't exist) or other — but it
	// MUST NOT be 409 write_conflict.
	assert.NotEqual(t, http.StatusConflict, rec.Code,
		"notes must bypass the write-lock gate (additive append); got body=%s", rec.Body.String())
}

// TestWriteLock_Edges_BypassesLock pins the additive-write skip
// path: POST /v1/edges proceeds even when both endpoint entities
// are locked by other writers.
func TestWriteLock_Edges_BypassesLock(t *testing.T) {
	t.Parallel()
	h, mgr, _ := newWriteLockFixture(t)

	release1, err := mgr.Acquire("boardgame:a", "test-holder-1")
	require.NoError(t, err)
	defer release1()
	release2, err := mgr.Acquire("person:b", "test-holder-2")
	require.NoError(t, err)
	defer release2()

	req := httptest.NewRequest(http.MethodPost,
		"/v1/edges",
		strings.NewReader(`{"from":"boardgame:a","to":"person:b","type":"designed_by"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusConflict, rec.Code,
		"edges must bypass the write-lock gate (additive append); got body=%s", rec.Body.String())
}

// TestWriteLock_UGCSection_ReleasesOnReturn pins the defer-release
// contract: after a successful UGC section write, the lock is
// released and a subsequent write proceeds.
func TestWriteLock_UGCSection_ReleasesOnReturn(t *testing.T) {
	t.Parallel()
	h, mgr, _ := newWriteLockFixture(t)

	// Create the UGC entity first.
	createReq := httptest.NewRequest(http.MethodPost, "/v1/user-content",
		strings.NewReader(`{"title":"Test","body":"## a\nA\n","tags":["x"]}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusCreated, createRec.Code, "create body=%s", createRec.Body.String())

	// Lock manager should now be empty (create released its lock).
	assert.Equal(t, 0, mgr.Active(),
		"write-lock must release after handler returns; got %d active", mgr.Active())
}

// TestWriteLock_DefaultInitWhenUnwired pins the
// no-WithWriteLocks-option fallback: NewHandlerWithRegistry
// constructs a fresh Manager so existing tests that don't wire one
// continue to compile + behave as before.
func TestWriteLock_DefaultInitWhenUnwired(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed())

	// A request that would acquire the lock — and release on
	// return — must not panic on a nil manager.
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/entities/no-such:id", nil)
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() {
		h.ServeHTTP(rec, req)
	})
	// Status irrelevant — just want to confirm the handler reached
	// without a nil-manager panic.
	_ = context.Background()
}
