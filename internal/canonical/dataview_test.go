package canonical

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// newDataviewDeps wires a real store + vault + write-lock stack for the
// AppendDataviewParagraph tests. The Writer is configured
// WithCanonicalKinds (matching the serve-path wiring in
// cmd/yaad-index/main.go) so synthesizeAliases reads data.name for the
// canonical-shape boardgame kind.
func newDataviewDeps(t *testing.T) (DataviewAppendDeps, store.Store, *vault.Reader) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root, vault.WithCanonicalKinds([]string{"boardgame"}))
	require.NoError(t, err)
	rd, err := vault.NewReader(root)
	require.NoError(t, err)

	deps := DataviewAppendDeps{
		Store:       st,
		VaultReader: rd,
		VaultWriter: w,
		WriteLocks:  writelocks.New(),
		KindReg:     map[string]config.CanonicalKindConfig{"boardgame": {}},
	}
	return deps, st, rd
}

// TestAppendDataviewParagraph_MaterializeCapturesSourceName pins #405:
// when AppendDataviewParagraph materializes a canonical thin-edge for
// the first time, the source-of-slug name lands as data.name, is
// synthesized into the vault frontmatter aliases, AND is mirrored into
// the DB resolver index so it resolves immediately (no reindex wait).
func TestAppendDataviewParagraph_MaterializeCapturesSourceName(t *testing.T) {
	t.Parallel()
	deps, st, rd := newDataviewDeps(t)
	ctx := context.Background()
	const id = "boardgame:moon-colony-bloodbath"

	appended, err := AppendDataviewParagraph(ctx, deps, id,
		map[string]string{"rating": "10"}, "tagged_as", "", "Moon Colony Bloodbath")
	require.NoError(t, err)
	require.True(t, appended)

	// Vault frontmatter: data.name set + alias synthesized.
	ve, err := rd.ReadByID("boardgame", id)
	require.NoError(t, err)
	assert.Equal(t, "Moon Colony Bloodbath", ve.Data["name"], "source name captured as data.name")
	assert.Contains(t, ve.Aliases, "Moon Colony Bloodbath", "vault frontmatter carries the synthesized alias")

	// DB resolver index: the alias resolves to the canonical id, scoped
	// to the kind — the #405 payoff.
	resolved, err := st.ResolveAlias(ctx, "Moon Colony Bloodbath", "boardgame")
	require.NoError(t, err)
	assert.Equal(t, id, resolved, "source name resolves to the canonical id immediately")
}

// TestAppendDataviewParagraph_NoSourceName_NoAlias pins that a nameless
// materialize (auto-resolve / pre-formed-id paths pass sourceName="")
// registers no alias — the entity still materializes, it just has no
// source-name to capture.
func TestAppendDataviewParagraph_NoSourceName_NoAlias(t *testing.T) {
	t.Parallel()
	deps, st, rd := newDataviewDeps(t)
	ctx := context.Background()
	const id = "boardgame:brass-birmingham"

	appended, err := AppendDataviewParagraph(ctx, deps, id,
		map[string]string{"rating": "9"}, "tagged_as", "", "")
	require.NoError(t, err)
	require.True(t, appended)

	ve, err := rd.ReadByID("boardgame", id)
	require.NoError(t, err)
	assert.NotContains(t, ve.Data, "name", "no name captured when sourceName empty")

	aliases, err := st.ListAliasesForEntity(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, aliases, "nameless materialize registers no alias")
}

// TestAppendDataviewParagraph_ExistingEntity_AliasesUntouched pins that
// a second append onto an already-materialized target does NOT rewrite
// its aliases — only a fresh materialize captures the name, so an
// existing entity keeps the aliases it already owns (the gate on
// `materialized` guards against wiping plugin-sourced aliases).
func TestAppendDataviewParagraph_ExistingEntity_AliasesUntouched(t *testing.T) {
	t.Parallel()
	deps, st, _ := newDataviewDeps(t)
	ctx := context.Background()
	const id = "boardgame:moon-colony-bloodbath"

	// First append materializes with the name.
	_, err := AppendDataviewParagraph(ctx, deps, id,
		map[string]string{"rating": "10"}, "tagged_as", "", "Moon Colony Bloodbath")
	require.NoError(t, err)

	// Second append on the now-existing entity passes a different name;
	// it must be ignored (no re-materialize → no alias rewrite).
	_, err = AppendDataviewParagraph(ctx, deps, id,
		map[string]string{"weight": "4.2"}, "tagged_as", "", "A Totally Different Name")
	require.NoError(t, err)

	resolved, err := st.ResolveAlias(ctx, "Moon Colony Bloodbath", "boardgame")
	require.NoError(t, err)
	assert.Equal(t, id, resolved, "original alias survives a second append")

	stray, err := st.ResolveAlias(ctx, "A Totally Different Name", "boardgame")
	require.NoError(t, err)
	assert.Empty(t, stray, "a second append never registers a new name")
}
