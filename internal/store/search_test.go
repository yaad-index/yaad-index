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

	hits, total, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedExclude, false)
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

	hits, total, err := s.Search(ctx, "tolkien", "", 50, 0, ArchivedExclude, false)
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

	all, _, err := s.Search(ctx, "Brass", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err, "Search (all kinds)")
	assert.Len(t, all, 2, "unfiltered: %v", hitIDs(all))

	bg, total, err := s.Search(ctx, "Brass", "boardgame", 50, 0, ArchivedExclude, false)
	require.NoError(t, err, "Search (kind=boardgame)")
	require.Len(t, bg, 1, "kind=boardgame: %v", hitIDs(bg))
	assert.Equal(t, "boardgame:brass-birmingham", bg[0].ID)
	assert.Equal(t, 1, total, "total with kind filter")

	person, _, err := s.Search(ctx, "Brass", "person", 50, 0, ArchivedExclude, false)
	require.NoError(t, err, "Search (kind=person)")
	require.Len(t, person, 1, "kind=person: %v", hitIDs(person))
	assert.Equal(t, "person:wallace", person[0].ID)

	none, total, err := s.Search(ctx, "Brass", "book", 50, 0, ArchivedExclude, false)
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

	hits, total, err := s.Search(ctx, "match", "", 2, 0, ArchivedExclude, false)
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
	page1, total, err := s.Search(ctx, "page-test", "", 2, 0, ArchivedExclude, false)
	require.NoError(t, err, "Search page1")
	assert.Equal(t, 5, total)
	assert.Equal(t, []string{"boardgame:p1", "boardgame:p2"}, hitIDs(page1), "page1")

	// limit=2 offset=2 → middle two
	page2, _, err := s.Search(ctx, "page-test", "", 2, 2, ArchivedExclude, false)
	require.NoError(t, err, "Search page2")
	assert.Equal(t, []string{"boardgame:p3", "boardgame:p4"}, hitIDs(page2), "page2")

	// limit=2 offset=4 → last single
	page3, _, err := s.Search(ctx, "page-test", "", 2, 4, ArchivedExclude, false)
	require.NoError(t, err, "Search page3")
	assert.Equal(t, []string{"boardgame:p5"}, hitIDs(page3), "page3")

	// limit=2 offset=10 → nothing (past the end)
	page4, total, err := s.Search(ctx, "page-test", "", 2, 10, ArchivedExclude, false)
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

	hits, total, err := s.Search(ctx, "", "", 50, 0, ArchivedExclude, false)
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

	hits, total, err := s.Search(ctx, "nothing-matches-this", "", 50, 0, ArchivedExclude, false)
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

// TestSearch_JournalOnlyFilter pins ADR-0025 cut 3 (#222): when
// the journalOnly flag is true, Search restricts to entities
// whose vault data carries `is_journal: true`. The flag lives in
// the data column (mirrored from vault frontmatter `data:`) and
// the SQL filter uses json_extract via SQLite's built-in JSON1.
func TestSearch_JournalOnlyFilter(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	seedSearchableEntity(t, s, "day:2026-11-11", "day", map[string]any{
		"is_journal": true,
	})
	seedSearchableEntity(t, s, "day:2026-11-12", "day", map[string]any{
		"is_journal": false,
	})
	seedSearchableEntity(t, s, "day:2026-11-13", "day", map[string]any{})

	// journalOnly=false → all three day entities returned.
	all, total, err := s.Search(ctx, "", "day", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, all, 3)

	// journalOnly=true → only the flagged entity.
	journal, total, err := s.Search(ctx, "", "day", 50, 0, ArchivedExclude, true)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, journal, 1)
	assert.Equal(t, "day:2026-11-11", journal[0].ID)
}

// TestSearch_JournalOnlyFilter_KindAgnostic pins that the
// is_journal filter applies regardless of kind — operator may
// flag any entity as journal-shaped. Combine with `?kind=day`
// for the canonical use case.
func TestSearch_JournalOnlyFilter_KindAgnostic(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	seedSearchableEntity(t, s, "day:2026-11-11", "day", map[string]any{
		"is_journal": true,
	})
	seedSearchableEntity(t, s, "note:morning-thought", "note", map[string]any{
		"is_journal": true,
	})

	hits, total, err := s.Search(ctx, "", "", 50, 0, ArchivedExclude, true)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Len(t, hits, 2, "is_journal filter ignores kind by default")
}

// TestSearch_MatchesByAlias pins the #3 contract: a query that
// substring-matches an entry in entity_aliases returns the owning
// entity, even when neither id nor data carries the term.
func TestSearch_MatchesByAlias(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "book:piranesi", "book",
		map[string]any{"title": "Piranesi", "year": float64(2020)})

	require.NoError(t, s.ReplaceAliases(ctx, "book:piranesi", []Alias{
		{Alias: "Susanna Clarke's labyrinth book"},
		{Alias: "author: Susanna Clarke", Kind: AliasKindTyped},
	}))

	hits, total, err := s.Search(ctx, "labyrinth", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "alias-only term must match via entity_aliases")
	assert.Equal(t, "book:piranesi", hits[0].ID)
	assert.Equal(t, 1, total)

	// Same on a typed-prefix alias — the label portion is the
	// substring being searched.
	hits, total, err = s.Search(ctx, "Clarke", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "typed-prefix alias label must match")
	assert.Equal(t, "book:piranesi", hits[0].ID)
	assert.Equal(t, 1, total)
}

// TestSearch_AliasMatchDoesNotDuplicate pins the EXISTS-shape
// contract: an entity with multiple matching aliases returns one
// row, not one per alias.
func TestSearch_AliasMatchDoesNotDuplicate(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "book:piranesi", "book",
		map[string]any{"title": "Piranesi"})

	require.NoError(t, s.ReplaceAliases(ctx, "book:piranesi", []Alias{
		{Alias: "Piranesi-alt-1"},
		{Alias: "Piranesi-alt-2"},
		{Alias: "Piranesi-alt-3"},
	}))

	hits, total, err := s.Search(ctx, "Piranesi", "", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "EXISTS subquery must not row-multiply")
	assert.Equal(t, 1, total, "total must count entities, not alias matches")
}

// TestSearch_AliasMatchRespectsKindFilter pins that the alias
// branch is OR'd into the WHERE but the kind filter still applies
// as an AND on the outer query.
func TestSearch_AliasMatchRespectsKindFilter(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	seedSearchableEntity(t, s, "book:piranesi", "book",
		map[string]any{"title": "Piranesi"})
	seedSearchableEntity(t, s, "person:susanna", "person",
		map[string]any{"display_name": "S.C."})

	require.NoError(t, s.ReplaceAliases(ctx, "book:piranesi", []Alias{
		{Alias: "Clarke novel"},
	}))
	require.NoError(t, s.ReplaceAliases(ctx, "person:susanna", []Alias{
		{Alias: "Clarke"},
	}))

	hits, _, err := s.Search(ctx, "Clarke", "book", 50, 0, ArchivedExclude, false)
	require.NoError(t, err)
	require.Len(t, hits, 1, "kind=book filters out the person hit")
	assert.Equal(t, "book:piranesi", hits[0].ID)
}
