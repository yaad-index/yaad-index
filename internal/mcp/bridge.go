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
//
// **Synthetic-host caveat.** `httptest.NewRequest` builds
// a request with the synthetic host `example.com`; any tool
// that wants to emit a self-referential URL back to the
// caller (e.g. an attachment download link) MUST source
// the host from operator config or X-Forwarded-Host
// headers, NOT from `r.Host` — the bridged request never
// carries the operator's real host. Tools added in Cut 2
// only return JSON shapes the daemon already produces, so
// this caveat doesn't bite today; documented for any future
// tool that constructs URLs.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/mark3labs/mcp-go/mcp"
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

// callWithHeaders synthesizes an http.Request with the given
// method + path + body + extra headers, attaches the per-
// request Authorization header from ctx, runs it through the
// daemon's mux via httptest.ResponseRecorder, and returns the
// recorded status + body. Extra headers carry semantics that
// route contracts read from headers rather than the JSON body
// (e.g. `If-Match` for the UGC section-edit etag concurrency
// contract). Errors are reserved for synthesis-side failures
// (request construction, body read); inner-route HTTP error
// statuses are returned in callResult.status so the caller
// can shape them into MCP error results.
func (b *bridge) callWithHeaders(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (*callResult, error) {
	req := httptest.NewRequest(method, path, body)
	req = req.WithContext(ctx)
	if auth := authHeaderFromContext(ctx); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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

// asMCPError projects the inner route's error response body
// into a structured MCP error text. The daemon's canonical
// error envelope is `{"ok": false, "error": "<code>",
// "message": "<msg>"}`; this helper parses that shape and
// returns `"<code>: <msg>"` so MCP clients see the error
// code + a concise message rather than the verbose JSON
// envelope. Falls back to `HTTP <status>: <body>` when the
// body doesn't parse as the canonical envelope (e.g. a
// plain-text 401 from upstream middleware).
//
// Centralized so 32+ tools share one error-projection
// contract — prevents per-tool divergence in how internal
// errors surface to MCP callers.
func (r *callResult) asMCPError() string {
	var env struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(r.body, &env); err == nil && env.Error != "" {
		if env.Message != "" {
			return fmt.Sprintf("%s: %s", env.Error, env.Message)
		}
		return env.Error
	}
	return fmt.Sprintf("HTTP %d: %s", r.status, r.bodyString())
}

// callTool is the standard call → MCP-result projection
// pipeline every tool handler uses. Performs the bridge
// call, projects the result into an MCP CallToolResult,
// and returns. Tool handlers that just need to forward an
// HTTP request collapse to a single line; tools with
// per-call argument validation invoke b.callTool after
// validating their args.
//
// The (error) return on the bridge call itself stays
// captured as an MCP error result rather than propagating
// as a Go error — MCP-spec semantics: tool-call errors live
// inside the result envelope, not in the JSON-RPC error
// channel.
func (b *bridge) callTool(ctx context.Context, method, path string, body io.Reader) (*mcp.CallToolResult, error) {
	return b.callToolWithHeaders(ctx, method, path, body, nil)
}

// callToolWithHeaders is the header-aware variant of callTool.
// Same projection contract; additional request headers are
// merged onto the synthesized request. Reserved for routes
// whose contract carries semantics in headers (UGC etag
// concurrency via `If-Match`).
func (b *bridge) callToolWithHeaders(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (*mcp.CallToolResult, error) {
	res, err := b.callWithHeaders(ctx, method, path, body, headers)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("bridge call: %v", err)), nil
	}
	if !res.isSuccess() {
		return mcp.NewToolResultError(res.asMCPError()), nil
	}
	return mcp.NewToolResultText(res.bodyString()), nil
}
