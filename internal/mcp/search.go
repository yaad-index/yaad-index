// Search tools — local full-text search + plugin-federated
// upstream search. Both wrap query+limit query-string
// shapes; search_upstream additionally takes a plugins
// allowlist + per-plugin timeout via JSON body.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerSearchLocal(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("search_local",
		mcp.WithDescription(
			"Full-text search across the LOCAL yaad-index — entities "+
				"already ingested (not the upstream sources). Returns "+
				"`{results, total, limit, offset}` where each result is "+
				"`{id, kind, snippet, score}` — call `get_entity(id)` for "+
				"any id to load full state. Use this when looking for "+
				"already-ingested entities by keyword; use `ingest(url)` "+
				"when the goal is to FETCH new content from upstream.",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Text query — full-text searched across entity bodies."),
		),
		mcp.WithString("kind",
			mcp.Description("Optional kind filter. When set, only results matching the kind are returned."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Max results to return. Defaults to 20."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := req.GetString("query", "")
		if query == "" {
			return mcp.NewToolResultError("`query` is required"), nil
		}
		q := url.Values{}
		q.Set("q", query)
		if kind := req.GetString("kind", ""); kind != "" {
			q.Set("kind", kind)
		}
		if limit := req.GetFloat("limit", 0); limit > 0 {
			q.Set("limit", fmt.Sprintf("%d", int(limit)))
		}
		return b.callTool(ctx, "GET", "/v1/search?"+q.Encode(), nil)
	})
}

func registerSearchUpstream(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("search_upstream",
		mcp.WithDescription(
			"Plugin-federated search across UPSTREAM sources — fans "+
				"the query out to every yaad-index plugin that opted in via "+
				"`Capabilities.SupportsSearch=true`. Returns `{results, "+
				"per_plugin_status, query, limit, per_plugin_timeout_seconds}`. "+
				"A single plugin timeout / failure does NOT fail the call — "+
				"`per_plugin_status` surfaces per-plugin outcome. Use this "+
				"for disambiguation flows when the agent has a topic string "+
				"and needs candidates to pick from before calling `ingest`.",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query string."),
		),
		mcp.WithArray("plugins",
			mcp.Description("Optional explicit plugin-name allowlist. Omitted/empty → federate to every opted-in plugin."),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithNumber("limit",
			mcp.Description("Per-plugin candidate cap. Defaults to 10; daemon caps at 50."),
		),
		mcp.WithNumber("per_plugin_timeout_seconds",
			mcp.Description("Per-plugin wall-clock budget. Defaults to 5; daemon caps at 30."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := req.GetString("query", "")
		if query == "" {
			return mcp.NewToolResultError("`query` is required"), nil
		}
		args := map[string]any{"query": query}
		// Empty array is the same intent as omit ("federate to
		// every opted-in plugin") on the daemon side — drop it
		// so we don't send `plugins: []` which the route's
		// codepath treats as "filter to zero plugins."
		if v, ok := req.GetArguments()["plugins"].([]any); ok && len(v) > 0 {
			args["plugins"] = v
		}
		if limit := req.GetFloat("limit", 0); limit > 0 {
			args["limit"] = int(limit)
		}
		if timeout := req.GetFloat("per_plugin_timeout_seconds", 0); timeout > 0 {
			args["per_plugin_timeout_seconds"] = timeout
		}
		body, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/search/upstream", bytes.NewReader(body))
	})
}
