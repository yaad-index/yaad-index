// task_resolve — mark a workflow-produced task done per
// ADR-0024 §"task.resolve". Stamps resolved_at + (when
// auto-archive applies) moves the file to _archive/.

import type { YaadIndexClient } from "../client/yaad_index.js";

export const taskResolveTool = {
 name: "task_resolve",
 description:
 "Mark a workflow-produced task done. Stamps `resolved_at: " +
 "<now>` on the task's frontmatter; auto-archives (moves to " +
 "tasks/_archive/<id>.md) when the originating workflow has " +
 "`auto_archive_on_done: true` (the default). Err-tasks always " +
 "auto-archive regardless of the workflow opt-out per ADR-0024 " +
 "§\"Runtime errors\". Returns `{ok, id, errored, auto_archived, " +
 "resolved_at}` verbatim from POST /v1/tasks/{id}/resolve. " +
 "Idempotent: re-resolving an active task preserves the " +
 "original timestamp; re-resolving an already-archived task " +
 "is a no-op success.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description:
 "task id (the markdown file's basename without `.md`).",
 },
 },
 required: ["id"],
 additionalProperties: false,
 },
} as const;

export async function runTaskResolve(
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
 return await client.resolveTask(id);
}
