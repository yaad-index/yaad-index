// Canonical-vocabulary drift status tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `GET /v1/cv-status` endpoint
// (ADR-0013 §3, landed in a prior PR). yaad-mcp adds
// NO logic beyond HTTP transport + MCP framing — agents read the
// canonical response shape directly and decide if/how to cache
// (typically keyed by `config_hash`).
//
// Pairs with `structure()` and `needs_fill()` to complete the
// ADR-0013 §3 introspection trio on the agent-facing MCP side:
//
// - structure() — what's configured / what's loaded
// - needs_fill() — what entities have open gaps
// - cv_status() — given the configured shape, what's drifting

import type { YaadIndexClient } from "../client/yaad_index.js";

export const cvStatusTool = {
 name: "cv_status",
 description:
 "Check canonical-vocabulary drift on the operator-configured " +
 "yaad-index. Returns `{ok, config_hash, drift, last_reindex_at, " +
 "reindex_hint}` verbatim from `GET /v1/cv-status`. `drift` " +
 "carries per-(plugin, kind/edge_type) counters of canonical " +
 "stubs the operator's config dropped at ingest time — the " +
 "concrete signal of \"you would have materialized N more " +
 "entities if you enabled these kinds.\" `config_hash` is a " +
 "deterministic SHA over the canonical-vocabulary subset of " +
 "the config (distinct from /v1/structure's `version`); same " +
 "config → same hash. Agents that cache results key on " +
 "`config_hash` and re-fetch when it changes. Verbatim pass-" +
 "through; yaad-mcp adds no reshape, summary, diff helpers, " +
 "or client-side caching.",
 inputSchema: {
 type: "object",
 properties: {},
 additionalProperties: false,
 },
} as const;

export async function runCVStatus(
 client: YaadIndexClient,
 _args: Record<string, unknown>,
): Promise<unknown> {
 return await client.getCVStatus();
}
