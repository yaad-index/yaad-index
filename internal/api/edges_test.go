package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func edgeRequestBody(t *testing.T, req edgeRequest) io.Reader {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err, "marshal edge request")
	return strings.NewReader(string(b))
}

func Test_Edges_HappyPath(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEntity(t, st, "person:martin-wallace", "person")

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
		Metadata: map[string]any{"role": "lead designer"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got edgeResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")
	assert.True(t, got.OK)

	assert.Equal(t, "designed_by", got.Edge.Type)
	assert.Equal(t, "boardgame:brass-birmingham", got.Edge.From)
	assert.Equal(t, "person:martin-wallace", got.Edge.To)
	assert.Equal(t, "lead designer", got.Edge.Metadata["role"])

	require.Len(t, got.Edge.Provenance, 1, "edge.provenance: want exactly 1 entry on a fresh stub write")
	prov := got.Edge.Provenance[0]
	assert.NotEmpty(t, prov.Source, "edge.provenance[0].source")
	assert.True(t, prov.OK, "edge.provenance[0].ok")
	_, err := time.Parse(time.RFC3339, prov.FetchedAt)
	assert.NoError(t, err, "edge.provenance[0].fetched_at: want RFC3339, got %q", prov.FetchedAt)
}

// Test_Edges_EchoedTypeIsRegisteredKind locks in the same closure invariant
// as a prior PR / a prior PR: an edge produced by the API must reference an edge_kind
// declared by /v1/kinds. Today this is enforced at validate time
// (isRegisteredEdgeKind), so any successful response is by construction
// closure-consistent — the test ensures a future refactor that drops the
// validation step still trips CI before shipping.
func Test_Edges_EchoedTypeIsRegisteredKind(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "book:lotr", "book")
	seedEntity(t, st, "person:tolkien", "person")

	body := edgeRequestBody(t, edgeRequest{
		Type: "authored_by",
		From: "book:lotr",
		To: "person:tolkien",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got edgeResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode response")

	declared := make(map[string]struct{}, len(testSeedEdgeKinds))
	for _, k := range testSeedEdgeKinds {
		declared[k] = struct{}{}
	}
	_, ok := declared[got.Edge.Type]
	assert.True(t, ok, "edge.type=%q not in testSeedEdgeKinds (closure invariant)", got.Edge.Type)
}

func Test_Edges_MissingType(t *testing.T) {
	t.Parallel()

	body := edgeRequestBody(t, edgeRequest{
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "type is required")
}

func Test_Edges_UnknownType(t *testing.T) {
	t.Parallel()

	body := edgeRequestBody(t, edgeRequest{
		Type: "rated_by",
		From: "boardgame:brass-birmingham",
		To: "person:martin-wallace",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument",
		"not in the registered edge_kinds")
}

func Test_Edges_MissingFrom(t *testing.T) {
	t.Parallel()

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		To: "person:martin-wallace",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "from is required")
}

func Test_Edges_MissingTo(t *testing.T) {
	t.Parallel()

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "to is required")
}

func Test_Edges_MalformedJSON(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/v1/edges", strings.NewReader(`{`))
	rec := httptest.NewRecorder()
	newAPI(t).ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "JSON")
}

func Test_Edges_MissingFromEntity_Returns422(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	// Only seed `to`; `from` is unknown.
	seedEntity(t, st, "person:martin-wallace", "person")

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:no-such-thing",
		To: "person:martin-wallace",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "missing_entity",
		"boardgame:no-such-thing")
}

func Test_Edges_MissingToEntity_Returns422(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")

	body := edgeRequestBody(t, edgeRequest{
		Type: "designed_by",
		From: "boardgame:brass-birmingham",
		To: "person:no-such-person",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "missing_entity",
		"person:no-such-person")
}

// --- POST /v1/edges/update-target (#304 Cut B) -------------------------

func updateEdgeTargetBody(t *testing.T, req updateEdgeTargetRequest) io.Reader {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err, "marshal update-target request")
	return strings.NewReader(string(b))
}

func Test_UpdateEdgeTarget_HappyPath(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "book:lotr", "book")
	seedEntity(t, st, "person:tolkien", "person")
	seedEntity(t, st, "person:tolkien-real", "person")

	createBody := edgeRequestBody(t, edgeRequest{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
		Metadata: map[string]any{"role": "primary"},
	})
	createReq := httptest.NewRequest(http.MethodPost, "/v1/edges", createBody)
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusOK, createRec.Code, "seed body=%s", createRec.Body.String())

	body := updateEdgeTargetBody(t, updateEdgeTargetRequest{
		From: "book:lotr", Type: "authored_by",
		OldTarget: "person:tolkien", NewTarget: "person:tolkien-real",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/update-target", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got edgeResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "person:tolkien-real", got.Edge.To)
	assert.Equal(t, "book:lotr", got.Edge.From)
	assert.Equal(t, "authored_by", got.Edge.Type)
	assert.Equal(t, "primary", got.Edge.Metadata["role"], "metadata preserved across rewrite")
}

func Test_UpdateEdgeTarget_StaleTupleReturns409(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "book:lotr", "book")
	seedEntity(t, st, "person:tolkien", "person")
	seedEntity(t, st, "person:tolkien-real", "person")

	body := updateEdgeTargetBody(t, updateEdgeTargetRequest{
		From: "book:lotr", Type: "authored_by",
		OldTarget: "person:never-existed", NewTarget: "person:tolkien-real",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/update-target", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusConflict, "edge_stale",
		"does not match a current edge")
}

func Test_UpdateEdgeTarget_MissingNewTargetReturns422(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "book:lotr", "book")
	seedEntity(t, st, "person:tolkien", "person")

	// Seed the edge.
	createBody := edgeRequestBody(t, edgeRequest{
		Type: "authored_by", From: "book:lotr", To: "person:tolkien",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/v1/edges", createBody)
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusOK, createRec.Code)

	body := updateEdgeTargetBody(t, updateEdgeTargetRequest{
		From: "book:lotr", Type: "authored_by",
		OldTarget: "person:tolkien", NewTarget: "person:does-not-exist",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/update-target", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "missing_entity",
		"person:does-not-exist")
}

func Test_UpdateEdgeTarget_RejectsNoop(t *testing.T) {
	t.Parallel()

	h, _ := newAPIWithStore(t)
	body := updateEdgeTargetBody(t, updateEdgeTargetRequest{
		From: "book:lotr", Type: "authored_by",
		OldTarget: "person:tolkien", NewTarget: "person:tolkien",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/update-target", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument",
		"no-op")
}

func Test_UpdateEdgeTarget_RejectsMissingFields(t *testing.T) {
	t.Parallel()

	h, _ := newAPIWithStore(t)
	cases := []struct {
		name    string
		req     updateEdgeTargetRequest
		hint    string
	}{
		{"empty from", updateEdgeTargetRequest{Type: "authored_by", OldTarget: "a", NewTarget: "b"}, "from"},
		{"empty type", updateEdgeTargetRequest{From: "book:lotr", OldTarget: "a", NewTarget: "b"}, "type"},
		{"empty old_target", updateEdgeTargetRequest{From: "book:lotr", Type: "authored_by", NewTarget: "b"}, "old_target"},
		{"empty new_target", updateEdgeTargetRequest{From: "book:lotr", Type: "authored_by", OldTarget: "a"}, "new_target"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := updateEdgeTargetBody(t, tc.req)
			req := httptest.NewRequest(http.MethodPost, "/v1/edges/update-target", body)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", tc.hint)
		})
	}
}

func Test_UpdateEdgeTarget_RejectsUnknownEdgeType(t *testing.T) {
	t.Parallel()

	h, _ := newAPIWithStore(t)
	body := updateEdgeTargetBody(t, updateEdgeTargetRequest{
		From: "book:lotr", Type: "totally-not-registered",
		OldTarget: "person:tolkien", NewTarget: "person:tolkien-real",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges/update-target", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument",
		"edge_kinds")
}
