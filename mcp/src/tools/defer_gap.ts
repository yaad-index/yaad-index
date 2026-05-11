import type { YaadIndexClient } from "../client/yaad_index.js";

export const deferGapTool = {
 name: "defer_gap",
 description:
 "Mark a single gap deferred. Convenience " +
 "wrapper around `set_operator_fill` — POSTs `{<field>: " +
 "{\"defer\": true}}` to `/v1/entities/{id}/operator-fill`. " +
 "After defer, the field stops surfacing on `/v1/needs-fill` " +
 "responses for both audiences (agent and operator). The " +
 "operator un-defers via `set_operator_fill(id, {<field>: " +
 "{\"defer\": false}})` to bring the gap back. " +
 "**Constraint**: the field MUST currently be **unfilled**. " +
 "Defer on a filled field returns 409 " +
 "`deferred_requires_unfilled`; the recovery path is two-step — " +
 "first clear the value via `set_operator_fill(id, {<field>: " +
 "null})`, then call this tool. " +
 "**Operator-only** — same auth gate as `set_operator_fill`: " +
 "the JWT MUST have Subject == Operator. Agent tokens reject " +
 "with 403 `agent_not_allowed`.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description:
 "Full entity id in the `<kind>:<local-id>` shape — e.g. " +
 "`boardgame:brass-birmingham-2018`.",
 },
 field: {
 type: "string",
 description:
 "The single gap field name to defer. Must be a currently-" +
 "unfilled gap on the entity; otherwise the call returns " +
 "409 `deferred_requires_unfilled`.",
 },
 },
 required: ["id", "field"],
 },
} as const;

export async function runDeferGap(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 const field = String(args.field ?? "");
 if (!field) {
 return { ok: false, error: "invalid_argument", message: "`field` is required" };
 }
 return client.setOperatorFill(id, { [field]: { defer: true } });
}
