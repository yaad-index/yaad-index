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

func registerWorkflowGet(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("workflow_get",
		mcp.WithDescription(
			"Fetch the full markdown body of a single workflow by name. "+
				"Returns the raw file content (frontmatter + prose + YAML "+
				"fence) verbatim from `GET /v1/workflows/<name>`. Use this "+
				"to read the current definition before editing via "+
				"`workflow_define`. Unknown workflow → 404; invalid name "+
				"(must match `[a-z0-9]+([_-][a-z0-9]+)*`) → 400.",
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Workflow name (matches frontmatter `name:` on vault/workflows/<name>.md)."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("name", "")
		if name == "" {
			return mcp.NewToolResultError("`name` is required"), nil
		}
		return b.callTool(ctx, "GET", "/v1/workflows/"+url.PathEscape(name), nil)
	})
}

func registerWorkflowDefine(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("workflow_define",
		mcp.WithDescription(
			"Define (create or overwrite) a workflow by writing its full "+
				"markdown content to the vault. The body MUST be a complete "+
				"workflow file (frontmatter + prose + one ```yaml fence). "+
				"Pre-validated server-side via the parser; an invalid body "+
				"returns 422 with the rule violation + nothing is written. "+
				"Mismatch between the `name` argument and the body's "+
				"frontmatter `name:` field returns 400. Idempotent: a "+
				"successful PUT overwrites any existing file at the path. "+
				"The loader's mtime poll reconciles engine state on the "+
				"next pass.",
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Workflow name (must match the frontmatter `name:` field in `content`)."),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("Full markdown body: frontmatter (`---` envelope), prose, and one ```yaml code-fence with the workflow rules."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("name", "")
		if name == "" {
			return mcp.NewToolResultError("`name` is required"), nil
		}
		content := req.GetString("content", "")
		if content == "" {
			return mcp.NewToolResultError("`content` is required"), nil
		}
		return b.callTool(ctx, "PUT", "/v1/workflows/"+url.PathEscape(name), bytes.NewReader([]byte(content)))
	})
}

func registerWorkflowDelete(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("workflow_delete",
		mcp.WithDescription(
			"Remove a workflow file from the vault. Idempotent: a missing "+
				"file returns `{ok, name, existed: false}`. The loader's "+
				"mtime poll unregisters the workflow from the engine on the "+
				"next pass. Use this to retire a workflow definition; "+
				"in-flight workflow runs that already fired are unaffected "+
				"(they live in the engine's run history, not the file).",
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Workflow name to remove."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("name", "")
		if name == "" {
			return mcp.NewToolResultError("`name` is required"), nil
		}
		return b.callTool(ctx, "DELETE", "/v1/workflows/"+url.PathEscape(name), nil)
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
