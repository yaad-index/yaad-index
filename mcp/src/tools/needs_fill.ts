// Pull-based batch gap-call queue tool (per yaad-mcp the source issue).
//
// Thin wrapper around yaad-index's `GET /v1/needs-fill` endpoint
// (ADR-0013 §6, landed in yaad-index a prior PR/172). Surfaces
// gap-callable entities with their full needs_fill payload —
// yaad-mcp adds NO client-side auto-pagination loop or filtering;
// the agent passes the returned `next_cursor` back as an arg to
// fetch the subsequent page until the cursor is absent (queue
// exhausted).

import type { YaadIndexClient } from "../client/yaad_index.js";

export const needsFillTool = {
 name: "needs_fill",
 description:
 "Browse the open-gap queue: entities with unfilled gaps that " +
 "haven't been gap-called for the current fetch-cycle. Returns " +
 "`{ok, entities, next_cursor?}` verbatim from " +
 "`GET /v1/needs-fill`. Each entry carries the full gap-call " +
 "payload (id, kind, gaps, clean_content, instruction, " +
 "canonical_vocabulary). Optional `limit` (server clamps; " +
 "default 50, cap 200) and `cursor` (opaque, base64 of last-" +
 "seen entity id from a prior call's `next_cursor`) drive " +
 "pagination. The agent decides whether to keep paginating " +
 "or stop — yaad-mcp does NOT auto-iterate. When the queue " +
 "is exhausted, `next_cursor` is absent from the response.",
 inputSchema: {
 type: "object",
 properties: {
 limit: {
 // `integer` rather than `number` so schema-conforming MCP
 // clients reject `1.5` at the schema layer instead of having
 // the runtime `Number.isInteger` check return
 // `invalid_argument` after the fact (the cold-reviewer's a prior PR catch).
 type: "integer",
 description:
 "Page size. Server clamps; default 50, cap 200, lenient " +
 "on bad values (≤0 or non-integer → server defaults to 50).",
 },
 cursor: {
 type: "string",
 description:
 "Opaque pagination token from a previous response's " +
 "`next_cursor`. Pass back as-is to fetch the next page. " +
 "Omit (or pass empty) on the first call.",
 },
 },
 additionalProperties: false,
 },
} as const;

export async function runNeedsFill(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const out: { limit?: number; cursor?: string } = {};
 if (args.limit !== undefined) {
 if (typeof args.limit !== "number" || !Number.isInteger(args.limit)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`limit` must be an integer",
 };
 }
 out.limit = args.limit;
 }
 if (args.cursor !== undefined) {
 if (typeof args.cursor !== "string") {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`cursor` must be a string",
 };
 }
 if (args.cursor !== "") {
 out.cursor = args.cursor;
 }
 }
 return await client.getNeedsFill(out);
}
