// task_list — list workflow-produced tasks per ADR-0024
// §"task.list". Optional `errored` filter routes to
// ?errored=true|false on the wire.

import type { YaadIndexClient } from "../client/yaad_index.js";

export const taskListTool = {
 name: "task_list",
 description:
 "List workflow-produced tasks (markdown files under " +
 "vault/tasks/). Returns `{ok, tasks: [{id, workflow, subject?, " +
 "errored?, dedup_key?, created_at}]}` verbatim from GET " +
 "/v1/tasks. Sorted by id. Optional `errored` filter: true → " +
 "only err-tasks (per ADR-0024 §\"Runtime errors\" surface); " +
 "false → only normal tasks; omitted → both. Active tasks " +
 "only — resolved + auto-archived tasks live under tasks/" +
 "_archive/ and aren't included.",
 inputSchema: {
 type: "object",
 properties: {
 errored: {
 type: "boolean",
 description:
 "filter by the task's `errored:` frontmatter field. " +
 "true → only err-tasks; false → only normal tasks; " +
 "omit to list both.",
 },
 },
 additionalProperties: false,
 },
} as const;

export async function runTaskList(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const opts: { errored?: boolean } = {};
 if (typeof args.errored === "boolean") {
 opts.errored = args.errored;
 }
 return await client.listTasks(opts);
}
