package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

func searchRequest(target string) (*http.Request, *httptest.ResponseRecorder) {
	return httptest.NewRequest(http.MethodGet, target, nil), httptest.NewRecorder()
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) searchResponse {
	t.Helper()
	var got searchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode search response")
	return got
}

func Test_Search_HappyPath(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	req, rec := searchRequest("/v1/search?q=Brass")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	got := decodeSearch(t, rec)
	assert.True(t, got.OK)
	assert.Equal(t, 1, got.Total, "one matching seeded entity")
	assert.Equal(t, searchDefaultLimit, got.Limit)
	assert.Equal(t, 0, got.Offset)
	require.Len(t, got.Results, 1)
	first := got.Results[0]
	assert.Equal(t, "boardgame:brass-birmingham", first.ID)
	assert.Equal(t, "boardgame", first.Kind)
}

func Test_Search_KindFilterMatchesBoardgame(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	req, rec := searchRequest("/v1/search?q=Brass&kind=boardgame")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeSearch(t, rec)
	assert.Len(t, got.Results, 1, "kind matches")
}

func Test_Search_KindFilterExcludesPerson(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	req, rec := searchRequest("/v1/search?q=Brass&kind=person")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeSearch(t, rec)
	assert.Empty(t, got.Results, "kind=person filter excludes the seeded boardgame")
}

// Test_Search_TotalCountIndependentOfLimit seeds enough entities that a
// LIMIT clause has to truncate, then asserts total reports the full
// count anyway. The closure-invariant test (below) and the happy path
// don't exercise this distinction; this one does.
func Test_Search_TotalCountIndependentOfLimit(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	for i := range 5 {
		seedEntity(t, st, "boardgame:tc-"+string(rune('a'+i)), "boardgame")
	}

	req, rec := searchRequest("/v1/search?q=tc-&limit=2")
	h.ServeHTTP(rec, req)

	got := decodeSearch(t, rec)
	assert.Len(t, got.Results, 2, "LIMIT applied")
	assert.Equal(t, 5, got.Total, "total independent of LIMIT")
}

// Test_Search_OffsetSkipsResults seeds three entities and confirms the
// offset advances the result window correctly.
func Test_Search_OffsetSkipsResults(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	for i := range 3 {
		seedEntity(t, st, "boardgame:off-"+string(rune('1'+i)), "boardgame")
	}

	req, rec := searchRequest("/v1/search?q=off-&limit=10&offset=1")
	h.ServeHTTP(rec, req)

	got := decodeSearch(t, rec)
	require.Len(t, got.Results, 2, "results with offset=1")
	assert.Equal(t, 3, got.Total, "total independent of offset")
	// Order is id ASC; offset=1 skips boardgame:off-1.
	assert.Equal(t, "boardgame:off-2", got.Results[0].ID, "results[0] after offset=1")
}

// Test_Search_UnknownKindReturnsEmpty pins the #110 contract: the
// kind filter is a discovery surface, not an operator-config gate.
// A kind the DB has no rows for returns 200 + empty results (same
// shape as any other no-hit query), NOT a 400 — pre-#110 this path
// rejected the request and blocked legitimate source-shape /
// plugin-emitted-not-enabled kinds from being queryable.
func Test_Search_UnknownKindReturnsEmpty(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&kind=alien")
	newAPI(t).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeSearch(t, rec)
	assert.True(t, got.OK)
	assert.Empty(t, got.Results, "kind=alien matches no DB rows")
	assert.Equal(t, 0, got.Total)
}

func Test_Search_MissingQAndKind(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search")
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "q or kind is required")
}

func Test_Search_QWhitespaceOnlyAndNoKind(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=%20%20")
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "q or kind is required")
}

func Test_Search_LimitNotParseable(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&limit=abc")
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "limit")
}

func Test_Search_LimitOverMax(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&limit=200")
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "limit")
}

func Test_Search_LimitUnderMin(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&limit=0")
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "limit")
}

func Test_Search_OffsetNegative(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&offset=-1")
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "offset")
}

func Test_Search_LimitAndOffsetEchoed(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&limit=10&offset=5")
	newAPI(t).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeSearch(t, rec)
	assert.Equal(t, 10, got.Limit, "limit echoed")
	assert.Equal(t, 5, got.Offset, "offset echoed")
}

func Test_Search_LimitAtMaxIsAllowed(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?q=brass&limit=100")
	newAPI(t).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "status at limit=%d", searchMaxLimit)
	got := decodeSearch(t, rec)
	assert.Equal(t, searchMaxLimit, got.Limit, "limit echoed")
}

// seedTaggedEntity writes a boardgame entity whose `data.tags` carries
// the given tags — the #453 tags filter reads json_extract(data,
// '$.tags').
func seedTaggedEntity(t *testing.T, st store.Store, id string, tags ...string) {
	t.Helper()
	anyTags := make([]any, len(tags))
	for i, tg := range tags {
		anyTags[i] = tg
	}
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   id,
		Kind: "boardgame",
		Data: map[string]any{"title": id, "tags": anyTags},
	}), "seed %s", id)
}

// Test_Search_TagsFilter pins #453 at the HTTP boundary: a single
// `tags=` returns matching entities; multiple repeated params AND-filter;
// comma-separated values are equivalent to repeated params.
func Test_Search_TagsFilter(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedTaggedEntity(t, st, "boardgame:test-game-2099", "alpha", "beta")
	seedTaggedEntity(t, st, "boardgame:test-game-2100", "alpha", "gamma")
	seedTaggedEntity(t, st, "boardgame:test-game-2101", "delta")

	// Single tag → both alpha-carrying entities.
	req, rec := searchRequest("/v1/search?kind=boardgame&tags=alpha")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeSearch(t, rec)
	assert.Equal(t, 2, got.Total)
	assert.ElementsMatch(t,
		[]string{"boardgame:test-game-2099", "boardgame:test-game-2100"},
		searchResultIDs(got))

	// Repeated params AND-filter → only the entity with BOTH.
	req, rec = searchRequest("/v1/search?kind=boardgame&tags=alpha&tags=beta")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got = decodeSearch(t, rec)
	require.Len(t, got.Results, 1)
	assert.Equal(t, "boardgame:test-game-2099", got.Results[0].ID)

	// Comma-separated is equivalent to the repeated-param AND.
	req, rec = searchRequest("/v1/search?kind=boardgame&tags=alpha,beta")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got = decodeSearch(t, rec)
	require.Len(t, got.Results, 1)
	assert.Equal(t, "boardgame:test-game-2099", got.Results[0].ID)

	// A tag no entity carries → empty.
	req, rec = searchRequest("/v1/search?kind=boardgame&tags=omega")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got = decodeSearch(t, rec)
	assert.Empty(t, got.Results)
	assert.Equal(t, 0, got.Total)
}

// searchResultIDs lifts the result ids for ElementsMatch comparisons.
func searchResultIDs(resp searchResponse) []string {
	out := make([]string, len(resp.Results))
	for i, r := range resp.Results {
		out[i] = r.ID
	}
	return out
}

// Test_parseTags covers the comma + repeated-param flattening, trimming,
// and empty-drop directly (the #453 handler-side parse).
func Test_parseTags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  []string
		want []string
	}{
		{name: "nil → nil", raw: nil, want: nil},
		{name: "single", raw: []string{"alpha"}, want: []string{"alpha"}},
		{name: "repeated", raw: []string{"alpha", "beta"}, want: []string{"alpha", "beta"}},
		{name: "comma split", raw: []string{"alpha,beta"}, want: []string{"alpha", "beta"}},
		{name: "trim whitespace", raw: []string{" alpha , beta "}, want: []string{"alpha", "beta"}},
		{name: "drop empties", raw: []string{"", "alpha", ",", "beta,"}, want: []string{"alpha", "beta"}},
		{name: "mixed repeated+comma", raw: []string{"alpha,beta", "gamma"}, want: []string{"alpha", "beta", "gamma"}},
		{name: "all empty → nil", raw: []string{"", " , "}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, parseTags(tc.raw))
		})
	}
}

// Test_parseBoundedInt covers the helper directly, including the
// `max < 0` sentinel path that disables the upper bound. The HTTP-level
// search tests exercise the limit/offset surface but never reach a value
// large enough to prove the no-upper-bound branch — naming that here so a
// future caller (e.g. ingest's wait_seconds, also bounded but with
// different limits) can rely on the helper's documented semantics.
func Test_parseBoundedInt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw string
		def int
		min int
		max int
		want int
		wantError bool
	}{
		{name: "empty returns default", raw: "", def: 20, min: 1, max: 100, want: 20},
		{name: "in range", raw: "50", def: 20, min: 1, max: 100, want: 50},
		{name: "at min", raw: "1", def: 20, min: 1, max: 100, want: 1},
		{name: "at max", raw: "100", def: 20, min: 1, max: 100, want: 100},
		{name: "below min", raw: "0", def: 20, min: 1, max: 100, wantError: true},
		{name: "above max", raw: "101", def: 20, min: 1, max: 100, wantError: true},
		{name: "not parseable", raw: "abc", def: 20, min: 1, max: 100, wantError: true},
		{name: "negative below min", raw: "-1", def: 0, min: 0, max: 100, wantError: true},
		{name: "no upper bound accepts large value", raw: "1000000", def: 0, min: 0, max: -1, want: 1000000},
		{name: "no upper bound still rejects below min", raw: "-5", def: 0, min: 0, max: -1, wantError: true},
		{name: "no upper bound empty still returns default", raw: "", def: 7, min: 0, max: -1, want: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseBoundedInt(tc.raw, tc.def, tc.min, tc.max)
			if tc.wantError {
				assert.Error(t, err, "want error, got %d", got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Test_Search_ResultKindsAreRegistered locks in the closure invariant:
// every result.kind must appear in bootstrapKinds.EntityKinds. Mirrors the
// closure-invariant tests on PRs (kinds) (entities), and (edges).
// Now seeds a real entity so the loop runs at least one real iteration
// against a hit.
func Test_Search_ResultKindsAreRegistered(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	req, rec := searchRequest("/v1/search?q=Brass")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeSearch(t, rec)
	require.NotEmpty(t, got.Results, "results: want at least one hit after seeding")

	declared := make(map[string]struct{}, len(testSeedEntityKinds))
	for _, k := range testSeedEntityKinds {
		declared[k] = struct{}{}
	}
	for i, r := range got.Results {
		_, ok := declared[r.Kind]
		assert.True(t, ok, "results[%d].kind=%q not in testSeedEntityKinds", i, r.Kind)
	}
}

// --- Issue: kind-only filtering / Issue: snippet-from-summary ---

// seedSnippetableBoardgame writes a boardgame entity. The data shape
// keeps `description` as a regular field (kind-only listing tests use
// it) while ALSO populating `summary` — the agent-filled prose that
// post-a prior PR drives the search-result snippet. The `summary` field is
// what land in the data map via a prior PR's vaultEntityDataForDB
// projection in the production flow; tests seed it directly via
// SaveEntity for the same on-the-wire shape.
func seedSnippetableBoardgame(t *testing.T, st store.Store, id string, summary string) {
	t.Helper()
	fetched := mustParseTime(t, "2024-04-12T15:03:11Z")
	e := &store.Entity{
		ID: id,
		Kind: "boardgame",
		Data: map[string]any{
			"title": "Snippet Test Game",
			"summary": summary,
			// `description` retained so the kind-only listing tests
			// have a non-summary data field to assert pagination on
			// without coupling them to the snippet contract.
			"description": summary,
		},
		Provenance: []store.ProvenanceEntry{
			{Source: "test:" + id, FetchedAt: &fetched, OK: true},
		},
		Edges: []store.EdgeRef{},
	}
	require.NoError(t, st.SaveEntity(context.Background(), e), "seed %s", id)
}

// Test_Search_KindOnly_ListsAllOfKind covers the source issueA: a kind-only
// query (no `q`) returns every entity of that kind, paginated.
func Test_Search_KindOnly_ListsAllOfKind(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedSnippetableBoardgame(t, st, "boardgame:kind-only-a", "A boardgame for kind-only listing.")
	seedSnippetableBoardgame(t, st, "boardgame:kind-only-b", "Another boardgame for kind-only listing.")

	req, rec := searchRequest("/v1/search?kind=boardgame")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeSearch(t, rec)
	assert.GreaterOrEqual(t, got.Total, 2, "total: want at least 2 (the two seeded boardgames)")
	require.GreaterOrEqual(t, len(got.Results), 2, "results: want at least 2 entries")
	for i, r := range got.Results {
		assert.Equal(t, "boardgame", r.Kind, "results[%d].kind", i)
	}
}

// Test_Search_KindOnly_UnknownKindReturnsEmpty pins the kind-only
// branch of the #110 contract: a kind with no DB rows returns 200 +
// empty rather than 400. Same shape as the q+kind branch.
func Test_Search_KindOnly_UnknownKindReturnsEmpty(t *testing.T) {
	t.Parallel()

	req, rec := searchRequest("/v1/search?kind=not-a-real-kind")
	newAPI(t).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeSearch(t, rec)
	assert.True(t, got.OK)
	assert.Empty(t, got.Results)
	assert.Equal(t, 0, got.Total)
}

// Test_Search_KindFilter_FindsSourceShape pins the #110 fix: an
// entity persisted with a source-shape kind (`gmail`, `wikipedia`,
// etc — derived from a plugin's source_namespace, NOT advertised in
// Capabilities().EntityKinds) is queryable via the kind filter
// even though no plugin's EntityKinds includes that name. Pre-#110
// the request 400'd; post-fix the rows the DB has come back.
func Test_Search_KindFilter_FindsSourceShape(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	// "gmail" is the source_namespace for a gmail plugin's
	// source-shape entities; the test fixture's plugin
	// (EntityKinds=boardgame/book/person) doesn't declare it.
	fetched := mustParseTime(t, "2026-05-16T22:00:00Z")
	e := &store.Entity{
		ID: "gmail:hello-world",
		Kind: "gmail",
		Data: map[string]any{"title": "hello world"},
		Provenance: []store.ProvenanceEntry{
			{Source: "gmail:fetch", FetchedAt: &fetched, OK: true},
		},
		Edges: []store.EdgeRef{},
	}
	require.NoError(t, st.SaveEntity(context.Background(), e))

	req, rec := searchRequest("/v1/search?kind=gmail")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeSearch(t, rec)
	require.Len(t, got.Results, 1, "want 1 row for kind=gmail; got=%+v", got)
	assert.Equal(t, "gmail:hello-world", got.Results[0].ID)
	assert.Equal(t, "gmail", got.Results[0].Kind)
}

// Test_Search_Snippet_FromSummary pins the post-a prior PR contract: the
// snippet on each hit is the entity's agent-filled `summary`,
// returned verbatim (subject only to maxChars truncation, exercised
// separately).
func Test_Search_Snippet_FromSummary(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	const summary = "A short summary that should land on the search result snippet."
	seedSnippetableBoardgame(t, st, "boardgame:snip-test", summary)

	req, rec := searchRequest("/v1/search?q=summary")
	h.ServeHTTP(rec, req)

	got := decodeSearch(t, rec)
	require.NotEmpty(t, got.Results, "results: want at least one match for q=summary")
	var found bool
	for _, r := range got.Results {
		if r.ID == "boardgame:snip-test" {
			assert.Equal(t, summary, r.Snippet,
				"results[%s].snippet: want verbatim summary", r.ID)
			found = true
		}
	}
	assert.True(t, found, "results: want boardgame:snip-test in matches, got %v", got.Results)
}

// Test_Search_Snippet_EmptyForUnfilledEntity is the negative half of
// the snippet-as-property contract from ADR-0008: an entity with no
// agent-filled summary returns an empty snippet on search hits. The
// fallback substring-strip from `description` / etc. is gone — the
// gap closes via the agent fill flow + plugin starter-summary
// emission, not via search-time computation.
func Test_Search_Snippet_EmptyForUnfilledEntity(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	// Seed an entity with `description` populated but no `summary` —
	// pre-a prior PR this would have produced a description-derived
	// snippet; post-a prior PR it must be empty.
	fetched := mustParseTime(t, "2024-04-12T15:03:11Z")
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: "boardgame:no-summary",
		Kind: "boardgame",
		Data: map[string]any{
			"title": "Unfilled",
			"description": "this WAS the snippet source pre-a prior PR, but no longer is",
		},
		Provenance: []store.ProvenanceEntry{
			{Source: "test:no-summary", FetchedAt: &fetched, OK: true},
		},
	}), "seed unfilled entity")

	req, rec := searchRequest("/v1/search?q=Unfilled")
	h.ServeHTTP(rec, req)

	got := decodeSearch(t, rec)
	require.NotEmpty(t, got.Results)
	for _, r := range got.Results {
		if r.ID == "boardgame:no-summary" {
			assert.Empty(t, r.Snippet,
				"results[%s].snippet: must be empty when summary is unfilled (no derivation fallback)", r.ID)
			return
		}
	}
	t.Fatalf("results: want boardgame:no-summary in matches, got %v", got.Results)
}

// Test_Search_Snippet_TruncatedToMaxChars asserts the truncation cap
// from snippet.go: a description longer than the configured max gets
// trimmed and ellipsis-suffixed.
func Test_Search_Snippet_TruncatedToMaxChars(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a long description. ", 30) // ~600 chars
	h, st := newAPIWithStore(t)
	seedSnippetableBoardgame(t, st, "boardgame:long-snip", long)

	req, rec := searchRequest("/v1/search?q=long")
	h.ServeHTTP(rec, req)

	got := decodeSearch(t, rec)
	var snip string
	for _, r := range got.Results {
		if r.ID == "boardgame:long-snip" {
			snip = r.Snippet
		}
	}
	require.NotEmpty(t, snip, "snippet: want non-empty for boardgame:long-snip")
	// utf8.RuneCountInString of the trimmed snippet must be at most
	// DefaultSnippetMaxChars + 1 (the trailing ellipsis).
	runes := utf8.RuneCountInString(snip)
	assert.LessOrEqual(t, runes, DefaultSnippetMaxChars+1,
		"snippet length: want ≤ %d runes (incl. ellipsis)", DefaultSnippetMaxChars+1)
	assert.True(t, strings.HasSuffix(snip, "…"),
		"snippet: want ellipsis suffix on truncated output, got %q", snip)
}

// (Per-kind SnippetFields override / per-plugin substring chain
// removed entirely in a prior PR — snippet is the entity's agent-filled
// `summary`, not a query-time derivation. The Test_Search_Snippet_
// PerKindOverrideWins case from is gone with the per-kind chain.)

// Test_Search_IsJournalFilter pins ADR-0025 cut 3 (#222): the
// `?is_journal=true` query param forwards to the store-layer
// journalOnly flag, restricting the result set to entities whose
// data carries `is_journal: true`. Mirrors the store-level test
// at the HTTP boundary.
func Test_Search_IsJournalFilter(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "day:2026-11-11",
		Kind: "day",
		Data: map[string]any{"is_journal": true},
	}))
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "day:2026-11-12",
		Kind: "day",
		Data: map[string]any{"is_journal": false},
	}))
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "day:2026-11-13",
		Kind: "day",
		Data: map[string]any{},
	}))

	// Filter off → all three day entities.
	req, rec := searchRequest("/v1/search?kind=day")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeSearch(t, rec)
	assert.Equal(t, 3, got.Total, "no filter ⇒ all day entities")

	// Filter on → only the flagged entity.
	req, rec = searchRequest("/v1/search?kind=day&is_journal=true")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got = decodeSearch(t, rec)
	require.Equal(t, 1, got.Total, "filter ⇒ only is_journal=true entries")
	require.Len(t, got.Results, 1)
	assert.Equal(t, "day:2026-11-11", got.Results[0].ID)
}
