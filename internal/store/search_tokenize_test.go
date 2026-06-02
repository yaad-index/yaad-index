package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSearch_MultiWordSpansPunctuation pins the #391 fix: a multi-word
// query matches a stored title that has punctuation between the words.
// Before tokenize-and-AND, the single contiguous LIKE %query% missed
// "Brass: Birmingham" (colon) and the brass-birmingham slug (hyphen)
// for the query "Brass Birmingham"; a punctuation-free title like "Moon
// Colony Bloodbath" matched. Per-term matching fixes the punctuated case.
func TestSearch_MultiWordSpansPunctuation(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:brass-birmingham", "boardgame",
		map[string]any{"title": "Brass: Birmingham", "year": float64(2018)})
	require.NoError(t, s.ReplaceAliases(ctx, "boardgame:brass-birmingham",
		[]Alias{{Alias: "Brass: Birmingham"}}))

	// The reported failing query now resolves the entity.
	hits, total, err := s.Search(ctx, "Brass Birmingham", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "multi-word query must span the colon; hits=%v", hitIDs(hits))
	assert.Equal(t, "boardgame:brass-birmingham", hits[0].ID)
	assert.Equal(t, 1, total)

	// Case-insensitive (LIKE is ASCII-case-insensitive) and order-
	// independent — both are properties callers expect from name search.
	for _, q := range []string{"brass birmingham", "Birmingham Brass"} {
		h, _, err := s.Search(ctx, q, "", 50, 0, ArchivedExclude, false)
		require.NoError(t, err)
		assert.Len(t, h, 1, "query %q should match", q)
	}

	// Single-term behavior is unchanged.
	h, _, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	assert.Len(t, h, 1, "single-term query unchanged")
}

// TestSearch_TokenizeIsAND pins the AND-of-terms semantics: every term
// must match somewhere (id/data/alias), so a query naming two entities
// matches neither. Guards against the tokenizer degrading to OR.
func TestSearch_TokenizeIsAND(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:brass-birmingham", "boardgame",
		map[string]any{"title": "Brass: Birmingham"})
	seedSearchableEntity(t, s, "boardgame:concordia", "boardgame",
		map[string]any{"title": "Concordia"})

	// "Brass" matches one entity, "Concordia" the other — no single
	// entity has both, so AND yields zero.
	hits, total, err := s.Search(ctx, "Brass Concordia", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	assert.Empty(t, hits, "AND-of-terms: no entity has both terms; hits=%v", hitIDs(hits))
	assert.Equal(t, 0, total)
}

// TestSearch_TokenizeAcrossFields pins that the per-term groups span
// id/data/aliases independently: one term can match the data while
// another matches an alias, and the row still resolves.
func TestSearch_TokenizeAcrossFields(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:wingspan", "boardgame",
		map[string]any{"title": "Wingspan"})
	require.NoError(t, s.ReplaceAliases(ctx, "boardgame:wingspan",
		[]Alias{{Alias: "Flügelschlag"}}))

	// "Wingspan" is in data, "Flügelschlag" is an alias — both terms
	// match the same entity across different fields.
	hits, _, err := s.Search(ctx, "Wingspan Flügelschlag", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "terms may match across data + alias; hits=%v", hitIDs(hits))
	assert.Equal(t, "boardgame:wingspan", hits[0].ID)
}

// TestSearch_KindOnlyEmptyQuery pins the defensive guard: a kind filter
// with an empty query lists the kind (no term clauses produced) rather
// than erroring on a malformed WHERE.
func TestSearch_KindOnlyEmptyQuery(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:catan", "boardgame",
		map[string]any{"title": "Catan"})
	seedSearchableEntity(t, s, "person:teuber", "person",
		map[string]any{"display_name": "Klaus Teuber"})

	hits, total, err := s.Search(ctx, "", "boardgame", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "kind-only listing; hits=%v", hitIDs(hits))
	assert.Equal(t, "boardgame:catan", hits[0].ID)
	assert.Equal(t, 1, total)
}
