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
				"disambiguation response (e.g. `wikipedia: Tehran`).",
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("URL to ingest, or `<plugin>: <id>` shorthand."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		u := req.GetString("url", "")
		if u == "" {
			return mcp.NewToolResultError("`url` is required"), nil
		}
		body, err := json.Marshal(map[string]any{"url": u})
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/ingest", bytes.NewReader(body))
	})
}
