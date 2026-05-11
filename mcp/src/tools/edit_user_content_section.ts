import type { YaadIndexClient } from "../client/yaad_index.js";

export const editUserContentSectionTool = {
 name: "edit_user_content_section",
 description:
 "Replace one section's body on a UGC entity. THE ETAG IS REQUIRED: " +
 "read it from a prior `get_user_content` / `get_user_content_section` " +
 "call (lifted onto the response object as `etag`) and pass it back " +
 "here as If-Match concurrency. Server returns " +
 "`{ok, id, section, etag}` on success. " +
 "**Error envelopes** (passthrough, NOT throws — branch on `ok===false`): " +
 "`error: \"precondition_failed\"` (412, stale etag — `current_etag` " +
 "rides on the envelope; refetch + retry); `error: \"precondition_required\"` " +
 "(428, missing etag — caller forgot to pass it); `error: \"author_mismatch\"` " +
 "(403, JWT claim doesn't match the entity's author/operator). 5xx still " +
 "throws.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id (must start with `user-content:`).",
 },
 sec: {
 type: "string",
 description:
 "Section address: heading-text-slug for unique headings OR positional index (`0`, `1`, …).",
 },
 body: {
 type: "string",
 description:
 "New section body, taken verbatim. Per the containment model, this replaces the whole sub-tree under the addressed section — the agent owns whether to keep nested headings.",
 },
 etag: {
 type: "string",
 description:
 "If-Match etag from a prior read of this entity. Stale → 412 precondition_failed (envelope carries `current_etag` for retry).",
 },
 },
 required: ["id", "sec", "body", "etag"],
 },
} as const;

export async function runEditUserContentSection(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 const sec = String(args.sec ?? "");
 if (!sec) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`sec` is required (heading slug OR positional index)",
 };
 }
 if (typeof args.body !== "string") {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`body` is required and must be a string",
 };
 }
 const etag = String(args.etag ?? "");
 if (!etag) {
 return {
 ok: false,
 error: "invalid_argument",
 message:
 "`etag` is required — read it from a prior get_user_content / get_user_content_section call (yaad-mcp lifts the HTTP ETag header onto the response as `etag`)",
 };
 }
 return client.editUserContentSection(id, sec, args.body, etag);
}
