import type { YaadIndexClient } from "../client/yaad_index.js";

export const listUserContentSectionsTool = {
 name: "list_user_content_sections",
 description:
 "Paginated section list for a UGC entity. Returns " +
 "`{ok, entries, next_cursor?, etag?}`. Use this when the embedded " +
 "section list on `get_user_content` doesn't fit in one page, OR " +
 "when you only want the section list without re-fetching entity " +
 "metadata. `etag` matches the entity's current state — pass it " +
 "back as `etag` on the eventual `edit_user_content_section` call.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id (must start with `user-content:`).",
 },
 limit: {
 type: "integer",
 description: "Optional page size (default 20; cap 100, server-clamped).",
 },
 cursor: {
 type: "string",
 description:
 "Optional opaque cursor returned as `next_cursor` from a prior call. Omit to start at the first page.",
 },
 },
 required: ["id"],
 },
} as const;

export async function runListUserContentSections(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 const opts: { limit?: number; cursor?: string } = {};
 if (typeof args.limit === "number" && Number.isInteger(args.limit)) {
 opts.limit = args.limit;
 }
 if (typeof args.cursor === "string" && args.cursor !== "") {
 opts.cursor = args.cursor;
 }
 return client.listUserContentSections(id, opts);
}
