// task_load — load one workflow-produced task per ADR-0024
// §"task.load".

import type { YaadIndexClient } from "../client/yaad_index.js";

export const taskLoadTool = {
 name: "task_load",
 description:
 "Load one workflow-produced task by id. Returns `{ok, task: " +
 "{id, workflow, subject?, errored?, dedup_key?, created_at, " +
 "body}}` verbatim from GET /v1/tasks/{id}. `body` is the " +
 "markdown content after the frontmatter, verbatim — includes " +
 "section headers + content lines. 404 when the id doesn't " +
 "resolve. Path-traversal-resistant: ids with `/` or `\\` " +
 "reject at the daemon.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description:
 "task id (the markdown file's basename without `.md`, " +
 "e.g. `classify-boardgame-brass` or `classify-err`).",
 },
 },
 required: ["id"],
 additionalProperties: false,
 },
} as const;

export async function runTaskLoad(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`id` is required",
 };
 }
 return await client.loadTask(id);
}
