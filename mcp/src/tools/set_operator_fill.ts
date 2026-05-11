import type { YaadIndexClient } from "../client/yaad_index.js";

export const setOperatorFillTool = {
 name: "set_operator_fill",
 description:
 "Operator-fill endpoint. POSTs to " +
 "`/v1/entities/{id}/operator-fill` with per-field operations. " +
 "**Operator-only** — the agent caller is acting on behalf of " +
 "the operator (the JWT MUST have Subject == Operator). Agent " +
 "tokens (Subject != Operator) reject with 403 `agent_not_allowed`. " +
 "Per-field value shapes: scalar (number / boolean / string / " +
 "etc.) sets the field and stamps `gap_state.source=operator + " +
 "filled_at`; explicit `null` clears the field and drops the " +
 "gap_state entry (the field re-appears as an open gap that the " +
 "operator can re-fill or defer); `{defer: true}` marks the " +
 "field deferred (must be currently unfilled, see below); " +
 "`{defer: false}` un-defers. Validation runs against the " +
 "resolved canonical-kind config: type mismatch (e.g. `\"9\"` " +
 "for an int field), out-of-range, max_length exceeded, enum " +
 "value not in `values:` all reject with 400. " +
 "**409 `deferred_requires_unfilled`** is the load-bearing " +
 "non-200 the agent caller must handle: deferring an " +
 "already-filled field is rejected, and the recovery path is " +
 "**two steps**: first clear the value (set the field to `null`), " +
 "then defer it. The convenience tool `defer_gap` does step 2 " +
 "in isolation but only works on already-unfilled fields; for " +
 "already-filled fields use this tool with `null` then call " +
 "`defer_gap`.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description:
 "Full entity id in the `<kind>:<local-id>` shape — e.g. " +
 "`boardgame:brass-birmingham-2018`.",
 },
 fields: {
 type: "object",
 description:
 "Per-field operations. Each value can be a scalar (set), " +
 "`null` (clear), `{\"defer\": true}` (mark deferred — " +
 "MUST be unfilled), or `{\"defer\": false}` (un-defer). " +
 "Example: `{\"rating\": 9, \"played\": {\"defer\": " +
 "true}, \"summary\": null}`.",
 additionalProperties: true,
 },
 },
 required: ["id", "fields"],
 },
} as const;

export async function runSetOperatorFill(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 const fields = args.fields;
 if (fields === undefined || fields === null || typeof fields !== "object" || Array.isArray(fields)) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`fields` must be a non-null object of per-field operations",
 };
 }
 return client.setOperatorFill(id, fields as Record<string, unknown>);
}
