// Per-plugin capability discovery tool (per yaad-index #13).
//
// Thin wrapper around yaad-index's `GET /v1/plugins` endpoint
// (`internal/api/plugins.go`). Returns the live per-plugin slice of
// each registered plugin's --init Capabilities: name, version,
// url_patterns, commands, entity_kinds, edge_kinds,
// source_namespace.
//
// Inverse shape of `kinds()`:
//   - `kinds()` returns the union view ("kind → plugins"), deduped
//     by kind name with `source_plugins` cross-reference.
//   - `plugins()` returns the per-plugin view ("plugin → kinds +
//     url_patterns + commands + namespace"). Use to discover what
//     plugins are loaded, what URL shapes they accept, what
//     commands they advertise (per ADR-0022 `<plugin>: !<command>`
//     dispatch).
//
// Call at session start to get a live view that replaces what
// pre-#13 SKILL.md carried as per-plugin sections. SKILL.md keeps
// abbreviated stubs as offline fallback (URL patterns + commands +
// one-line ingest summary) so the agent can still operate when the
// daemon isn't reachable.

import type { YaadIndexClient } from "../client/yaad_index.js";
import type { PluginsResponse } from "../types.js";

export const pluginsTool = {
 name: "plugins",
 description:
 "Per-plugin capability discovery. Wraps `GET /v1/plugins`. " +
 "Returns `{ok, plugins: [{name, version, url_patterns, " +
 "commands, entity_kinds, edge_kinds, source_namespace}, ...]}` — " +
 "the per-plugin view of each registered plugin's --init " +
 "Capabilities. Inverse of `kinds()` (which aggregates kind → " +
 "plugins). Call at session start to discover what plugins are " +
 "loaded, what URL patterns each accepts, what commands each " +
 "advertises (per ADR-0022 `<plugin>: !<command>` dispatch), " +
 "and what source namespace each emits entities under. " +
 "Plugins are listed in registry order (matching the first-" +
 "match-wins dispatch precedence at /v1/ingest); within each " +
 "plugin, entity_kinds + edge_kinds sort alphabetically by name.",
 inputSchema: {
 type: "object",
 properties: {},
 additionalProperties: false,
 },
} as const;

export async function runPlugins(
 client: YaadIndexClient,
 _args: Record<string, unknown>,
): Promise<unknown> {
 const response: PluginsResponse = await client.getPlugins();
 return response;
}
