// Plugin-emitted source-kinds registry tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `GET /v1/kinds` endpoint
// (ADR-0002 §"GET /v1/kinds"). Returns the union of every registered
// plugin's declared entity-kinds and edge-kinds — distinct from
// `structure()` which surfaces the canonical-shape registry
// (operator config + ADR-0016 four-layer merged gaps), and from
// `cv_status()` which surfaces the gating-shape drift counters.
//
// `/v1/kinds` does not accept a name= query filter — the optional
// `name` parameter is implemented client-side at the tool boundary,
// filtering both arrays in the response down to entries whose
// `name` matches. Empty matches → both arrays empty (`ok` stays
// true; the daemon's response shape is preserved).

import type { YaadIndexClient } from "../client/yaad_index.js";
import type { KindsResponse } from "../types.js";

export const kindsTool = {
 name: "kinds",
 description:
 "Plugin-emitted source-kinds registry. Wraps `GET /v1/kinds`. " +
 "Returns `{ok, entity_kinds, edge_kinds}` — the union of every " +
 "registered plugin's declared kinds. Each entry: `{name, " +
 "description, source_plugins, [from_kind, to_kind]}`. Distinct " +
 "from `structure()` (canonical-shape registry with merged gaps + " +
 "instructions) and `cv_status()` (gating-shape " +
 "drift counters): `kinds()` is the plugin-author-perspective " +
 "surface — \"what does each plugin declare it can produce.\" " +
 "Optional `name` argument filters both arrays client-side to " +
 "entries whose `name` matches; the daemon endpoint itself " +
 "doesn't accept a filter param. Use when you need to know " +
 "which plugin emits a kind, or what edge-types the daemon " +
 "knows about across all loaded plugins.",
 inputSchema: {
 type: "object",
 properties: {
 name: {
 type: "string",
 description:
 "Optional kind name to filter to. When set, both " +
 "`entity_kinds` and `edge_kinds` arrays are reduced to " +
 "entries whose `name` matches. Filtering is client-side; " +
 "no narrowing on the wire.",
 },
 },
 additionalProperties: false,
 },
} as const;

export async function runKinds(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const name = args.name;
 if (name !== undefined && (typeof name !== "string" || name === "")) {
 return {
 ok: false,
 error: "invalid_argument",
 message: `name must be a non-empty string when provided (got ${JSON.stringify(name)})`,
 };
 }
 const response = await client.getKinds();
 if (typeof name !== "string") {
 return response;
 }
 const filtered: KindsResponse = {
 ok: response.ok,
 entity_kinds: response.entity_kinds.filter((e) => e.name === name),
 edge_kinds: response.edge_kinds.filter((e) => e.name === name),
 };
 return filtered;
}
