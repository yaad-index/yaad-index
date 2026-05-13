import type { YaadIndexClient } from "../client/yaad_index.js";

export const searchUpstreamTool = {
 name: "search_upstream",
 description:
 "Plugin-federated search across UPSTREAM sources — fans the query " +
 "out to every yaad-index plugin that opted in via " +
 "`Capabilities.SupportsSearch=true`. Returns `{results, " +
 "per_plugin_status, query, limit, per_plugin_timeout_seconds}` where " +
 "`results` is the merged candidate list `[{plugin, id, label, " +
 "summary}]` in plugin-declaration order and `per_plugin_status` " +
 "surfaces per-plugin outcome (ok / candidates count / duration / " +
 "error_message).\n\n" +
 "Partial-results semantic: a single plugin timeout / failure does " +
 "NOT fail the call — the per_plugin_status block tells you which " +
 "plugins returned vs errored.\n\n" +
 "Use this for disambiguation flows when the agent has a topic " +
 "string and needs candidates to pick from before calling `ingest`. " +
 "Use `search_local` instead when looking through already-ingested " +
 "entities (DB-only, sub-ms, no network).",
 inputSchema: {
 type: "object",
 properties: {
 query: {
 type: "string",
 description: "Search query string. Required, non-empty.",
 },
 plugins: {
 type: "array",
 items: { type: "string" },
 description:
 "Optional explicit plugin-name allowlist. Omitted / empty → " +
 "federate to every opted-in plugin. Names not in the registered " +
 "plugin set yield 400; names whose plugin SupportsSearch=false " +
 "yield 422.",
 },
 limit: {
 type: "number",
 description:
 "Per-plugin candidate cap. Defaults to 10; daemon caps at 50.",
 },
 per_plugin_timeout_seconds: {
 type: "number",
 description:
 "Per-plugin wall-clock budget. Defaults to 5; daemon caps at " +
 "30. A plugin that exceeds this gets cancelled + surfaces in " +
 "per_plugin_status as ok=false with the context-deadline error.",
 },
 },
 required: ["query"],
 },
} as const;

export async function runSearchUpstream(
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
 const plugins = Array.isArray(args.plugins)
 ? (args.plugins.filter((p): p is string => typeof p === "string"))
 : undefined;
 const limit = typeof args.limit === "number" ? args.limit : undefined;
 const perPluginTimeoutSeconds =
 typeof args.per_plugin_timeout_seconds === "number"
 ? args.per_plugin_timeout_seconds
 : undefined;
 return await client.searchUpstream({
 query,
 plugins,
 limit,
 per_plugin_timeout_seconds: perPluginTimeoutSeconds,
 });
}
