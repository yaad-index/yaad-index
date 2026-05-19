// Fill-family tools — fill, set_operator_fill, defer_gap,
// needs_fill, cv_status. Read tools (needs_fill, cv_status)
// take optional cursor / kind filters; write tools (fill,
// set_operator_fill) take entity id + per-field op maps.

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

func registerFill(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("fill",
		mcp.WithDescription(
			"Fill an entity's open gaps with agent-derived values. Each "+
				"key in `fields` must be a current gap on the entity (per "+
				"the entity's frontmatter `gaps:` list); a key that isn't "+
				"currently a gap fails the whole call (no partial success). "+
				"Returns the updated entity + the remaining unfilled gap "+
				"field names.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id whose gaps are being filled."),
		),
		mcp.WithObject("fields",
			mcp.Required(),
			mcp.Description("{field-name → value} map. Field names must be in the entity's current `gaps` set."),
			mcp.AdditionalProperties(true),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		fields := req.GetArguments()["fields"]
		fm, ok := fields.(map[string]any)
		if !ok || len(fm) == 0 {
			return mcp.NewToolResultError("`fields` is required and must be a non-empty object"), nil
		}
		body, err := json.Marshal(map[string]any{"fields": fm})
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/fill", bytes.NewReader(body))
	})
}

func registerSetOperatorFill(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("set_operator_fill",
		mcp.WithDescription(
			"Operator-fill endpoint. POSTs to "+
				"`/v1/entities/{id}/operator-fill` with per-field operations. "+
				"Operator-only — the caller's JWT MUST have Subject == "+
				"Operator. Per-field value shapes: scalar (number / "+
				"boolean / string / list) sets the field; explicit `null` "+
				"clears it; `{defer: true}` marks the field deferred (must "+
				"be currently unfilled); `{defer: false}` un-defers.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id."),
		),
		mcp.WithObject("fields",
			mcp.Required(),
			mcp.Description("{field-name → value-or-op} map per the description."),
			mcp.AdditionalProperties(true),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		fields := req.GetArguments()["fields"]
		fm, ok := fields.(map[string]any)
		if !ok || len(fm) == 0 {
			return mcp.NewToolResultError("`fields` is required and must be a non-empty object"), nil
		}
		body, err := json.Marshal(fm)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/operator-fill", bytes.NewReader(body))
	})
}

func registerDeferGap(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("defer_gap",
		mcp.WithDescription(
			"Mark a single gap deferred. Convenience wrapper around "+
				"`set_operator_fill` that POSTs `{<field>: {\"defer\": "+
				"true}}`. The field stops surfacing on `/v1/needs-fill` "+
				"responses; un-defer via `set_operator_fill(id, {<field>: "+
				"{\"defer\": false}})`. Constraint: the field MUST be "+
				"currently unfilled (defer on a filled field returns 409 "+
				"`deferred_requires_unfilled`). Operator-only.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id."),
		),
		mcp.WithString("field",
			mcp.Required(),
			mcp.Description("Single gap field name to defer."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		field := req.GetString("field", "")
		if field == "" {
			return mcp.NewToolResultError("`field` is required"), nil
		}
		body, err := json.Marshal(map[string]any{field: map[string]any{"defer": true}})
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/operator-fill", bytes.NewReader(body))
	})
}

func registerNeedsFill(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("needs_fill",
		mcp.WithDescription(
			"Browse the open-gap queue: entities with unfilled gaps that "+
				"haven't been gap-called for the current fetch-cycle. Returns "+
				"`{ok, entities, next_cursor?}` verbatim from "+
				"`GET /v1/needs-fill`. Each entry carries the full gap-call "+
				"payload (id, kind, gaps, clean_content, instruction, "+
				"canonical_vocabulary). Optional `limit` (server clamps; "+
				"default 50, cap 200) and `cursor` (opaque, from a prior "+
				"call's `next_cursor`) drive pagination. The caller decides "+
				"whether to keep paginating or stop.",
		),
		mcp.WithNumber("limit",
			mcp.Description("Page size. Server clamps; default 50, cap 200."),
		),
		mcp.WithString("cursor",
			mcp.Description("Opaque base64 cursor from a prior call's `next_cursor`."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		q := url.Values{}
		if limit := req.GetFloat("limit", 0); limit > 0 {
			q.Set("limit", fmt.Sprintf("%d", int(limit)))
		}
		if cursor := req.GetString("cursor", ""); cursor != "" {
			q.Set("cursor", cursor)
		}
		path := "/v1/needs-fill"
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
		return b.callTool(ctx, "GET", path, nil)
	})
}

func registerCVStatus(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("cv_status",
		mcp.WithDescription(
			"Vault coverage status — returns per-kind counts of "+
				"entities, with-gaps, archived. Useful for operator "+
				"dashboards + agent self-assessment of vault coverage.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/cv-status", nil)
	})
}
