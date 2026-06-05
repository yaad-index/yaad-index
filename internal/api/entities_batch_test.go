package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

func batchRequestBody(t *testing.T, ids []string, withEdges []string) io.Reader {
	t.Helper()
	body := batchRequest{IDs: ids, WithEdges: withEdges}
	b, err := json.Marshal(body)
	require.NoError(t, err, "marshal batch request")
	return strings.NewReader(string(b))
}

// newAPI builds a fresh handler backed by an empty in-memory SQLite store
// and registers t.Cleanup to close it. Every test in this package uses
// this helper so the store wiring is exercised end-to-end (store creation,
// migration, close) on every test run.
func newAPI(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newAPIWithStore(t)
	return h
}

// newAPIWithStore is the same as newAPI but also returns the underlying
// store so the caller can seed fixtures before issuing requests. Wires
// the seeded test registry so historical closure invariants over
// boardgame / book / person + designed_by / authored_by keep holding
// after the bootstrapKinds retirement.
func newAPIWithStore(t *testing.T) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New(:memory:)")
	t.Cleanup(func() { _ = st.Close() })
	return NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), st, testRegistryWithSeed()), st
}

// seedBrassBirmingham writes the historical brass-birmingham fixture to
// the store. Tests that previously relied on the hardcoded fixture call
// this so they continue to assert the same wire shape against the
// store-backed entity handler.
func seedBrassBirmingham(t *testing.T, st store.Store) {
	t.Helper()
	fetched1 := mustParseTime(t, "2024-04-12T15:03:11Z")
	fetched2 := mustParseTime(t, "2024-04-13T06:00:00Z")
	e := &store.Entity{
		ID: "boardgame:brass-birmingham",
		Kind: "boardgame",
		Data: map[string]any{
			"title": "Brass: Birmingham",
			"year": float64(2018),
		},
		Provenance: []store.ProvenanceEntry{
			{
				Source: "bgg:14-2024-04-12",
				FetchedAt: &fetched1,
				OK: true,
			},
			{
				Source: "bgg:14-2024-04-13",
				FetchedAt: &fetched2,
				OK: false,
				Error: "extractor_timeout",
				ErrorMessage: "AI extraction did not complete within 60s",
			},
		},
		Edges: []store.EdgeRef{},
	}
	require.NoError(t, st.SaveEntity(context.Background(), e), "seed brass-birmingham")
}

// seedEntity writes a minimal entity (id + kind, no provenance, no edges)
// to the store so handler tests that need a valid `from` / `to` for
// edge creation can satisfy CreateEdge's existence check without
// constructing full fixture data.
func seedEntity(t *testing.T, st store.Store, id, kind string) {
	t.Helper()
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: kind,
		Data: map[string]any{"id": id},
		Edges: []store.EdgeRef{},
	}), "seed %s", id)
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err, "parse %q", s)
	return v
}

func Test_BatchEntities_OneKnownOneUnknown(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	body := batchRequestBody(t,
		[]string{"boardgame:brass-birmingham", "person:nobody"},
		nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got batchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	assert.True(t, got.OK)

	gotIDs := make([]string, 0, len(got.Entities))
	for _, e := range got.Entities {
		gotIDs = append(gotIDs, e.ID)
	}
	assert.Equal(t, []string{"boardgame:brass-birmingham"}, gotIDs)
	assert.Equal(t, []string{"person:nobody"}, got.Missing)
}

func Test_BatchEntities_AllUnknown(t *testing.T) {
	t.Parallel()

	body := batchRequestBody(t,
		[]string{"person:nobody", "boardgame:also-nope"}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var got batchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	assert.Empty(t, got.Entities)
	assert.Equal(t, []string{"person:nobody", "boardgame:also-nope"}, got.Missing)
}

func Test_BatchEntities_AllKnown(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	body := batchRequestBody(t,
		[]string{"boardgame:brass-birmingham"}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var got batchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	require.Len(t, got.Entities, 1)
	assert.Equal(t, "boardgame:brass-birmingham", got.Entities[0].ID)
	assert.Empty(t, got.Missing)
}

func Test_BatchEntities_AcceptsWithEdgesParam(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedBrassBirmingham(t, st)

	body := batchRequestBody(t,
		[]string{"boardgame:brass-birmingham"},
		[]string{"designed_by"})
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code,
		"with_edges accepted; the seeded entity has no edges so the list is empty")
}

// Test_BatchEntities_WithEdges_Expands pins #452: /v1/entities/batch
// now honors with_edges, attaching each entity's edges (same wire shape
// as single-GET) instead of silently discarding the field.
func Test_BatchEntities_WithEdges_Expands(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	ctx := context.Background()
	// Two source entities (one with an edge, one without) + the edge
	// target. Fictional slugs.
	seedEntity(t, st, "boardgame:test-game-2099", "boardgame")
	seedEntity(t, st, "boardgame:other-game-2099", "boardgame")
	seedEntity(t, st, "person:designer-a", "person")
	require.NoError(t, st.CreateEdge(ctx, &store.Edge{
		Type: "designed_by", From: "boardgame:test-game-2099", To: "person:designer-a",
	}))

	post := func(t *testing.T, ids, withEdges []string) batchResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/entities/batch",
			batchRequestBody(t, ids, withEdges)))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got batchResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
		return got
	}
	byID := func(resp batchResponse) map[string]entity {
		m := map[string]entity{}
		for _, e := range resp.Entities {
			m[e.ID] = e
		}
		return m
	}

	t.Run("with_edges expands per entity", func(t *testing.T) {
		t.Parallel()
		m := byID(post(t,
			[]string{"boardgame:test-game-2099", "boardgame:other-game-2099"},
			[]string{"*"}))
		require.Len(t, m["boardgame:test-game-2099"].Edges, 1, "source entity carries its edge")
		assert.Equal(t, "designed_by", m["boardgame:test-game-2099"].Edges[0].Type)
		assert.Equal(t, "person:designer-a", m["boardgame:test-game-2099"].Edges[0].To)
		assert.Empty(t, m["boardgame:other-game-2099"].Edges, "edge-less entity gets an empty list")
	})

	t.Run("default (omitted) returns no edges", func(t *testing.T) {
		t.Parallel()
		m := byID(post(t, []string{"boardgame:test-game-2099"}, nil))
		assert.Empty(t, m["boardgame:test-game-2099"].Edges, "no with_edges → unchanged, no expansion")
	})

	t.Run("type filter excludes non-matching edges", func(t *testing.T) {
		t.Parallel()
		m := byID(post(t, []string{"boardgame:test-game-2099"}, []string{"is_about"}))
		assert.Empty(t, m["boardgame:test-game-2099"].Edges, "filter on is_about excludes designed_by")
	})
}

func Test_BatchEntities_EmptyIds(t *testing.T) {
	t.Parallel()

	body := batchRequestBody(t, []string{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "non-empty")
}

func Test_BatchEntities_TooManyIds(t *testing.T) {
	t.Parallel()

	ids := make([]string, batchMaxIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("boardgame:fake-%d", i)
	}
	body := batchRequestBody(t, ids, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "too_many_ids",
		fmt.Sprintf("got %d", batchMaxIDs+1))
}

func Test_BatchEntities_AtLimitIsAllowed(t *testing.T) {
	t.Parallel()

	ids := make([]string, batchMaxIDs)
	for i := range ids {
		ids[i] = fmt.Sprintf("boardgame:fake-%d", i)
	}
	body := batchRequestBody(t, ids, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "status at exactly %d ids", batchMaxIDs)
}

func Test_BatchEntities_MalformedJSON(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/v1/entities/batch",
		strings.NewReader(`{"ids": [`)) // truncated
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "JSON")
}

func Test_BatchEntities_GetReturns405OnBatchPath(t *testing.T) {
	t.Parallel()

	// Sanity check: GET /v1/entities/batch must not fall through to the
	// {id} matcher and return a "no entity with id batch" 404. The carve-out
	// in api.go reserves this path for the POST handler.
	req := httptest.NewRequest(http.MethodGet, "/v1/entities/batch", nil)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"GET /v1/entities/batch should return 405")
	assert.Equal(t, "POST", rec.Header().Get("Allow"))
	assertErrorEnvelope(t, rec, http.StatusMethodNotAllowed, "method_not_allowed", "POST")
}

// assertErrorEnvelope decodes the response body as the canonical error
// envelope and asserts status, error code, and a substring of the message.
// Reused across the validation-error tests to keep them tight.
//
// Uses require for the status + decode preconditions (any further check
// nil-derefs without them) and assert for the body fields (multiple
// mismatches all surface). Per ADR-0007.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode, msgSubstr string) {
	t.Helper()
	require.Equal(t, wantStatus, rec.Code, "status (body=%s)", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode error envelope")
	assert.False(t, body.OK, "ok")
	assert.Equal(t, wantCode, body.Error)
	assert.Contains(t, body.Message, msgSubstr, "message")
}
