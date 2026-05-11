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
