package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/reindex"
	"github.com/yaad-index/yaad-index/internal/store"
)

// fakeReindexer is a minimal reindex.Reindexer stand-in for HTTP-level
// tests. Records the mode it was invoked with and returns a canned
// summary or error.
type fakeReindexer struct {
	gotMode reindex.Mode
	summary reindex.Summary
	err error
}

func (f *fakeReindexer) Run(_ context.Context, mode reindex.Mode) (reindex.Summary, error) {
	f.gotMode = mode
	return f.summary, f.err
}

func newReindexAPI(t *testing.T, runner reindexRunner) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewHandlerWithRegistry(logger, st, plugins.NewRegistry(),
		WithReindexHandler(HandleReindex(logger, runner)))
}

func postReindex(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest(http.MethodPost, "/v1/reindex", bodyReader)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/v1/reindex", nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReindex_HTTP_DefaultsToIncremental(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{summary: reindex.Summary{Mode: "incremental", Scanned: 3, Parsed: 1}}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, "")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, reindex.Incremental, fake.gotMode)

	var got reindex.Summary
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "incremental", got.Mode)
	assert.Equal(t, 3, got.Scanned)
	assert.Equal(t, 1, got.Parsed)
}

func TestReindex_HTTP_ExplicitIncrementalMode(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{summary: reindex.Summary{Mode: "incremental"}}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, `{"mode":"incremental"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, reindex.Incremental, fake.gotMode)
}

func TestReindex_HTTP_FullMode(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{summary: reindex.Summary{Mode: "full", EntitiesCreated: 5}}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, `{"mode":"full"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, reindex.Full, fake.gotMode)

	var got reindex.Summary
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "full", got.Mode)
	assert.Equal(t, 5, got.EntitiesCreated)
}

func TestReindex_HTTP_RejectsUnknownMode(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, `{"mode":"sideways"}`)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "sideways")
}

func TestReindex_HTTP_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, `{`)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "decode")
}

func TestReindex_HTTP_RejectsUnknownFields(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, `{"mode":"full","unknown":1}`)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "unknown")
}

func TestReindex_HTTP_500OnRunnerError(t *testing.T) {
	t.Parallel()

	fake := &fakeReindexer{err: errors.New("disk on fire")}
	h := newReindexAPI(t, fake)

	rec := postReindex(t, h, "")
	assertErrorEnvelope(t, rec, http.StatusInternalServerError, "internal_error", "disk on fire")
}

// TestReindex_HTTP_Unregistered locks the negative case: when
// WithReindexHandler is omitted (the default), POST /v1/reindex
// returns 404 from the mux — no half-wired endpoint, no panic.
func TestReindex_HTTP_Unregistered(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), st, plugins.NewRegistry())

	rec := postReindex(t, h, "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}
