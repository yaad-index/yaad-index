package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureHandler records the latest request method/path/body so
// tests can assert what the MCP tool synthesized at the bridge
// boundary. Responds with a minimal 200 JSON envelope so the
// tool handler returns success without further interpretation.
type captureHandler struct {
	method string
	path   string
	body   []byte
}

func (h *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.method = r.Method
	h.path = r.URL.Path
	h.body, _ = io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true,"state":"complete","status":"complete"}`))
}

func invokeIngest(t *testing.T, args map[string]any) (*captureHandler, *mcp.CallToolResult) {
	t.Helper()
	h := &captureHandler{}
	b := newBridge(h)
	req := mcp.CallToolRequest{}
	req.Params.Name = "ingest"
	req.Params.Arguments = args
	res, err := ingestHandler(b)(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return h, res
}

// TestIngestHandler_DefaultsOmitForceRefetch pins #372 default: when
// the caller doesn't pass force_refetch, the synthesized POST body
// does NOT carry the field (preserves daemon's omitempty semantics
// + matches the prior call shape so existing flows are untouched).
func TestIngestHandler_DefaultsOmitForceRefetch(t *testing.T) {
	t.Parallel()
	h, res := invokeIngest(t, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Tehran",
	})

	assert.False(t, res.IsError, "default call must succeed: %+v", res)
	assert.Equal(t, "POST", h.method)
	assert.Equal(t, "/v1/ingest", h.path)

	var got map[string]any
	require.NoError(t, json.Unmarshal(h.body, &got))
	assert.Equal(t, "https://en.wikipedia.org/wiki/Tehran", got["url"])
	_, present := got["force_refetch"]
	assert.False(t, present, "force_refetch must be absent when caller omitted it")
}

// TestIngestHandler_PassesForceRefetchTrue pins #372: when the
// caller sets force_refetch=true, the synthesized POST body carries
// the boolean through to the daemon's /v1/ingest route.
func TestIngestHandler_PassesForceRefetchTrue(t *testing.T) {
	t.Parallel()
	h, res := invokeIngest(t, map[string]any{
		"url":           "https://en.wikipedia.org/wiki/Tehran",
		"force_refetch": true,
	})

	assert.False(t, res.IsError, "force_refetch=true call must succeed: %+v", res)
	var got map[string]any
	require.NoError(t, json.Unmarshal(h.body, &got))
	assert.Equal(t, true, got["force_refetch"], "force_refetch must pass through as true")
	assert.Equal(t, "https://en.wikipedia.org/wiki/Tehran", got["url"])
}

// TestIngestHandler_ForceRefetchFalseOmitted pins the default-state
// path: caller explicitly passes false → still omitted (matches the
// `omitempty` shape on the daemon's ingestRequest struct, keeps the
// wire body minimal). The daemon treats absent + false identically.
func TestIngestHandler_ForceRefetchFalseOmitted(t *testing.T) {
	t.Parallel()
	h, res := invokeIngest(t, map[string]any{
		"url":           "https://en.wikipedia.org/wiki/Tehran",
		"force_refetch": false,
	})

	assert.False(t, res.IsError)
	var got map[string]any
	require.NoError(t, json.Unmarshal(h.body, &got))
	_, present := got["force_refetch"]
	assert.False(t, present, "force_refetch=false should not be emitted (matches daemon omitempty)")
}

// TestIngestHandler_MissingURLRejected keeps the url-required guard
// pinned so the force_refetch addition doesn't accidentally relax
// the existing validation path.
func TestIngestHandler_MissingURLRejected(t *testing.T) {
	t.Parallel()
	h, res := invokeIngest(t, map[string]any{"force_refetch": true})
	assert.True(t, res.IsError, "missing url must surface as MCP error")
	assert.Empty(t, h.method, "bridge must not be invoked on validation failure")
}
