package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// seedEdge inserts a single edge between pre-existing entities. The
// caller is responsible for ensuring both endpoints exist.
func seedEdge(t *testing.T, st store.Store, edgeType, from, to string) {
	t.Helper()
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: edgeType,
		From: from,
		To: to,
	}), "seed edge %s: %s → %s", edgeType, from, to)
}

func decodeContextResponse(t *testing.T, body []byte) contextResponse {
	t.Helper()
	var got contextResponse
	require.NoError(t, json.Unmarshal(body, &got), "decode context response")
	return got
}

// neighborIDsAtDepth flattens the result for assertion convenience.
func neighborIDsAtDepth(c contextResponse, depth int) []string {
	out := []string{}
	for _, n := range c.Neighbors {
		if n.Depth == depth {
			out = append(out, n.Entity.ID)
		}
	}
	return out
}

// Depth 0 — root only, no neighbors.
func TestEntitiesContext_DepthZero(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEdge(t, st, "designed_by", "boardgame:brass-birmingham", "person:martin-wallace")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham/context?depth=0", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.Equal(t, "boardgame:brass-birmingham", got.Root.ID)
	assert.Empty(t, got.Neighbors)
	assert.False(t, got.Truncated)
}

// Depth 1 — root + direct neighbors.
func TestEntitiesContext_DepthOne(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "person:gavan-brown", "person")
	seedEdge(t, st, "designed_by", "boardgame:brass-birmingham", "person:martin-wallace")
	seedEdge(t, st, "designed_by", "boardgame:brass-birmingham", "person:gavan-brown")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham/context?depth=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.Equal(t, "boardgame:brass-birmingham", got.Root.ID)
	assert.ElementsMatch(t, []string{"person:martin-wallace", "person:gavan-brown"},
		neighborIDsAtDepth(got, 1))
	assert.False(t, got.Truncated)

	for _, n := range got.Neighbors {
		assert.Equal(t, "designed_by", n.Edge.Type)
		assert.Equal(t, "boardgame:brass-birmingham", n.Edge.From)
		assert.Equal(t, n.Entity.ID, n.Edge.To)
	}
}

// Depth 2 — chain root → A → B; both neighbors land, with depths.
func TestEntitiesContext_DepthTwo(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "book:age-of-steam", "book")
	seedEdge(t, st, "designed_by", "boardgame:brass-birmingham", "person:martin-wallace")
	seedEdge(t, st, "authored_by", "person:martin-wallace", "book:age-of-steam")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:brass-birmingham/context?depth=2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.Equal(t, []string{"person:martin-wallace"}, neighborIDsAtDepth(got, 1))
	assert.Equal(t, []string{"book:age-of-steam"}, neighborIDsAtDepth(got, 2))
	assert.False(t, got.Truncated)
}

// Depth 3 — full cap.
func TestEntitiesContext_DepthThree(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")
	seedEntity(t, st, "person:b", "person")
	seedEntity(t, st, "book:c", "book")
	seedEntity(t, st, "person:d", "person")
	seedEdge(t, st, "designed_by", "boardgame:a", "person:b")
	seedEdge(t, st, "authored_by", "person:b", "book:c")
	seedEdge(t, st, "authored_by", "book:c", "person:d") // depth-3 hop

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:a/context?depth=3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.Equal(t, []string{"person:b"}, neighborIDsAtDepth(got, 1))
	assert.Equal(t, []string{"book:c"}, neighborIDsAtDepth(got, 2))
	assert.Equal(t, []string{"person:d"}, neighborIDsAtDepth(got, 3))
}

// Depth 4 — rejected at the request boundary with 400 invalid_argument.
func TestEntitiesContext_DepthAboveCapRejected(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:a/context?depth=4", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["ok"])
	assert.Equal(t, "invalid_argument", body["error"])
	assert.Equal(t, "depth", body["field"])
}

// Missing depth → 400 (caller MUST specify; no implicit default).
func TestEntitiesContext_DepthRequired(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:a/context", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "depth", body["field"])
}

// Cycle: A → B → A returns A only once.
func TestEntitiesContext_CycleHandling(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "person:a", "person")
	seedEntity(t, st, "person:b", "person")
	seedEdge(t, st, "designed_by", "person:a", "person:b")
	seedEdge(t, st, "designed_by", "person:b", "person:a") // back-edge

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/person:a/context?depth=3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	// Root once, B once at depth 1, no second A from the back-edge.
	assert.Equal(t, "person:a", got.Root.ID)
	assert.Len(t, got.Neighbors, 1, "back-edge to root must not re-introduce root")
	assert.Equal(t, "person:b", got.Neighbors[0].Entity.ID)
	assert.Equal(t, 1, got.Neighbors[0].Depth)
}

// Edge-type filter: only `designed_by` walked; `authored_by` excluded.
func TestEntitiesContext_EdgeTypeFilter(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")
	seedEntity(t, st, "person:b", "person")
	seedEntity(t, st, "person:c", "person")
	seedEdge(t, st, "designed_by", "boardgame:a", "person:b")
	seedEdge(t, st, "authored_by", "boardgame:a", "person:c")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:a/context?depth=2&edge_types=designed_by", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.Equal(t, []string{"person:b"}, neighborIDsAtDepth(got, 1))
	for _, n := range got.Neighbors {
		assert.Equal(t, "designed_by", n.Edge.Type, "filtered-out edge types must not appear")
	}
}

// Pagination: max_results truncates the result and sets truncated: true.
func TestEntitiesContext_PaginationTruncates(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("person:p%d", i)
		seedEntity(t, st, id, "person")
		seedEdge(t, st, "designed_by", "boardgame:a", id)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:a/context?depth=1&max_results=3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.Len(t, got.Neighbors, 3)
	assert.True(t, got.Truncated)
}

// 404 envelope when path id resolves to nothing.
func TestEntitiesContext_RootNotFound(t *testing.T) {
	t.Parallel()

	h := newAPI(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:does-not-exist/context?depth=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.False(t, body.OK)
	assert.Equal(t, "not_found", body.Error)
	assert.Contains(t, body.Message, "boardgame:does-not-exist")
}

// Invalid max_results (above cap or non-positive) → 400.
func TestEntitiesContext_InvalidMaxResults(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")

	for _, raw := range []string{"0", "-1", "1001", "abc"} {
		raw := raw
		t.Run("max_results="+raw, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet,
				"/v1/entities/boardgame:a/context?depth=1&max_results="+raw, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "invalid_argument", body["error"])
			assert.Equal(t, "max_results", body["field"])
		})
	}
}

// Mixed edge_types filter: comma-separated list with whitespace and
// trailing empties parses cleanly.
func TestEntitiesContext_EdgeTypesFilterMultiplePresent(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:a", "boardgame")
	seedEntity(t, st, "person:b", "person")
	seedEntity(t, st, "person:c", "person")
	seedEdge(t, st, "designed_by", "boardgame:a", "person:b")
	seedEdge(t, st, "authored_by", "boardgame:a", "person:c")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:a/context?depth=1&edge_types=designed_by,%20authored_by%20", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.ElementsMatch(t, []string{"person:b", "person:c"}, neighborIDsAtDepth(got, 1))
}

// ADR-0018 step 3: archived neighbors in /context responses surface
// archived=true on the entity wire shape. Edges themselves remain in
// DB regardless of archive state — the consumer decides whether to
// follow.
func TestEntitiesContext_ArchivedNeighborSurfacesFlag(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:root-game", "boardgame")
	seedEntity(t, st, "person:archived-neighbor", "person")
	seedEntity(t, st, "person:active-neighbor", "person")
	seedEdge(t, st, "designed_by", "boardgame:root-game", "person:archived-neighbor")
	seedEdge(t, st, "designed_by", "boardgame:root-game", "person:active-neighbor")
	require.NoError(t, st.ArchiveEntity(context.Background(), "person:archived-neighbor"))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:root-game/context?depth=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.False(t, got.Root.Archived, "root active")

	byID := make(map[string]bool, len(got.Neighbors))
	for _, n := range got.Neighbors {
		byID[n.Entity.ID] = n.Entity.Archived
	}
	assert.True(t, byID["person:archived-neighbor"], "archived neighbor: archived=true")
	assert.False(t, byID["person:active-neighbor"], "active neighbor: archived omitted")
}

// ADR-0018 step 3: when the root itself is archived (an operator is
// inspecting an archived entity's neighborhood pre-destroy), the
// Root entity surfaces archived=true. Edges still resolve.
func TestEntitiesContext_ArchivedRootSurfacesFlag(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:archived-root", "boardgame")
	seedEntity(t, st, "person:still-active", "person")
	seedEdge(t, st, "designed_by", "boardgame:archived-root", "person:still-active")
	require.NoError(t, st.ArchiveEntity(context.Background(), "boardgame:archived-root"))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:archived-root/context?depth=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeContextResponse(t, rec.Body.Bytes())
	assert.True(t, got.Root.Archived, "archived root surfaces flag")
	require.Len(t, got.Neighbors, 1, "edge retained even when source archived")
	assert.False(t, got.Neighbors[0].Entity.Archived, "neighbor still active")
}
