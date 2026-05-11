import type { YaadIndexClient } from "../client/yaad_index.js";

export const archiveEntityTool = {
 name: "archive_entity",
 description:
 "Archive a yaad-index entity by id. The entity " +
 "is moved out of active queries — its vault file relocates from " +
 "`<kind>/<slug>.md` to `_archive/<kind>/<slug>.md` (an `archive: " +
 "<id>` git commit), and the DB `archived_at` timestamp is set. " +
 "Edges into and out of the archived entity are RETAINED in the " +
 "DB so audit / restore flows can still traverse them; consumers " +
 "see `archived: true` on endpoint objects via edge expansion. " +
 "Idempotent: re-archiving an already-archived entity preserves " +
 "the original timestamp and is a no-op vault-side. Returns " +
 "`{ok, id, archived: true}`. Inverse of `restore_entity`. **This " +
 "is the prerequisite for `delete_entity` and `delete_user_content`** " +
 "— the daemon's DELETE state-machine refuses active entities on " +
 "both routes with 409.",
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

export async function runArchiveEntity(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 return client.archiveEntity(id);
}
