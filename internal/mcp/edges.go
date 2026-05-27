// edges tool — single-hop edge query around an entity.

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

// registerUpdateEdgeTarget wires the #304 Cut B primitive: a
// single transactional API that rewrites an edge's target. Used
// by agent-driven flows that resolve a name (via plugin search,
// operator-disambiguation task, etc.) and want to swap the
// placeholder target for the resolved canonical entity without
// losing the edge's identity / created_at audit trail.
//
// Returns the new edge envelope on success. Error envelopes
// (passthrough — branch on `ok === false`):
//
//   - `edge_stale` (409) — the (from, type, old_target) tuple
//     doesn't match a current edge. Re-read state + retry with
//     the fresh tuple. Concurrent rewrites converge here.
//   - `missing_entity` (422) — new_target doesn't resolve to a
//     known entity. The caller materializes it first (via
//     `ingest` or by picking a valid canonical id) and retries.
//   - `invalid_argument` (400) — any required field empty, or
//     old_target == new_target (no-op).
func registerUpdateEdgeTarget(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("update_edge_target",
		mcp.WithDescription(
			"Rewrite an edge's target in a single transaction. "+
				"Wraps `POST /v1/edges/update-target`. Deletes "+
				"`(from, type, old_target)` and creates "+
				"`(from, type, new_target)` preserving the "+
				"original edge's `created_at` + metadata so the "+
				"audit trail shows \"edge existed since T, target "+
				"finalized at T'\" rather than a delete+create pair. "+
				"Stale-safety: returns `edge_stale` (409) when the "+
				"old tuple doesn't match current state (already "+
				"rewritten, deleted, or never existed) so concurrent "+
				"rewrites converge cleanly. `new_target` MUST "+
				"reference an existing entity; absent → "+
				"`missing_entity` (422). Used by agent flows that "+
				"resolve a disambiguation and need to swap a "+
				"placeholder edge target for the resolved canonical "+
				"id without losing edge identity.",
		),
		mcp.WithString("from",
			mcp.Required(),
			mcp.Description("Source entity id."),
		),
		mcp.WithString("type",
			mcp.Required(),
			mcp.Description("Edge type (must be a registered edge_kind)."),
		),
		mcp.WithString("old_target",
			mcp.Required(),
			mcp.Description("Current target entity id of the edge being rewritten."),
		),
		mcp.WithString("new_target",
			mcp.Required(),
			mcp.Description("Replacement target entity id."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		from := req.GetString("from", "")
		if from == "" {
			return mcp.NewToolResultError("`from` is required"), nil
		}
		edgeType := req.GetString("type", "")
		if edgeType == "" {
			return mcp.NewToolResultError("`type` is required"), nil
		}
		oldTarget := req.GetString("old_target", "")
		if oldTarget == "" {
			return mcp.NewToolResultError("`old_target` is required"), nil
		}
		newTarget := req.GetString("new_target", "")
		if newTarget == "" {
			return mcp.NewToolResultError("`new_target` is required"), nil
		}
		body, err := json.Marshal(map[string]any{
			"from":       from,
			"type":       edgeType,
			"old_target": oldTarget,
			"new_target": newTarget,
		})
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/edges/update-target", bytes.NewReader(body))
	})
}
