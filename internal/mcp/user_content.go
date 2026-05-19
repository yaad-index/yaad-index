// User-content (UGC) tools — create, get, delete, list-sections,
// get-section, edit-section. UGC is operator-authored entities
// (author = JWT subject; operator = JWT operator).

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

func registerCreateUserContent(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("create_user_content",
		mcp.WithDescription(
			"Create a new user-content (UGC) entity. Server slugifies "+
				"`title` to derive `id = user-content:<slug>`, stamps "+
				"`author` from the JWT subject and `operator` from the "+
				"pair-claim. Returns the full entity envelope plus an "+
				"`etag` for chaining edits. 409 conflict on slug "+
				"collision. Optional `data` carries frontmatter fields; "+
				"fields declared in `user_content_frontmatter_edges:` "+
				"config produce canonical-label edges.",
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("Human-readable title. Slugified server-side."),
		),
		mcp.WithArray("tags",
			mcp.Required(),
			mcp.Description("Non-empty tag list."),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("Markdown body. Empty allowed."),
		),
		mcp.WithObject("data",
			mcp.Description("Optional frontmatter map. See description for edge-derivation rules."),
			mcp.AdditionalProperties(true),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title := req.GetString("title", "")
		if title == "" {
			return mcp.NewToolResultError("`title` is required"), nil
		}
		tagsRaw, ok := req.GetArguments()["tags"].([]any)
		if !ok || len(tagsRaw) == 0 {
			return mcp.NewToolResultError("`tags` is required and must be a non-empty array"), nil
		}
		body := req.GetString("body", "")
		args := map[string]any{
			"title": title,
			"tags":  tagsRaw,
			"body":  body,
		}
		if data, ok := req.GetArguments()["data"].(map[string]any); ok && len(data) > 0 {
			args["data"] = data
		}
		bodyBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/user-content", bytes.NewReader(bodyBytes))
	})
}

func registerGetUserContent(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("get_user_content",
		mcp.WithDescription(
			"Fetch a UGC entity with the first page of parsed sections "+
				"embedded. Returns the entity envelope + `etag` (from the "+
				"HTTP ETag header) — pass the etag back to "+
				"`edit_user_content_section` as If-Match concurrency.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id (starts with `user-content:`)."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "GET", "/v1/user-content/"+url.PathEscape(id), nil)
	})
}

func registerDeleteUserContent(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("delete_user_content",
		mcp.WithDescription(
			"Delete a UGC entity. Per ADR-0018 the entity must be "+
				"archived first via `archive_entity`. JWT must match the "+
				"entity's author or operator.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id to delete."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "DELETE", "/v1/user-content/"+url.PathEscape(id), nil)
	})
}

func registerListUserContentSections(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("list_user_content_sections",
		mcp.WithDescription(
			"List the parsed `## section` headings on a UGC entity's "+
				"body. Returns each section's address (slug or positional "+
				"index) + heading text — addresses are what `edit_user_"+
				"content_section` accepts.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "GET", "/v1/user-content/"+url.PathEscape(id)+"/sections", nil)
	})
}

func registerGetUserContentSection(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("get_user_content_section",
		mcp.WithDescription(
			"Fetch one section of a UGC entity by address. Address is "+
				"either the heading-text-slug or a positional index "+
				"(`0`, `1`, ...). Returns the section body + an etag for "+
				"chaining edits.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id."),
		),
		mcp.WithString("sec",
			mcp.Required(),
			mcp.Description("Section address: heading slug or positional index."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		sec := req.GetString("sec", "")
		if sec == "" {
			return mcp.NewToolResultError("`sec` is required"), nil
		}
		return b.callTool(ctx, "GET", fmt.Sprintf("/v1/user-content/%s/sections/%s", url.PathEscape(id), url.PathEscape(sec)), nil)
	})
}

func registerEditUserContentSection(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("edit_user_content_section",
		mcp.WithDescription(
			"Replace one section's body on a UGC entity. THE ETAG IS "+
				"REQUIRED: read it from a prior `get_user_content` / "+
				"`get_user_content_section` call and pass it back here as "+
				"If-Match concurrency. Errors as JSON envelopes: "+
				"`precondition_failed` (412, stale etag), "+
				"`precondition_required` (428, missing etag), "+
				"`author_mismatch` (403, JWT doesn't match author/operator).",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id."),
		),
		mcp.WithString("sec",
			mcp.Required(),
			mcp.Description("Section address: heading slug or positional index."),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("New section body, taken verbatim."),
		),
		mcp.WithString("etag",
			mcp.Required(),
			mcp.Description("If-Match etag from a prior read of this entity."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		sec := req.GetString("sec", "")
		if sec == "" {
			return mcp.NewToolResultError("`sec` is required"), nil
		}
		body := req.GetString("body", "")
		etag := req.GetString("etag", "")
		if etag == "" {
			return mcp.NewToolResultError("`etag` is required"), nil
		}
		// Daemon contract: etag travels as the If-Match HTTP
		// header (route uses DisallowUnknownFields, so an etag
		// field in the JSON body would 400). The PUT body is
		// `{"body": "..."}` only.
		args := map[string]any{"body": body}
		bodyBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		headers := map[string]string{"If-Match": etag}
		return b.callToolWithHeaders(ctx, "PUT", fmt.Sprintf("/v1/user-content/%s/sections/%s", url.PathEscape(id), url.PathEscape(sec)), bytes.NewReader(bodyBytes), headers)
	})
}
