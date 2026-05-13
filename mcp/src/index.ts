#!/usr/bin/env bun
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
 CallToolRequestSchema,
 ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

import { YaadIndexClient } from "./client/yaad_index.js";
import { addCommentTool, runAddComment } from "./tools/add_comment.js";
import { archiveEntityTool, runArchiveEntity } from "./tools/archive_entity.js";
import { deferGapTool, runDeferGap } from "./tools/defer_gap.js";
import { edgesTool, runEdges } from "./tools/edges.js";
import {
 createUserContentTool,
 runCreateUserContent,
} from "./tools/create_user_content.js";
import { cvStatusTool, runCVStatus } from "./tools/cv_status.js";
import { deleteEntityTool, runDeleteEntity } from "./tools/delete_entity.js";
import {
 deleteUserContentTool,
 runDeleteUserContent,
} from "./tools/delete_user_content.js";
import {
 editUserContentSectionTool,
 runEditUserContentSection,
} from "./tools/edit_user_content_section.js";
import { fillTool, runFill } from "./tools/fill.js";
import {
 getEntitiesBatchTool,
 runGetEntitiesBatch,
} from "./tools/get_entities_batch.js";
import { getEntityTool, runGetEntity } from "./tools/get_entity.js";
import {
 getEntityWithContextTool,
 runGetEntityWithContext,
} from "./tools/get_entity_with_context.js";
import {
 getUserContentTool,
 runGetUserContent,
} from "./tools/get_user_content.js";
import {
 getUserContentSectionTool,
 runGetUserContentSection,
} from "./tools/get_user_content_section.js";
import { ingestTool, runIngest } from "./tools/ingest.js";
import { kindsTool, runKinds } from "./tools/kinds.js";
import { listEntitiesTool, runListEntities } from "./tools/list_entities.js";
import {
 listUserContentSectionsTool,
 runListUserContentSections,
} from "./tools/list_user_content_sections.js";
import { needsFillTool, runNeedsFill } from "./tools/needs_fill.js";
import { pluginsTool, runPlugins } from "./tools/plugins.js";
import { reindexTool, runReindex } from "./tools/reindex.js";
import { runSearchLocal, searchLocalTool } from "./tools/search_local.js";
import { runSearchUpstream, searchUpstreamTool } from "./tools/search_upstream.js";
import { restoreEntityTool, runRestoreEntity } from "./tools/restore_entity.js";
import { runSetOperatorFill, setOperatorFillTool } from "./tools/set_operator_fill.js";
import { runStructure, structureTool } from "./tools/structure.js";

const TOOLS = [
 ingestTool,
 getEntityTool,
 getEntityWithContextTool,
 edgesTool,
 getEntitiesBatchTool,
 fillTool,
 addCommentTool,
 listEntitiesTool,
 needsFillTool,
 reindexTool,
 searchLocalTool,
 searchUpstreamTool,
 structureTool,
 cvStatusTool,
 kindsTool,
 pluginsTool,
 getUserContentTool,
 listUserContentSectionsTool,
 getUserContentSectionTool,
 createUserContentTool,
 editUserContentSectionTool,
 deleteUserContentTool,
 archiveEntityTool,
 restoreEntityTool,
 deleteEntityTool,
 setOperatorFillTool,
 deferGapTool,
] as const;

async function main() {
 const baseUrl = process.env.YAAD_INDEX_URL;
 if (!baseUrl) {
 process.stderr.write(
 "yaad-mcp: YAAD_INDEX_URL is required (e.g. http://localhost:7433).\n",
 );
 process.exit(1);
 }
 const client = new YaadIndexClient({
 baseUrl,
 authToken: process.env.YAAD_INDEX_AUTH_TOKEN,
 });

 const server = new Server(
 { name: "yaad-mcp", version: "0.1.0" },
 { capabilities: { tools: {} } },
 );

 server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: TOOLS }));

 server.setRequestHandler(CallToolRequestSchema, async (request) => {
 const args = (request.params.arguments ?? {}) as Record<string, unknown>;
 try {
 const result = await dispatch(client, request.params.name, args);
 return asContent(result);
 } catch (e) {
 const message = e instanceof Error ? e.message : String(e);
 return asContent({ ok: false, error: "internal_error", message });
 }
 });

 const transport = new StdioServerTransport();
 await server.connect(transport);
}

async function dispatch(
 client: YaadIndexClient,
 name: string,
 args: Record<string, unknown>,
): Promise<unknown> {
 switch (name) {
 case "ingest":
 return runIngest(client, args);
 case "get_entity":
 return runGetEntity(client, args);
 case "get_entity_with_context":
 return runGetEntityWithContext(client, args);
 case "get_entities_batch":
 return runGetEntitiesBatch(client, args);
 case "fill":
 return runFill(client, args);
 case "add_comment":
 return runAddComment(client, args);
 case "list_entities":
 return runListEntities(client, args);
 case "needs_fill":
 return runNeedsFill(client, args);
 case "reindex":
 return runReindex(client, args);
 case "search_local":
 return runSearchLocal(client, args);
 case "search_upstream":
 return runSearchUpstream(client, args);
 case "structure":
 return runStructure(client, args);
 case "cv_status":
 return runCVStatus(client, args);
 case "kinds":
 return runKinds(client, args);
 case "plugins":
 return runPlugins(client, args);
 case "get_user_content":
 return runGetUserContent(client, args);
 case "list_user_content_sections":
 return runListUserContentSections(client, args);
 case "get_user_content_section":
 return runGetUserContentSection(client, args);
 case "create_user_content":
 return runCreateUserContent(client, args);
 case "edit_user_content_section":
 return runEditUserContentSection(client, args);
 case "delete_user_content":
 return runDeleteUserContent(client, args);
 case "archive_entity":
 return runArchiveEntity(client, args);
 case "restore_entity":
 return runRestoreEntity(client, args);
 case "delete_entity":
 return runDeleteEntity(client, args);
 case "edges":
 return runEdges(client, args);
 case "set_operator_fill":
 return runSetOperatorFill(client, args);
 case "defer_gap":
 return runDeferGap(client, args);
 default:
 return { ok: false, error: "unknown_tool", message: `no such tool: ${name}` };
 }
}

function asContent(result: unknown) {
 return {
 content: [{ type: "text" as const, text: JSON.stringify(result) }],
 isError: isError(result),
 };
}

function isError(result: unknown): boolean {
 if (typeof result !== "object" || result === null) return false;
 const r = result as { ok?: unknown };
 return r.ok === false;
}

main().catch((e: unknown) => {
 process.stderr.write(`yaad-mcp fatal: ${e instanceof Error ? e.stack ?? e.message : String(e)}\n`);
 process.exit(1);
});
