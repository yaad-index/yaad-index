// Task tools — list, load, resolve. Wrappers around the
// `/v1/tasks*` routes per ADR-0024 §"Agent surface".

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerTaskList(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("task_list",
		mcp.WithDescription(
			"List workflow-produced tasks (markdown files under "+
				"vault/tasks/). Returns `{ok, tasks: [{id, workflow, "+
				"subject?, errored?, dedup_key?, created_at}]}` verbatim "+
				"from `GET /v1/tasks`. Sorted by id. Optional `errored` "+
				"filter: true → only err-tasks; false → only normal tasks; "+
				"omitted → both. Active tasks only — resolved + auto-"+
				"archived tasks live under tasks/_archive/ and aren't "+
				"included.",
		),
		mcp.WithBoolean("errored",
			mcp.Description("Filter by the task's `errored:` frontmatter. true → err-tasks only; false → normal-tasks only; omit → both."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := "/v1/tasks"
		if v, ok := req.GetArguments()["errored"].(bool); ok {
			q := url.Values{}
			q.Set("errored", strconv.FormatBool(v))
			path += "?" + q.Encode()
		}
		return b.callTool(ctx, "GET", path, nil)
	})
}

func registerTaskLoad(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("task_load",
		mcp.WithDescription(
			"Load one workflow-produced task by id. Returns `{ok, task: "+
				"{id, workflow, subject?, errored?, dedup_key?, created_at, "+
				"body}}` verbatim from `GET /v1/tasks/{id}`. `body` is the "+
				"markdown content after the frontmatter, verbatim. 404 "+
				"when the id doesn't resolve. Path-traversal-resistant: "+
				"ids with `/` or `\\` reject at the daemon.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Task id (markdown file basename without `.md`)."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "GET", "/v1/tasks/"+url.PathEscape(id), nil)
	})
}

func registerTaskResolve(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("task_resolve",
		mcp.WithDescription(
			"Mark a workflow-produced task done. Stamps `resolved_at: "+
				"<now>` on the task's frontmatter; auto-archives (moves "+
				"to tasks/_archive/<id>.md) when the originating workflow "+
				"has `auto_archive_on_done: true` (default). Err-tasks "+
				"always auto-archive regardless of the workflow opt-out. "+
				"Returns `{ok, id, errored, auto_archived, resolved_at}` "+
				"verbatim from `POST /v1/tasks/{id}/resolve`. Idempotent. "+
				"\n\n"+
				"For resolution-tasks (#304 Cut C3 — `kind: resolution-"+
				"task` frontmatter), pass `option` to pick one of the "+
				"recorded candidates: the daemon re-ingests via the "+
				"resolver plugin's shorthand, lands a canonical-edge "+
				"edge (or rewrites a stale one via Cut B's "+
				"update_edge_target), and auto-archives the task. "+
				"Returns `{ok, id, auto_archived, resolved_at, "+
				"chosen_id, edge_outcome, from_id, edge_type, "+
				"target_kind}` instead. `edge_outcome` is one of "+
				"`created` / `rewritten` / `unchanged`.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Task id (markdown file basename without `.md`)."),
		),
		mcp.WithString("option",
			mcp.Description(
				"For resolution-tasks: the option `id` from the task's "+
					"frontmatter `options` list. Must match exactly. "+
					"Leave empty for legacy text-task resolves.",
			),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		var body io.Reader
		if option := req.GetString("option", ""); option != "" {
			encoded, err := json.Marshal(map[string]string{"option": option})
			if err != nil {
				return mcp.NewToolResultError("encode option: " + err.Error()), nil
			}
			body = bytes.NewReader(encoded)
		}
		return b.callTool(ctx, "POST", "/v1/tasks/"+url.PathEscape(id)+"/resolve", body)
	})
}
