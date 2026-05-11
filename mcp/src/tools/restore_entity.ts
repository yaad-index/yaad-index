import type { YaadIndexClient } from "../client/yaad_index.js";

export const restoreEntityTool = {
 name: "restore_entity",
 description:
 "Restore a previously-archived yaad-index entity. Inverse of " +
 "`archive_entity`: the vault file moves " +
 "back from `_archive/<kind>/<slug>.md` to the active layout " +
 "(`restore: <id>` git commit), and the DB `archived_at` is " +
 "cleared. The entity reappears in default-filtered list / " +
 "search results. Returns `{ok, id, archived: false}`.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description:
 "Full entity id in the `<kind>:<local-id>` shape — e.g. " +
 "`boardgame:brass-birmingham-2018`.",
 },
 },
 required: ["id"],
 },
} as const;

export async function runRestoreEntity(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 return client.restoreEntity(id);
}
