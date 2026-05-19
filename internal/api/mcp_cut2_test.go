// Representative #101 Cut 2 tests: one happy-path sample
// per tool category (read / write / system) plus the cross-
// cutting error-projection contract + the registration
// wiring proof. Auth path itself is covered exhaustively
// in mcp_test.go (Cut 1); this file avoids 4×-per-tool auth
// duplication.

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// mcpCallTool is mcpCall + tool-result envelope decode, the
// common-path projection every per-tool test wants. Returns
// the decoded text content + the isError flag.
func mcpCallTool(t *testing.T, h http.Handler, token, toolName string, args map[string]any) (text string, isError bool) {
	t.Helper()
	rec := mcpCall(t, h, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "tool %s: body=%s", toolName, rec.Body.String())
	var env struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env),
		"tool %s: decode envelope; body=%s", toolName, rec.Body.String())
	require.Nil(t, env.Error, "tool %s: JSON-RPC error envelope; body=%s", toolName, rec.Body.String())
	require.NotEmpty(t, env.Result.Content, "tool %s: empty content; body=%s", toolName, rec.Body.String())
	return env.Result.Content[0].Text, env.Result.IsError
}

// TestMCP_Cut2_ToolsListEnumeratesAll proves the registerAll
// wiring landed — the full set of 33 tools surfaces in
// tools/list. Failure here means a register* call was
// dropped from registerAll, not that any individual tool
// is broken.
func TestMCP_Cut2_ToolsListEnumeratesAll(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	rec := mcpCall(t, h, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var env struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	names := map[string]struct{}{}
	for _, tt := range env.Result.Tools {
		names[tt.Name] = struct{}{}
	}
	want := []string{
		// Entities.
		"get_entity", "get_entities_batch", "get_entity_with_context",
		"list_entities", "archive_entity", "restore_entity", "delete_entity",
		// Search.
		"search_local", "search_upstream",
		// Ingest.
		"ingest",
		// Fill.
		"fill", "set_operator_fill", "defer_gap", "needs_fill", "cv_status",
		// Edges.
		"edges",
		// Notes.
		"add_note",
		// System metadata.
		"structure", "kinds", "plugins", "reindex",
		// UGC.
		"create_user_content", "get_user_content", "delete_user_content",
		"list_user_content_sections", "get_user_content_section",
		"edit_user_content_section",
		// Workflows.
		"workflow_list", "workflow_discover", "workflow_trigger",
		// Tasks.
		"task_list", "task_load", "task_resolve",
	}
	for _, n := range want {
		_, ok := names[n]
		assert.Truef(t, ok, "tool %s not registered; have %d tools", n, len(names))
	}
}

// TestMCP_Cut2_ListEntities is the read-category sample —
// list_entities round-trips against the seeded store and
// returns the entity in the JSON shape the daemon emits.
func TestMCP_Cut2_ListEntities(t *testing.T) {
	t.Parallel()
	h, signer, st := newAuthedMCPHandler(t)
	const id = "wikipedia:list-entities-fixture"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   id,
		Kind: "wikipedia",
		Data: map[string]any{
			"id":    id,
			"title": "List Fixture",
		},
	}))
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	text, isErr := mcpCallTool(t, h, token, "list_entities", map[string]any{
		"kind": "wikipedia",
	})
	require.False(t, isErr, "list_entities returned error; body=%s", text)
	assert.Contains(t, text, id, "list_entities returned the seeded entity id")
}

// TestMCP_Cut2_ArchiveEntity_VaultRequired is the write-
// category sample. The newAuthedMCPHandler fixture wires no
// vault, so the archive route correctly surfaces 503
// vault_required — this proves the bridge actually reaches
// the daemon's write handler AND that asMCPError shapes the
// daemon's envelope (vs masking it as a JSON-RPC error). The
// write-with-real-side-effect path is exercised in the
// daemon's own per-route tests; the MCP layer's contract is
// solely "wrap + project", verified here.
func TestMCP_Cut2_ArchiveEntity_VaultRequired(t *testing.T) {
	t.Parallel()
	h, signer, st := newAuthedMCPHandler(t)
	const id = "wikipedia:archive-fixture"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   id,
		Kind: "wikipedia",
		Data: map[string]any{"id": id, "title": "Archive Fixture"},
	}))
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	text, isErr := mcpCallTool(t, h, token, "archive_entity", map[string]any{
		"id": id,
	})
	require.True(t, isErr, "archive without vault wiring surfaces as error; got %q", text)
	assert.True(t, strings.HasPrefix(text, "vault_required"),
		"asMCPError emitted the daemon's envelope code; got %q", text)
}

// TestMCP_Cut2_Structure is the system-metadata-category
// sample — structure takes no args + always returns 200.
func TestMCP_Cut2_Structure(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	text, isErr := mcpCallTool(t, h, token, "structure", map[string]any{})
	require.False(t, isErr, "structure returned error; body=%s", text)
	// `kinds` is the load-bearing field every daemon /v1/structure
	// response carries — proves the bridge round-tripped the
	// daemon's emitted JSON verbatim.
	assert.Contains(t, text, "kinds",
		"structure result carries the kinds field; got %q", text)
}

// TestMCP_Cut2_ErrorProjection_InvalidArgument proves the
// asMCPError projection contract: a bridged 4xx surfaces as
// an MCP error result with the daemon's `<code>: <message>`
// envelope shape, not as a JSON-RPC error.
func TestMCP_Cut2_ErrorProjection_InvalidArgument(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	// edit_user_content_section requires an etag — call without
	// one. The local arg-validator catches it BEFORE the bridge,
	// so this exercises the per-tool validate path.
	text, isErr := mcpCallTool(t, h, token, "edit_user_content_section", map[string]any{
		"id":   "user-content:nonexistent",
		"sec":  "intro",
		"body": "x",
	})
	require.True(t, isErr, "missing etag should produce an MCP error result; got %q", text)
	assert.Contains(t, strings.ToLower(text), "etag",
		"error text mentions the missing etag; got %q", text)
}

// TestMCP_Cut2_ErrorProjection_DaemonEnvelope proves the
// daemon's error envelope projects through asMCPError:
// a get_entity for an unknown id surfaces a `not_found`
// MCP error text, not the raw `HTTP 404: {...}` fallback.
func TestMCP_Cut2_ErrorProjection_DaemonEnvelope(t *testing.T) {
	t.Parallel()
	h, signer, _ := newAuthedMCPHandler(t)
	token := mintMCPToken(t, signer, "agent:alice", "alice")

	text, isErr := mcpCallTool(t, h, token, "get_entity", map[string]any{
		"id": "wikipedia:envelope-test-never-existed",
	})
	require.True(t, isErr, "unknown entity should produce an MCP error result")
	assert.True(t, strings.HasPrefix(text, "not_found"),
		"asMCPError emitted the `<code>: <message>` shape; got %q", text)
}
