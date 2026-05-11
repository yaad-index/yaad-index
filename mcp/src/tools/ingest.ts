import type { YaadIndexClient } from "../client/yaad_index.js";

export const ingestTool = {
 name: "ingest",
 description:
 "Ingest a URL into yaad-index. Returns the entity id, state " +
 "(complete / needs_fill / disambiguation / queued), plus options[] " +
 "when the URL resolves to multiple candidates.",
 inputSchema: {
 type: "object",
 properties: {
 url: {
 type: "string",
 description:
 "URL to ingest, OR the `<plugin>: <id>` shorthand from a prior " +
 "disambiguation response (e.g. `wikipedia: Tehran`).",
 },
 },
 required: ["url"],
 },
} as const;

export async function runIngest(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const url = String(args.url ?? "");
 if (!url) {
 return { ok: false, error: "invalid_argument", message: "`url` is required" };
 }
 const res = await client.ingest(url);
 return res;
}
