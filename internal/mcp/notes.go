// add_note tool — append a note (entity-level note, ADR-0020
// shape) to an existing entity. Server stamps date + author
// from the JWT pair-claim.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerAddNote(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("add_note",
		mcp.WithDescription(
			"Append a note to an existing entity. Server stamps date "+
				"(UTC), the JWT subject as author, and the operator from "+
				"the pair-claim. Empty author is server-filled; an explicit "+
				"author MUST equal the JWT subject or the call returns the "+
				"upstream 403 `author_mismatch` envelope verbatim.",
		),
		mcp.WithString("entity_id",
			mcp.Required(),
			mcp.Description("Target entity id, e.g. `boardgame:acme-game`."),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("Note body. Server trims surrounding whitespace."),
		),
		mcp.WithString("author",
			mcp.Description("Optional. If set, MUST equal the JWT subject. Omit to let the server fill."),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("entity_id", "")
		if id == "" {
			return mcp.NewToolResultError("`entity_id` is required"), nil
		}
		text := req.GetString("text", "")
		if text == "" {
			return mcp.NewToolResultError("`text` is required"), nil
		}
		args := map[string]any{"text": text}
		if author := req.GetString("author", ""); author != "" {
			args["author"] = author
		}
		body, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("encode args: " + err.Error()), nil
		}
		return b.callTool(ctx, "POST", "/v1/entities/"+url.PathEscape(id)+"/notes", bytes.NewReader(body))
	})
}
