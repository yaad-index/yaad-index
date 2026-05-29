package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestListEdges_OutDirectionDefault: ?entity_id=X with no
// ?direction= returns outbound edges only (back-compat with the
// existing /v1/entities/{id}?with_edges= semantic).
func TestListEdges_OutDirectionDefault(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "person:gavan-brown", "person")

	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:gavan-brown",
	}))
	// One inbound that should NOT appear (default direction=out).
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "is_about", From: "person:martin-wallace", To: "boardgame:brass-birmingham",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=boardgame:brass-birmingham", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	require.Len(t, got.Edges, 2, "default direction=out — only outbound edges")
	for _, e := range got.Edges {
		assert.Equal(t, "boardgame:brass-birmingham", e.FromID)
		assert.Equal(t, "designed_by", e.Type)
	}
	assert.Nil(t, got.NextCursor, "v1 cursor reserved but always null")
}

// TestListEdges_InDirection: ?direction=in returns only edges
// whose to_id matches the requested entity_id.
func TestListEdges_InDirection(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")

	// Outbound from brass — should NOT appear when querying martin-wallace direction=in.
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=person:martin-wallace&direction=in", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 1)
	assert.Equal(t, "boardgame:brass-birmingham", got.Edges[0].FromID)
	assert.Equal(t, "person:martin-wallace", got.Edges[0].ToID)
	assert.Equal(t, "designed_by", got.Edges[0].Type)
}

// TestListEdges_BothDirections: ?direction=both returns inbound +
// outbound combined.
func TestListEdges_BothDirections(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "boardgame:another-game", "boardgame")

	// Outbound from martin (1).
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed", From: "person:martin-wallace", To: "boardgame:another-game",
	}))
	// Inbound to martin (1).
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=person:martin-wallace&direction=both", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Len(t, got.Edges, 2, "direction=both surfaces 1 outbound + 1 inbound")
}

// TestListEdges_EdgeTypesFilter: ?edge_types=A,B narrows the
// result to those types only.
func TestListEdges_EdgeTypesFilter(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "company:roxley", "boardgame")

	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "published_by", From: "boardgame:brass-birmingham", To: "company:roxley",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=boardgame:brass-birmingham&edge_types=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Edges, 1, "filter narrowed to designed_by")
	assert.Equal(t, "designed_by", got.Edges[0].Type)
}

// TestListEdges_EmptyResult: entity has no edges → empty array,
// not null. Wire shape is stable.
func TestListEdges_EmptyResult(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedEntity(t, st, "person:lonely", "person")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=person:lonely", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.NotNil(t, got.Edges, "edges array always non-nil (stable wire shape)")
	assert.Empty(t, got.Edges)
}

// TestListEdges_MissingEntityID_400: missing ?entity_id= rejects
// with a clear error so the agent fixes the call shape.
func TestListEdges_MissingEntityID_400(t *testing.T) {
	t.Parallel()
	h := newAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/edges", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "entity_id is required")
}

// TestListEdges_InvalidDirection_400: ?direction=sideways rejects
// with the canonical {out, in, both} hint.
func TestListEdges_InvalidDirection_400(t *testing.T) {
	t.Parallel()
	h := newAPI(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=person:foo&direction=sideways", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid_direction")
}

// TestListEdges_LimitClamped: bad / oversize limit values fall
// back to the default; a valid in-range limit caps the result.
func TestListEdges_LimitClamped(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	for i := 0; i < 5; i++ {
		seedEntity(t, st, "person:p"+string(rune('a'+i)), "person")
		require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
			Type: "designed_by",
			From: "boardgame:brass-birmingham",
			To: "person:p" + string(rune('a'+i)),
		}))
	}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=boardgame:brass-birmingham&limit=3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Len(t, got.Edges, 3, "limit=3 caps the response")
}

// TestListEdges_TotalReportsPreCapCount pins the #338 contract:
// `total` reports the count of edges matching the tuple before
// limit truncation, so callers see how many edges exist when
// the limit cuts the response.
func TestListEdges_TotalReportsPreCapCount(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	for i := 0; i < 5; i++ {
		seedEntity(t, st, "person:p"+string(rune('a'+i)), "person")
		require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
			Type: "designed_by",
			From: "boardgame:brass-birmingham",
			To: "person:p" + string(rune('a'+i)),
		}))
	}

	// With limit=3 the response cuts to 3; total still reports 5.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=boardgame:brass-birmingham&limit=3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Len(t, got.Edges, 3)
	assert.Equal(t, 5, got.Total,
		"total reports the pre-cap match count even when limit truncates the response")
}

// TestListEdges_TotalEqualsLenWhenUnderLimit pins the no-truncation
// case: total equals len(edges) when the limit doesn't cut.
func TestListEdges_TotalEqualsLenWhenUnderLimit(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)
	seedEntity(t, st, "person:martin-wallace", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by", From: "boardgame:brass-birmingham", To: "person:martin-wallace",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/edges?entity_id=boardgame:brass-birmingham", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got edgeListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, len(got.Edges), got.Total,
		"total equals len(edges) when the limit doesn't truncate")
	assert.Equal(t, 1, got.Total)
}
