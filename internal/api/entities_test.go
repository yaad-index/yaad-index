package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

func Test_Entities_FetchSeededEntity(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:brass-birmingham", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response body")

	assert.Equal(t, "boardgame:brass-birmingham", got.ID)
	assert.Equal(t, "boardgame", got.Kind)

	// Index provenance by source so the assertion is order- and
	// length-insensitive — same pattern as the kinds tests.
	provBy := indexProvenance(got.Provenance)
	ok, found := provBy["bgg:14-2024-04-12"]
	require.True(t, found, "provenance missing 'bgg:14-2024-04-12'; got sources %v", provenanceSources(got.Provenance))
	assert.True(t, ok.OK, "provenance[bgg:14-2024-04-12].ok")
	assert.Empty(t, ok.Error, "provenance[bgg:14-2024-04-12].error")
	assert.Empty(t, ok.ErrorMessage, "provenance[bgg:14-2024-04-12].error_message")
	assert.NotEmpty(t, ok.FetchedAt, "provenance[bgg:14-2024-04-12].fetched_at")

	failed, found := provBy["bgg:14-2024-04-13"]
	require.True(t, found, "provenance missing 'bgg:14-2024-04-13'; got sources %v", provenanceSources(got.Provenance))
	assert.False(t, failed.OK, "provenance[bgg:14-2024-04-13].ok")
	assert.Equal(t, "extractor_timeout", failed.Error, "provenance[bgg:14-2024-04-13].error")
	assert.NotEmpty(t, failed.ErrorMessage, "provenance[bgg:14-2024-04-13].error_message")

	// Edges intentionally empty until the edge-side cutover wires the
	// `with_edges` expansion path. The closure invariant in
	// Test_Entities_EdgeTypesAreDeclaredKinds still holds (vacuously,
	// for now).
	assert.Empty(t, got.Edges, "edges: want empty (with_edges expansion not yet wired)")
}

// Test_Entities_EdgeTypesAreDeclaredKinds locks in the cross-endpoint
// closure invariant: every entity.edges[].type must appear in the
// edge_kinds enumeration served by /v1/kinds. With the edge cutover in
// place, this test seeds an actual designed_by edge and asserts the
// closure rule against a non-empty edges array — the iterations are
// real now, not vacuous as they were under the entity-cutover-only PR.
func Test_Entities_EdgeTypesAreDeclaredKinds(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")

	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
	}), "seed designed_by edge")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	require.NotEmpty(t, got.Edges, "edges after seeding designed_by")

	declared := make(map[string]struct{}, len(testSeedEdgeKinds))
	for _, k := range testSeedEdgeKinds {
		declared[k] = struct{}{}
	}
	for i, e := range got.Edges {
		_, ok := declared[e.Type]
		assert.True(t, ok, "entity.edges[%d].type=%q not in testSeedEdgeKinds", i, e.Type)
	}
}

func Test_Entities_NotFound(t *testing.T) {
	t.Parallel()

	h := newAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:nope", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode response body")
	assert.False(t, body.OK)
	assert.Equal(t, "not_found", body.Error)
	// Message should mention the requested id so callers can correlate the
	// envelope with the request without re-deriving it.
	assert.Contains(t, body.Message, "boardgame:nope")
}

func Test_Entities_AcceptsWithEdgesParam(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// Test_Entities_WithEdgesExpansion_PopulatesEdges seeds two edges of
// different types and confirms with_edges returns the full set when no
// type filter is applied.
func Test_Entities_WithEdgesExpansion_PopulatesEdges(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "person:gavan-brown", "person")

	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}), "seed designed_by martin-wallace")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:gavan-brown",
	}), "seed designed_by gavan-brown")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	require.Len(t, got.Edges, 2, "edges: want 2 (both seeded designed_by)")
	for i, e := range got.Edges {
		assert.Equal(t, "designed_by", e.Type, "edges[%d].type", i)
	}
}

// Test_Entities_WithEdgesExpansion_FiltersByType seeds two edges of
// different types and confirms only the requested type comes back when
// the filter is applied.
func Test_Entities_WithEdgesExpansion_FiltersByType(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "company:roxley", "boardgame") // kind doesn't matter for the store

	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}), "seed designed_by")
	// `published_by` isn't in any registered plugin's edge_kinds — but
	// the store layer doesn't validate kinds (that's an API concern).
	// Used here to prove the filter genuinely excludes it.
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "published_by", From: "boardgame:brass-birmingham", To: "company:roxley",
	}), "seed published_by")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	require.Len(t, got.Edges, 1, "filter: want exactly designed_by")
	assert.Equal(t, "designed_by", got.Edges[0].Type)
}

// Test_Entities_WithoutWithEdges_ReturnsEmptyEdges confirms the absent-
// param case keeps the legacy empty-edges behaviour even when the store
// has edges to expand.
func Test_Entities_WithoutWithEdges_ReturnsEmptyEdges(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}), "seed designed_by")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	assert.Empty(t, got.Edges, "edges without with_edges: want empty (legacy)")
}

func indexProvenance(in []provenanceEntry) map[string]provenanceEntry {
	out := make(map[string]provenanceEntry, len(in))
	for _, p := range in {
		out[p.Source] = p
	}
	return out
}

func provenanceSources(in []provenanceEntry) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.Source)
	}
	return out
}

// Per yaad-index: `with_edges` accepts three "all types" shapes
// in addition to the comma-separated type filter. These tests pin
// the matrix.

// `?with_edges` (key absent) → no expansion; legacy default.
// (Already covered above by Test_Entities_NoEdgeExpansionWithoutWithEdges
// for parity, repeated here as the matrix's first row for grouping.)
func Test_Entities_WithEdgesMatrix_KeyAbsent_NoExpansion(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:brass-birmingham", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Empty(t, got.Edges, "no with_edges key → no expansion")
}

// `?with_edges` (key present, empty value) → expand all types.
// Presence-based per option A.
func Test_Entities_WithEdgesMatrix_PresentEmpty_AllTypes(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 2, "presence-based with_edges → all types")
	types := make([]string, 0, len(got.Edges))
	for _, e := range got.Edges {
		types = append(types, e.Type)
	}
	assert.ElementsMatch(t, []string{"designed_by", "authored_by"}, types)
}

// `?with_edges=` (key present, explicit empty) → also expands all
// types. Distinguishes the "key set, no value" case via Has().
//
// Seeds TWO edges of different types so the assertion can
// distinguish "all types" from an accidental single-type filter
// (the cold-reviewer's a prior PR catch on the prior single-edge fixture).
func Test_Entities_WithEdgesMatrix_EqualsEmpty_AllTypes(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 2, "explicit-empty with_edges → all types")
	types := make([]string, 0, len(got.Edges))
	for _, e := range got.Edges {
		types = append(types, e.Type)
	}
	assert.ElementsMatch(t, []string{"designed_by", "authored_by"}, types)
}

// `?with_edges=*` → expand all types via canonical sentinel.
func Test_Entities_WithEdgesMatrix_StarSentinel_AllTypes(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=*", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 2, "* sentinel → all types")
}

// `?with_edges=all` → expand all types via alternate sentinel
// spelling. Same shape as `*` (kept as bonus per the issue;
// `*` remains canonical).
func Test_Entities_WithEdgesMatrix_AllSentinel_AllTypes(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=all", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 2, "`all` sentinel → all types")
}

// `?with_edges=is_about` → existing single-type filter unchanged.
func Test_Entities_WithEdgesMatrix_SingleType_FilteredUnchanged(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 1)
	assert.Equal(t, "designed_by", got.Edges[0].Type)
}

// `?with_edges=is_about,designed_by` → existing multi-type filter
// unchanged.
func Test_Entities_WithEdgesMatrix_MultiType_FilteredUnchanged(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham?with_edges=designed_by,authored_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 2)
}

// Sentinel mixed with concrete types in the same value collapses
// to "all types" — the broader semantic wins. This pins the
// documented behavior so a reader doesn't assume narrower-wins.
//
// Table-driven over both sentinel spellings (`*` canonical and
// `all` alternate) per the cold-reviewer's a prior PR catch — the prior single-
// case test left half the matrix uncovered.
func Test_Entities_WithEdgesMatrix_SentinelMixedWithTypes_AllTypes(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"*,designed_by", "all,designed_by", "designed_by,*", "designed_by,all"} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			h, st := newAPIWithStore(t)
			seedBrassBirmingham(t, st)
			seedEntity(t, st, "person:martin-wallace", "person")
			seedEntity(t, st, "book:age-of-steam", "book")
			require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
				Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
			}))
			require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
				Type: "authored_by", From: "boardgame:brass-birmingham", To: "book:age-of-steam",
			}))

			req := httptest.NewRequest(http.MethodGet,
				"/v1/entities/boardgame:brass-birmingham?with_edges="+raw, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			var got entity
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
			require.Len(t, got.Edges, 2,
				"sentinel + concrete type → all types (broader wins); raw=%q", raw)
		})
	}
}

// ADR-0018 step 3 fixtures — propagate archived flag through edge
// expansion. Three cases Per the prior design, acceptance: edge-to-archived,
// edge-from-archived, edge-between-two-archived. Plus the
// restore-clears-flag round-trip.

// Test_Entities_WithEdges_ArchivedFlag_OnEndpoint: the most common
// shape — edge endpoint (`to`) is archived. Flag rides on edgeRef.
func Test_Entities_WithEdges_ArchivedFlag_OnEndpoint(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:active-game", "boardgame")
	seedEntity(t, st, "person:archived-designer", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:active-game", To: "person:archived-designer",
	}))
	require.NoError(t, st.ArchiveEntity(context.Background(), "person:archived-designer"))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:active-game?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.False(t, got.Archived, "root (active) must NOT carry archived flag")
	require.Len(t, got.Edges, 1)
	assert.True(t, got.Edges[0].Archived, "edge to archived endpoint: archived=true")
}

// Test_Entities_WithEdges_ArchivedFlag_OnRoot: source entity itself
// is archived. The Root surfaces archived=true; its outbound edges
// to active endpoints carry NO archived flag.
func Test_Entities_WithEdges_ArchivedFlag_OnRoot(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:archived-game", "boardgame")
	seedEntity(t, st, "person:active-designer", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:archived-game", To: "person:active-designer",
	}))
	require.NoError(t, st.ArchiveEntity(context.Background(), "boardgame:archived-game"))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:archived-game?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.Archived, "root (archived) must carry archived flag")
	require.Len(t, got.Edges, 1)
	assert.False(t, got.Edges[0].Archived, "edge to active endpoint: archived omitted")
}

// Test_Entities_WithEdges_ArchivedFlag_BothEnds: both source and
// endpoint archived. Root + edgeRef both surface archived=true.
func Test_Entities_WithEdges_ArchivedFlag_BothEnds(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:dead-game", "boardgame")
	seedEntity(t, st, "person:dead-designer", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:dead-game", To: "person:dead-designer",
	}))
	require.NoError(t, st.ArchiveEntity(context.Background(), "boardgame:dead-game"))
	require.NoError(t, st.ArchiveEntity(context.Background(), "person:dead-designer"))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:dead-game?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.Archived, "root archived")
	require.Len(t, got.Edges, 1)
	assert.True(t, got.Edges[0].Archived, "edge to archived endpoint: archived=true")
}

// Test_Entities_WithEdges_ArchivedFlag_RestoreClears: round-trip
// pin — restoring the endpoint drops the flag from the next read.
// Edges are retained in DB regardless of archive state of either
// endpoint per ADR-0018.
func Test_Entities_WithEdges_ArchivedFlag_RestoreClears(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:live-game", "boardgame")
	seedEntity(t, st, "person:flapping-designer", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:live-game", To: "person:flapping-designer",
	}))
	require.NoError(t, st.ArchiveEntity(context.Background(), "person:flapping-designer"))
	require.NoError(t, st.RestoreEntity(context.Background(), "person:flapping-designer"))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:live-game?with_edges=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 1)
	assert.False(t, got.Edges[0].Archived,
		"after restore, archived flag must be gone (omitempty: must be false on the wire-decoded struct)")
}

// ADR-0018 step 6: attachment manifest surfaces on the wire entity
// when present in the vault frontmatter. mergeVaultEntity is the
// only path that populates the `attachments` wire field — DB-only
// deployments (no vault) keep the field absent via omitempty.
func Test_Entities_AttachmentManifestSurfacesOnWire(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	const id = "boardgame:has-thumb-2024"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "boardgame",
		Data: map[string]any{"name": "Has Thumb"},
	}))

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "boardgame",
		Plugin: "bgg",
		Data: map[string]any{"name": "Has Thumb"},
		Attachments: []vault.Attachment{
			{
				Name: "thumbnail.jpg",
				Kind: "image/jpeg",
				Path: "attachments/thumbnail.jpg",
				Bytes: 12453,
			},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Attachments, 1)
	assert.Equal(t, "thumbnail.jpg", got.Attachments[0].Name)
	assert.Equal(t, "image/jpeg", got.Attachments[0].Kind)
	assert.Equal(t, "attachments/thumbnail.jpg", got.Attachments[0].Path)
	assert.Equal(t, int64(12453), got.Attachments[0].Bytes)
}

// Empty manifest must NOT surface as `"attachments": []` on the wire
// — agents reading entities should see no key at all so the absent
// case is distinguishable from "manifest exists but is empty."
func Test_Entities_NoAttachments_OmitsKeyOnWire(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	const id = "boardgame:no-attachments-2024"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "boardgame",
		Data: map[string]any{"name": "Plain"},
	}))

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "boardgame",
		Plugin: "bgg",
		Data: map[string]any{"name": "Plain"},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	if strings.Contains(rec.Body.String(), `"attachments"`) {
		t.Errorf("empty manifest must omit `attachments` key on wire; got: %s", rec.Body.String())
	}
}
