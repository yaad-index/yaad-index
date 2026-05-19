// System metadata tools — structure, kinds, plugins,
// reindex. All thin wrappers around their corresponding
// `/v1/...` routes; minimal per-tool boilerplate.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerStructure(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("structure",
		mcp.WithDescription(
			"Return the daemon's canonical structure: known kinds + "+
				"canonical edge types + per-kind gap declarations. "+
				"Operators use this to author workflows against the "+
				"live registry; agents use it to validate canonical "+
				"references before emitting them.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/structure", nil)
	})
}

func registerKinds(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("kinds",
		mcp.WithDescription(
			"Return the operator's canonical-kinds registry — the set "+
				"of kinds the daemon recognizes plus per-kind gap "+
				"declarations. Subset of `structure`; useful when only "+
				"the kind list is needed.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/kinds", nil)
	})
}

func registerPlugins(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("plugins",
		mcp.WithDescription(
			"List registered yaad-index plugins with their capabilities "+
				"(canonical kinds emitted, edge types emitted, gap specs, "+
				"search support flag, etc.). Used by agents to discover "+
				"which sources can ingest a given URL shape.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/plugins", nil)
	})
}

func registerReindex(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("reindex",
		mcp.WithDescription(
			"Trigger a vault → DB reindex run. POSTs to `/v1/reindex`; "+
				"the daemon walks every vault file + refreshes the derived "+
				"index. Returns the per-file counts (created / updated / "+
				"unchanged / errored). Long-running on large vaults; the "+
				"call is synchronous.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Empty JSON body — the route accepts POST without args.
		body, _ := json.Marshal(map[string]any{})
		return b.callTool(ctx, "POST", "/v1/reindex", bytes.NewReader(body))
	})
}
