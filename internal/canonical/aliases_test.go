package canonical

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestMirrorAliases_DerivesTypedAndBareKinds pins #445: MirrorAliases
// must derive each mirrored alias's Kind from the operator's
// canonical_edge_types registry — typed when the alias has the
// `<prefix>: <label>` shape AND the prefix is registered, bare
// otherwise — matching the reindex + ingest paths. Before the fix
// every mirrored alias was written bare.
func TestMirrorAliases_DerivesTypedAndBareKinds(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	const id = "task:plan-the-week"
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{ID: id, Kind: "task"}))

	// Plugin-emitted aliases carry one typed (registered prefix) and
	// one bare alias; MergedAliasesFor passes them through verbatim.
	pluginAliases := []string{"references_day: 2026-06-05", "Plan The Week"}
	canonicalEdgeTypes := []string{"references_day"}

	require.NoError(t, MirrorAliases(ctx, st, id, "task", nil, pluginAliases, nil, canonicalEdgeTypes))

	got, err := st.ListAliasesForEntity(ctx, id)
	require.NoError(t, err)

	byAlias := make(map[string]string, len(got))
	for _, a := range got {
		byAlias[a.Alias] = a.Kind
	}
	assert.Equal(t, store.AliasKindTyped, byAlias["references_day: 2026-06-05"],
		"registered prefix → typed")
	assert.Equal(t, store.AliasKindBare, byAlias["Plan The Week"],
		"bare alias → bare")
}
