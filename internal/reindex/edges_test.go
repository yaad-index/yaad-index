// Tests for the vault-edges-as-source-of-truth contract:
// reindex consumes vault.Entity.Edges, applies operator-config gates,
// auto-materializes canonical-label thin rows, and is idempotent
// across re-runs via delete-then-create per (source, edge_type)
// tuple.

package reindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newGuardedTestEnv mirrors newTestEnv but wires a CanonicalGuard
// from explicit kind / edge-type allowlists so tests can exercise
// the gated reindex path. The guard fires AllowKind on the canonical
// kinds + AllowEdgeType on the edge types listed.
func newGuardedTestEnv(t *testing.T, allowKinds, allowEdges []string) (*Reindexer, store.Store, *vault.Writer, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New")
	t.Cleanup(func() { _ = st.Close() })

	vaultRoot := t.TempDir()
	w, err := vault.NewWriter(vaultRoot)
	require.NoError(t, err, "vault.NewWriter")

	guard := config.NewCanonicalGuard(allowKinds, allowEdges)
	r, err := New(st, vaultRoot, guard, nil)
	require.NoError(t, err, "reindex.New")

	return r, st, w, vaultRoot
}

// TestReindex_VaultEdgesPopulateDB pins the headline contract:
// a vault file's `edges:` block reconstitutes the DB edge graph on
// reindex without any upstream re-fetch.
func TestReindex_VaultEdgesPopulateDB(t *testing.T) {
	t.Parallel()
	r, st, w, _ := newGuardedTestEnv(t,
		[]string{"person", "boardgame"},
		[]string{"is_about", "designed_by"},
	)

	src := newEntity(t, "bgg:age-of-steam", "bgg")
	src.Edges = []vault.Edge{
		{Type: "is_about", To: "boardgame:age-of-steam"},
		{Type: "designed_by", To: "person:john-bohrer"},
		{Type: "designed_by", To: "person:martin-wallace"},
	}
	require.NoError(t, w.Write(src))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, summary.Errors)
	assert.Equal(t, 3, summary.EdgeRowsWritten, "all three vault edges written to DB")

	out, err := st.GetEdgesFor(context.Background(), "bgg:age-of-steam", nil)
	require.NoError(t, err)
	require.Len(t, out, 3)

	// Auto-materialized thin rows for the canonical-label endpoints.
	for _, label := range []string{"boardgame:age-of-steam", "person:john-bohrer", "person:martin-wallace"} {
		got, err := st.GetEntity(context.Background(), label)
		require.NoError(t, err, "thin row for %s", label)
		assert.Empty(t, got.Data, "thin row %s carries no Data", label)
	}
}

// TestReindex_EdgesIdempotentAcrossReruns guarantees the
// delete-then-create per (source, edge_type) tuple — a second
// reindex on an unchanged vault produces the same DB edge set,
// not duplicates.
func TestReindex_EdgesIdempotentAcrossReruns(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newGuardedTestEnv(t,
		[]string{"person"},
		[]string{"designed_by"},
	)

	src := newEntity(t, "bgg:caverna", "bgg")
	src.Edges = []vault.Edge{
		{Type: "designed_by", To: "person:uwe-rosenberg"},
	}
	require.NoError(t, w.Write(src))

	first, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, first.EdgeRowsWritten)

	// Bump mtime so the file re-parses on incremental — exercises
	// the delete-then-create path on a row that already exists.
	bumped := time.Now().Add(2 * time.Second)
	require.NoError(t, touchPath(filepath.Join(vaultRoot, "bgg", "caverna.md"), bumped))

	second, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, second.Errors)
	assert.Equal(t, 1, second.EdgeRowsWritten, "delete-then-create produces same row count, not duplicates")

	out, err := st.GetEdgesFor(context.Background(), "bgg:caverna", nil)
	require.NoError(t, err)
	require.Len(t, out, 1, "exactly one designed_by edge after two reindex runs")
}

// TestReindex_VaultEdgesReplacePriorDBState pins the
// delete-then-create semantic from the operator-fill path: when a
// vault file's edge set shrinks (the plugin re-emitted fewer edges),
// reindex DROPS the prior excess from the DB on the next run.
func TestReindex_VaultEdgesReplacePriorDBState(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newGuardedTestEnv(t,
		[]string{"person"},
		[]string{"designed_by"},
	)

	src := newEntity(t, "bgg:brass", "bgg")
	src.Edges = []vault.Edge{
		{Type: "designed_by", To: "person:martin-wallace"},
		{Type: "designed_by", To: "person:other"},
	}
	require.NoError(t, w.Write(src))

	first, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 2, first.EdgeRowsWritten)

	// Re-write src with a shrunk edge set + bump mtime so reindex
	// re-parses. The "other" edge must drop on the second walk.
	src.Edges = []vault.Edge{{Type: "designed_by", To: "person:martin-wallace"}}
	require.NoError(t, w.Write(src))
	bumped := time.Now().Add(2 * time.Second)
	require.NoError(t, touchPath(filepath.Join(vaultRoot, "bgg", "brass.md"), bumped))

	second, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, second.Errors)
	assert.Equal(t, 1, second.EdgeRowsWritten)

	out, err := st.GetEdgesFor(context.Background(), "bgg:brass", nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "person:martin-wallace", out[0].To, "shrunk fill replaces prior wider fill")
}

// TestReindex_EdgesRespectAllowEdgeType drops a whole bucket whose
// edge_type is not in the operator's `canonical_edge_types:` config
// and bumps the per-edge-type drop counter.
func TestReindex_EdgesRespectAllowEdgeType(t *testing.T) {
	t.Parallel()
	r, st, w, _ := newGuardedTestEnv(t,
		[]string{"person", "boardgame"},
		[]string{"is_about"}, // designed_by deliberately not in allowlist
	)

	src := newEntity(t, "bgg:age-of-steam", "bgg")
	src.Edges = []vault.Edge{
		{Type: "is_about", To: "boardgame:age-of-steam"},
		{Type: "designed_by", To: "person:martin-wallace"},
	}
	require.NoError(t, w.Write(src))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, summary.Errors)
	assert.Equal(t, 1, summary.EdgeRowsWritten, "only the allowed-edge-type bucket lands")

	out, err := st.GetEdgesFor(context.Background(), "bgg:age-of-steam", nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "is_about", out[0].Type)

	// Per yaad-index #31: reindex.Run clears the drop-counter tables
	// at the end of a successful walk so post-reindex drift surfaces
	// as zero. The drop behavior is still verified by the edge count
	// above (1 edge written, designed_by silently dropped) — this
	// table is now the "since-last-reindex" view, not the cumulative
	// counter it used to be.
	dropped, err := st.ListDroppedCanonicalEdges(context.Background())
	require.NoError(t, err)
	assert.Empty(t, dropped,
		"post-#31: dropped_canonical_edges cleared after successful reindex")
}

// TestReindex_EdgesRespectAllowKind drops an edge whose target's
// kind is not in `canonical_kinds:`. source-type endpoints bypass
// the gate (system-reserved).
func TestReindex_EdgesRespectAllowKind(t *testing.T) {
	t.Parallel()
	r, st, w, _ := newGuardedTestEnv(t,
		[]string{"boardgame"}, // person deliberately not in allowlist
		[]string{"is_about", "designed_by", "is_a"},
	)

	src := newEntity(t, "bgg:age-of-steam", "bgg")
	src.Edges = []vault.Edge{
		{Type: "is_about", To: "boardgame:age-of-steam"},
		{Type: "designed_by", To: "person:martin-wallace"}, // dropped: person not in allowlist
		{Type: "is_a", To: "source-type:bgg-record"}, // bypasses the gate
	}
	require.NoError(t, w.Write(src))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, summary.Errors)
	assert.Equal(t, 2, summary.EdgeRowsWritten, "is_about + is_a land; person edge drops")

	out, err := st.GetEdgesFor(context.Background(), "bgg:age-of-steam", nil)
	require.NoError(t, err)
	require.Len(t, out, 2)

	// person:martin-wallace MUST NOT have been auto-materialized — its
	// kind is not in canonical_kinds, so the thin-row materialize
	// step short-circuits before UpsertEntity.
	_, err = st.GetEntity(context.Background(), "person:martin-wallace")
	assert.ErrorIs(t, err, store.ErrNotFound, "person row not materialized when kind not allowed")

	// source-type bypass: the source-type row IS materialized.
	_, err = st.GetEntity(context.Background(), "source-type:bgg-record")
	assert.NoError(t, err, "source-type row materialized via bypass")
}

// TestReindex_AbsentEdgesBlockBackCompat pins the back-compat
// promise: a vault file with no `edges:` block (legacy legacy
// shape) decodes cleanly with Edges == nil and reindex doesn't
// surface a parse error or wipe prior DB edges for the entity.
func TestReindex_AbsentEdgesBlockBackCompat(t *testing.T) {
	t.Parallel()
	r, st, w, _ := newGuardedTestEnv(t,
		[]string{"person"},
		[]string{"designed_by"},
	)

	// Legacy vault file: no Edges populated.
	src := newEntity(t, "bgg:legacy", "bgg")
	require.NoError(t, w.Write(src))

	// Pre-seed both endpoints + a DB edge that legacy ingest would
	// have written. reindex on the absent-edges-block file MUST NOT
	// delete it (silence is not an authoritative drop signal).
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{ID: "bgg:legacy", Kind: "bgg"}))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{ID: "person:legacy-author", Kind: "person"}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "bgg:legacy", To: "person:legacy-author",
	}))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, summary.Errors, "absent edges block decodes cleanly")
	assert.Equal(t, 0, summary.EdgeRowsWritten, "no edges in vault means no edges written")

	// Pre-existing DB edge survives.
	out, err := st.GetEdgesFor(context.Background(), "bgg:legacy", nil)
	require.NoError(t, err)
	require.Len(t, out, 1, "absent edges block does NOT wipe prior DB state")
}

// TestReindex_PrunesStaleDayRefEdge pins #446: removing a `day:`
// frontmatter field and reindexing drops the orphaned day-ref edge.
// EmitDayRefs is add-only, so without the prune the references_day edge
// to the removed day would persist with no live source.
func TestReindex_PrunesStaleDayRefEdge(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newTestEnv(t)

	src := newEntity(t, "bgg:event-2026", "bgg")
	src.Data["meeting"] = "day:2026-06-05"
	require.NoError(t, w.Write(src))

	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	// The day-ref edge exists after the first pass.
	out, err := st.GetEdgesFor(context.Background(), "bgg:event-2026", []string{"references_day"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "day:2026-06-05", out[0].To)

	// Operator removes the day field; bump mtime so reindex re-parses.
	delete(src.Data, "meeting")
	require.NoError(t, w.Write(src))
	require.NoError(t, touchPath(filepath.Join(vaultRoot, "bgg", "event-2026.md"), time.Now().Add(2*time.Second)))

	_, err = r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	out, err = st.GetEdgesFor(context.Background(), "bgg:event-2026", []string{"references_day"})
	require.NoError(t, err)
	assert.Empty(t, out, "removed day: field → day-ref edge pruned on reindex")
}

// TestReindex_DayRefEdgeIdempotentAcrossReruns pins that the prune does
// not regress the steady state: an entity that keeps its `day:` field
// has exactly one day-ref edge after repeated reindex passes — the
// prune deletes then EmitDayRefs re-creates, never zero or duplicate.
func TestReindex_DayRefEdgeIdempotentAcrossReruns(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newTestEnv(t)

	src := newEntity(t, "bgg:standup", "bgg")
	src.Data["occurs"] = "day:2026-06-05"
	require.NoError(t, w.Write(src))

	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	require.NoError(t, touchPath(filepath.Join(vaultRoot, "bgg", "standup.md"), time.Now().Add(2*time.Second)))
	_, err = r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	out, err := st.GetEdgesFor(context.Background(), "bgg:standup", []string{"references_day"})
	require.NoError(t, err)
	require.Len(t, out, 1, "day-ref edge stays at exactly one across reindex reruns")
	assert.Equal(t, "day:2026-06-05", out[0].To)
}

// touchPath sets the mtime of a vault file so an incremental walk
// re-parses it. Mirrors the inline helper used by the existing
// TestReindex_ForwardEdgeReferenceLandsOnSecondPass test.
func touchPath(path string, when time.Time) error {
	return os.Chtimes(path, when, when)
}

// TestReindex_CascadeRederivesSurvivorEdges pins the #447 fix: when a
// vault file for a TARGET entity B disappears, the disappeared-file
// cascade strips A→B (the FK forbids dangling inbound edges). A's own
// file is unchanged, so the walk skips it — without the re-derive step
// A→B would stay gone until `reindex --full`. After the fix the
// incremental pass re-derives A's vault edges, restoring A→B and re-
// materializing B as a thin canonical-label row (the --full steady
// state).
func TestReindex_CascadeRederivesSurvivorEdges(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newGuardedTestEnv(t,
		[]string{"boardgame"},
		[]string{"is_about"},
	)
	ctx := context.Background()

	// A (bgg:test-game-2099) declares an edge A→B (B = boardgame:...).
	a := newEntity(t, "bgg:test-game-2099", "bgg")
	a.Edges = []vault.Edge{{Type: "is_about", To: "boardgame:test-game-2099"}}
	require.NoError(t, w.Write(a))

	// B is its own full vault file so both exist as real rows first.
	b := newEntity(t, "boardgame:test-game-2099", "boardgame")
	require.NoError(t, w.Write(b))

	first, err := r.Run(ctx, Incremental)
	require.NoError(t, err)
	assert.Empty(t, first.Errors)

	edges, err := st.GetEdgesFor(ctx, "bgg:test-game-2099", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "A→B present after first reindex")
	assert.Equal(t, "boardgame:test-game-2099", edges[0].To)

	bRow, err := st.GetEntity(ctx, "boardgame:test-game-2099")
	require.NoError(t, err)
	assert.NotEmpty(t, bRow.Data, "B has full Data from its own vault file")

	// Delete B's vault FILE only; leave A untouched.
	require.NoError(t, os.Remove(filepath.Join(vaultRoot, "boardgame", "test-game-2099.md")))

	second, err := r.Run(ctx, Incremental)
	require.NoError(t, err)
	assert.Empty(t, second.Errors)
	assert.Equal(t, 1, second.EntitiesDeleted, "B's file disappearance cascaded a delete")

	// A→B re-derived despite A being skipped by the walk.
	edges, err = st.GetEdgesFor(ctx, "bgg:test-game-2099", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "A→B re-derived after B's file disappeared (#447)")
	assert.Equal(t, "boardgame:test-game-2099", edges[0].To)

	// B re-materialized as a thin row: exists, but Data is empty.
	bRow, err = st.GetEntity(ctx, "boardgame:test-game-2099")
	require.NoError(t, err, "B re-materialized as a thin canonical-label row")
	assert.Empty(t, bRow.Data, "thin B carries no Data after re-materialize")
}

// TestReindex_CascadeRederiveStableAcrossReruns pins determinism: with
// B's file still gone and A untouched, a 3rd incremental pass keeps
// A→B present exactly once and B thin — no duplicate, no loss.
func TestReindex_CascadeRederiveStableAcrossReruns(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newGuardedTestEnv(t,
		[]string{"boardgame"},
		[]string{"is_about"},
	)
	ctx := context.Background()

	a := newEntity(t, "bgg:test-game-2099", "bgg")
	a.Edges = []vault.Edge{{Type: "is_about", To: "boardgame:test-game-2099"}}
	require.NoError(t, w.Write(a))
	b := newEntity(t, "boardgame:test-game-2099", "boardgame")
	require.NoError(t, w.Write(b))

	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(vaultRoot, "boardgame", "test-game-2099.md")))

	// Second pass: cascade + re-derive.
	_, err = r.Run(ctx, Incremental)
	require.NoError(t, err)

	// Third pass: B's file still gone, A still untouched, B already
	// thin (no full row to cascade) → no delete, edge stays stable.
	third, err := r.Run(ctx, Incremental)
	require.NoError(t, err)
	assert.Empty(t, third.Errors)

	edges, err := st.GetEdgesFor(ctx, "bgg:test-game-2099", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "A→B still present exactly once on the 3rd run")
	assert.Equal(t, "boardgame:test-game-2099", edges[0].To)

	bRow, err := st.GetEntity(ctx, "boardgame:test-game-2099")
	require.NoError(t, err, "B still exists as a thin row")
	assert.Empty(t, bRow.Data)
}

// TestReindex_CascadeDoesNotResurrectDroppedEdge pins the "read
// current vault, not a stale snapshot" half of the fix: if A drops its
// A→B edge in the SAME incremental pass that B's file disappears, the
// re-derive must NOT resurrect A→B — A no longer declares it.
func TestReindex_CascadeDoesNotResurrectDroppedEdge(t *testing.T) {
	t.Parallel()
	r, st, w, vaultRoot := newGuardedTestEnv(t,
		[]string{"boardgame"},
		[]string{"is_about"},
	)
	ctx := context.Background()

	a := newEntity(t, "bgg:test-game-2099", "bgg")
	a.Edges = []vault.Edge{{Type: "is_about", To: "boardgame:test-game-2099"}}
	require.NoError(t, w.Write(a))
	b := newEntity(t, "boardgame:test-game-2099", "boardgame")
	require.NoError(t, w.Write(b))

	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	edges, err := st.GetEdgesFor(ctx, "bgg:test-game-2099", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "A→B present after first reindex")

	// Same pass: rewrite A to DROP the edge (bump mtime so it
	// re-parses) AND delete B's file.
	a.Edges = nil
	require.NoError(t, w.Write(a))
	bumped := time.Now().Add(2 * time.Second)
	require.NoError(t, touchPath(filepath.Join(vaultRoot, "bgg", "test-game-2099.md"), bumped))
	require.NoError(t, os.Remove(filepath.Join(vaultRoot, "boardgame", "test-game-2099.md")))

	second, err := r.Run(ctx, Incremental)
	require.NoError(t, err)
	assert.Empty(t, second.Errors)

	// A no longer declares A→B; the re-derive reads A's CURRENT vault
	// and must not resurrect the stripped edge.
	edges, err = st.GetEdgesFor(ctx, "bgg:test-game-2099", nil)
	require.NoError(t, err)
	assert.Empty(t, edges, "dropped A→B not resurrected (current vault, not stale snapshot)")
}
