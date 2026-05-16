// workflow_discover — list workflows whose condition matches
// a given entity per ADR-0024 §"workflow.discover(entity_id)".

import type { YaadIndexClient } from "../client/yaad_index.js";

export const workflowDiscoverTool = {
 name: "workflow_discover",
 description:
 "Find every workflow whose condition predicate evaluates true " +
 "for the given entity. Returns `{ok, entity_id, workflows: " +
 "[<name>, ...]}` verbatim from GET /v1/workflows/discover. " +
 "Walks every registered workflow + evaluates each condition " +
 "against the resolved entity (cost O(W × T); W is single-" +
 "digit in v1). Best-effort surface: condition eval errors " +
 "are treated as non-matching, not as a fire commitment. " +
 "Unknown entity surfaces as a daemon 404. Use this to " +
 "discover which workflows would fire on an entity before " +
 "explicitly invoking `workflow_trigger`.",
 inputSchema: {
 type: "object",
 properties: {
 entity_id: {
 type: "string",
 description:
 "canonical entity id (`<kind>:<slug>`, e.g. `boardgame:" +
 "brass-birmingham`) to test workflow conditions against.",
 },
 },
 required: ["entity_id"],
 additionalProperties: false,
 },
} as const;

export async function runWorkflowDiscover(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const entityID = String(args.entity_id ?? "");
 if (!entityID) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`entity_id` is required",
 };
 }
 return await client.discoverWorkflows(entityID);
}
