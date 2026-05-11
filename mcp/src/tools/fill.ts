import type { YaadIndexClient } from "../client/yaad_index.js";

export const fillTool = {
 name: "fill",
 description:
 "Fill an entity's open gaps with agent-derived values. Each key in " +
 "`fields` must be a current gap on the entity (per the entity's " +
 "frontmatter `gaps:` list); a key that isn't currently a gap fails " +
 "the whole call (no partial success). Returns the updated entity + " +
 "the remaining unfilled gap field names.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id whose gaps are being filled.",
 },
 fields: {
 type: "object",
 description:
 "{field-name → value} map. Field names must be in the entity's " +
 "current `gaps` set; values are the agent-derived content (string, " +
 "number, list — whatever shape the gap declared).",
 additionalProperties: true,
 },
 },
 required: ["id", "fields"],
 },
} as const;

export async function runFill(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 const fields = args.fields;
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 if (!fields || typeof fields !== "object" || Array.isArray(fields)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`fields` is required and must be a non-empty object",
 };
 }
 return await client.fill(id, fields as Record<string, unknown>);
}
