import type { YaadIndexClient } from "../client/yaad_index.js";

export const searchLocalTool = {
 name: "search_local",
 description:
 "Full-text search across the LOCAL yaad-index — entities already " +
 "ingested (not the upstream sources). Returns `{results, total, " +
 "limit, offset}` where each result is `{id, kind, snippet, score}` " +
 "— call `get_entity(id)` for any id to load full state.\n\n" +
 "Use this when the agent wants to find entities it (or earlier " +
 "agents) ingested by keyword. Use `ingest(url)` instead when the " +
 "goal is to FETCH new content from upstream.",
 inputSchema: {
 type: "object",
 properties: {
 query: {
 type: "string",
 description: "Text query — full-text searched across entity bodies. Required.",
 },
 kind: {
 type: "string",
 description:
 "Optional kind filter (e.g. `wikipedia-article`, `person`). " +
 "When set, only results matching the kind are returned.",
 },
 limit: {
 type: "number",
 description: "Max results to return. Defaults to 20 if omitted.",
 },
 },
 required: ["query"],
 },
} as const;

export async function runSearchLocal(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const query = typeof args.query === "string" ? args.query : "";
 if (!query) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`query` is required and must be a non-empty string",
 };
 }
 const kind =
 typeof args.kind === "string" && args.kind.length > 0 ? args.kind : undefined;
 const limit = typeof args.limit === "number" ? args.limit : undefined;
 return await client.searchLocal(query, kind, limit);
}
