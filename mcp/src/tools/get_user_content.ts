import type { YaadIndexClient } from "../client/yaad_index.js";

export const getUserContentTool = {
 name: "get_user_content",
 description:
 "Fetch a user-content (UGC) entity by id. Returns " +
 "`{ok, id, kind, data, tags, provenance, sections: {entries, " +
 "next_cursor?}, etag?}`. The body comes back as a flat list of " +
 "parsed sections per the containment model: " +
 "every ATX heading is one addressable section; deeper headings are " +
 "textually contained in the parent's body. `etag` is lifted from the " +
 "HTTP ETag header — pass it back as the `etag` argument of " +
 "`edit_user_content_section` for If-Match concurrency. Optional " +
 "`limit` + `cursor` paginate the embedded sections.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id, must start with `user-content:` (e.g. `user-content:my-note`).",
 },
 limit: {
 type: "integer",
 description:
 "Optional page size for the embedded sections (default 20; cap 100, server-clamped).",
 },
 cursor: {
 type: "string",
 description:
 "Optional opaque cursor returned as `sections.next_cursor` from a prior call. Omit to start at the first page.",
 },
 },
 required: ["id"],
 },
} as const;

export async function runGetUserContent(
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
 return client.getUserContent(id, opts);
}
