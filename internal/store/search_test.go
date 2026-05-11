package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSearchableEntity writes an entity with given id, kind, and data
// payload — used by the search tests to exercise both id and data
// substring matches.
func seedSearchableEntity(t *testing.T, s Store, id, kind string, data map[string]any) {
	t.Helper()
	require.NoError(t, s.SaveEntity(context.Background(), &Entity{
		ID: id,
		Kind: kind,
		Data: data,
		Edges: nil,
	}), "seed %s", id)
}

func TestSearch_MatchesByDataSubstring(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:brass-birmingham", "boardgame",
		map[string]any{"title": "Brass: Birmingham", "year": float64(2018)})
	seedSearchableEntity(t, s, "boardgame:concordia", "boardgame",
		map[string]any{"title": "Concordia", "year": float64(2013)})

	hits, total, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search")
	require.Len(t, hits, 1, "hits: %v", hitIDs(hits))
	assert.Equal(t, "boardgame:brass-birmingham", hits[0].ID)
	assert.Equal(t, "boardgame", hits[0].Kind)
	assert.Equal(t, 1, total)
}

func TestSearch_MatchesByIDSubstring(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	// Data has nothing matching "tolkien", but the id does.
	seedSearchableEntity(t, s, "person:tolkien", "person",
		map[string]any{"display_name": "J. R. R."})

	hits, total, err := s.Search(ctx, "tolkien", "", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search")
	require.Len(t, hits, 1, "hits: %v", hitIDs(hits))
	assert.Equal(t, "person:tolkien", hits[0].ID)
	assert.Equal(t, 1, total)
}

func TestSearch_KindFilter(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:brass-birmingham", "boardgame",
		map[string]any{"title": "Brass: Birmingham"})
	seedSearchableEntity(t, s, "person:wallace", "person",
		map[string]any{"display_name": "Martin Wallace, designer of Brass"})

	all, _, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search (all kinds)")
	assert.Len(t, all, 2, "unfiltered: %v", hitIDs(all))

	bg, total, err := s.Search(ctx, "Brass", "boardgame", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search (kind=boardgame)")
	require.Len(t, bg, 1, "kind=boardgame: %v", hitIDs(bg))
	assert.Equal(t, "boardgame:brass-birmingham", bg[0].ID)
	assert.Equal(t, 1, total, "total with kind filter")

	person, _, err := s.Search(ctx, "Brass", "person", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search (kind=person)")
	require.Len(t, person, 1, "kind=person: %v", hitIDs(person))
	assert.Equal(t, "person:wallace", person[0].ID)

	none, total, err := s.Search(ctx, "Brass", "book", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search (kind=book, none expected)")
	assert.Empty(t, none, "kind=book: %v", hitIDs(none))
	assert.Equal(t, 0, total, "total for non-matching kind filter")
}

func TestSearch_TotalCountIndependentOfLimit(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	// Seven entities all containing "match" in their data — limit=2 must
	// truncate hits while total reports the full 7.
	for i := range 7 {
		seedSearchableEntity(t, s,
			"boardgame:m-"+string(rune('a'+i)), "boardgame",
			map[string]any{"title": "match-" + string(rune('a'+i))})
	}

	hits, total, err := s.Search(ctx, "match", "", 2, 0, ArchivedExclude)
	require.NoError(t, err, "Search")
	assert.Len(t, hits, 2, "hits: want 2 (LIMIT applied)")
	assert.Equal(t, 7, total, "total: want 7 (independent of LIMIT)")
}

func TestSearch_OffsetPagination(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	// Five entities with deterministic ids so ORDER BY id gives a stable
	// sequence the test can index into.
	for i := range 5 {
		seedSearchableEntity(t, s,
			"boardgame:p"+string(rune('1'+i)), "boardgame",
			map[string]any{"title": "page-test"})
	}

	// limit=2 offset=0 → first two
	page1, total, err := s.Search(ctx, "page-test", "", 2, 0, ArchivedExclude)
	require.NoError(t, err, "Search page1")
	assert.Equal(t, 5, total)
	assert.Equal(t, []string{"boardgame:p1", "boardgame:p2"}, hitIDs(page1), "page1")

	// limit=2 offset=2 → middle two
	page2, _, err := s.Search(ctx, "page-test", "", 2, 2, ArchivedExclude)
	require.NoError(t, err, "Search page2")
	assert.Equal(t, []string{"boardgame:p3", "boardgame:p4"}, hitIDs(page2), "page2")

	// limit=2 offset=4 → last single
	page3, _, err := s.Search(ctx, "page-test", "", 2, 4, ArchivedExclude)
	require.NoError(t, err, "Search page3")
	assert.Equal(t, []string{"boardgame:p5"}, hitIDs(page3), "page3")

	// limit=2 offset=10 → nothing (past the end)
	page4, total, err := s.Search(ctx, "page-test", "", 2, 10, ArchivedExclude)
	require.NoError(t, err, "Search page4")
	assert.Empty(t, page4, "page4: %v", hitIDs(page4))
	assert.Equal(t, 5, total, "total at past-end offset")
}

func TestSearch_EmptyQueryMatchesAll(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "a", "boardgame", map[string]any{"x": 1})
	seedSearchableEntity(t, s, "b", "person", map[string]any{"x": 2})

	hits, total, err := s.Search(ctx, "", "", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search empty query")
	assert.Len(t, hits, 2, "hits: want 2 (empty pattern '%%%%' matches all)")
	assert.Equal(t, 2, total)
}

func TestSearch_NoMatchesReturnsEmpty(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "boardgame:brass-birmingham", "boardgame",
		map[string]any{"title": "Brass: Birmingham"})

	hits, total, err := s.Search(ctx, "nothing-matches-this", "", 50, 0, ArchivedExclude)
	require.NoError(t, err, "Search")
	assert.Empty(t, hits, "hits: %v", hitIDs(hits))
	assert.Equal(t, 0, total)
}

func hitIDs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}
