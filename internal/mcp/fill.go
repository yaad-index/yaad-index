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

// registerSetOperatorFill registers the legacy `set_operator_fill`
// alias per ADR-0029 §7. The tool now routes to the unified
// /v1/entities/{id}/fill endpoint (the prior /v1/operator-fill URL
// returns 410). Description is updated to point operators at the
// preferred fill_field name; the alias remains for one minor
// version after the cut so existing workflow YAMLs keep working.
func registerSetOperatorFill(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("set_operator_fill",
		mcp.WithDescription(
			"Alias for `fill_field` per ADR-0029. POSTs to "+
				"`/v1/entities/{id}/fill` with per-field operations. "+
				"The caller's JWT determines the trigger-mode: "+
				"Subject == Operator OR an `operator_delegated` "+
				"claim (an agent-on-behalf-of-operator token) → "+
				"operator-trigger; a bare agent token → "+
				"agent-trigger. The strategy gate is one-directional: "+
				"operator-strategy gaps accept agent-trigger "+
				"writes (the agent writes the operator's confirmed "+
				"value; provenance stamps the agent), while "+
				"agent-strategy gaps reject operator-trigger writes "+
				"(`agent_only_field`). operator-trigger additionally "+
				"authorizes ad-hoc writes + defer. "+
				"Per-field value shapes: scalar (number / boolean / "+
				"string / list) sets the field; explicit `null` "+
				"clears it; `{defer: true}` marks the field deferred "+
				"(must be currently unfilled); `{defer: false}` "+
				"un-defers. Existing call sites keep working; new "+
				"work should prefer the `fill_field` name.",
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
	s.AddTool(tool, fillFieldHandler(b))
}

// registerFillField registers the preferred ADR-0029 §7 tool
// name. Behavior is identical to set_operator_fill — they share
// the same handler. New workflow YAMLs + agent integrations
// should call fill_field; set_operator_fill remains as a
// compat alias for one minor version after the cut.
func registerFillField(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("fill_field",
		mcp.WithDescription(
			"Unified fill endpoint per ADR-0029. POSTs to "+
				"`/v1/entities/{id}/fill` with per-field operations. "+
				"Routes per-field through three cases: open gap "+
				"(one-directional strategy gate: "+
				"operator-strategy gaps accept agent-trigger writes, "+
				"agent-strategy gaps reject operator-trigger writes "+
				"with `agent_only_field`); overwrite (requires "+
				"`force=true` query param); ad-hoc property write "+
				"(operator-trigger only). The trigger-mode follows "+
				"the caller's JWT — Subject == Operator or an "+
				"`operator_delegated` claim → operator-trigger "+
				"(which now matters only for the ad-hoc / defer "+
				"paths). Per-field value "+
				"shapes: scalar sets the field; explicit `null` "+
				"clears it; `{defer: true}` marks the field "+
				"deferred; `{defer: false}` un-defers.",
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
	s.AddTool(tool, fillFieldHandler(b))
}

// fillFieldHandler is the shared callback both `fill_field` and
// the `set_operator_fill` alias dispatch through.
func fillFieldHandler(b *bridge) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/fill", bytes.NewReader(body))
	}
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
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/fill", bytes.NewReader(body))
	})
}

func registerNeedsFill(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("needs_fill",
		mcp.WithDescription(
			"Browse the open-gap queue: entities with unfilled gaps that "+
				"haven't been gap-called for the current fetch-cycle. Returns "+
				"`{ok, entities, next_cursor?, canonical_vocabulary?}` verbatim "+
				"from `GET /v1/needs-fill`. `canonical_vocabulary` lives at the "+
				"response root (one copy per response, not one per "+
				"entry) and is included by default. Each entry carries the "+
				"per-gap-call payload (id, kind, gaps, gap_metadata, "+
				"clean_content, instruction). Optional `limit` (server clamps; "+
				"default 50, cap 200) and `cursor` (opaque, from a prior call's "+
				"`next_cursor`) drive pagination. Optional `exclude` is a comma-"+
				"separated list of fields to strip from the response — supports "+
				"`canonical_vocabulary` (drops the top-level registry block when "+
				"the agent has already cached it from /v1/structure or /v1/kinds) "+
				"and `clean_content` (drops the per-entry body when the agent "+
				"has cached it from /v1/entities). The caller decides whether to "+
				"keep paginating or stop.",
		),
		mcp.WithNumber("limit",
			mcp.Description("Page size. Server clamps; default 50, cap 200."),
		),
		mcp.WithString("cursor",
			mcp.Description("Opaque base64 cursor from a prior call's `next_cursor`."),
		),
		mcp.WithString("exclude",
			mcp.Description("Comma-separated field names to strip from the response. Supported: `canonical_vocabulary`, `clean_content`. Default empty (include everything)."),
		),
		mcp.WithString("source",
			mcp.Description("Filter the queue to gaps from a single source / plugin namespace (e.g. `gmail`, `wikipedia`) — useful when one source spikes and floods the queue. Omit for all sources."),
		),
		mcp.WithString("kind",
			mcp.Description("Filter the queue to gaps on a single entity kind (e.g. `person`, `boardgame`). Composes (AND) with `source`. Omit for all kinds."),
		),
		mcp.WithString("fill_strategy",
			mcp.Description("Filter the queue to one audience's gaps: `agent` (agent-fillable) or `operator` (operator-fillable). Overrides the auth-derived audience so an operator can review the agent queue (and vice versa). Omit for the caller's default slice."),
		),
	)
	s.AddTool(tool, needsFillHandler(b))
}

// needsFillHandler is the needs_fill tool handler (extracted so the
// query-param threading is unit-testable). It maps the optional
// limit / cursor / exclude / source / kind / fill_strategy args onto
// the GET /v1/needs-fill query string.
func needsFillHandler(b *bridge) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		q := url.Values{}
		if limit := req.GetFloat("limit", 0); limit > 0 {
			q.Set("limit", fmt.Sprintf("%d", int(limit)))
		}
		if cursor := req.GetString("cursor", ""); cursor != "" {
			q.Set("cursor", cursor)
		}
		if exclude := req.GetString("exclude", ""); exclude != "" {
			q.Set("exclude", exclude)
		}
		if source := req.GetString("source", ""); source != "" {
			q.Set("source", source)
		}
		if kind := req.GetString("kind", ""); kind != "" {
			q.Set("kind", kind)
		}
		if fs := req.GetString("fill_strategy", ""); fs != "" {
			q.Set("fill_strategy", fs)
		}
		path := "/v1/needs-fill"
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
		return b.callTool(ctx, "GET", path, nil)
	}
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
