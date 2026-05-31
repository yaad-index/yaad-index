// ingest tool — fetch a URL into yaad-index, returning the
// entity id + state + per-plugin disambiguation options.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerIngest(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("ingest",
		mcp.WithDescription(
			"Ingest a URL into yaad-index. Returns the entity id, state "+
				"(complete / needs_fill / disambiguation / queued), plus "+
				"options[] when the URL resolves to multiple candidates. "+
				"Accepts `<plugin>: <id>` shorthand from a prior "+
				"disambiguation response (e.g. `wikipedia: Tehran`). "+
				"Pass `force_refetch: true` to bypass the cache and "+
				"re-run the plugin Fetch even when a fresh entity row "+
				"exists.",
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("URL to ingest, or `<plugin>: <id>` shorthand."),
		),
		mcp.WithBoolean("force_refetch",
			mcp.Description("When true, skip the cache lookup and force a "+
				"fresh plugin Fetch. Default false."),
		),
	)
	s.AddTool(tool, ingestHandler(b))
}

// ingestHandler is the shared callback the `ingest` tool dispatches
// through. Extracted from the closure so unit tests can construct
// a CallToolRequest + invoke the handler directly without driving
// the full MCP server transport.
func ingestHandler(b *bridge) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		u := req.GetString("url", "")
		if u == "" {
			return mcp.NewToolResultError("`url` is required"), nil
		}
		payload := map[string]any{"url": u}
		if req.GetBool("force_refetch", false) {
			payload["force_refetch"] = true
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/ingest", bytes.NewReader(body))
	}
}
