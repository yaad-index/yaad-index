// Workflow tools — list, discover, trigger. Wrappers around
// the `/v1/workflows*` routes per ADR-0024 §"Agent surface".

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerWorkflowList(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("workflow_list",
		mcp.WithDescription(
			"List every workflow pattern currently registered with the "+
				"running daemon. Returns `{ok, workflows: [{name, version, "+
				"status, trigger_type, dedup_policy}]}` verbatim from "+
				"`GET /v1/workflows`. Sorted by name. Use this to discover "+
				"what workflows exist before calling `workflow_trigger` or "+
				"`workflow_discover`.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/workflows", nil)
	})
}

func registerWorkflowDiscover(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("workflow_discover",
		mcp.WithDescription(
			"List workflows whose condition predicate evaluates true for "+
				"the given entity. Returns `{ok, entity_id, workflows: "+
				"[<name>, ...]}` verbatim from `GET /v1/workflows/discover`. "+
				"Context-binding failures + condition eval errors are "+
				"treated as non-matching (best-effort discovery, not a "+
				"fire commitment). Unknown entity surfaces as a 404 from "+
				"the daemon.",
		),
		mcp.WithString("entity_id",
			mcp.Required(),
			mcp.Description("Canonical entity id (`<kind>:<slug>`) to test workflow conditions against."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entityID := req.GetString("entity_id", "")
		if entityID == "" {
			return mcp.NewToolResultError("`entity_id` is required"), nil
		}
		q := url.Values{}
		q.Set("entity", entityID)
		return b.callTool(ctx, "GET", "/v1/workflows/discover?"+q.Encode(), nil)
	})
}

func registerWorkflowTrigger(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("workflow_trigger",
		mcp.WithDescription(
			"Manually fire a registered workflow against the given input. "+
				"Returns the Decision envelope `{ok, workflow, entity_id, "+
				"subject, fired, missing_refs?, err?, at}` verbatim from "+
				"`POST /v1/workflows/trigger`. `input` shapes: empty "+
				"(target-less manual fire — only valid for `trigger.type="+
				"manual` workflows), canonical entity id (`<kind>:<slug>`, "+
				"direct attach), or URL (routes through the daemon's "+
				"ingest-or-lookup pipeline). Unknown workflow → 404; "+
				"empty input on an event-driven workflow → 422.",
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Workflow name (matches frontmatter `name:` on vault/workflows/<file>.md)."),
		),
		mcp.WithString("input",
			mcp.Description("Trigger input: empty for target-less manual fires, canonical entity id, or URL."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("name", "")
		if name == "" {
			return mcp.NewToolResultError("`name` is required"), nil
		}
		// Daemon's workflowTriggerRequest is `{name, input string}`;
		// absent + empty-string both deserialize to "" and
		// engine.Dispatch treats them identically — empty input
		// fires a target-less manual workflow, the same as omitting.
		// So always emitting `input` is harmless on the wire.
		args := map[string]any{
			"name":  name,
			"input": req.GetString("input", ""),
		}
		body, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/workflows/trigger", bytes.NewReader(body))
	})
}
