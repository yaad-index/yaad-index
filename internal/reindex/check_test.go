// Tests for reindex Check (#455) — the read-only dry-run that reports
// vault↔store divergences without mutating the store. One test per
// divergence class plus a clean-vault case and a no-mutation guarantee.
//
// Slugs are fictional throughout (rule-9): no real brand/product names.

package reindex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newCheckEnv builds a Reindexer wired with the operator's
// canonicalEdgeTypes (via NewWithOptions) so the alias-mismatch class
// derives typed aliases the way a daemon reindex would. Mirrors
// newTestEnv otherwise (nil guard, in-memory store, temp vault).
func newCheckEnv(t *testing.T, canonicalEdgeTypes []string) (*Reindexer, store.Store, *vault.Writer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New")
	t.Cleanup(func() { _ = st.Close() })

	vaultRoot := t.TempDir()
	w, err := vault.NewWriter(vaultRoot)
	require.NoError(t, err, "vault.NewWriter")

	r, err := NewWithOptions(st, vaultRoot, nil, nil, canonicalEdgeTypes)
	require.NoError(t, err, "reindex.NewWithOptions")

	return r, st, w
}

func TestCheck_CleanVaultIsAllZeros(t *testing.T) {
	t.Parallel()
	r, _, w := newCheckEnv(t, []string{"author"})

	e := newEntity(t, "person:designer-a", "person")
	e.Data["day_started"] = "day:2099-01-02"
	e.Aliases = []string{"author: Designer A"}
	require.NoError(t, w.Write(e))

	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	report, err := r.Check(context.Background())
	require.NoError(t, err)
	assert.Empty(t, report.Errors)
	assert.Equal(t, 1, report.Scanned)
	assert.Zero(t, report.StaleDayRefEdges)
	assert.Zero(t, report.CascadeStrippedEdges)
	assert.Zero(t, report.AliasMismatches)
	assert.Equal(t, 0, report.Total())
}

func TestCheck_StaleDayRefEdge(t *testing.T) {
	t.Parallel()
	r, _, w := newCheckEnv(t, nil)

	// Seed an entity carrying a day-reference; reindex writes the
	// day-targeting edge into the store.
	e := newEntity(t, "task:plan-the-week", "task")
	e.Data["due"] = "day:2099-03-04"
	require.NoError(t, w.Write(e))
	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	// Remove the day field from the vault file WITHOUT reindexing, so
	// the store keeps a day edge the current frontmatter no longer
	// declares (#446).
	delete(e.Data, "due")
	require.NoError(t, w.Write(e))

	report, err := r.Check(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, report.StaleDayRefEdges)
	assert.Zero(t, report.CascadeStrippedEdges)
	assert.Zero(t, report.AliasMismatches)
	assert.Equal(t, 1, report.Total())
}

func TestCheck_CascadeStrippedEdge(t *testing.T) {
	t.Parallel()
	r, st, w := newCheckEnv(t, nil)

	// Vault declares an edge; reindex writes it to the store.
	src := newEntity(t, "task:ship-feature", "task")
	src.Edges = []vault.Edge{{Type: "blocks", To: "task:write-spec"}}
	require.NoError(t, w.Write(src))
	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	// Strip the store edge out-of-band (simulating the #447 cascade
	// that removed it) while the vault still declares it.
	_, err = st.DeleteEdgesByTypeFrom(context.Background(), "task:ship-feature", "blocks")
	require.NoError(t, err)

	report, err := r.Check(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, report.CascadeStrippedEdges, 1)
	assert.Zero(t, report.StaleDayRefEdges)
	assert.Zero(t, report.AliasMismatches)
	assert.Positive(t, report.Total())
}

func TestCheck_AliasMismatch(t *testing.T) {
	t.Parallel()
	r, st, w := newCheckEnv(t, []string{"author"})

	// Vault frontmatter alias `author: Designer A` derives as TYPED
	// because `author` is a registered canonical edge type.
	e := newEntity(t, "person:designer-b", "person")
	e.Aliases = []string{"author: Designer A"}
	require.NoError(t, w.Write(e))
	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	// Seed the store with the SAME alias but the WRONG kind (bare) —
	// the typed/bare mismatch #445 fixed.
	require.NoError(t, st.ReplaceAliases(context.Background(), e.ID, []store.Alias{
		{Alias: "author: Designer A", EntityID: e.ID, Kind: store.AliasKindBare},
	}))

	report, err := r.Check(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, report.AliasMismatches)
	assert.Zero(t, report.StaleDayRefEdges)
	assert.Zero(t, report.CascadeStrippedEdges)
	assert.Equal(t, 1, report.Total())
}

// TestCheck_DoesNotMutate pins the read-only guarantee: edge + alias
// counts are identical before and after a Check over a vault seeded
// with all three divergence classes.
func TestCheck_DoesNotMutate(t *testing.T) {
	t.Parallel()
	r, st, w := newCheckEnv(t, []string{"author"})
	ctx := context.Background()

	e := newEntity(t, "person:designer-c", "person")
	e.Data["seen_on"] = "day:2099-05-06"
	e.Aliases = []string{"author: Designer A"}
	e.Edges = []vault.Edge{{Type: "blocks", To: "task:some-task"}}
	require.NoError(t, w.Write(e))
	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	// Introduce divergences without reindexing.
	delete(e.Data, "seen_on")
	require.NoError(t, w.Write(e))
	_, err = st.DeleteEdgesByTypeFrom(ctx, e.ID, "blocks")
	require.NoError(t, err)
	require.NoError(t, st.ReplaceAliases(ctx, e.ID, []store.Alias{
		{Alias: "author: Designer A", EntityID: e.ID, Kind: store.AliasKindBare},
	}))

	edgesBefore, err := st.GetEdgesFor(ctx, e.ID, nil)
	require.NoError(t, err)
	aliasesBefore, err := st.ListAliasesForEntity(ctx, e.ID)
	require.NoError(t, err)

	report, err := r.Check(ctx)
	require.NoError(t, err)
	assert.Positive(t, report.Total(), "all three classes diverge")

	edgesAfter, err := st.GetEdgesFor(ctx, e.ID, nil)
	require.NoError(t, err)
	aliasesAfter, err := st.ListAliasesForEntity(ctx, e.ID)
	require.NoError(t, err)

	assert.Equal(t, edgesBefore, edgesAfter, "Check must not mutate edges")
	assert.Equal(t, aliasesBefore, aliasesAfter, "Check must not mutate aliases")
}
