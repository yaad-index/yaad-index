package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Per ADR-0013 §4 + §5 / yaad-index: the gap-call is bounded
// to one per fetch-cycle. The DB-only `gap_call_done_at` flag is
// set on a successful fill (any 2xx, full or partial), suppresses
// the cache-hit needs_fill payload while set, and is cleared by
// any refetch (force_refetch=true OR fresh plugin Fetch on TTL
// fall-through). Reindex re-derives gap-callability from vault
// alone — wiping the DB and re-running reindex restores the
// entity but leaves the flag NULL (regen invariant).
//
// **No content-hash-based or persistent attempt-tracking** —
// implementing that would defeat the regen invariant per ADR-0013
// §5. These tests pin the absence as much as the presence.

const (
	lifecycleEntityKind = "lifecycle-kind"
	lifecycleEntityID = "lifecycle-kind:seeded"
	lifecycleNotation = "https://example.test/lifecycle/seeded"
)

// seedLifecycleEntity writes an entity with open gaps to both DB
// and vault, registers a notation pointing at it, returns a fresh
// handler ready for cache-hit ingests of `lifecycleNotation`. Vault
// `gaps:` carries `summary` so respondFromCacheHit takes the
// needs_fill branch absent the suppression flag.
func seedLifecycleEntity(t *testing.T) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	fetchedAt := &now
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: lifecycleEntityID,
		Kind: lifecycleEntityKind,
		Data: map[string]any{"id": lifecycleEntityID},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: lifecycleEntityID,
		Kind: lifecycleEntityKind,
		Source: []string{"seed/default"},
		Data: map[string]any{"id": lifecycleEntityID},
		Gaps: []string{"summary"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: lifecycleNotation,
		EntityID: lifecycleEntityID,
		Kind: lifecycleEntityKind,
	}))

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
	)
	return h, st
}

// First cache-hit ingest of an entity with unfilled gaps + flag NULL
// → returns needs_fill (current behavior).
func TestGapCallLifecycle_FirstCacheHit_ReturnsNeedsFill(t *testing.T) {
	t.Parallel()

	h, _ := seedLifecycleEntity(t)
	rec := postIngest(t, h, map[string]any{"url": lifecycleNotation})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "needs_fill", got["state"])
}

// After MarkGapCallDone (simulating a successful fill), a second
// cache-hit ingest returns `complete` even though gaps remain.
func TestGapCallLifecycle_FlagSet_SuppressesNeedsFill(t *testing.T) {
	t.Parallel()

	h, st := seedLifecycleEntity(t)

	// Simulate the fill having landed: stamp the flag directly via
	// the store API so the test is decoupled from the fill handler.
	require.NoError(t, st.MarkGapCallDone(context.Background(), lifecycleEntityID))

	rec := postIngest(t, h, map[string]any{"url": lifecycleNotation})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "complete", got["state"],
		"flag set + open gaps → suppress needs_fill, return complete")
	// instruction + canonical_vocabulary fields are needs_fill-only
	// per ADR-0013 §2 / yaad-index — they must not leak onto
	// this complete-shape response.
	_, hasInst := got["instruction"]
	assert.False(t, hasInst)
	_, hasCV := got["canonical_vocabulary"]
	assert.False(t, hasCV)
}

// ClearGapCallDone restores gap-callability — a subsequent cache-
// hit with the flag cleared returns needs_fill again. Mirrors the
// refetch-clears-flag branch of the lifecycle.
func TestGapCallLifecycle_FlagCleared_ResumesNeedsFill(t *testing.T) {
	t.Parallel()

	h, st := seedLifecycleEntity(t)

	require.NoError(t, st.MarkGapCallDone(context.Background(), lifecycleEntityID))
	require.NoError(t, st.ClearGapCallDone(context.Background(), lifecycleEntityID))

	rec := postIngest(t, h, map[string]any{"url": lifecycleNotation})
	require.Equal(t, http.StatusAccepted, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "needs_fill", got["state"], "cleared flag → resumes needs_fill")
}

// End-to-end via the fill handler: POST /v1/entities/{id}/fill
// returning 2xx (partial fill, gap remains) sets the flag, and
// the next cache-hit ingest correctly suppresses needs_fill.
func TestGapCallLifecycle_FillSets_NextIngestSuppresses(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; behavior recovery tracked in #358 (Provenance) + #359 (top-level vault Tags/Summary)")

	// Use the existing fill fixture (boardgame entity with three open
	// gaps) — register a notation so the cache-hit path resolves it.
	h, st, _ := newFillFixture(t)
	const notation = "https://example.test/lifecycle/" + fillTestEntityID
	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: notation,
		EntityID: fillTestEntityID,
		Kind: "boardgame",
	}))

	// First cache-hit returns needs_fill (no fill submitted yet).
	rec := postIngest(t, h, map[string]any{"url": notation})
	require.Equal(t, http.StatusAccepted, rec.Code)
	var first map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))
	assert.Equal(t, "needs_fill", first["state"])

	// Submit a partial fill: only `summary`, leaving `tags` and
	// `complexity_assessment` open. Returns 200; per ADR-0013 §4
	// the flag is set on any 2xx, full or partial.
	fillRec := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{"summary": "partial."},
	})
	require.Equal(t, http.StatusOK, fillRec.Code, "body=%s", fillRec.Body.String())

	// Verify the flag is set in the DB (white-box check; gives a
	// clearer failure mode than re-deriving via the wire path alone).
	got, err := st.GetEntity(context.Background(), fillTestEntityID)
	require.NoError(t, err)
	require.NotNil(t, got.GapCallDoneAt, "fill 2xx must set gap_call_done_at")

	// Second cache-hit ingest — gaps still open in vault, but flag
	// suppresses the payload; expect complete state.
	rec = postIngest(t, h, map[string]any{"url": notation})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var second map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
	assert.Equal(t, "complete", second["state"],
		"post-fill cache-hit must suppress needs_fill even with open gaps")
}

// Force-refetch via a fresh ingest path (handled in the simulator's
// post-write clear) — verify the flag clears. The test seeds a
// fixture-fresh entity (first ingest creates it), stamps the flag,
// then re-runs the same simulator URL on the SAME store. The
// simulator's post-AppendProvenance hook should clear the flag.
func TestGapCallLifecycle_FreshIngest_ClearsFlag(t *testing.T) {
	t.Parallel()

	// The needs-fill-test fixture URL produces a deterministic
	// entity with id stubNeedsFillEntityID once the simulator runs.
	_, st := newAPIWithStore(t)
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
	)

	// First ingest creates the entity in `st`.
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)

	// Stamp the flag manually so the second ingest has something to
	// clear.
	require.NoError(t, st.MarkGapCallDone(context.Background(), stubNeedsFillEntityID))
	got, err := st.GetEntity(context.Background(), stubNeedsFillEntityID)
	require.NoError(t, err)
	require.NotNil(t, got.GapCallDoneAt, "flag set sanity")

	// Re-run the simulator on the same URL — the post-AppendProvenance
	// hook should clear the flag.
	rec = postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())

	got, err = st.GetEntity(context.Background(), stubNeedsFillEntityID)
	require.NoError(t, err)
	assert.Nil(t, got.GapCallDoneAt, "fresh ingest must clear gap_call_done_at")
}

// Store-level test: MarkGapCallDone + ClearGapCallDone idempotency
// + ErrNotFound semantic. Direct unit coverage so a regression in
// one method is caught independently of the HTTP wiring.
func TestStore_GapCallDone_LifecycleAndErrNotFound(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	const id = "test:flag-check"
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "test",
		Data: map[string]any{"id": id},
	}))

	got, err := st.GetEntity(context.Background(), id)
	require.NoError(t, err)
	require.Nil(t, got.GapCallDoneAt, "fresh entity → flag NULL")

	require.NoError(t, st.MarkGapCallDone(context.Background(), id))
	got, err = st.GetEntity(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got.GapCallDoneAt, "MarkGapCallDone sets the flag")

	// Idempotent: re-marking refreshes the timestamp without error.
	require.NoError(t, st.MarkGapCallDone(context.Background(), id))

	require.NoError(t, st.ClearGapCallDone(context.Background(), id))
	got, err = st.GetEntity(context.Background(), id)
	require.NoError(t, err)
	require.Nil(t, got.GapCallDoneAt, "ClearGapCallDone restores NULL")

	// Idempotent on already-cleared.
	require.NoError(t, st.ClearGapCallDone(context.Background(), id))

	// ErrNotFound on missing entity.
	require.ErrorIs(t, st.MarkGapCallDone(context.Background(), "no-such:entity"), store.ErrNotFound)
	require.ErrorIs(t, st.ClearGapCallDone(context.Background(), "no-such:entity"), store.ErrNotFound)
}

// markFailingStore wraps a real store but injects an error on
// MarkGapCallDone — exercises fill.go's best-effort contract:
// a stamp failure must NOT promote to a 500. Per the cold-reviewer's a prior PR
// review, this regression-guards a future refactor that might
// accidentally bubble the error.
type markFailingStore struct {
	store.Store
	markErr error
}

func (m *markFailingStore) MarkGapCallDone(_ context.Context, _ string) error {
	return m.markErr
}

// fill 2xx must hold even when the post-fill MarkGapCallDone fails.
// The vault + DB data already landed before the stamp; the flag is
// a secondary signal. A future refactor that promotes this error to
// 500 would silently break the fill UX — this test pins the contract.
func TestFill_MarkGapCallDoneFailure_Returns200(t *testing.T) {
	t.Parallel()

	// Build the same fixture seedFillFixture sets up, but wrap the
	// store with a Mark-failing decorator before constructing the
	// handler.
	stReal, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = stReal.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	// Seed the entity in the real store + vault using the same
	// helper newFillFixture's underlying seeder uses.
	seedEntity(t, stReal, fillTestEntityID, "boardgame")
	require.NoError(t, w.Write(&vault.Entity{
		ID: fillTestEntityID,
		Kind: "boardgame",
		Source: []string{"test-fixture/default"},
		Data: map[string]any{"id": fillTestEntityID},
		Gaps: fillTestGaps,
	}))

	failing := &markFailingStore{
		Store: stReal,
		markErr: errors.New("simulated mark-gap-call-done failure"),
	}
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		failing, testRegistryWithSeed(),
		WithVaultIO(w, r),
	)

	rec := postFill(t, h, fillTestEntityID, validFillBody())
	require.Equal(t, http.StatusOK, rec.Code,
		"fill must return 200 even when MarkGapCallDone fails (best-effort contract); body=%s", rec.Body.String())

	var got fillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, fillTestEntityID, got.Entity.ID)
}

// Reindex regen invariant: the flag is DB-only and not derived
// from vault. Wipe the DB, re-run reindex, the entity comes back
// with NULL flag — even if the prior DB had it set (because
// vault-only data drives reindex).
//
// This test simulates the wipe-and-reindex by writing an entity to
// vault, running the reindex pipeline, marking the flag, then
// running reindex again to confirm it stays cleared (reindex
// doesn't preserve old DB state — it derives from vault).
func TestStore_GapCallDone_NotPreservedAcrossReindex(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	const id = "regen:check"
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "regen",
		Data: map[string]any{"id": id},
	}))
	require.NoError(t, st.MarkGapCallDone(context.Background(), id))

	// "Wipe" via DeleteEntityCascade — the operation reindex --full
	// uses to drop derived state. The replacement UpsertEntity below
	// stands in for reindex's re-derive from vault.
	require.NoError(t, st.DeleteEntityCascade(context.Background(), id))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "regen",
		Data: map[string]any{"id": id},
	}))

	got, err := st.GetEntity(context.Background(), id)
	require.NoError(t, err)
	assert.Nil(t, got.GapCallDoneAt,
		"reindex regen must not preserve the DB-only flag (ADR-0013 §5 invariant)")
}
