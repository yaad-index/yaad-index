package reindex

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// TestReindex_NotationsRederivedFromVault — the canonical a prior PR
// contract: a vault file's `notations:` frontmatter list reconstitutes
// the entity_notations DB rows on reindex.
func TestReindex_NotationsRederivedFromVault(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	want := &vault.Entity{
		ID: "wikipedia:tehran",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Tehran"},
		Notations: []string{
			"https://en.wikipedia.org/wiki/Tehran",
			"wikipedia: Tehran",
			"https://en.m.wikipedia.org/wiki/Tehran",
		},
	}
	require.NoError(t, w.Write(want))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Parsed, "want one parsed file")

	for _, n := range want.Notations {
		got, err := st.GetNotation(context.Background(), n)
		require.NoError(t, err, "notation %q must be in DB after reindex", n)
		assert.Equal(t, want.ID, got.EntityID)
	}
}

// TestReindex_NotationsDropsOrphansFromDB — vault wins: a DB
// notation row that ISN'T present in the vault frontmatter is
// dropped on reindex (DELETE-then-INSERT shape).
func TestReindex_NotationsDropsOrphansFromDB(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	require.NoError(t, w.Write(&vault.Entity{
		ID: "wikipedia:tehran",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Tehran"},
		// Vault carries only ONE notation form.
		Notations: []string{"https://en.wikipedia.org/wiki/Tehran"},
	}))

	// Pre-seed the entity row + an orphan notation that the vault
	// frontmatter doesn't carry.
	ctx := context.Background()
	require.NoError(t, st.UpsertEntity(ctx, &store.Entity{
		ID: "wikipedia:tehran",
		Kind: "wikipedia-article",
		Data: map[string]any{"title": "Tehran"},
	}))
	require.NoError(t, st.UpsertNotation(ctx, store.Notation{
		Notation: "wikipedia: Tehran (orphan)",
		EntityID: "wikipedia:tehran",
		Kind: store.NotationKindShorthand,
	}))

	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	// The vault-listed notation survives; the orphan is gone.
	survivor, err := st.GetNotation(ctx, "https://en.wikipedia.org/wiki/Tehran")
	require.NoError(t, err)
	assert.Equal(t, "wikipedia:tehran", survivor.EntityID)

	_, err = st.GetNotation(ctx, "wikipedia: Tehran (orphan)")
	assert.True(t, errors.Is(err, store.ErrNotFound),
		"orphan DB row not in vault frontmatter must be dropped on reindex")
}

// TestReindex_EmptyNotationsClearsDBRows — a vault file with no
// `notations:` frontmatter clears any DB notation rows for that
// entity. Mirrors the ReplaceProvenance "empty entries clears"
// shape from a prior PR.
func TestReindex_EmptyNotationsClearsDBRows(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	require.NoError(t, w.Write(&vault.Entity{
		ID: "wikipedia:tehran",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Tehran"},
		// No Notations.
	}))

	ctx := context.Background()
	require.NoError(t, st.UpsertEntity(ctx, &store.Entity{
		ID: "wikipedia:tehran",
		Kind: "wikipedia-article",
		Data: map[string]any{"title": "Tehran"},
	}))
	require.NoError(t, st.UpsertNotation(ctx, store.Notation{
		Notation: "https://en.wikipedia.org/wiki/Tehran",
		EntityID: "wikipedia:tehran",
		Kind: store.NotationKindURL,
	}))

	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	_, err = st.GetNotation(ctx, "https://en.wikipedia.org/wiki/Tehran")
	assert.True(t, errors.Is(err, store.ErrNotFound),
		"empty vault notations must clear all DB rows for the entity")
}
