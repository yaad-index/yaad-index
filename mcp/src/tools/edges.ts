import type { YaadIndexClient } from "../client/yaad_index.js";

export const edgesTool = {
 name: "edges",
 description:
 "Single-hop edge query. Wraps " +
 "`GET /v1/edges?entity_id=X[&edge_types=...][&direction=...]`. " +
 "Returns the typed edges from/to the entity in one round-trip. " +
 "Distinct from `get_entity_with_context`: this is the **flat one-hop** " +
 "surface (\"who designed this game\" / \"what cites this person\"); " +
 "`get_entity_with_context` does N-hop BFS traversal — overkill for " +
 "direct-edge queries. " +
 "Direction defaults to **out** (backward-compat with the existing " +
 "per-entity edge-expansion semantic). `direction=in` returns " +
 "inbound edges only (entity_id matches `to_id`); `direction=both` " +
 "returns inbound + outbound combined. " +
 "`edge_types` is an optional allowlist; absent → no type filter " +
 "(every edge type returns). " +
 "Response shape: `{ok, edges: [{type, from_id, to_id, metadata?}], " +
 "next_cursor}`. `next_cursor` is reserved for forward-compat and " +
 "always null today (single-hop counts per entity are bounded).",
 inputSchema: {
 type: "object",
 properties: {
 entity_id: {
 type: "string",
 description:
 "Full entity id in the `<kind>:<local-id>` shape — e.g. " +
 "`boardgame:brass-birmingham-2018`. The role (from vs " +
 "to) depends on direction.",
 },
 edge_types: {
 type: "array",
 items: { type: "string" },
 description:
 "Optional allowlist of edge types to return (e.g. " +
 "`[\"designed_by\", \"authored_by\"]`). Absent or empty " +
 "→ no filter, every edge type returns.",
 },
 direction: {
 type: "string",
 enum: ["out", "in", "both"],
 description:
 "Edge direction relative to entity_id. `out` (default) " +
 "= entity_id is from_id. `in` = entity_id is to_id. " +
 "`both` = inbound + outbound combined.",
 },
 },
 required: ["entity_id"],
 },
} as const;

export async function runEdges(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.entity_id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`entity_id` is required" };
 }
 let edgeTypes: string[] | undefined;
 if (args.edge_types !== undefined && args.edge_types !== null) {
 if (!Array.isArray(args.edge_types)) {
 return { ok: false, error: "invalid_argument", message: "`edge_types` must be an array of strings" };
 }
 edgeTypes = args.edge_types.map((s) => String(s));
 }
 const direction = args.direction;
 if (direction !== undefined && direction !== "out" && direction !== "in" && direction !== "both") {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`direction` must be one of {out, in, both}",
 };
 }
 return client.edges(id, edgeTypes, direction as "out" | "in" | "both" | undefined);
}
