// workflow_list — list registered workflow patterns per
// ADR-0024 §"Agent surface". Thin wrapper around yaad-index's
// GET /v1/workflows endpoint; yaad-mcp adds no logic beyond
// HTTP transport + MCP framing.

import type { YaadIndexClient } from "../client/yaad_index.js";

export const workflowListTool = {
 name: "workflow_list",
 description:
 "List every workflow pattern currently registered with the " +
 "running yaad-index. Returns `{ok, workflows: [{name, version, " +
 "status, trigger_type, dedup_policy}]}` verbatim from GET " +
 "/v1/workflows. Sorted by name. Use this to discover what " +
 "workflows exist before calling `workflow_trigger` or " +
 "`workflow_discover`. Verbatim pass-through; yaad-mcp adds no " +
 "reshape, summary, or client-side caching.",
 inputSchema: {
 type: "object",
 properties: {},
 additionalProperties: false,
 },
} as const;

export async function runWorkflowList(
 client: YaadIndexClient,
 _args: Record<string, unknown>,
): Promise<unknown> {
 return await client.listWorkflows();
}
