// Canonical-registry introspection tools per #48 slice 3.
// Wrappers around the `/v1/canonical_registry/*` HTTP routes
// for agent-side discovery of the merged canonical-kinds
// registry + the daemon-shipped starter-pool kinds operators
// can opt into.

package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerCanonicalRegistryEffective(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("canonical_registry_effective",
		mcp.WithDescription(
			"Return the merged canonical_kinds registry the daemon "+
				"is currently using, annotated with per-(kind, field) "+
				"source_layer provenance (one of: code_defaults / "+
				"builtin_kind / plugin_extras / operator_defaults / "+
				"operator) so callers can tell which layer of the "+
				"4-layer merge supplied each gap spec. Verbatim from "+
				"`GET /v1/canonical_registry/effective`. Use this to "+
				"answer 'what's actually active for kind X right now, "+
				"and which gaps are operator-overridden vs riding "+
				"plugin or daemon defaults?'.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/canonical_registry/effective", nil)
	})
}

func registerCanonicalRegistryAvailable(s *server.MCPServer, b *bridge) {
	tool := mcp.NewTool("canonical_registry_available",
		mcp.WithDescription(
			"Return daemon-shipped Layer 1.5 canonical-kinds that "+
				"exist as starter-pool defaults but aren't currently "+
				"active in the merged registry. Operators inspect "+
				"'what could I opt into?' before writing config. "+
				"Verbatim from `GET /v1/canonical_registry/available`. "+
				"A kind activates once a plugin's "+
				"`canonical_kinds_emitted` triggers it OR operator "+
				"config explicitly lists it under `canonical_kinds:` — "+
				"at which point it leaves the available set and "+
				"appears under `canonical_registry/effective` "+
				"instead.",
		),
	)
	s.AddTool(tool, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.callTool(ctx, "GET", "/v1/canonical_registry/available", nil)
	})
}
