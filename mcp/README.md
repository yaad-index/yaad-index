# yaad-mcp

MCP server wrapping yaad-index — the agent's door to the yaad platform.

## What it does

Exposes yaad-index's REST API as MCP tools so any MCP-aware agent (Claude Code, Claude Desktop, future yaad-fleet agents) can ingest URLs and interact with entities directly. Outbox channels (Discord / email / etc.) will land via a separate yaad-outbox surface when that repo ships.

## Tools

In-side (real, against the operator-configured yaad-index):

| Tool | What it does |
|------|--------------|
| `ingest(url)` | POST `/v1/ingest`. Returns `{state, entity?, options?}` — `state` is `complete` / `needs_fill` / `disambiguation` / `queued`. Disambiguation surfaces ADR-0006 candidates as `options[]`. |
| `get_entity(id)` | GET `/v1/entities/{id}?with_edges=is_about`. Returns `{id, kind, data, provenance, edges}`; the `is_about` (canonical-axis) edge type expands inline. Other types not currently surfaced (upstream API requires non-empty `with_edges` value). |
| `get_entity_with_context(id, depth?, edge_types?, max_results?)` | GET `/v1/entities/{id}/context?depth=N`. BFS-stitches an entity plus its surrounding context (linked entities reachable within N edge-hops) in one call. Server-side cycle detection + depth bounding (cap 3) + max_results pagination. Returns `{root, neighbors: [{edge, entity, depth}], truncated}` verbatim. |
| `structure()` | GET `/v1/structure`. Returns the operator-configured yaad-index's structural signature: enabled canonical kinds (with gaps + per-kind instructions), enabled edge types, active plugin metadata, and a deterministic config-hash `version`. Verbatim pass-through. Agents that cache key on `version`. |
| `needs_fill(limit?, cursor?)` | GET `/v1/needs-fill`. Browse the open-gap queue: entities with unfilled gaps that haven't been gap-called for the current fetch-cycle. Paginated via opaque `next_cursor`; agent passes it back as-is on the next call until absent (queue exhausted). Verbatim pass-through; no auto-pagination. |
| `cv_status()` | GET `/v1/cv-status`. Canonical-vocabulary drift snapshot: per-(plugin, kind/edge_type) counters of canonical stubs the operator's config dropped at ingest time, plus `config_hash` for change detection and `last_reindex_at` for the operator-prompted reindex hint. Verbatim pass-through. Agents that cache key on `config_hash`. |
| `fill(id, fields)` | POST `/v1/entities/{id}/fill`. Each key in `fields` must be a current gap on the entity; returns the updated entity + remaining gap names. |
| `add_comment(entity_id, text, author?)` | POST `/v1/entities/{id}/comments` (via the comments API). Server stamps `date` UTC, `author` from JWT subject (when omitted), and `operator` from the pair-claim. Explicit `author` MUST equal the JWT subject or upstream returns `{ok:false, error:"author_mismatch"}` verbatim. Distinct from other tools: 4xx error envelopes pass through structured (no exception) so the agent can branch on `error === "author_mismatch"` / `"missing_authorization"`. |
| `list_entities(kind)` | GET `/v1/search?kind=...&limit=100`. `kind` is required — yaad-index has no list-all route. Returns `{results, total, limit, offset}` where each result is `{id, kind, snippet, score}`. |

## Setup

```bash
git clone git@github.com:yaad-index/yaad-mcp.git
cd yaad-mcp
bun install
```

Configuration via env:

| Variable | Default | Meaning |
|----------|---------|---------|
| `YAAD_INDEX_URL` | (required) | Base URL for yaad-index, e.g. `http://localhost:7433`. |
| `YAAD_INDEX_AUTH_TOKEN` | (none) | Bearer token sent as `Authorization: Bearer <token>` if set. |

Smoke-run:

```bash
YAAD_INDEX_URL=http://localhost:7433 bun run start
```

Server boots on stdio. To verify, send the `tools/list` JSON-RPC request via your MCP host of choice — should return all 25 tools.

## Add to Claude Code

In `~/.claude.json` under `mcpServers`:

```jsonc
{
 "mcpServers": {
 "yaad-mcp": {
 "command": "bun",
 "args": ["run", "/absolute/path/to/yaad-mcp/src/index.ts"],
 "env": {
 "YAAD_INDEX_URL": "http://localhost:7433"
 }
 }
 }
}
```

Restart Claude Code. The tools become available; the agent can ingest URLs, fill gaps, append comments, and explore entities directly.

## Tests

```bash
bun test
```

Tests run against an injected fake `fetch` so they don't need a live yaad-index. The client (`src/client/yaad_index.ts`) takes a `fetchImpl` option exactly so tests can swap it in.

## For agents using this MCP

See [SKILL.md](./SKILL.md) — the agent-facing skill explaining the mental model (source-shape vs canonical-shape entities, kind-driven discovery, the gap/fill protocol, conventions). Plugin-agnostic; teaches the graph and the tools, not "how to ingest Wikipedia."

## Status

v0. Tool surface is iterating per yaad-index's contract changes (re-read SKILL.md when behavior shifts). Outbox channels land via a separate yaad-outbox surface when that repo ships.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
