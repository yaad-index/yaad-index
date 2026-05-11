import type { YaadIndexClient } from "../client/yaad_index.js";

export const getEntityTool = {
 name: "get_entity",
 description:
 "Fetch a yaad-index entity by id. Returns `{id, kind, data, " +
 "provenance, edges}`. The `is_about` edge type is expanded inline " +
 "(canonical-axis traversal); other edge types are not currently " +
 "surfaced (yaad-index API limitation, follow-up tracked). Call " +
 "`get_entity(<edge.to>)` to walk from a source-shape entity to " +
 "its canonical stub.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id, e.g. `wikipedia:tehran` or `person:martin-wallace`.",
 },
 },
 required: ["id"],
 },
} as const;

export async function runGetEntity(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 return await client.getEntity(id);
}
