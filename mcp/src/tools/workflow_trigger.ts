// workflow_trigger — manual workflow trigger per ADR-0024
// §"workflow.trigger(input) input semantics".

import type { YaadIndexClient } from "../client/yaad_index.js";

export const workflowTriggerTool = {
 name: "workflow_trigger",
 description:
 "Manually fire a registered workflow against the given input. " +
 "Returns the recorded Decision envelope `{ok, workflow, " +
 "entity_id, subject, fired, missing_refs?, err?, at}` " +
 "verbatim from POST /v1/workflows/trigger. `input` shapes: " +
 "empty (target-less manual fire — only valid for trigger." +
 "type=manual workflows), canonical entity id (`<kind>:<slug>`, " +
 "direct attach), or URL (routes through the daemon's ingest-" +
 "or-lookup pipeline; the trigger call itself fails synchronously " +
 "on routing errors). Unknown workflow surfaces as a 404; " +
 "empty input on an event-driven workflow surfaces as a 422.",
 inputSchema: {
 type: "object",
 properties: {
 name: {
 type: "string",
 description:
 "workflow name (matches the frontmatter `name:` on the " +
 "workflow file in vault/workflows/).",
 },
 input: {
 type: "string",
 description:
 "trigger input — empty for target-less manual fires, " +
 "canonical entity id (`<kind>:<slug>`), or URL.",
 },
 },
 required: ["name"],
 additionalProperties: false,
 },
} as const;

export async function runWorkflowTrigger(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const name = String(args.name ?? "");
 if (!name) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`name` is required",
 };
 }
 const input = args.input === undefined ? "" : String(args.input);
 return await client.triggerWorkflow(name, input);
}
