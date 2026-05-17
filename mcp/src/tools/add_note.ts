import type { YaadIndexClient } from "../client/yaad_index.js";

export const addNoteTool = {
 name: "add_note",
 description:
 "Append a note to an existing entity. " +
 "Server stamps date (UTC), the JWT subject as author, and the " +
 "operator from the pair-claim. Empty author is server-filled; an " +
 "explicit author MUST equal the JWT subject or the call returns " +
 "the upstream 403 author_mismatch envelope verbatim.",
 inputSchema: {
 type: "object",
 properties: {
 entity_id: {
 type: "string",
 description: "Target entity id (e.g. `boardgame:catan`).",
 },
 text: {
 type: "string",
 description: "Note body. Server-trims surrounding whitespace.",
 },
 author: {
 type: "string",
 description:
 "Optional; if set MUST equal the JWT subject (otherwise upstream returns 403 author_mismatch). Omit to let the server fill from the token (recommended).",
 },
 },
 required: ["entity_id", "text"],
 },
} as const;

export async function runAddNote(
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
 const text = typeof args.text === "string" ? args.text : "";
 if (!text.trim()) {
 return {
 ok: false,
 error: "invalid_argument",
 message: "`text` is required and must be non-empty after whitespace trim",
 };
 }
 const author =
 typeof args.author === "string" && args.author !== "" ? args.author : undefined;
 return client.addNote(entityID, text, author);
}
