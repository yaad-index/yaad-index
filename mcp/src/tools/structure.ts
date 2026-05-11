// Structure introspection tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `GET /v1/structure` endpoint
// (ADR-0013 §7, landed in a prior PR). yaad-mcp adds
// NO logic beyond HTTP transport + MCP framing — the agent reads
// the canonical response shape directly. No reshape, no summary,
// no client-side caching keyed by `version` (the agent decides
// whether to cache and on which key).

import type { YaadIndexClient } from "../client/yaad_index.js";

export const structureTool = {
 name: "structure",
 description:
 "Introspect the operator-configured yaad-index. Returns the " +
 "structural signature: enabled canonical kinds (with gaps + " +
 "per-kind instructions), enabled canonical edge types, active " +
 "plugin metadata (name, version, url_patterns, supports_search, " +
 "emits_kinds, emits_edges), and a deterministic config-hash " +
 "`version` field. Same config → same `version`; reorder-stable. " +
 "Agents that cache results key on `version` and re-fetch when " +
 "the value changes. Verbatim pass-through from " +
 "`GET /v1/structure`; yaad-mcp adds no reshape, summary, or " +
 "client-side caching.",
 inputSchema: {
 type: "object",
 properties: {},
 additionalProperties: false,
 },
} as const;

export async function runStructure(
 client: YaadIndexClient,
 _args: Record<string, unknown>,
): Promise<unknown> {
 return await client.getStructure();
}
