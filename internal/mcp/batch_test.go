package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// batchFakeHandler responds 200 for paths whose id does NOT contain
// "fail" and a 409 canonical-error envelope for ids containing "fail",
// recording each "METHOD path" it served so tests can assert the
// fan-out shape + per-id aggregation.
type batchFakeHandler struct {
	calls []string
}

func (h *batchFakeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls = append(h.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.URL.Path, "fail") {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok":false,"error":"conflict","message":"must archive before delete"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

type batchResultEnvelope struct {
	OK        bool `json:"ok"`
	Total     int  `json:"total"`
	Succeeded int  `json:"succeeded"`
	Failed    int  `json:"failed"`
	Results   []struct {
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Error  string `json:"error"`
	} `json:"results"`
}

func decodeBatchResult(t *testing.T, res *mcp.CallToolResult) batchResultEnvelope {
	t.Helper()
	require.NotNil(t, res)
	require.False(t, res.IsError, "batch tool should not surface as an MCP error: %+v", res)
	require.Len(t, res.Content, 1)
	txt, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", res.Content[0])
	var out batchResultEnvelope
	require.NoError(t, json.Unmarshal([]byte(txt.Text), &out))
	return out
}

func batchReq(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// TestArchiveEntities_PartialFailureAggregates pins the #383 contract: a
// per-id failure doesn't abort the batch, and the aggregate reports
// per-id outcomes + counts. Also pins the archive path/verb synthesis.
func TestArchiveEntities_PartialFailureAggregates(t *testing.T) {
	t.Parallel()
	h := &batchFakeHandler{}
	b := newBridge(h)
	res, err := archiveEntitiesHandler(b)(context.Background(), batchReq(map[string]any{
		"entity_ids": []any{"gmail:ok-1", "gmail:fail-2", "gmail:ok-3"},
	}))
	require.NoError(t, err)
	got := decodeBatchResult(t, res)

	assert.False(t, got.OK, "aggregate ok=false when any item fails")
	assert.Equal(t, 3, got.Total)
	assert.Equal(t, 2, got.Succeeded)
	assert.Equal(t, 1, got.Failed)
	require.Len(t, got.Results, 3)
	assert.Equal(t, "gmail:ok-1", got.Results[0].ID)
	assert.True(t, got.Results[0].OK)
	assert.False(t, got.Results[1].OK)
	assert.Equal(t, http.StatusConflict, got.Results[1].Status)
	assert.Contains(t, got.Results[1].Error, "conflict")
	assert.True(t, got.Results[2].OK)
	assert.Equal(t, "POST /v1/entities/gmail:ok-1/archive", h.calls[0], "archive verb+path synthesis")
}

func TestArchiveEntities_AllSucceed(t *testing.T) {
	t.Parallel()
	h := &batchFakeHandler{}
	b := newBridge(h)
	res, err := archiveEntitiesHandler(b)(context.Background(), batchReq(map[string]any{
		"entity_ids": []any{"x:a", "x:b"},
	}))
	require.NoError(t, err)
	got := decodeBatchResult(t, res)
	assert.True(t, got.OK)
	assert.Equal(t, 2, got.Succeeded)
	assert.Equal(t, 0, got.Failed)
}

func TestDeleteEntities_UsesDeleteVerbAndPath(t *testing.T) {
	t.Parallel()
	h := &batchFakeHandler{}
	b := newBridge(h)
	_, err := deleteEntitiesHandler(b)(context.Background(), batchReq(map[string]any{
		"entity_ids": []any{"x:a"},
	}))
	require.NoError(t, err)
	require.Len(t, h.calls, 1)
	assert.Equal(t, "DELETE /v1/entities/x:a", h.calls[0])
}

func TestTaskResolveBatch_UsesResolvePath(t *testing.T) {
	t.Parallel()
	h := &batchFakeHandler{}
	b := newBridge(h)
	_, err := taskResolveBatchHandler(b)(context.Background(), batchReq(map[string]any{
		"task_ids": []any{"task-1"},
	}))
	require.NoError(t, err)
	require.Len(t, h.calls, 1)
	assert.Equal(t, "POST /v1/tasks/task-1/resolve", h.calls[0])
}

// TestBatchHandlers_EmptyIDsRejected pins that an absent or empty id list
// surfaces an MCP error and never touches the bridge.
func TestBatchHandlers_EmptyIDsRejected(t *testing.T) {
	t.Parallel()
	h := &batchFakeHandler{}
	b := newBridge(h)

	absent, err := archiveEntitiesHandler(b)(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	assert.True(t, absent.IsError, "absent entity_ids → MCP error")

	empty, err := archiveEntitiesHandler(b)(context.Background(), batchReq(map[string]any{
		"entity_ids": []any{},
	}))
	require.NoError(t, err)
	assert.True(t, empty.IsError, "empty entity_ids → MCP error")

	assert.Empty(t, h.calls, "bridge must not be invoked on validation failure")
}

// TestArchiveEntities_InvalidEntriesCountedAsFailed pins the #383 review
// fix: a non-string / empty entry is NOT silently dropped — it surfaces
// as a failed result so `total` covers the caller's full input (critical
// for destructive batches), and only valid ids reach the bridge.
func TestArchiveEntities_InvalidEntriesCountedAsFailed(t *testing.T) {
	t.Parallel()
	h := &batchFakeHandler{}
	b := newBridge(h)
	res, err := archiveEntitiesHandler(b)(context.Background(), batchReq(map[string]any{
		"entity_ids": []any{"x:a", "", 42},
	}))
	require.NoError(t, err)
	got := decodeBatchResult(t, res)

	assert.False(t, got.OK)
	assert.Equal(t, 3, got.Total, "total covers the original input incl invalid entries")
	assert.Equal(t, 1, got.Succeeded)
	assert.Equal(t, 2, got.Failed)
	require.Len(t, got.Results, 3)
	assert.True(t, got.Results[0].OK, "valid id processed")
	assert.False(t, got.Results[1].OK)
	assert.Contains(t, got.Results[1].Error, "invalid id")
	assert.False(t, got.Results[2].OK)
	assert.Contains(t, got.Results[2].Error, "invalid id")
	assert.Equal(t, []string{"POST /v1/entities/x:a/archive"}, h.calls, "only the valid id reaches the bridge")
}
