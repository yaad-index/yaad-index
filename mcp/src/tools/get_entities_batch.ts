// Batch entity fetch tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `POST /v1/entities/batch` endpoint
// (ADR-0002 §"GET /v1/entities/batch"; method actually POST per the
// daemon's mux registration). yaad-mcp adds NO logic beyond HTTP
// transport, MCP framing, and arg validation — the daemon's response
// shape (`{ok, entities, missing}`) is the agent-facing surface
// verbatim. The `missing` field name is daemon-canonical (NOT
// reshaped to `not_found` even though that may read more naturally
// to TS-side callers — the source of truth is the daemon).

import type { YaadIndexClient } from "../client/yaad_index.js";

export const getEntitiesBatchTool = {
 name: "get_entities_batch",
 description:
 "Batch-fetch multiple entities in one round-trip. Wraps " +
 "`POST /v1/entities/batch`. Returns `{ok, entities, missing}` " +
 "verbatim — `entities` carries the resolved entities (same wire " +
 "shape as `get_entity`), `missing` is the array of ids the " +
 "daemon has no row for. Placeholder entities from an in-flight " +
 "ingest land in `entities` " +
 "with sparse `data`; `missing` is reserved for ids the daemon " +
 "has never seen. The daemon caps batch size at 100; calls " +
 "exceeding that bubble as a `too_many_ids` error (split + retry). " +
 "Use this when walking edges to fetch N targets in one round-trip " +
 "instead of N× `get_entity`. The `with_edges` param is plumbed " +
 "through but currently accepted-and-ignored on the batch surface — " +
 "edges return empty regardless. Use `get_entity` or " +
 "`get_entity_with_context` when you need edge expansion.",
 inputSchema: {
 type: "object",
 properties: {
 ids: {
 type: "array",
 items: { type: "string" },
 minItems: 1,
 description:
 "Entity ids to fetch in one round-trip (e.g. " +
 "`['person:martin-wallace', 'company:roxley']`). Daemon " +
 "caps at 100 ids per call.",
 },
 with_edges: {
 type: "array",
 items: { type: "string" },
 description:
 "Forward-compat hint for inline edge expansion. The daemon " +
 "accepts the field but does not yet expand edges on the " +
 "batch surface — `entities[i].edges` returns empty " +
 "regardless. Pass when the edge-side cutover lands.",
 },
 },
 required: ["ids"],
 additionalProperties: false,
 },
} as const;

export async function runGetEntitiesBatch(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const ids = args.ids;
 if (!Array.isArray(ids) || ids.length === 0) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "ids must be a non-empty array of strings",
 };
 }
 for (const id of ids) {
 if (typeof id !== "string" || id === "") {
 return {
 ok: false,
 error: "invalid_argument",
 message: `ids must contain non-empty strings (got ${JSON.stringify(id)})`,
 };
 }
 }
 let withEdges: string[] | undefined;
 if (args.with_edges !== undefined) {
 if (!Array.isArray(args.with_edges)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "with_edges must be an array of strings",
 };
 }
 for (const t of args.with_edges) {
 if (typeof t !== "string" || t === "") {
 return {
 ok: false,
 error: "invalid_argument",
 message: `with_edges must contain non-empty strings (got ${JSON.stringify(t)})`,
 };
 }
 }
 withEdges = args.with_edges as string[];
 }
 return await client.getEntitiesBatch(ids as string[], withEdges);
}
