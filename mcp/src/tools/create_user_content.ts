import type { YaadIndexClient } from "../client/yaad_index.js";

export const createUserContentTool = {
 name: "create_user_content",
 description:
 "Create a new user-content (UGC) entity. Server slugifies " +
 "`title` to derive `id = user-content:<slug>`, stamps `author` " +
 "from the JWT subject and `operator` from the pair-claim. Returns " +
 "the full entity envelope with the first page of parsed sections " +
 "embedded (same shape as `get_user_content`) plus an `etag` lifted " +
 "from the HTTP ETag header — the agent can chain edits without an " +
 "extra GET. 409 conflict on slug collision: the agent picks a new " +
 "title (auto-suffix is a deferred follow-up). " +
 "Optional `data` carries frontmatter fields; when the operator's " +
 "`user_content_frontmatter_edges:` config declares mappings, " +
 "fields named in those mappings produce canonical-label edges.",
 inputSchema: {
 type: "object",
 properties: {
 title: {
 type: "string",
 description:
 "Human-readable title. Slugified server-side for the entity id (lowercase, ASCII alphanumeric runs hyphen-separated). Empty / unslugifiable → 400 invalid_argument.",
 },
 tags: {
 type: "array",
 items: { type: "string" },
 description: "Non-empty tag list.",
 },
 body: {
 type: "string",
 description:
 "Markdown body. Can include `##` / `###` headings to seed the section structure. Empty body is allowed — the agent can fill content via `edit_user_content_section` later.",
 },
 data: {
 type: "object",
 description:
 "Optional frontmatter map. Fields declared in the operator's `user_content_frontmatter_edges:` config trigger canonical-label edge derivation. Each declared field's value is a `{name, kind}` object (or list of such objects), or a pre-formed `<kind>:<slug>` canonical-label string (or list of strings). UGC is operator-authored so the pre-formed shape is accepted (parity with operator-fill, distinct from agent-fill which rejects it). Other fields land verbatim under the entity's `data` map without edge derivation.",
 additionalProperties: true,
 },
 },
 required: ["title", "tags", "body"],
 },
} as const;

export async function runCreateUserContent(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const title = typeof args.title === "string" ? args.title : "";
 if (!title.trim()) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`title` is required and must be non-empty after whitespace trim",
 };
 }
 const tags = Array.isArray(args.tags) ? (args.tags as unknown[]).map(String) : [];
 if (tags.length === 0) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`tags` is required and must be a non-empty list",
 };
 }
 const body = typeof args.body === "string" ? args.body : "";
 const data =
 args.data !== undefined && args.data !== null && typeof args.data === "object" && !Array.isArray(args.data)
 ? (args.data as Record<string, unknown>)
 : undefined;
 return client.createUserContent({ title, tags, body, data });
}
