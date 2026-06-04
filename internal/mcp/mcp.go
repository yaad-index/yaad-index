// Package mcp exposes the yaad-index daemon's HTTP API as a
// Model Context Protocol server per #101. Built on
// mark3labs/mcp-go's Streamable HTTP transport: a single
// `/mcp` endpoint speaks MCP JSON-RPC + SSE; per-tool
// handlers bridge into the daemon's existing HTTP routes via
// httptest.ResponseRecorder + the same mux that serves the
// REST surface (no network loopback, same auth gate, same
// per-route logic).
//
// **Design contract**: each MCP tool is a thin wrapper around
// an existing `/v1/...` route. The bridge synthesizes an
// http.Request, hands it to the daemon's mux, reads the
// recorded response — zero new business logic in the MCP
// layer. Future-tracked refactor (#101 follow-up) may extract
// per-route core functions for a cleaner abstraction; for
// Cut 1 the bridge approach minimizes handler-carving cost.
//
// **Auth**: the `/mcp` route lives behind the same
// `buildAuthMiddleware` chain as every other protected
// route, so the JWT is validated once at MCP entry. The
// Authorization header is extracted into the per-request
// context via `WithHTTPContextFunc` so the bridge can pass
// it through to the inner route's own auth check. Two-layer
// validation (the entry middleware AND the bridged route's
// middleware) is intentional: each MCP tool re-enters the
// mux from scratch so the route's own auth gate fires
// regardless of the entry-point check.
package mcp

import (
	"context"
	"net/http"

	"github.com/mark3labs/mcp-go/server"
)

// authContextKey carries the Authorization header from the
// incoming MCP HTTP request through to the bridge. Unexported
// type prevents collision with other packages' context keys.
type authContextKey struct{}

// NewHandler constructs the Streamable HTTP MCP handler.
// apiHandler is the daemon's full mux (the SAME instance that
// also routes /mcp itself — the bridge re-enters the mux for
// each tool invocation). version is surfaced in the MCP
// initialize handshake.
//
// The returned handler is mounted under the daemon's auth
// middleware in main.go — same protect() chain that wraps
// every /v1/... protected route.
func NewHandler(apiHandler http.Handler, version string) http.Handler {
	srv := server.NewMCPServer(
		"yaad-index",
		version,
		server.WithToolCapabilities(false),
		// WithRecovery is the safety net for tool handler
		// panics — translates a panic into a structured
		// error result rather than crashing the worker.
		server.WithRecovery(),
	)

	bridge := newBridge(apiHandler)
	registerAll(srv, bridge)

	// Stateless session management (every request stands
	// alone, server-side keeps no session state) is the
	// natural fit for the daemon's JWT pair-claim model:
	// each MCP request carries its own Bearer token, which
	// the auth middleware validates independently. Stateful
	// session affinity is unnecessary — there's no per-
	// session server state to preserve across requests.
	// This resolves open-question #2 from #101.
	return server.NewStreamableHTTPServer(srv,
		server.WithHTTPContextFunc(extractAuthHeader),
		server.WithStateLess(true),
	)
}

// registerAll wires every tool the daemon exposes via MCP.
// Grouped by responsibility — entities / search / ingest /
// fill / edges / notes / system / user-content / workflows /
// tasks — matching the per-file layout under internal/mcp/.
// Adding a tool means appending one `register*` call here and
// dropping the implementation in the appropriate file.
func registerAll(s *server.MCPServer, b *bridge) {
	// Entity reads + lifecycle.
	registerGetEntity(s, b)
	registerGetEntitiesBatch(s, b)
	registerGetEntityWithContext(s, b)
	registerListEntities(s, b)
	registerArchiveEntity(s, b)
	registerRestoreEntity(s, b)
	registerDeleteEntity(s, b)

	// Search.
	registerSearchLocal(s, b)
	registerSearchUpstream(s, b)

	// Ingest.
	registerIngest(s, b)

	// Fill (canonical-kind gap workflow). #355 Cut 3: fill_field is
	// the preferred name per ADR-0029 §7; set_operator_fill stays
	// registered as a compat alias for one minor version.
	registerFill(s, b)
	registerFillField(s, b)
	registerSetOperatorFill(s, b)
	registerDeferGap(s, b)
	registerNeedsFill(s, b)
	registerCVStatus(s, b)
	registerCanonicalRegistryEffective(s, b)
	registerCanonicalRegistryAvailable(s, b)
	registerCreateCanonicalEntity(s, b)

	// Edges.
	registerEdges(s, b)
	registerUpdateEdgeTarget(s, b)

	// Notes (entity-level ADR-0020 notes).
	registerAddNote(s, b)
	registerEditNote(s, b)
	registerDeleteNote(s, b)

	// System metadata.
	registerStructure(s, b)
	registerKinds(s, b)
	registerPlugins(s, b)
	registerReindex(s, b)

	// User content (UGC).
	registerCreateUserContent(s, b)
	registerGetUserContent(s, b)
	registerDeleteUserContent(s, b)
	registerMoveUserContent(s, b)
	registerListUserContentSections(s, b)
	registerGetUserContentSection(s, b)
	registerEditUserContentSection(s, b)
	registerAddUserContentSection(s, b)
	registerRenameUserContentSection(s, b)
	registerDeleteUserContentSection(s, b)

	// Workflows.
	registerWorkflowList(s, b)
	registerWorkflowDiscover(s, b)
	registerWorkflowTrigger(s, b)
	registerWorkflowGet(s, b)
	registerWorkflowDefine(s, b)
	registerWorkflowDelete(s, b)

	// Tasks.
	registerTaskList(s, b)
	registerTaskLoad(s, b)
	registerTaskResolve(s, b)
}

// extractAuthHeader is invoked on every incoming MCP HTTP
// request. Stashes the Authorization header in the per-
// request context so the bridge can re-attach it to the
// synthesized http.Request that re-enters the mux for a
// tool's underlying /v1/... route.
func extractAuthHeader(ctx context.Context, r *http.Request) context.Context {
	auth := r.Header.Get("Authorization")
	if auth != "" {
		ctx = context.WithValue(ctx, authContextKey{}, auth)
	}
	return ctx
}

// authHeaderFromContext returns the Authorization header
// stashed by extractAuthHeader, or "" when the MCP request
// arrived without one (which the daemon's auth middleware
// rejects upstream when auth.required=true; the empty
// fallback is for the auth-disabled / test path).
func authHeaderFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(authContextKey{}).(string); ok {
		return v
	}
	return ""
}
