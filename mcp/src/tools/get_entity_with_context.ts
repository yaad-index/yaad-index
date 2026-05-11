// Multi-hop context stitch tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `GET /v1/entities/{id}/context`
// (yaad-index the source issue). Adds NO client-side traversal logic — the
// server handles BFS, cycle detection, depth bounding, edge-type
// filtering, and pagination. yaad-mcp just surfaces the canonical
// response shape under an MCP tool name.

import type { YaadIndexClient } from "../client/yaad_index.js";

const DEFAULT_DEPTH = 1;
const SERVER_DEPTH_CAP = 3;

export const getEntityWithContextTool = {
 name: "get_entity_with_context",
 description:
 "Fetch an entity plus its surrounding context (linked entities " +
 "reachable within N edge-hops) in one call. Server-side BFS with " +
 "cycle detection: each entity appears at most once across `root` + " +
 "`neighbors`. `depth` defaults to 1 (entity + direct neighbors); " +
 "0 = just the entity; cap is 3. Optional `edge_types` filter " +
 "restricts traversal to the named edge types AND surfaces only " +
 "those edges. Returns `{root, neighbors: [{edge, entity, depth}], " +
 "truncated}` verbatim from yaad-index. Use case: assemble the full " +
 "context for a PR review (PR + linked Jira ticket + linked " +
 "Confluence doc + canonical process stub) without per-hop loops.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id, e.g. `wikipedia:tehran` or `boardgame:brass-birmingham`.",
 },
 depth: {
 // `integer` so JSON-Schema-conforming MCP clients reject 1.5
 // at the schema layer; matches the runtime `Number.isInteger`
 // guard. Same-class fix as needs_fill per the cold-reviewer's a prior PR catch.
 type: "integer",
 description:
 "BFS hops to walk. 0 = just the entity (no neighbors); 1 = entity + " +
 "direct neighbors (default); cap is 3. Higher values are rejected by " +
 "the server with `400 invalid_argument`.",
 minimum: 0,
 maximum: SERVER_DEPTH_CAP,
 },
 edge_types: {
 type: "array",
 items: { type: "string" },
 description:
 "Optional edge-type filter. When set, only edges of the named types " +
 "are walked AND only those edges appear on the response neighbors. " +
 "Omit to walk all edge types.",
 },
 max_results: {
 // `integer` matches the runtime `Number.isInteger` guard;
 // same-class fix as needs_fill per the cold-reviewer's a prior PR catch.
 type: "integer",
 description:
 "Optional cap on neighbors array. Default 200, server cap 1000. " +
 "When the unbounded result exceeds this, the response is " +
 "truncated to the prefix that fit and `truncated: true` is set.",
 minimum: 1,
 maximum: 1000,
 },
 },
 required: ["id"],
 },
} as const;

export async function runGetEntityWithContext(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }

 let depth = DEFAULT_DEPTH;
 if (args.depth !== undefined) {
 if (typeof args.depth !== "number" || !Number.isInteger(args.depth)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`depth` must be an integer",
 };
 }
 if (args.depth < 0 || args.depth > SERVER_DEPTH_CAP) {
 return {
 ok: false,
 error: "invalid_argument",
 message: `\`depth\` must be in [0, ${SERVER_DEPTH_CAP}]`,
 };
 }
 depth = args.depth;
 }

 let edgeTypes: string[] | undefined;
 if (args.edge_types !== undefined) {
 if (!Array.isArray(args.edge_types)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`edge_types` must be an array of strings",
 };
 }
 edgeTypes = args.edge_types
 .map((t) => (typeof t === "string" ? t.trim() : ""))
 .filter((t) => t !== "");
 if (edgeTypes.length === 0) {
 // An array that parses to nothing is treated as "no filter" rather
 // than rejected — same forgiving behavior as the server-side
 // parser, so passing an empty array is an explicit "include all
 // edge types" signal.
 edgeTypes = undefined;
 }
 }

 let maxResults: number | undefined;
 if (args.max_results !== undefined) {
 if (typeof args.max_results !== "number" || !Number.isInteger(args.max_results)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`max_results` must be an integer",
 };
 }
 if (args.max_results <= 0 || args.max_results > 1000) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`max_results` must be in (0, 1000]",
 };
 }
 maxResults = args.max_results;
 }

 return await client.getEntityContext(id, depth, edgeTypes, maxResults);
}
