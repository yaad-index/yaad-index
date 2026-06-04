package mcp

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queryCaptureHandler records the request path + raw query so tests can
// assert what a tool synthesized at the bridge boundary, then replies 200.
type queryCaptureHandler struct {
	path  string
	query string
}

func (h *queryCaptureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.path = r.URL.Path
	h.query = r.URL.RawQuery
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true,"entities":[]}`))
}

func invokeNeedsFill(t *testing.T, args map[string]any) *queryCaptureHandler {
	t.Helper()
	h := &queryCaptureHandler{}
	b := newBridge(h)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := needsFillHandler(b)(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "needs_fill should not error: %+v", res)
	return h
}

// TestNeedsFill_SourceAndKindFilters pins #385: the source / kind args
// thread onto the GET /v1/needs-fill query (AND-composed when both set).
func TestNeedsFill_SourceAndKindFilters(t *testing.T) {
	t.Parallel()
	h := invokeNeedsFill(t, map[string]any{"source": "gmail", "kind": "person"})
	assert.Equal(t, "/v1/needs-fill", h.path)
	vals, err := url.ParseQuery(h.query)
	require.NoError(t, err)
	assert.Equal(t, "gmail", vals.Get("source"))
	assert.Equal(t, "person", vals.Get("kind"))
}

// TestNeedsFill_NoFiltersOmitsParams pins the preserved default: absent
// source / kind don't appear in the query (full-list behavior).
func TestNeedsFill_NoFiltersOmitsParams(t *testing.T) {
	t.Parallel()
	h := invokeNeedsFill(t, map[string]any{})
	assert.Equal(t, "/v1/needs-fill", h.path)
	vals, _ := url.ParseQuery(h.query)
	_, hasSource := vals["source"]
	_, hasKind := vals["kind"]
	assert.False(t, hasSource, "source omitted when not supplied")
	assert.False(t, hasKind, "kind omitted when not supplied")
}
