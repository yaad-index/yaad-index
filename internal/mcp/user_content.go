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
				"`etag` (lifted from the HTTP ETag header onto the JSON "+
				"body) for chaining edits. 409 conflict on slug collision. "+
				"Optional `data` carries frontmatter fields; fields "+
				"declared in `user_content_frontmatter_edges:` config "+
				"produce canonical-label edges. `body` may be empty.",
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
			mcp.Description("Markdown body. Empty allowed; omit or pass \"\"."),
		),
		mcp.WithObject("data",
			mcp.Description("Optional frontmatter map. See description for edge-derivation rules."),
			mcp.AdditionalProperties(true),
		),
		mcp.WithString("subfolder",
			mcp.Description(
				"Optional organization folder. When set, the vault file "+
					"lands at user-content/<subfolder>/<slug>.md instead of "+
					"user-content/<slug>.md — operator-visible organization "+
					"only. A single path segment of lowercase alphanumerics "+
					"and hyphens (e.g. notes, drafts, projects). The entity "+
					"id stays flat (user-content:<slug>), so every id-taking "+
					"tool keeps working unchanged.",
			),
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
		if sf := req.GetString("subfolder", ""); sf != "" {
			args["subfolder"] = sf
		}
		bodyBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callToolWithEtagLift(ctx, "POST", "/v1/user-content", bytes.NewReader(bodyBytes), nil)
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
		return b.callToolWithEtagLift(ctx, "GET", "/v1/user-content/"+url.PathEscape(id), nil, nil)
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

func registerMoveUserContent(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("move_user_content",
		mcp.WithDescription(
			"Relocate a UGC entity's vault file to a different on-disk "+
				"subfolder in place — no archive -> delete -> recreate "+
				"dance. Provenance, edges, and the entity id are all "+
				"preserved (the id is flat per #415; the subfolder is "+
				"path-only organization). An empty / omitted subfolder "+
				"moves it to the flat user-content/<slug>.md path. Same "+
				"subfolder is an idempotent no-op. A bad subfolder (not a "+
				"single segment of lowercase alphanumerics + hyphens) "+
				"rejects with 400.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id to move."),
		),
		mcp.WithString("subfolder",
			mcp.Description(
				"Destination subfolder (single path segment of lowercase "+
					"alphanumerics and hyphens, e.g. notes, drafts, "+
					"projects). Empty -> flat user-content/<slug>.md.",
			),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		args := map[string]any{}
		if sf := req.GetString("subfolder", ""); sf != "" {
			args["subfolder"] = sf
		}
		bodyBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callToolWithEtagLift(ctx, "POST", "/v1/user-content/"+url.PathEscape(id)+"/move", bytes.NewReader(bodyBytes), nil)
	})
}

func registerListUserContentSections(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("list_user_content_sections",
		mcp.WithDescription(
			"List the parsed `## section` headings on a UGC entity's "+
				"body. Returns each section's address (slug or positional "+
				"index) + heading text — addresses are what `edit_user_"+
				"content_section` accepts. Response carries `etag` "+
				"(lifted from the HTTP ETag header) so callers can chain "+
				"a section edit without a second read.",
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
		return b.callToolWithEtagLift(ctx, "GET", "/v1/user-content/"+url.PathEscape(id)+"/sections", nil, nil)
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
		return b.callToolWithEtagLift(ctx, "GET", fmt.Sprintf("/v1/user-content/%s/sections/%s", url.PathEscape(id), url.PathEscape(sec)), nil, nil)
	})
}

func registerAddUserContentSection(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("add_user_content_section",
		mcp.WithDescription(
			"Insert a new section into an existing UGC entity. THE "+
				"ETAG IS REQUIRED (same If-Match concurrency as "+
				"`edit_user_content_section`). `after_sec` accepts the "+
				"heading-slug or positional index of the section to "+
				"insert AFTER; pass `\"-1\"` (or empty) to prepend; "+
				"omit / null to append at end. `heading` is required. "+
				"`depth` defaults to the after-section's depth (or 1 "+
				"when appending to an empty doc); pass an explicit "+
				"value (1..6) to override. `body` is taken verbatim "+
				"(daemon normalizes trailing newline so the next "+
				"section's heading parses cleanly). Returns the "+
				"inserted section's index + a fresh `etag`. Error "+
				"envelopes: `precondition_failed` (412, stale etag), "+
				"`precondition_required` (428, missing etag), "+
				"`operator_mismatch` (403), `conflict` (409 when the "+
				"new heading slugifies to an existing sibling at the "+
				"same depth).",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id."),
		),
		mcp.WithString("after_sec",
			mcp.Description("Section address to insert after (slug or positional index). `\"-1\"` or empty to prepend; omit to append."),
		),
		mcp.WithString("heading",
			mcp.Required(),
			mcp.Description("Heading text of the new section."),
		),
		mcp.WithString("body",
			mcp.Description("Body of the new section. Empty allowed."),
		),
		mcp.WithNumber("depth",
			mcp.Description("Heading depth (1..6). Defaults to the after-section's depth."),
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
		heading := req.GetString("heading", "")
		if heading == "" {
			return mcp.NewToolResultError("`heading` is required"), nil
		}
		etag := req.GetString("etag", "")
		if etag == "" {
			return mcp.NewToolResultError("`etag` is required"), nil
		}
		args := map[string]any{
			"heading": heading,
			"body":    req.GetString("body", ""),
		}
		if v, ok := req.GetArguments()["after_sec"]; ok && v != nil {
			if s, isStr := v.(string); isStr {
				args["after_sec"] = s
			}
		}
		if d := req.GetFloat("depth", 0); d > 0 {
			args["depth"] = int(d)
		}
		bodyBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		headers := map[string]string{"If-Match": etag}
		return b.callToolWithEtagLift(ctx, "POST", "/v1/user-content/"+url.PathEscape(id)+"/sections", bytes.NewReader(bodyBytes), headers)
	})
}

func registerRenameUserContentSection(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("rename_user_content_section",
		mcp.WithDescription(
			"Rename a UGC section's heading. THE ETAG IS REQUIRED "+
				"(same If-Match contract as `edit_user_content_section`). "+
				"Body + nested headings preserved verbatim — this only "+
				"rewrites the heading line. The depth (`#`-count) is "+
				"preserved. Returns the renamed section + a fresh "+
				"`etag`. Error envelopes: `precondition_failed` (412), "+
				"`precondition_required` (428), `operator_mismatch` "+
				"(403), `conflict` (409 when the new heading slugifies "+
				"to a sibling's existing slug), `invalid_argument` "+
				"(400 for renaming the pre-heading section).",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id."),
		),
		mcp.WithString("sec",
			mcp.Required(),
			mcp.Description("Section address: heading slug or positional index."),
		),
		mcp.WithString("new_heading",
			mcp.Required(),
			mcp.Description("New heading text."),
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
		newHeading := req.GetString("new_heading", "")
		if newHeading == "" {
			return mcp.NewToolResultError("`new_heading` is required"), nil
		}
		etag := req.GetString("etag", "")
		if etag == "" {
			return mcp.NewToolResultError("`etag` is required"), nil
		}
		args := map[string]any{"new_heading": newHeading}
		bodyBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		headers := map[string]string{"If-Match": etag}
		return b.callToolWithEtagLift(ctx, "PATCH", fmt.Sprintf("/v1/user-content/%s/sections/%s/heading", url.PathEscape(id), url.PathEscape(sec)), bytes.NewReader(bodyBytes), headers)
	})
}

func registerDeleteUserContentSection(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("delete_user_content_section",
		mcp.WithDescription(
			"Remove a UGC section + every nested heading textually "+
				"contained within it (containment model). THE ETAG IS "+
				"REQUIRED (same If-Match contract as "+
				"`edit_user_content_section`). Returns the entity's "+
				"new etag + the removed section's old index. Error "+
				"envelopes: `precondition_failed` (412), "+
				"`precondition_required` (428), `operator_mismatch` "+
				"(403), `not_found` (404), `invalid_argument` (400 "+
				"for deleting the pre-heading section — clear it via "+
				"`edit_user_content_section` with empty body instead).",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("UGC entity id."),
		),
		mcp.WithString("sec",
			mcp.Required(),
			mcp.Description("Section address: heading slug or positional index."),
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
		etag := req.GetString("etag", "")
		if etag == "" {
			return mcp.NewToolResultError("`etag` is required"), nil
		}
		headers := map[string]string{"If-Match": etag}
		return b.callToolWithEtagLift(ctx, "DELETE", fmt.Sprintf("/v1/user-content/%s/sections/%s", url.PathEscape(id), url.PathEscape(sec)), nil, headers)
	})
}

func registerEditUserContentSection(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("edit_user_content_section",
		mcp.WithDescription(
			"Replace one section's body on a UGC entity. THE ETAG IS "+
				"REQUIRED: read it from a prior `get_user_content` / "+
				"`get_user_content_section` call (response JSON carries "+
				"`etag` lifted from the HTTP ETag header) and pass it "+
				"back here as If-Match concurrency. Success returns the "+
				"post-edit envelope with a fresh `etag`. Errors as JSON "+
				"envelopes: `precondition_failed` (412, stale etag — "+
				"envelope carries `current_etag` for retry), "+
				"`precondition_required` (428, missing etag), "+
				"`operator_mismatch` (403, JWT operator doesn't match entity's operator).",
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
		return b.callToolWithEtagLift(ctx, "PUT", fmt.Sprintf("/v1/user-content/%s/sections/%s", url.PathEscape(id), url.PathEscape(sec)), bytes.NewReader(bodyBytes), headers)
	})
}
