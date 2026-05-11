import type { YaadIndexClient } from "../client/yaad_index.js";

export const listEntitiesTool = {
 name: "list_entities",
 description:
 "List entities of a given kind. Returns `{results, total, limit, " +
 "offset}` where each result is `{id, kind, snippet, score}` — call " +
 "`get_entity(id)` for any id to load full state.\n\n" +
 "The `kind` parameter is required. yaad-index's `/v1/search` " +
 "endpoint requires either a query string or a kind filter, and the " +
 "MCP surface only exposes kind-only listing — there is no list-all " +
 "route. For free-text search across entities, file an issue if " +
 "needed.",
 inputSchema: {
 type: "object",
 properties: {
 kind: {
 type: "string",
 description:
 "Kind filter, e.g. `wikipedia-article`, `person`, `boardgame`. " +
 "Required.",
 },
 },
 required: ["kind"],
 },
} as const;

export async function runListEntities(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const kind = typeof args.kind === "string" && args.kind.length > 0 ? args.kind : "";
 if (!kind) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`kind` is required (yaad-index has no list-all route)",
 };
 }
 return await client.listEntities(kind);
}
