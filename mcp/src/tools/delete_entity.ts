import type { YaadIndexClient } from "../client/yaad_index.js";

export const deleteEntityTool = {
 name: "delete_entity",
 description:
 "Hard-destroy a yaad-index entity by id. " +
 "**Two-step state-machine: archive first, then delete.** This " +
 "tool refuses active entities — call `archive_entity` first, " +
 "then call `delete_entity` to permanently remove the archived " +
 "entity. The two explicit calls *are* the safety property; " +
 "there is no `?confirm=permanent` shortcut. " +
 "On an active entity the daemon returns 409 with the structured " +
 "envelope `{ok: false, error: 'must archive before delete', " +
 "message: 'POST /v1/entities/<id>/archive first; ...'}` — this " +
 "tool surfaces it verbatim (no exception) so the agent can branch " +
 "on `error`. " +
 "On an archived entity the call removes the `_archive/...` vault " +
 "file (with a `destroy: <id> [<kind>] by <agent>` git commit) and " +
 "cascade-drops the DB row + inbound/outbound edges + provenance. " +
 "**Destructive and irreversible** at this point. Returns " +
 "`{ok, id, deleted: true}` on success. Other non-2xx (401, 404 " +
 "already-gone, 503 vault not configured) still throw " +
 "YaadIndexError.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description:
 "Full entity id in the `<kind>:<local-id>` shape — e.g. " +
 "`boardgame:brass-birmingham-2018` or `wikipedia:susanna-clarke`. " +
 "The local-id portion must conform to `^[a-z0-9_-]+$`; the " +
 "daemon's path-traversal guard rejects anything else.",
 },
 },
 required: ["id"],
 },
} as const;

export async function runDeleteEntity(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 return client.deleteEntity(id);
}
