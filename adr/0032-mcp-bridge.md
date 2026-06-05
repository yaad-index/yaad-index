# ADR-0032: The MCP bridge — in-process HTTP synthesis over the daemon mux

## Status

Proposed (2026-06-05). Documents the as-built MCP bridge (#435). This ADR
codifies an architecture already shipped on `main` (`internal/mcp/bridge.go`);
no code change accompanies it.

## Depends on

- ADR-0001 (fresh rewrite, AI-first remote API) — the remote-API-first premise.
  The MCP surface is a second front-end over the same API, not a parallel one.
- ADR-0002 (API surface) — the `/v1/...` HTTP routes the bridge invokes. The
  route contract *is* the MCP tool contract.
- ADR-0024 (workflows and tasks) — defines the agent-facing tool families
  (`workflow_*`, `task_*`) that ride the same bridge as the entity tools.

## Context

yaad-index is API-first (ADR-0001): every capability is an HTTP route under
`/v1/...`, fronted by one JWT auth middleware (ADR-0002). The daemon also
exposes those same capabilities to AI agents as MCP tools at `/mcp` over
Streamable HTTP — a tool surface spanning entity reads + lifecycle, search,
ingest, fill, edges, notes, user-generated content, workflows, and tasks.

The question this ADR settles is how an MCP tool handler actually *executes*.
Three shapes were available:

1. **Direct method invocation** — each tool handler calls the underlying Go
   service function (store method, vault writer, …) directly, bypassing the
   HTTP layer.
2. **Out-of-process IPC / loopback HTTP** — the MCP server issues real HTTP
   requests to the daemon's own listen port (or a Unix socket).
3. **In-process HTTP synthesis** — the tool handler builds an `http.Request`
   in memory and runs it through the same `http.Handler` (mux) the daemon
   serves on its public port, with no socket in between.

The README points evaluators at the ADRs first; the bridge was the largest
architectural decision that lacked one.

## Decision

**MCP tools execute by synthesizing in-process `http.Request`s against the
daemon's own mux.** `internal/mcp/bridge.go` owns this. A single `bridge`
wraps the daemon's `http.Handler`; every tool handler calls
`bridge.callTool(ctx, method, path, body)` (or a header-aware variant). The
bridge:

1. Builds the request with `httptest.NewRequest(method, path, body)` and
   attaches the per-request context.
2. Re-attaches the caller's `Authorization` header — sourced from the
   per-request context set by the MCP entry layer — so the inner route's auth
   gate sees the exact JWT the MCP caller presented.
3. Runs it through `handler.ServeHTTP(httptest.NewRecorder(), req)`: the **same**
   mux, **same** middleware chain, **same** per-route logic the daemon serves on
   its public port.
4. Captures the recorded status, body bytes, and response headers into a
   normalized `callResult`, which tool handlers project into an
   `mcp.CallToolResult`.

**The tool surface is the mux surface.** Each tool is a thin adapter:
validate args → `callTool` against a `/v1/...` route → project the result. No
tool re-implements business logic; there is exactly one code path for, say,
"ingest an entity" whether the caller arrives over REST or over MCP.

### Why in-process synthesis, not the alternatives

- **vs. direct method invocation.** Direct calls would force every tool to
  re-do what the HTTP middleware already does — auth verification, claim
  extraction, request validation, error-envelope shaping, logging. Each tool
  would re-derive the auth/validation contract, and the two front-ends would
  drift over time. Synthesis reuses the middleware chain verbatim, so REST and
  MCP cannot diverge on auth or validation.
- **vs. out-of-process / loopback HTTP.** A real socket buys nothing here — the
  MCP server and the daemon mux live in the same process — and costs a second
  listener, a serialization round-trip, and another trust boundary to secure.
  In-process `ServeHTTP` gives identical routing semantics with none of that.

### Auth claim propagation

The MCP entry layer extracts the inbound `Authorization` header into the
request context; the bridge reads it back and stamps it onto every synthesized
request. Tool-supplied extra headers may carry route semantics (e.g.
`If-Match` for the UGC etag-concurrency contract), but an `Authorization` key
in that map is **silently dropped** — the context-sourced JWT is the single
source of auth truth, so a tool can never accidentally escalate by overwriting
it.

### Error wrapping

Inner-route HTTP error statuses are not Go errors — they ride back in
`callResult.status`. `asMCPError` projects the daemon's canonical error
envelope (`{ok:false, error:<code>, message:<msg>}`) into a concise
`"<code>: <msg>"` MCP error string, falling back to `HTTP <status>: <body>`
for bodies that aren't the canonical envelope (e.g. a plain 401 from
middleware). The projection is centralized so all tools share one error
contract. Per MCP semantics, tool-call failures live inside the result
envelope (`isError`), not the JSON-RPC error channel; a Go `error` from the
bridge is reserved for synthesis-side faults (request construction, body read).

## Consequences

- **No streaming.** `httptest.ResponseRecorder` buffers the full response
  before the tool sees it; `callResult` holds the complete body in memory.
  Routes that would stream (SSE, chunked) collapse to a single buffered body
  through the bridge. Acceptable today — every tool returns a bounded JSON
  document the daemon already materializes fully — but a future streaming tool
  would need a path that doesn't go through the recorder.
- **Self-referential URLs are unsafe through the bridge.**
  `httptest.NewRequest` stamps the synthetic host `example.com`; a tool that
  builds a URL back to the caller must source the host from operator config or
  `X-Forwarded-Host`, never from `r.Host`. No current tool emits such URLs; the
  constraint is documented for future ones.
- **Header-carried contracts need explicit lifting.** Routes whose contract
  lives in response headers (UGC etag concurrency emits `ETag` on the header,
  never in the body) require a dedicated variant (`callToolWithEtagLift`) that
  lifts the header onto the body JSON, because the MCP result shape carries
  only a text body. New header-contract routes need the same treatment.
- **One contract, two front-ends, zero drift.** Adding or changing a `/v1/...`
  route changes the MCP tool's behavior in lockstep — auth, validation, and
  error shape are inherited, not re-coded. The cost is that MCP cannot expose
  anything the HTTP surface doesn't; that coupling is intentional.
- **Testing.** Because the bridge is `ServeHTTP` over the real mux, a tool test
  exercises the full middleware + route stack in-process with no network — the
  same property the daemon's own handler tests rely on.
