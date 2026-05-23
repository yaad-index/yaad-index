// Read tools wrapping the entity surface — get_entity,
// get_entities_batch, get_entity_with_context. Each tool
// validates its required args + invokes bridge.callTool
// against the corresponding `/v1/...` route. Error
// projection + success body pass-through live in the
// bridge layer per #101 Cut 2.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerGetEntity(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("get_entity",
		mcp.WithDescription(
			"Fetch a yaad-index entity by id. Returns `{id, kind, data, "+
				"provenance, edges}`. The `is_about` edge type is expanded "+
				"inline (canonical-axis traversal); other edge types are "+
				"not currently surfaced (yaad-index API limitation, follow-up "+
				"tracked). Call `get_entity(<edge.to>)` to walk from a "+
				"source-shape entity to its canonical stub.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id, e.g. `wikipedia:tehran` or `person:alex-example`."),
		),
		mcp.WithString("notes_kind",
			mcp.Description("Optional filter for the returned `notes` "+
				"array. `note` returns operator-style notes only; "+
				"`annotation` returns agent-feedback annotations only. "+
				"Omit / empty returns all notes."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		path := "/v1/entities/" + url.PathEscape(id)
		if notesKind := req.GetString("notes_kind", ""); notesKind != "" {
			path += "?notes_kind=" + url.QueryEscape(notesKind)
		}
		return b.callTool(ctx, "GET", path, nil)
	})
}

func registerGetEntitiesBatch(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("get_entities_batch",
		mcp.WithDescription(
			"Fetch many yaad-index entities by id in a single round-trip. "+
				"Returns `{results: [{id, kind, data, provenance, edges} | "+
				"{id, error}]}` — one entry per requested id in arrival order. "+
				"A per-id error (e.g. not-found) lands as `{id, error}` on "+
				"that entry; the batch never aborts on partial failure.",
		),
		mcp.WithArray("ids",
			mcp.Required(),
			mcp.Description("List of entity ids to fetch."),
			mcp.Items(map[string]any{"type": "string"}),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := req.GetArguments()["ids"]
		ids, ok := raw.([]any)
		if !ok || len(ids) == 0 {
			return mcp.NewToolResultError("`ids` is required and must be a non-empty array"), nil
		}
		body, err := json.Marshal(map[string]any{"ids": ids})
		if err != nil {
			return mcp.NewToolResultError("encode batch ids: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/batch", bytes.NewReader(body))
	})
}

func registerGetEntityWithContext(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("get_entity_with_context",
		mcp.WithDescription(
			"Fetch an entity plus its N-hop edge context in a single "+
				"round-trip. Returns the entity shape from get_entity PLUS "+
				"an `expanded` map keyed by edge type with the resolved "+
				"endpoints. Use this for traversal — for direct one-hop "+
				"edge queries use the `edges` tool instead.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id whose context should be expanded."),
		),
		mcp.WithString("notes_kind",
			mcp.Description("Optional filter for the `notes` array on "+
				"the root entity and every neighbor. `note` returns "+
				"operator-style notes only; `annotation` returns "+
				"agent-feedback annotations only. Omit / empty returns all."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		path := "/v1/entities/" + url.PathEscape(id) + "/context"
		if notesKind := req.GetString("notes_kind", ""); notesKind != "" {
			path += "?notes_kind=" + url.QueryEscape(notesKind)
		}
		return b.callTool(ctx, "GET", path, nil)
	})
}

func registerListEntities(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("list_entities",
		mcp.WithDescription(
			"List entities of a given kind. Returns `{results, total, "+
				"limit, offset}` where each result is `{id, kind, snippet, "+
				"score}` — call `get_entity(id)` for any id to load full "+
				"state. The `kind` parameter is required (the underlying "+
				"`/v1/search` route requires either a query or a kind "+
				"filter; this tool exposes kind-only listing).",
		),
		mcp.WithString("kind",
			mcp.Required(),
			mcp.Description("Kind filter, e.g. `wikipedia`, `person`, `boardgame`."),
		),
		mcp.WithBoolean("is_journal",
			mcp.Description("Optional filter for journal-flagged entities "+
				"per ADR-0025 cut 3. When true, restricts results to "+
				"entities whose vault frontmatter sets "+
				"`data.is_journal: true` — canonically used to surface "+
				"only journal-shaped `day` entities (combine with "+
				"`kind: day`)."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		kind := req.GetString("kind", "")
		if kind == "" {
			return mcp.NewToolResultError("`kind` is required"), nil
		}
		path := "/v1/search?kind=" + url.QueryEscape(kind)
		if req.GetBool("is_journal", false) {
			path += "&is_journal=true"
		}
		return b.callTool(ctx, "GET", path, nil)
	})
}

func registerArchiveEntity(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("archive_entity",
		mcp.WithDescription(
			"Archive an entity (ADR-0018 step 1: archived → DELETE-able). "+
				"The vault file moves to `_archive/<kind>/<slug>.md` and the "+
				"store row gets `archived_at` stamped. Reversible via "+
				"`restore_entity`. Idempotent.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id to archive."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/archive", nil)
	})
}

func registerRestoreEntity(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("restore_entity",
		mcp.WithDescription(
			"Restore a previously-archived entity. Inverse of "+
				"`archive_entity`: vault file moves back from `_archive/` "+
				"to its active path, `archived_at` is cleared. Idempotent "+
				"on already-active entities.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id to restore."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/restore", nil)
	})
}

func registerDeleteEntity(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("delete_entity",
		mcp.WithDescription(
			"Permanently delete an archived entity (ADR-0018 step 2: "+
				"only archived entities are DELETE-able). Removes the vault "+
				"file + store row + edges. Non-reversible. Returns 409 if "+
				"the entity isn't archived yet — call `archive_entity` "+
				"first.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Entity id to delete. Must be archived first."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return mcp.NewToolResultError("`id` is required"), nil
		}
		return b.callTool(ctx, "DELETE", "/v1/entities/"+url.PathEscape(id), nil)
	})
}
