package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Batch operations (#383). High-cardinality ops (archiving ~1000 gmail
// entities, resolving a cascade of auto-generated tasks) cost one MCP
// round-trip per target today — slow and fragile to mid-loop connectivity
// hiccups. These batch tools collapse N MCP calls into one: the daemon's
// existing single-target endpoints are fanned out server-side (one
// in-process HTTP call per id via the bridge) and the per-id outcomes are
// aggregated. Purely additive — no new daemon endpoint, store change, or
// migration. A per-id failure never aborts the batch, so partial failures
// stay legible.

// batchItemResult is one target's outcome in a batch operation.
type batchItemResult struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

// runBatch applies a single-target operation across the raw input
// targets and projects the per-target outcomes into one MCP result.
// method is the HTTP verb; pathFor builds the daemon path for each id.
//
// EVERY input entry produces a result row — an entry that isn't a
// non-empty string is recorded as a failed result (NOT silently dropped),
// so `total` always equals the caller's input length and per-id
// accounting can't lie about how many targets were acted on (a
// load-bearing property for destructive batches). A per-id bridge/HTTP
// error is likewise recorded and the batch continues. The aggregate `ok`
// is true only when every entry succeeded.
func (b *bridge) runBatch(ctx context.Context, method string, targets []any, pathFor func(string) string) *mcp.CallToolResult {
	results := make([]batchItemResult, 0, len(targets))
	succeeded := 0
	for _, t := range targets {
		id, ok := t.(string)
		if !ok || id == "" {
			results = append(results, batchItemResult{
				ID:    fmt.Sprintf("%v", t),
				Error: "invalid id: must be a non-empty string",
			})
			continue
		}
		item := batchItemResult{ID: id}
		res, err := b.callWithHeaders(ctx, method, pathFor(id), nil, nil)
		switch {
		case err != nil:
			item.Error = fmt.Sprintf("bridge call: %v", err)
		case res.isSuccess():
			item.OK = true
			item.Status = res.status
			succeeded++
		default:
			item.Status = res.status
			item.Error = res.asMCPError()
		}
		results = append(results, item)
	}
	payload, err := json.Marshal(map[string]any{
		"ok":        succeeded == len(targets),
		"total":     len(targets),
		"succeeded": succeeded,
		"failed":    len(targets) - succeeded,
		"results":   results,
	})
	if err != nil {
		return mcp.NewToolResultError("encode batch result: " + err.Error())
	}
	return mcp.NewToolResultText(string(payload))
}

// batchTargets returns the raw entries of a required MCP array argument
// (each validated per-entry inside runBatch so invalid entries surface as
// failed results rather than vanishing). Returns (nil, false) when the
// argument is absent or not an array.
func batchTargets(req mcp.CallToolRequest, key string) ([]any, bool) {
	raw, ok := req.GetArguments()[key].([]any)
	return raw, ok
}

func registerArchiveEntities(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("archive_entities",
		mcp.WithDescription(
			"Batch variant of `archive_entity` — archive many entities in "+
				"one call (each via the same archived → DELETE-able step, "+
				"reversible with `restore_entity`, idempotent). Returns "+
				"`{ok, total, succeeded, failed, results}` where `results` "+
				"is a per-id `{id, ok, status, error?}` list; a failure on "+
				"one id does not stop the rest. Use when archiving a large "+
				"fan-out (e.g. hundreds of plugin entities) to avoid a "+
				"per-entity MCP round-trip.",
		),
		mcp.WithArray("entity_ids",
			mcp.Required(),
			mcp.Description("Entity ids to archive."),
			mcp.Items(map[string]any{"type": "string"}),
		),
	)
	s.AddTool(tool, archiveEntitiesHandler(b))
}

func archiveEntitiesHandler(b *bridge) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		targets, ok := batchTargets(req, "entity_ids")
		if !ok || len(targets) == 0 {
			return mcp.NewToolResultError("`entity_ids` is required and must be a non-empty list of strings"), nil
		}
		return b.runBatch(ctx, "POST", targets, func(id string) string {
			return "/v1/entities/" + url.PathEscape(id) + "/archive"
		}), nil
	}
}

func registerDeleteEntities(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("delete_entities",
		mcp.WithDescription(
			"Batch variant of `delete_entity` — permanently delete many "+
				"already-archived entities in one call (non-reversible; each "+
				"id must be archived first or it reports a per-id failure). "+
				"Returns `{ok, total, succeeded, failed, results}` with a "+
				"per-id `{id, ok, status, error?}` list; a failure on one id "+
				"does not stop the rest.",
		),
		mcp.WithArray("entity_ids",
			mcp.Required(),
			mcp.Description("Entity ids to delete. Each must already be archived."),
			mcp.Items(map[string]any{"type": "string"}),
		),
	)
	s.AddTool(tool, deleteEntitiesHandler(b))
}

func deleteEntitiesHandler(b *bridge) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		targets, ok := batchTargets(req, "entity_ids")
		if !ok || len(targets) == 0 {
			return mcp.NewToolResultError("`entity_ids` is required and must be a non-empty list of strings"), nil
		}
		return b.runBatch(ctx, "DELETE", targets, func(id string) string {
			return "/v1/entities/" + url.PathEscape(id)
		}), nil
	}
}

func registerTaskResolveBatch(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("task_resolve_batch",
		mcp.WithDescription(
			"Batch variant of `task_resolve` — mark many workflow-produced "+
				"tasks done in one call (plain resolves, same auto-archive "+
				"semantics as the single tool). Returns `{ok, total, "+
				"succeeded, failed, results}` with a per-id `{id, ok, status, "+
				"error?}` list; a failure on one id does not stop the rest. "+
				"For resolution-tasks that need an `option` chosen, use the "+
				"single `task_resolve` per task — the choice is per-task and "+
				"not batchable.",
		),
		mcp.WithArray("task_ids",
			mcp.Required(),
			mcp.Description("Task ids (markdown file basenames without `.md`) to resolve."),
			mcp.Items(map[string]any{"type": "string"}),
		),
	)
	s.AddTool(tool, taskResolveBatchHandler(b))
}

func taskResolveBatchHandler(b *bridge) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		targets, ok := batchTargets(req, "task_ids")
		if !ok || len(targets) == 0 {
			return mcp.NewToolResultError("`task_ids` is required and must be a non-empty list of strings"), nil
		}
		return b.runBatch(ctx, "POST", targets, func(id string) string {
			return "/v1/tasks/" + url.PathEscape(id) + "/resolve"
		}), nil
	}
}
