// get_entity is the Cut-1 representative MCP tool per #101.
// Wraps `GET /v1/entities/{id}` — chosen as the sample
// because: (a) it exercises the JWT auth path end-to-end
// (every read tool goes through `protect`), (b) the input
// schema is single-field (one `id` string), (c) the response
// is straight JSON that round-trips cleanly through the
// MCP text result shape.
//
// Subsequent tools in Cut 2 follow this exact pattern:
// `mcp.NewTool` declares the input schema, the handler
// validates required args, calls the bridge against the
// matching `/v1/...` route, and shapes the recorded
// response into an mcp.CallToolResult.

package mcp

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerGetEntity(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("get_entity",
		mcp.WithDescription(
			"Fetch a yaad-index entity by id. Returns `{id, kind, data, "+
				"provenance, edges}`. The `is_about` edge type is expanded "+
				"inline (canonical-axis traversal); other edge types are "+
				"not currently surfaced (yaad-index API limitation, follow-up "+
				"tracked). Call `get_entity(<edge.to>)` to walk from a "+
				"source-shape entity to its canonical stub.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id, e.g. `wikipedia:tehran` or `person:alex-example`."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		res, err := b.call(ctx, "GET", "/v1/entities/"+url.PathEscape(id), nil)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("bridge call: %v", err)), nil
		}
		if !res.isSuccess() {
			return mcp.NewToolResultError(fmt.Sprintf("HTTP %d: %s", res.status, res.bodyString())), nil
		}
		return mcp.NewToolResultText(res.bodyString()), nil
	})
}
