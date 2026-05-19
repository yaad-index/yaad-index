// Bridge is the in-process http.Request synthesizer that
// each MCP tool handler uses to invoke the daemon's existing
// HTTP routes. No network loopback, no handler carving — the
// bridge calls `apiHandler.ServeHTTP(recorder, req)` against
// the SAME mux the daemon serves on its public port, so
// auth + middleware + per-route logic stay identical.
//
// The Authorization header is pulled from the per-request
// context (set by extractAuthHeader at MCP entry) and
// re-attached to the synthesized request so the inner
// route's auth gate sees the same JWT.

package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
)

// bridge wraps the daemon's mux for in-process invocation.
// Constructed once at MCP server bootstrap; shared across
// every tool handler.
type bridge struct {
	handler http.Handler
}

func newBridge(handler http.Handler) *bridge {
	return &bridge{handler: handler}
}

// callResult is the bridge's normalized response shape — the
// HTTP status + body bytes the inner route emitted. Tool
// handlers translate these into mcp.CallToolResult values.
type callResult struct {
	status int
	body   []byte
}

// call synthesizes an http.Request with the given method +
// path + body, attaches the per-request Authorization header
// from ctx, runs it through the daemon's mux via
// httptest.ResponseRecorder, and returns the recorded
// status + body. Errors are reserved for synthesis-side
// failures (request construction, body read); inner-route
// HTTP error statuses are returned in callResult.status so
// the caller can shape them into MCP error results.
func (b *bridge) call(ctx context.Context, method, path string, body io.Reader) (*callResult, error) {
	req := httptest.NewRequest(method, path, body)
	req = req.WithContext(ctx)
	if auth := authHeaderFromContext(ctx); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, req)
	respBody, err := io.ReadAll(rec.Body)
	if err != nil {
		return nil, fmt.Errorf("read recorded response body: %w", err)
	}
	return &callResult{status: rec.Code, body: respBody}, nil
}

// isSuccess reports whether the recorded status falls in the
// 2xx range. Convenience for tool handlers that branch on
// success vs error shape.
func (r *callResult) isSuccess() bool {
	return r.status >= 200 && r.status < 300
}

// bodyString returns the recorded body as a string. The MCP
// tool result shape carries strings; the daemon's HTTP
// surface emits JSON; this is the bridge between them. No
// re-parse — the JSON the daemon emitted is passed through
// to the MCP caller verbatim.
func (r *callResult) bodyString() string {
	return string(bytes.TrimRight(r.body, "\n"))
}
