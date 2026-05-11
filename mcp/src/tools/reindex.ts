// Reindex tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `POST /v1/reindex` endpoint.
// Triggers a vault-walk that rebuilds the derived index (entities +
// edges from frontmatter). yaad-mcp adds NO logic beyond HTTP
// transport, MCP framing, and arg validation — the daemon's
// `reindex.Summary` is the agent-facing shape verbatim.
//
// The call BLOCKS until the daemon's walk completes. There's no
// client-side polling or progress reporting; the agent waits for the
// summary to come back.

import type { YaadIndexClient } from "../client/yaad_index.js";

export const reindexTool = {
 name: "reindex",
 description:
 "Trigger the daemon to walk the markdown vault and rebuild the " +
 "derived index (entities + edges from frontmatter). Wraps " +
 "`POST /v1/reindex`. Returns the daemon's `reindex.Summary` " +
 "verbatim: `{mode, scanned, skipped, parsed, entities_created, " +
 "entities_updated, entities_deleted, edge_rows_written, errors?, " +
 "started_at, finished_at, duration_ms}`. `mode` defaults to " +
 "`incremental` (skips files unchanged since the last walk); pass " +
 "`full` for a hard rebuild that re-parses every file. The call " +
 "blocks until the walk completes — yaad-mcp adds no polling or " +
 "progress reporting. Returns a 404-shaped error when `vault.path` " +
 "isn't configured operator-side (no vault → no reindex).",
 inputSchema: {
 type: "object",
 properties: {
 mode: {
 type: "string",
 enum: ["incremental", "full"],
 description:
 "Reindex shape. Omit for `incremental` (default; re-parses " +
 "only changed files). Pass `full` for a hard rebuild.",
 },
 },
 additionalProperties: false,
 },
} as const;

export async function runReindex(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const mode = args.mode;
 if (mode !== undefined && mode !== "incremental" && mode !== "full") {
 return {
 ok: false,
 error: "invalid_argument",
 message: `mode must be "incremental" or "full" (got ${JSON.stringify(mode)})`,
 };
 }
 return await client.reindex(mode as "incremental" | "full" | undefined);
}
