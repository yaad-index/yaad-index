// edges tool — single-hop edge query around an entity.

package mcp

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerEdges(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("edges",
		mcp.WithDescription(
			"Single-hop edge query. Wraps "+
				"`GET /v1/edges?entity_id=X[&edge_types=...][&direction=...]`. "+
				"Returns the typed edges from/to the entity in one round-trip. "+
				"Distinct from `get_entity_with_context`: this is the flat "+
				"one-hop surface; `get_entity_with_context` does N-hop BFS. "+
				"Direction defaults to `out` (entity_id is from_id); `in` "+
				"returns inbound edges; `both` returns both combined. "+
				"`edge_types` is an optional allowlist.",
		),
		mcp.WithString("entity_id",
			mcp.Required(),
			mcp.Description("Full entity id in `<kind>:<local-id>` shape."),
		),
		mcp.WithArray("edge_types",
			mcp.Description("Optional allowlist of edge types."),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithString("direction",
			mcp.Description("Edge direction relative to entity_id. `out` (default) | `in` | `both`."),
			mcp.Enum("out", "in", "both"),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("entity_id", "")
		if id == "" {
			return mcp.NewToolResultError("`entity_id` is required"), nil
		}
		q := url.Values{}
		q.Set("entity_id", id)
		if direction := req.GetString("direction", ""); direction != "" {
			q.Set("direction", direction)
		}
		if types, ok := req.GetArguments()["edge_types"].([]any); ok && len(types) > 0 {
			for _, t := range types {
				if s, ok := t.(string); ok && s != "" {
					q.Add("edge_types", s)
				}
			}
		}
		return b.callTool(ctx, "GET", fmt.Sprintf("/v1/edges?%s", q.Encode()), nil)
	})
}
