// Canonical-entity creation tool (#389) — create_canonical_entity.
// Direct, edge-side-effect-free creation of a canonical entity
// (`<kind>:<slug>`), without the plugin-ingest / edge-stub / UGC-
// frontmatter-edge indirection paths.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerCreateCanonicalEntity(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("create_canonical_entity",
		mcp.WithDescription(
			"Create a canonical entity (`<kind>:<slug>`) directly, without "+
				"going through plugin ingestion, an edge side-effect, or a "+
				"UGC frontmatter-edge stub. Use when you want a canonical "+
				"page (e.g. `person:alex-example`) to exist on its own — "+
				"before any source or UGC references it — so you can write "+
				"notes / frontmatter on it. Edge-side-effect-free: it creates "+
				"only the entity (no edges). Optional `data` seeds scalar gap "+
				"fields (validated + gap_state-stamped exactly like `fill`); a "+
				"`canonical_type` (edge) gap is rejected — seed those with "+
				"`fill_field` after creation. Returns the created entity "+
				"envelope + remaining open gaps. 400 if `kind` isn't a "+
				"registered canonical kind or `slug` is malformed; 409 on "+
				"slug collision.",
		),
		mcp.WithString("kind",
			mcp.Required(),
			mcp.Description("Canonical kind (must be in the operator's canonical_kinds registry), e.g. `person`, `boardgame`."),
		),
		mcp.WithString("slug",
			mcp.Required(),
			mcp.Description("Entity slug: lowercase alphanumeric segments joined by single hyphens (ADR-0008), e.g. `alex-example`."),
		),
		mcp.WithObject("data",
			mcp.Description("Optional {gap-field → scalar value} seed map. Same typing + gap_state stamping as `fill`; canonical_type (edge) fields are rejected."),
			mcp.AdditionalProperties(true),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		kind := req.GetString("kind", "")
		if kind == "" {
			return mcp.NewToolResultError("`kind` is required"), nil
		}
		slug := req.GetString("slug", "")
		if slug == "" {
			return mcp.NewToolResultError("`slug` is required"), nil
		}
		args := map[string]any{"kind": kind, "slug": slug}
		if data, ok := req.GetArguments()["data"].(map[string]any); ok && len(data) > 0 {
			args["data"] = data
		}
		body, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/canonical-entities", bytes.NewReader(body))
	})
}
