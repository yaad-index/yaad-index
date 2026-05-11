import type { YaadIndexClient } from "../client/yaad_index.js";

export const deleteUserContentTool = {
 name: "delete_user_content",
 description:
 "Hard-destroy a UGC entity. **Two-step " +
 "state-machine: archive first, then delete.** Same archive-first " +
 "contract as `delete_entity` — call `archive_entity` first, then " +
 "this tool to permanently remove the archived UGC row. " +
 "On an active entity the daemon returns 409 with the structured " +
 "envelope `{ok: false, error: 'must archive before delete', " +
 "message: 'POST /v1/entities/<id>/archive first; ...'}` — this " +
 "tool surfaces it verbatim so the agent can branch on `error` " +
 "without an exception. " +
 "On an archived UGC entity the call removes the `_archive/...` " +
 "vault file (with auto-commit) and cascade-drops the store row. " +
 "Returns `{ok, id, deleted: true}`. " +
 "Other non-2xx still throw YaadIndexError — notably 403 " +
 "author_mismatch (cross-author delete; operator-on-behalf still " +
 "allowed via the same operator on the claim) and 404 " +
 "(already-gone). The 403 check runs *before* the archive-first " +
 "gate so an intruder can't probe other authors' archive state.",
 inputSchema: {
 type: "object",
 properties: {
 id: {
 type: "string",
 description: "Entity id (must start with `user-content:`).",
 },
 },
 required: ["id"],
 },
} as const;

export async function runDeleteUserContent(
 client: YaadIndexClient,
 args: Record<string, unknown>,
): Promise<unknown> {
 const id = String(args.id ?? "");
 if (!id) {
 return { ok: false, error: "invalid_argument", message: "`id` is required" };
 }
 return client.deleteUserContent(id);
}
