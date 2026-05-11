import type { YaadIndexClient } from "../client/yaad_index.js";

export const getUserContentSectionTool = {
 name: "get_user_content_section",
 description:
 "Fetch one section from a UGC entity. `sec` accepts heading-text-slug " +
 "(e.g. `books-i-loved`) OR positional index (`0`, `1`, …). Server " +
 "canonicalizes either form. When two headings slugify identically, " +
 "the slug returns 404 and the agent must address by positional " +
 "index. Returns `{ok, id, section: {index, depth, heading, " +
 "heading_slug, body, byte_offset}, etag?}`. `etag` matches the " +
 "ENTITY's body (not just this section's content) — pass it back as " +
 "`etag` on `edit_user_content_section` for If-Match concurrency.",
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
 "Section address: heading-text-slug for unique headings, OR positional index (`0`, `1`, …). Falls back to positional on duplicate slugs.",
 },
 },
 required: ["id", "sec"],
 },
} as const;

export async function runGetUserContentSection(
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
 return client.getUserContentSection(id, sec);
}
