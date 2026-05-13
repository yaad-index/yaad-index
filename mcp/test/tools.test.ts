import { describe, expect, test } from "bun:test";

import { YaadIndexClient, YaadIndexError } from "../src/client/yaad_index.js";
import { runAddComment } from "../src/tools/add_comment.js";
import { runArchiveEntity } from "../src/tools/archive_entity.js";
import { runDeferGap } from "../src/tools/defer_gap.js";
import { runEdges } from "../src/tools/edges.js";
import { runRestoreEntity } from "../src/tools/restore_entity.js";
import { runSetOperatorFill } from "../src/tools/set_operator_fill.js";
import { runCreateUserContent } from "../src/tools/create_user_content.js";
import { runDeleteEntity } from "../src/tools/delete_entity.js";
import { runDeleteUserContent } from "../src/tools/delete_user_content.js";
import { runEditUserContentSection } from "../src/tools/edit_user_content_section.js";
import { runFill } from "../src/tools/fill.js";
import { runGetEntity } from "../src/tools/get_entity.js";
import { runGetUserContent } from "../src/tools/get_user_content.js";
import { runGetUserContentSection } from "../src/tools/get_user_content_section.js";
import { runGetEntitiesBatch } from "../src/tools/get_entities_batch.js";
import { runIngest } from "../src/tools/ingest.js";
import { runKinds } from "../src/tools/kinds.js";
import { runListEntities } from "../src/tools/list_entities.js";
import { runListUserContentSections } from "../src/tools/list_user_content_sections.js";
import { runReindex } from "../src/tools/reindex.js";
import { runSearchLocal } from "../src/tools/search_local.js";
import { runSearchUpstream } from "../src/tools/search_upstream.js";

function clientWith(
 responder: (url: string, init: RequestInit) => Response | Promise<Response>,
): YaadIndexClient {
 return new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: async (input: string | URL, init: RequestInit = {}) => {
 const url = typeof input === "string" ? input : input.toString();
 return await responder(url, init);
 },
 });
}

describe("ingest tool", () => {
 test("happy path passes through the upstream response", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ state: "complete", entity: { id: "wikipedia:foo", kind: "wikipedia-article" } }),
 { headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runIngest(client, { url: "https://en.wikipedia.org/wiki/Foo" })) as {
 state: string;
 };
 expect(got.state).toBe("complete");
 });

 test("missing url returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runIngest(client, {})) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });
});

describe("get_entity tool", () => {
 test("happy path passes through the upstream entity and includes ?with_edges=*", async () => {
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({
 id: "wikipedia:foo",
 kind: "wikipedia-article",
 edges: [{ type: "is_about", to: "person:foo" }],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runGetEntity(client, { id: "wikipedia:foo" })) as {
 id: string;
 edges?: Array<{ type: string; to: string }>;
 };
 expect(got.id).toBe("wikipedia:foo");
 expect(seen).toContain("?with_edges=*");
 expect(got.edges?.[0]?.type).toBe("is_about");
 });

 test("multi-edge-type entity returns ALL outgoing edges via the * wildcard (yaad-mcp)", async () => {
 // The legacy MCP layer pinned the with_edges param to is_about,
 // so plugin-emitted is_a / designed_by / artist_by / published_by
 // edges + agent-filled cross-canonical edges were dropped from
 // the get_entity response. The wildcard switch surfaces the
 // full edge graph in one call.
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({
 id: "bgg:age-of-steam",
 kind: "bgg",
 edges: [
 { type: "is_about", to: "boardgame:age-of-steam" },
 { type: "is_a", to: "source-type:bgg-record" },
 { type: "designed_by", to: "person:john-bohrer" },
 { type: "designed_by", to: "person:martin-wallace" },
 { type: "artist_by", to: "person:sean-brown" },
 { type: "published_by", to: "company:warfrog-games" },
 ],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runGetEntity(client, { id: "bgg:age-of-steam" })) as {
 edges?: Array<{ type: string; to: string }>;
 };
 expect(seen).toContain("?with_edges=*");
 const types = (got.edges ?? []).map((e) => e.type).sort();
 expect(types).toEqual([
 "artist_by", "designed_by", "designed_by", "is_a", "is_about", "published_by",
 ]);
 });

 test("missing id returns invalid_argument", async () => {
 const client = clientWith(
 () => new Response("{}", { headers: { "Content-Type": "application/json" } }),
 );
 const got = (await runGetEntity(client, {})) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 });
});

describe("fill tool", () => {
 test("happy path passes through fields to upstream", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({ ok: true, entity: { id: "person:foo", kind: "person" }, gaps: [] }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runFill(client, {
 id: "person:foo",
 fields: { birth_date: "1957" },
 })) as { ok: boolean; gaps: string[] };
 expect(got.ok).toBe(true);
 expect(JSON.parse(seenBody).fields.birth_date).toBe("1957");
 });

 test("missing fields returns invalid_argument", async () => {
 const client = clientWith(
 () => new Response("{}", { headers: { "Content-Type": "application/json" } }),
 );
 const got = (await runFill(client, { id: "person:foo" })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 });

 test("fields-as-array rejected (must be object)", async () => {
 const client = clientWith(
 () => new Response("{}", { headers: { "Content-Type": "application/json" } }),
 );
 const got = (await runFill(client, { id: "person:foo", fields: ["x", "y"] })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 });
});

describe("add_comment tool", () => {
 test("happy path posts to /v1/entities/{id}/comments and returns the appended comment", async () => {
 let seenURL = "";
 let seenBody = "";
 const client = clientWith((u, init) => {
 seenURL = u;
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 comment: {
 date: "2026-05-05T13:45:00Z",
 text: "first comment",
 author: "the cold-reviewer",
 operator: "alice",
 },
 entity: { id: "boardgame:catan", kind: "boardgame" },
 }),
 {
 status: 201,
 headers: { "Content-Type": "application/json" },
 },
 );
 });
 const got = (await runAddComment(client, {
 entity_id: "boardgame:catan",
 text: "first comment",
 author: "the cold-reviewer",
 })) as {
 ok: boolean;
 comment: { author?: string; operator?: string; text: string };
 };
 expect(got.ok).toBe(true);
 expect(got.comment.author).toBe("the cold-reviewer");
 expect(got.comment.operator).toBe("alice");
 expect(got.comment.text).toBe("first comment");
 expect(seenURL).toBe(
 "http://yaad-index.test/v1/entities/boardgame%3Acatan/comments",
 );
 const parsed = JSON.parse(seenBody);
 expect(parsed.text).toBe("first comment");
 expect(parsed.author).toBe("the cold-reviewer");
 });

 test("omits author from request body when not supplied (server fills from JWT)", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 comment: {
 date: "2026-05-05T13:46:00Z",
 text: "no author specified",
 author: "the cold-reviewer", // server filled from JWT
 operator: "alice",
 },
 entity: { id: "boardgame:catan", kind: "boardgame" },
 }),
 {
 status: 201,
 headers: { "Content-Type": "application/json" },
 },
 );
 });
 await runAddComment(client, {
 entity_id: "boardgame:catan",
 text: "no author specified",
 });
 const parsed = JSON.parse(seenBody);
 expect(parsed.author).toBeUndefined(); // client did not send it
 expect(parsed.text).toBe("no author specified");
 });

 test("403 author_mismatch surfaces the upstream envelope verbatim (no throw)", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "author_mismatch",
 message: "author claim does not match authenticated agent",
 }),
 {
 status: 403,
 headers: { "Content-Type": "application/json" },
 },
 ),
 );
 const got = (await runAddComment(client, {
 entity_id: "boardgame:catan",
 text: "alice2 pretending to be the cold-reviewer",
 author: "alice2",
 })) as { ok: boolean; error: string; message: string };
 // The contract: agent sees structured {ok:false, error, message}
 // — NOT a thrown exception, NOT wrapped in internal_error.
 expect(got.ok).toBe(false);
 expect(got.error).toBe("author_mismatch");
 expect(got.message).toBe("author claim does not match authenticated agent");
 });

 test("401 missing_authorization surfaces upstream envelope verbatim", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "missing_authorization",
 message:
 "missing or malformed Authorization: Bearer <token> header",
 }),
 {
 status: 401,
 headers: { "Content-Type": "application/json" },
 },
 ),
 );
 const got = (await runAddComment(client, {
 entity_id: "boardgame:catan",
 text: "no token",
 })) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("missing_authorization");
 });

 test("missing entity_id returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runAddComment(client, { text: "hi" })) as {
 ok: boolean;
 error: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("whitespace-only text returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runAddComment(client, {
 entity_id: "boardgame:catan",
 text: " \n ",
 })) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });
});

describe("list_entities tool", () => {
 test("kind required, hits /v1/search?kind=<kind>&limit=100", async () => {
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({ ok: true, results: [], total: 0, limit: 100, offset: 0 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runListEntities(client, { kind: "wikipedia-article" });
 expect(seen).toBe(
 "http://yaad-index.test/v1/search?kind=wikipedia-article&limit=100",
 );
 });

 test("missing kind → invalid_argument (no fetch)", async () => {
 const client = clientWith(() => {
 throw new Error("fetch should not be called when kind is missing");
 });
 const got = (await runListEntities(client, {})) as {
 ok: boolean;
 error: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 });

 test("empty-string kind → invalid_argument (no fetch)", async () => {
 const client = clientWith(() => {
 throw new Error("fetch should not be called when kind is empty");
 });
 const got = (await runListEntities(client, { kind: "" })) as {
 ok: boolean;
 error: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 });
});

describe("search_local tool", () => {
 test("query forwarded as q= URL param; default limit=20", async () => {
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({
 ok: true,
 results: [{ id: "wikipedia:susanna-clarke", kind: "wikipedia-article", snippet: "British author", score: 1.5 }],
 total: 1,
 limit: 20,
 offset: 0,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runSearchLocal(client, { query: "clarke" })) as {
 results: { id: string }[];
 total: number;
 limit: number;
 };
 expect(seen).toBe("http://yaad-index.test/v1/search?q=clarke&limit=20");
 expect(got.results[0]?.id).toBe("wikipedia:susanna-clarke");
 expect(got.total).toBe(1);
 expect(got.limit).toBe(20);
 });

 test("kind filter included when set", async () => {
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({ ok: true, results: [], total: 0, limit: 20, offset: 0 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runSearchLocal(client, { query: "clarke", kind: "person" });
 expect(seen).toBe("http://yaad-index.test/v1/search?q=clarke&kind=person&limit=20");
 });

 test("limit override forwarded", async () => {
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({ ok: true, results: [], total: 0, limit: 5, offset: 0 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runSearchLocal(client, { query: "clarke", limit: 5 });
 expect(seen).toBe("http://yaad-index.test/v1/search?q=clarke&limit=5");
 });

 test("missing query returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runSearchLocal(client, {})) as { ok: boolean; error: string };
 expect(called).toBe(false);
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 });

 test("empty-string query returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runSearchLocal(client, { query: "" })) as { ok: boolean; error: string };
 expect(called).toBe(false);
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 });

 test("special characters in query are URL-encoded", async () => {
 let seen = "";
 const client = clientWith((u) => {
 seen = u;
 return new Response(
 JSON.stringify({ ok: true, results: [], total: 0, limit: 20, offset: 0 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runSearchLocal(client, { query: "go (programming language)" });
 expect(seen).toContain("q=go+%28programming+language%29");
 });
});

describe("search_upstream tool", () => {
 test("query forwarded in JSON body to POST /v1/search/upstream", async () => {
 let seenUrl = "";
 let seenMethod = "";
 let seenBody = "";
 const client = clientWith((u, init) => {
 seenUrl = u;
 seenMethod = String(init?.method ?? "");
 seenBody = String(init?.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 results: [{ plugin: "wikipedia", id: "Brass", label: "Brass (board game)" }],
 per_plugin_status: [
 { plugin: "wikipedia", ok: true, candidates: 1, duration_ms: 42 },
 ],
 query: "Brass",
 limit: 10,
 per_plugin_timeout_seconds: 5,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runSearchUpstream(client, { query: "Brass" })) as {
 ok: boolean;
 results: { plugin: string; id: string }[];
 };
 expect(seenUrl).toBe("http://yaad-index.test/v1/search/upstream");
 expect(seenMethod).toBe("POST");
 expect(JSON.parse(seenBody)).toEqual({ query: "Brass" });
 expect(got.ok).toBe(true);
 expect(got.results[0]?.plugin).toBe("wikipedia");
 expect(got.results[0]?.id).toBe("Brass");
 });

 test("optional fields (plugins, limit, per_plugin_timeout_seconds) forwarded", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init?.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 results: [],
 per_plugin_status: [],
 query: "x",
 limit: 25,
 per_plugin_timeout_seconds: 12,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runSearchUpstream(client, {
 query: "x",
 plugins: ["wikipedia", "bgg"],
 limit: 25,
 per_plugin_timeout_seconds: 12,
 });
 expect(JSON.parse(seenBody)).toEqual({
 query: "x",
 plugins: ["wikipedia", "bgg"],
 limit: 25,
 per_plugin_timeout_seconds: 12,
 });
 });

 test("missing query returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runSearchUpstream(client, {})) as { ok: boolean; error: string };
 expect(called).toBe(false);
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 });

 test("non-string plugins entries are filtered out", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init?.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 results: [],
 per_plugin_status: [],
 query: "x",
 limit: 10,
 per_plugin_timeout_seconds: 5,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 // Pass a mixed array; the tool runner must drop non-strings
 // before forwarding to the daemon.
 await runSearchUpstream(client, {
 query: "x",
 plugins: ["wikipedia", 42, null, "bgg"],
 });
 expect(JSON.parse(seenBody)).toEqual({
 query: "x",
 plugins: ["wikipedia", "bgg"],
 });
 });

 test("partial-results semantic: per_plugin_status surfaces errors at 200", async () => {
 const client = clientWith(() =>
 new Response(
 JSON.stringify({
 ok: true,
 results: [{ plugin: "wikipedia", id: "x", label: "X" }],
 per_plugin_status: [
 { plugin: "wikipedia", ok: true, candidates: 1, duration_ms: 50 },
 {
 plugin: "broken",
 ok: false,
 candidates: 0,
 duration_ms: 5,
 error_message: "upstream is down",
 },
 ],
 query: "x",
 limit: 10,
 per_plugin_timeout_seconds: 5,
 }),
 { status: 200, headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runSearchUpstream(client, { query: "x" })) as {
 ok: boolean;
 results: unknown[];
 per_plugin_status: { plugin: string; ok: boolean; error_message?: string }[];
 };
 expect(got.ok).toBe(true);
 expect(got.results).toHaveLength(1);
 const broken = got.per_plugin_status.find((s) => s.plugin === "broken");
 expect(broken?.ok).toBe(false);
 expect(broken?.error_message).toContain("upstream is down");
 });
});

import { runGetEntityWithContext } from "../src/tools/get_entity_with_context.js";

describe("get_entity_with_context tool", () => {
 test("happy path passes server response through verbatim", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({
 root: { id: "wikipedia:tehran", kind: "wikipedia" },
 neighbors: [
 {
 edge: { type: "is_about", from: "wikipedia:tehran", to: "city:tehran" },
 entity: { id: "city:tehran", kind: "city" },
 depth: 1,
 },
 ],
 truncated: false,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runGetEntityWithContext(client, {
 id: "wikipedia:tehran",
 depth: 1,
 })) as {
 root: { id: string };
 neighbors: Array<{ depth: number; edge: { type: string }; entity: { id: string } }>;
 truncated: boolean;
 };
 expect(seenURL).toContain("/v1/entities/wikipedia%3Atehran/context");
 expect(seenURL).toContain("depth=1");
 expect(got.root.id).toBe("wikipedia:tehran");
 expect(got.neighbors).toHaveLength(1);
 expect(got.neighbors[0]?.depth).toBe(1);
 expect(got.neighbors[0]?.edge.type).toBe("is_about");
 expect(got.truncated).toBe(false);
 });

 test("default depth is 1 when omitted", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({ root: { id: "x:y", kind: "x" }, neighbors: [], truncated: false }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetEntityWithContext(client, { id: "x:y" });
 expect(seenURL).toContain("depth=1");
 });

 test("edge_types array becomes a comma-separated query param", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({ root: { id: "x:y", kind: "x" }, neighbors: [], truncated: false }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetEntityWithContext(client, {
 id: "x:y",
 depth: 2,
 edge_types: ["is_about", "references"],
 });
 expect(seenURL).toContain("edge_types=is_about%2Creferences");
 });

 test("max_results plumbs through when set", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({ root: { id: "x:y", kind: "x" }, neighbors: [], truncated: false }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetEntityWithContext(client, { id: "x:y", depth: 1, max_results: 50 });
 expect(seenURL).toContain("max_results=50");
 });

 test("missing id returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runGetEntityWithContext(client, {})) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("non-integer depth is rejected at the tool boundary", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runGetEntityWithContext(client, { id: "x:y", depth: 1.5 })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("depth out of [0, 3] is rejected at the tool boundary", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runGetEntityWithContext(client, { id: "x:y", depth: 4 })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("edge_types must be an array (not a string)", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runGetEntityWithContext(client, {
 id: "x:y",
 edge_types: "is_about,references",
 })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("empty edge_types array means no filter (server default)", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({ root: { id: "x:y", kind: "x" }, neighbors: [], truncated: false }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetEntityWithContext(client, { id: "x:y", edge_types: [] });
 expect(seenURL).not.toContain("edge_types=");
 });

 test("max_results above cap is rejected at the tool boundary", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runGetEntityWithContext(client, {
 id: "x:y",
 depth: 1,
 max_results: 1001,
 })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });
});

import { runStructure } from "../src/tools/structure.js";

describe("structure tool", () => {
 test("happy path passes the /v1/structure response through verbatim", async () => {
 let seenURL = "";
 let seenMethod = "";
 // Raw JSON fixture mirrors alice2's live test target from the dispatch
 // (yaad-index post-§3-series, fresh empty index running the
 // wikipedia plugin at 0.1.0). Build it as a string first so we can
 // raw-JSON-pin `edge_types: []` (the cold-reviewer's a prior PR catch — typed
 // `toEqual([])` accepts both `[]` and `null` so a regression where
 // the wire emits null would slip through). Mirrors yaad-index a prior PR's
 // explicit-fixture-string assertion.
 const fixtureBody = JSON.stringify({
 ok: true,
 version: "f730f89c4bfdbb25",
 kinds: {},
 edge_types: [],
 plugins: [
 {
 name: "wikipedia",
 version: "0.1.0",
 url_patterns: ["^https?://[a-z]+\\.wikipedia\\.org/wiki/.*"],
 supports_search: false,
 emits_kinds: ["wikipedia-article"],
 emits_edges: [],
 },
 ],
 });
 expect(fixtureBody).toContain(`"edge_types":[]`);
 expect(fixtureBody).not.toContain(`"edge_types":null`);

 const client = clientWith((u, init) => {
 seenURL = u;
 seenMethod = String(init.method ?? "GET");
 return new Response(fixtureBody, {
 headers: { "Content-Type": "application/json" },
 });
 });
 const got = (await runStructure(client, {})) as {
 ok: boolean;
 version: string;
 kinds: Record<string, unknown>;
 edge_types: string[];
 plugins: Array<{ name: string; version: string }>;
 };
 expect(seenMethod).toBe("GET");
 expect(seenURL).toContain("/v1/structure");
 expect(got.ok).toBe(true);
 expect(got.version).toBe("f730f89c4bfdbb25");
 expect(got.kinds).toEqual({});
 expect(got.edge_types).toEqual([]);
 expect(got.plugins).toHaveLength(1);
 expect(got.plugins[0]?.name).toBe("wikipedia");
 });

 test("populated kinds + edge_types pass through verbatim", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: true,
 version: "abc1234567890def",
 kinds: {
 person: {
 is_canonical: true,
 gaps: { name: "Full name.", summary: "..." },
 instruction: "Skip if absent.",
 },
 city: {
 is_canonical: true,
 gaps: { name: "City name." },
 },
 },
 edge_types: ["is_about", "lives_in"],
 plugins: [],
 }),
 { headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runStructure(client, {})) as {
 kinds: Record<
 string,
 { is_canonical: boolean; gaps: Record<string, string>; instruction?: string }
 >;
 edge_types: string[];
 };
 expect(got.kinds.person?.is_canonical).toBe(true);
 expect(got.kinds.person?.instruction).toBe("Skip if absent.");
 expect(got.kinds.city?.instruction).toBeUndefined();
 expect(got.edge_types).toEqual(["is_about", "lives_in"]);
 });

 test("HTTP error bubbles as YaadIndexError via existing client path", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "internal_error", message: "boom" }),
 { status: 500, headers: { "Content-Type": "application/json" } },
 ),
 );
 // Tightened from bare `toThrow()` to type-pinned per the cold-reviewer's a prior PR
 // review — guards against a future refactor that wraps the error
 // in a different class and silently changes the contract.
 await expect(runStructure(client, {})).rejects.toThrow(YaadIndexError);
 });
});

import { runNeedsFill } from "../src/tools/needs_fill.js";

describe("needs_fill tool", () => {
 test("empty queue (DB-only / vault-nil deploy) → entities:[] no cursor", async () => {
 let seenURL = "";
 let seenMethod = "";
 // alice2's live test target shape: empty index → vault-nil
 // short-circuit per yaad-index a prior PR → `entities: []` with
 // `next_cursor` omitted from the wire (the omitempty contract
 // the cold-reviewer pinned in a prior PR). Pin via raw JSON: `entities` field
 // is `[]` (not null), `next_cursor` is absent (not the string
 // `"null"`).
 const fixtureBody = JSON.stringify({ ok: true, entities: [] });
 expect(fixtureBody).toContain(`"entities":[]`);
 expect(fixtureBody).not.toContain(`"entities":null`);
 expect(fixtureBody).not.toContain(`"next_cursor"`);

 const client = clientWith((u, init) => {
 seenURL = u;
 seenMethod = String(init.method ?? "GET");
 return new Response(fixtureBody, {
 headers: { "Content-Type": "application/json" },
 });
 });
 const got = (await runNeedsFill(client, {})) as {
 ok: boolean;
 entities: unknown[];
 next_cursor?: string;
 };
 expect(seenMethod).toBe("GET");
 expect(seenURL).toContain("/v1/needs-fill");
 expect(got.ok).toBe(true);
 expect(got.entities).toEqual([]);
 expect(got.next_cursor).toBeUndefined();
 });

 test("limit + cursor pass through to URL verbatim", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(JSON.stringify({ ok: true, entities: [] }), {
 headers: { "Content-Type": "application/json" },
 });
 });
 await runNeedsFill(client, { limit: 25, cursor: "Ym9hcmRnYW1lOmI=" });
 expect(seenURL).toContain("limit=25");
 expect(seenURL).toContain("cursor=Ym9hcmRnYW1lOmI%3D");
 });

 test("cursor roundtrip: page 1 returns next_cursor → pass back → page 2 returns no cursor", async () => {
 // Two-page sequence: page 1 has 2 entities + a cursor; passing
 // that cursor back returns 1 entity + no cursor (queue exhausted).
 // Mirrors yaad-index a prior PR's pagination test pattern.
 let urlsSeen: string[] = [];
 const responses = [
 JSON.stringify({
 ok: true,
 entities: [
 {
 id: "boardgame:a",
 kind: "boardgame",
 gaps: { summary: "" },
 clean_content: "stub-a",
 clean_content_truncated: false,
 },
 {
 id: "boardgame:b",
 kind: "boardgame",
 gaps: { summary: "" },
 clean_content: "stub-b",
 clean_content_truncated: false,
 },
 ],
 next_cursor: "Ym9hcmRnYW1lOmI=",
 }),
 JSON.stringify({
 ok: true,
 entities: [
 {
 id: "boardgame:c",
 kind: "boardgame",
 gaps: { summary: "" },
 clean_content: "stub-c",
 clean_content_truncated: false,
 },
 ],
 }),
 ];
 let call = 0;
 const client = clientWith((u) => {
 urlsSeen.push(u);
 const body = responses[call++] ?? "{}";
 return new Response(body, {
 headers: { "Content-Type": "application/json" },
 });
 });

 // Page 1: limit=2, no cursor. Returns 2 entities + cursor.
 const page1 = (await runNeedsFill(client, { limit: 2 })) as {
 entities: Array<{ id: string }>;
 next_cursor?: string;
 };
 expect(page1.entities).toHaveLength(2);
 expect(page1.next_cursor).toBe("Ym9hcmRnYW1lOmI=");

 // Page 2: pass the cursor back. Returns 1 entity + no cursor.
 const page2 = (await runNeedsFill(client, {
 limit: 2,
 cursor: page1.next_cursor!,
 })) as { entities: Array<{ id: string }>; next_cursor?: string };
 expect(page2.entities).toHaveLength(1);
 expect(page2.entities[0]?.id).toBe("boardgame:c");
 expect(page2.next_cursor).toBeUndefined();

 // Both URLs hit the right path; second carried the cursor.
 expect(urlsSeen[0]).toContain("/v1/needs-fill");
 expect(urlsSeen[0]).toContain("limit=2");
 expect(urlsSeen[0]).not.toContain("cursor=");
 expect(urlsSeen[1]).toContain("cursor=Ym9hcmRnYW1lOmI%3D");
 });

 test("missing args → no query string at all", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(JSON.stringify({ ok: true, entities: [] }), {
 headers: { "Content-Type": "application/json" },
 });
 });
 await runNeedsFill(client, {});
 expect(seenURL).toContain("/v1/needs-fill");
 expect(seenURL).not.toContain("?");
 });

 test("non-integer limit → invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runNeedsFill(client, { limit: 1.5 })) as {
 ok: boolean;
 error: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("non-string cursor → invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runNeedsFill(client, { cursor: 42 })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("HTTP error bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "internal_error", message: "boom" }),
 { status: 500, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(runNeedsFill(client, {})).rejects.toThrow(YaadIndexError);
 });
});

import { runCVStatus } from "../src/tools/cv_status.js";

describe("cv_status tool", () => {
 test("happy path: empty drift verbatim with raw-JSON empty-array pin", async () => {
 let seenURL = "";
 let seenMethod = "";
 // Live test target shape from alice2's dispatch: empty index +
 // empty canonical_kinds → all four drift sections empty,
 // last_reindex_at null, config_hash matches the dispatch's
 // quoted value.
 const fixtureBody = JSON.stringify({
 ok: true,
 config_hash: "c1a6a67fb6cdae9d",
 drift: {
 kinds_emitted_not_enabled: [],
 kinds_enabled_not_emitted: [],
 edge_types_emitted_not_enabled: [],
 edge_types_enabled_not_emitted: [],
 },
 last_reindex_at: null,
 reindex_hint:
 "POST /v1/reindex to materialize stubs after enabling kinds/edges in config",
 });
 // Raw-JSON pin: every drift section serializes as `[]` not
 // `null` (the cold-reviewer's a prior PR/14/172 catch class — typed `toEqual([])`
 // accepts both shapes; explicit string-contains catches drift).
 expect(fixtureBody).toContain(`"kinds_emitted_not_enabled":[]`);
 expect(fixtureBody).toContain(`"kinds_enabled_not_emitted":[]`);
 expect(fixtureBody).toContain(`"edge_types_emitted_not_enabled":[]`);
 expect(fixtureBody).toContain(`"edge_types_enabled_not_emitted":[]`);
 expect(fixtureBody).not.toContain(`"kinds_emitted_not_enabled":null`);
 expect(fixtureBody).not.toContain(`"edge_types_emitted_not_enabled":null`);
 expect(fixtureBody).toContain(`"last_reindex_at":null`);

 const client = clientWith((u, init) => {
 seenURL = u;
 seenMethod = String(init.method ?? "GET");
 return new Response(fixtureBody, {
 headers: { "Content-Type": "application/json" },
 });
 });
 const got = (await runCVStatus(client, {})) as {
 ok: boolean;
 config_hash: string;
 drift: {
 kinds_emitted_not_enabled: unknown[];
 kinds_enabled_not_emitted: unknown[];
 edge_types_emitted_not_enabled: unknown[];
 edge_types_enabled_not_emitted: unknown[];
 };
 last_reindex_at: string | null;
 reindex_hint: string;
 };
 expect(seenMethod).toBe("GET");
 expect(seenURL).toContain("/v1/cv-status");
 expect(got.ok).toBe(true);
 expect(got.config_hash).toBe("c1a6a67fb6cdae9d");
 expect(got.drift.kinds_emitted_not_enabled).toEqual([]);
 expect(got.drift.edge_types_emitted_not_enabled).toEqual([]);
 expect(got.last_reindex_at).toBeNull();
 expect(got.reindex_hint).toContain("POST /v1/reindex");
 });

 test("populated drift: per-(plugin, kind / edge_type) rows pass through verbatim", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: true,
 config_hash: "abc1234567890def",
 drift: {
 kinds_emitted_not_enabled: [
 { plugin: "wikipedia", kind: "boardgame", would_materialize_count: 1 },
 { plugin: "wikipedia", kind: "person", would_materialize_count: 3 },
 ],
 kinds_enabled_not_emitted: [],
 edge_types_emitted_not_enabled: [
 { plugin: "wikipedia", edge_type: "is_about", would_materialize_count: 8 },
 ],
 edge_types_enabled_not_emitted: [],
 },
 last_reindex_at: "2026-05-04T15:45:18Z",
 reindex_hint: "POST /v1/reindex to materialize stubs after enabling kinds/edges in config",
 }),
 { headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runCVStatus(client, {})) as {
 drift: {
 kinds_emitted_not_enabled: Array<{
 plugin: string;
 kind: string;
 would_materialize_count: number;
 }>;
 edge_types_emitted_not_enabled: Array<{
 plugin: string;
 edge_type: string;
 would_materialize_count: number;
 }>;
 };
 last_reindex_at: string | null;
 };
 expect(got.drift.kinds_emitted_not_enabled).toHaveLength(2);
 expect(got.drift.kinds_emitted_not_enabled[0]?.plugin).toBe("wikipedia");
 expect(got.drift.kinds_emitted_not_enabled[0]?.kind).toBe("boardgame");
 expect(got.drift.kinds_emitted_not_enabled[0]?.would_materialize_count).toBe(1);
 expect(got.drift.kinds_emitted_not_enabled[1]?.kind).toBe("person");
 expect(got.drift.kinds_emitted_not_enabled[1]?.would_materialize_count).toBe(3);

 expect(got.drift.edge_types_emitted_not_enabled).toHaveLength(1);
 expect(got.drift.edge_types_emitted_not_enabled[0]?.edge_type).toBe("is_about");
 expect(got.drift.edge_types_emitted_not_enabled[0]?.would_materialize_count).toBe(8);

 expect(got.last_reindex_at).toBe("2026-05-04T15:45:18Z");
 });

 test("HTTP error bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "internal_error", message: "boom" }),
 { status: 500, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(runCVStatus(client, {})).rejects.toThrow(YaadIndexError);
 });
});

describe("get_user_content tool", () => {
 test("happy path returns entity envelope + sections + lifted etag", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 kind: "user-content",
 data: { title: "My Note", author: "the implementer", operator: "alice" },
 tags: ["note"],
 provenance: [{ source: "user", ok: true, fetched_at: "2026-05-05T20:00:00Z" }],
 sections: {
 entries: [
 {
 index: 0,
 depth: 2,
 heading: "First",
 heading_slug: "first",
 body: "first body\n",
 byte_offset: 0,
 },
 ],
 },
 }),
 {
 headers: {
 "Content-Type": "application/json",
 ETag: `"abc123def456"`,
 },
 },
 );
 });
 const got = (await runGetUserContent(client, { id: "user-content:my-note" })) as {
 ok: boolean;
 id: string;
 kind: string;
 sections: { entries: Array<{ heading?: string; body: string }> };
 etag?: string;
 };
 expect(got.ok).toBe(true);
 expect(got.id).toBe("user-content:my-note");
 expect(got.kind).toBe("user-content");
 expect(got.sections.entries[0]?.heading).toBe("First");
 expect(got.sections.entries[0]?.body).toBe("first body\n");
 expect(got.etag).toBe(`"abc123def456"`);
 expect(seenURL).toBe("http://yaad-index.test/v1/user-content/user-content%3Amy-note");
 });

 test("limit + cursor round-trip through the URL", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({ ok: true, id: "x", kind: "user-content", provenance: [], sections: { entries: [] } }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetUserContent(client, { id: "user-content:x", limit: 5, cursor: "MTI=" });
 expect(seenURL).toContain("limit=5");
 expect(seenURL).toContain("cursor=MTI%3D");
 });

 test("missing id returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runGetUserContent(client, {})) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("404 missing entity bubbles as YaadIndexError (read-side throw semantics)", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "not_found", message: "no entity with id user-content:gone" }),
 { status: 404, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(runGetUserContent(client, { id: "user-content:gone" })).rejects.toThrow(
 YaadIndexError,
 );
 });

 test("503 vault_required passes through structurally via the error message", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "vault_required",
 message: "user-content endpoints require vault.path configuration",
 }),
 { status: 503, headers: { "Content-Type": "application/json" } },
 ),
 );
 try {
 await runGetUserContent(client, { id: "user-content:x" });
 throw new Error("expected throw");
 } catch (e) {
 expect(e).toBeInstanceOf(YaadIndexError);
 const yaadErr = e as YaadIndexError;
 expect(yaadErr.status).toBe(503);
 expect(yaadErr.message).toContain("vault_required");
 }
 });

 test("absent ETag header → no etag field on response", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: true, id: "x", kind: "user-content", provenance: [], sections: { entries: [] } }),
 { headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runGetUserContent(client, { id: "user-content:x" })) as {
 etag?: string;
 };
 expect(got.etag).toBeUndefined();
 });
});

describe("list_user_content_sections tool", () => {
 test("happy path returns paginated entries + lifts ETag", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({
 ok: true,
 entries: [
 { index: 0, depth: 1, heading: "A", heading_slug: "a", body: "x", byte_offset: 0 },
 { index: 1, depth: 1, heading: "B", heading_slug: "b", body: "y", byte_offset: 5 },
 ],
 next_cursor: "Mg",
 }),
 {
 headers: {
 "Content-Type": "application/json",
 ETag: `"feedface"`,
 },
 },
 );
 });
 const got = (await runListUserContentSections(client, {
 id: "user-content:multi",
 limit: 2,
 })) as {
 entries: Array<{ heading?: string }>;
 next_cursor?: string;
 etag?: string;
 };
 expect(got.entries).toHaveLength(2);
 expect(got.entries[0]?.heading).toBe("A");
 expect(got.next_cursor).toBe("Mg");
 expect(got.etag).toBe(`"feedface"`);
 expect(seenURL).toContain("/sections?limit=2");
 });

 test("missing id returns invalid_argument", async () => {
 const client = clientWith(
 () => new Response("{}", { headers: { "Content-Type": "application/json" } }),
 );
 const got = (await runListUserContentSections(client, {})) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 });
});

describe("get_user_content_section tool", () => {
 test("happy path returns one section by slug + lifts ETag", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 section: {
 index: 1,
 depth: 2,
 heading: "Books I Loved",
 heading_slug: "books-i-loved",
 body: "fiction list\n",
 byte_offset: 12,
 },
 }),
 {
 headers: {
 "Content-Type": "application/json",
 ETag: `"deadc0de"`,
 },
 },
 );
 });
 const got = (await runGetUserContentSection(client, {
 id: "user-content:my-note",
 sec: "books-i-loved",
 })) as { section: { heading?: string; body: string }; etag?: string };
 expect(got.section.heading).toBe("Books I Loved");
 expect(got.section.body).toBe("fiction list\n");
 expect(got.etag).toBe(`"deadc0de"`);
 expect(seenURL).toContain("/sections/books-i-loved");
 });

 test("positional index addressing", async () => {
 let seenURL = "";
 const client = clientWith((u) => {
 seenURL = u;
 return new Response(
 JSON.stringify({
 ok: true,
 id: "x",
 section: { index: 1, depth: 0, body: "", byte_offset: 0 },
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetUserContentSection(client, { id: "user-content:x", sec: "1" });
 expect(seenURL).toContain("/sections/1");
 });

 test("missing id or sec returns invalid_argument", async () => {
 const client = clientWith(
 () => new Response("{}", { headers: { "Content-Type": "application/json" } }),
 );
 const noID = (await runGetUserContentSection(client, { sec: "x" })) as {
 ok: boolean;
 error: string;
 };
 expect(noID.error).toBe("invalid_argument");
 const noSec = (await runGetUserContentSection(client, { id: "user-content:x" })) as {
 ok: boolean;
 error: string;
 };
 expect(noSec.error).toBe("invalid_argument");
 });

 test("404 unknown section bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "not_found", message: "no section" }),
 { status: 404, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runGetUserContentSection(client, { id: "user-content:x", sec: "nope" }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("create_user_content tool", () => {
 test("happy path returns entity + lifted etag", async () => {
 let seenURL = "";
 let seenBody = "";
 const client = clientWith((u, init) => {
 seenURL = u;
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 kind: "user-content",
 data: { title: "My Note", author: "the implementer", operator: "alice" },
 tags: ["x"],
 provenance: [{ source: "user", ok: true }],
 sections: { entries: [] },
 }),
 {
 status: 201,
 headers: {
 "Content-Type": "application/json",
 ETag: `"newetag1234"`,
 },
 },
 );
 });
 const got = (await runCreateUserContent(client, {
 title: "My Note",
 body: "## Hello\nworld\n",
 tags: ["x"],
 })) as { ok: boolean; id: string; etag?: string };
 expect(got.ok).toBe(true);
 expect(got.id).toBe("user-content:my-note");
 expect(got.etag).toBe(`"newetag1234"`);
 expect(seenURL).toBe("http://yaad-index.test/v1/user-content");
 expect(JSON.parse(seenBody).title).toBe("My Note");
 expect(JSON.parse(seenBody).tags).toEqual(["x"]);
 });

 test("missing title returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runCreateUserContent(client, { tags: ["x"], body: "" })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("data field threads through to client request body", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 kind: "user-content",
 data: { title: "My Note", author: "the implementer", operator: "alice", topics: [{ name: "Rust", kind: "topic" }] },
 tags: ["x"],
 provenance: [{ source: "user", ok: true }],
 sections: { entries: [] },
 }),
 {
 status: 201,
 headers: { "Content-Type": "application/json", ETag: `"e1"` },
 },
 );
 });
 const got = (await runCreateUserContent(client, {
 title: "My Note",
 tags: ["x"],
 body: "",
 data: { topics: [{ name: "Rust", kind: "topic" }], rating: 5 },
 })) as { ok: boolean };
 expect(got.ok).toBe(true);
 const parsed = JSON.parse(seenBody) as Record<string, unknown>;
 expect(parsed.data).toEqual({ topics: [{ name: "Rust", kind: "topic" }], rating: 5 });
 });

 test("data omitted leaves request body without data field", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 kind: "user-content",
 data: {},
 tags: ["x"],
 provenance: [],
 sections: { entries: [] },
 }),
 { status: 201, headers: { "Content-Type": "application/json", ETag: `"e1"` } },
 );
 });
 await runCreateUserContent(client, { title: "My Note", tags: ["x"], body: "" });
 const parsed = JSON.parse(seenBody) as Record<string, unknown>;
 expect("data" in parsed).toBe(false);
 });

 test("non-object data is dropped (defensive coercion)", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 kind: "user-content",
 data: {},
 tags: ["x"],
 provenance: [],
 sections: { entries: [] },
 }),
 { status: 201, headers: { "Content-Type": "application/json", ETag: `"e1"` } },
 );
 });
 // Array / scalar / null all fall outside the `record<string, unknown>` shape
 // the inputSchema accepts, so the tool defensively drops them before the
 // client call. The schema layer already rejects these in MCP-aware clients
 // (additionalProperties=true on an `object` schema still requires the
 // top-level value to be an object); the runtime check is belt-and-braces.
 await runCreateUserContent(client, {
 title: "My Note",
 tags: ["x"],
 body: "",
 data: [1, 2, 3],
 });
 const parsed = JSON.parse(seenBody) as Record<string, unknown>;
 expect("data" in parsed).toBe(false);
 });

 test("missing tags returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runCreateUserContent(client, { title: "x", body: "" })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("409 conflict bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "conflict",
 message: "a user-content entity with id user-content:my-note already exists",
 }),
 { status: 409, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runCreateUserContent(client, { title: "My Note", body: "x", tags: ["x"] }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("edit_user_content_section tool", () => {
 test("happy path sends If-Match, returns section + new etag", async () => {
 let seenURL = "";
 let seenIfMatch = "";
 let seenBody = "";
 const client = clientWith((u, init) => {
 seenURL = u;
 seenIfMatch = (init.headers as Record<string, string>)["If-Match"] ?? "";
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 id: "user-content:my-note",
 section: {
 index: 0,
 depth: 2,
 heading: "First",
 heading_slug: "first",
 body: "edited content\n",
 byte_offset: 0,
 },
 }),
 {
 headers: {
 "Content-Type": "application/json",
 ETag: `"updatedetag5678"`,
 },
 },
 );
 });
 const got = (await runEditUserContentSection(client, {
 id: "user-content:my-note",
 sec: "first",
 body: "edited content\n",
 etag: `"oldetag1234"`,
 })) as { ok: boolean; section?: { body: string }; etag?: string };
 expect(got.ok).toBe(true);
 expect(got.section?.body).toBe("edited content\n");
 expect(got.etag).toBe(`"updatedetag5678"`);
 expect(seenURL).toContain("/v1/user-content/user-content%3Amy-note/sections/first");
 expect(seenIfMatch).toBe(`"oldetag1234"`);
 expect(JSON.parse(seenBody).body).toBe("edited content\n");
 });

 test("412 stale etag returns passthrough envelope with current_etag", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "precondition_failed",
 message: "If-Match etag does not match current entity body",
 }),
 {
 status: 412,
 headers: {
 "Content-Type": "application/json",
 ETag: `"freshcurrent"`,
 },
 },
 ),
 );
 const got = (await runEditUserContentSection(client, {
 id: "user-content:foo",
 sec: "a",
 body: "x",
 etag: `"stale"`,
 })) as { ok: boolean; error: string; current_etag?: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("precondition_failed");
 expect(got.current_etag).toBe(`"freshcurrent"`);
 });

 test("428 missing-If-Match returns passthrough envelope", async () => {
 // Simulate the server seeing no If-Match: would only fire if the
 // client logic had a bug; we test the envelope passthrough path
 // by responding with 428 directly even when client did send If-Match.
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "precondition_required",
 message: "If-Match header is required",
 }),
 {
 status: 428,
 headers: { "Content-Type": "application/json" },
 },
 ),
 );
 const got = (await runEditUserContentSection(client, {
 id: "user-content:foo",
 sec: "a",
 body: "x",
 etag: `"x"`,
 })) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("precondition_required");
 });

 test("403 author_mismatch returns passthrough envelope", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "author_mismatch",
 message: "only the original author or the entity's operator may edit",
 }),
 { status: 403, headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runEditUserContentSection(client, {
 id: "user-content:foo",
 sec: "a",
 body: "x",
 etag: `"x"`,
 })) as { ok: boolean; error: string };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("author_mismatch");
 });

 test("missing etag arg returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runEditUserContentSection(client, {
 id: "user-content:foo",
 sec: "a",
 body: "x",
 })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("5xx still throws (transient infrastructure failures)", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "internal_error", message: "boom" }),
 { status: 500, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runEditUserContentSection(client, {
 id: "user-content:foo",
 sec: "a",
 body: "x",
 etag: `"x"`,
 }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("delete_user_content tool", () => {
 test("happy path returns ok + id + deleted: true", async () => {
 let seenURL = "";
 let seenMethod = "";
 const client = clientWith((u, init) => {
 seenURL = u;
 seenMethod = init.method ?? "";
 return new Response(
 JSON.stringify({ ok: true, id: "user-content:gone", deleted: true }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runDeleteUserContent(client, { id: "user-content:gone" })) as {
 ok: boolean;
 id: string;
 deleted: boolean;
 };
 expect(got.ok).toBe(true);
 expect(got.id).toBe("user-content:gone");
 expect(got.deleted).toBe(true);
 expect(seenMethod).toBe("DELETE");
 expect(seenURL).toBe("http://yaad-index.test/v1/user-content/user-content%3Agone");
 });

 test("missing id returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runDeleteUserContent(client, {})) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("403 author_mismatch bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "author_mismatch",
 message: "only the original author or the entity's operator may delete",
 }),
 { status: 403, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runDeleteUserContent(client, { id: "user-content:locked" }),
 ).rejects.toThrow(YaadIndexError);
 });

 test("404 NOT silently ok — already-deleted bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "not_found", message: "no entity" }),
 { status: 404, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runDeleteUserContent(client, { id: "user-content:gone-already" }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("delete_entity tool", () => {
 test("happy path returns ok + id + deleted: true", async () => {
 let seenURL = "";
 let seenMethod = "";
 const client = clientWith((u, init) => {
 seenURL = u;
 seenMethod = init.method ?? "";
 return new Response(
 JSON.stringify({
 ok: true,
 id: "boardgame:gaia-project-2017",
 deleted: true,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runDeleteEntity(client, {
 id: "boardgame:gaia-project-2017",
 })) as { ok: boolean; id: string; deleted: boolean };
 expect(got.ok).toBe(true);
 expect(got.id).toBe("boardgame:gaia-project-2017");
 expect(got.deleted).toBe(true);
 expect(seenMethod).toBe("DELETE");
 expect(seenURL).toBe(
 "http://yaad-index.test/v1/entities/boardgame%3Agaia-project-2017",
 );
 });

 test("missing id returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runDeleteEntity(client, {})) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("404 NOT silently ok — already-deleted bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: false, error: "not_found", message: "no entity" }),
 { status: 404, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runDeleteEntity(client, { id: "boardgame:never-existed-2024" }),
 ).rejects.toThrow(YaadIndexError);
 });

 test("503 vault_required bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({
 ok: false,
 error: "vault_required",
 message: "DELETE /v1/entities/{id} requires vault.path configuration",
 }),
 { status: 503, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runDeleteEntity(client, { id: "boardgame:vaultless-2024" }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("reindex tool", () => {
 test("happy path with no args sends no body and passes through the summary", async () => {
 let seenBody: string | undefined;
 const client = clientWith((_u, init) => {
 seenBody = init.body === undefined || init.body === null ? undefined : String(init.body);
 return new Response(
 JSON.stringify({
 mode: "incremental",
 scanned: 0,
 skipped: 0,
 parsed: 0,
 entities_created: 0,
 entities_updated: 0,
 entities_deleted: 0,
 edge_rows_written: 0,
 started_at: "2026-05-07T16:00:00Z",
 finished_at: "2026-05-07T16:00:00Z",
 duration_ms: 0,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runReindex(client, {})) as { mode: string };
 expect(seenBody).toBeUndefined();
 expect(got.mode).toBe("incremental");
 });

 test("happy path with mode='full' sends {mode: 'full'}", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 mode: "full",
 scanned: 50,
 skipped: 0,
 parsed: 50,
 entities_created: 5,
 entities_updated: 45,
 entities_deleted: 0,
 edge_rows_written: 12,
 started_at: "2026-05-07T16:00:00Z",
 finished_at: "2026-05-07T16:00:01Z",
 duration_ms: 1000,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runReindex(client, { mode: "full" })) as { mode: string };
 expect(JSON.parse(seenBody)).toEqual({ mode: "full" });
 expect(got.mode).toBe("full");
 });

 test("invalid mode returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runReindex(client, { mode: "everything" })) as {
 ok: boolean;
 error: string;
 message: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(got.message).toContain("everything");
 expect(called).toBe(false);
 });

 test("404 (vault.path unset) bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ error: "not_found", message: "reindex unregistered" }),
 { status: 404, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(runReindex(client, {})).rejects.toThrow(YaadIndexError);
 });
});

describe("get_entities_batch tool", () => {
 test("happy path passes ids through and surfaces entities + missing", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 entities: [
 { id: "person:a", kind: "person", data: { name: "A" } },
 { id: "person:b", kind: "person", data: { name: "B" } },
 ],
 missing: ["person:c"],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runGetEntitiesBatch(client, {
 ids: ["person:a", "person:b", "person:c"],
 })) as { ok: boolean; entities: Array<{ id: string }>; missing: string[] };
 expect(JSON.parse(seenBody)).toEqual({
 ids: ["person:a", "person:b", "person:c"],
 });
 expect(got.entities).toHaveLength(2);
 expect(got.missing).toEqual(["person:c"]);
 });

 test("with_edges is plumbed through to the request body", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({ ok: true, entities: [], missing: [] }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runGetEntitiesBatch(client, {
 ids: ["person:a"],
 with_edges: ["is_about", "designed_by"],
 });
 expect(JSON.parse(seenBody)).toEqual({
 ids: ["person:a"],
 with_edges: ["is_about", "designed_by"],
 });
 });

 test("missing ids returns invalid_argument without calling API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runGetEntitiesBatch(client, {})) as {
 ok: boolean;
 error: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("empty ids array returns invalid_argument", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runGetEntitiesBatch(client, { ids: [] })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("non-string id in array returns invalid_argument", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runGetEntitiesBatch(client, { ids: ["ok", 42] })) as {
 ok: boolean;
 error: string;
 message: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(got.message).toContain("42");
 expect(called).toBe(false);
 });

 test("non-array with_edges returns invalid_argument", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runGetEntitiesBatch(client, {
 ids: ["person:a"],
 with_edges: "is_about",
 })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("400 too_many_ids bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 `{"error":"too_many_ids","message":"ids exceeds maximum of 100 (got 150)"}`,
 { status: 400, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runGetEntitiesBatch(client, {
 ids: Array.from({ length: 150 }, (_, i) => `person:p${i}`),
 }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("archive_entity tool", () => {
 test("happy path POSTs /archive and returns the upstream envelope", async () => {
 let seenUrl = "", seenMethod = "";
 const client = clientWith((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 return new Response(
 JSON.stringify({ ok: true, id: "boardgame:foo", archived: true }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runArchiveEntity(client, { id: "boardgame:foo" })) as {
 ok: boolean;
 archived: boolean;
 };
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/boardgame%3Afoo/archive");
 expect(seenMethod).toBe("POST");
 expect(got.ok).toBe(true);
 expect(got.archived).toBe(true);
 });

 test("missing id returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runArchiveEntity(client, {})) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });
});

describe("restore_entity tool", () => {
 test("happy path POSTs /restore and returns archived=false", async () => {
 let seenUrl = "";
 const client = clientWith((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ ok: true, id: "boardgame:foo", archived: false }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runRestoreEntity(client, { id: "boardgame:foo" })) as {
 ok: boolean;
 archived: boolean;
 };
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/boardgame%3Afoo/restore");
 expect(got.archived).toBe(false);
 });

 test("missing id returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runRestoreEntity(client, {})) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });
});

describe("delete_entity tool — ADR-0018 step 4 state-machine", () => {
 test("409 archive-first envelope is surfaced verbatim, not thrown", async () => {
 const client = clientWith(
 () =>
 new Response(
 `{"ok":false,"error":"must archive before delete","message":"POST /v1/entities/boardgame:foo/archive first; DELETE only destroys archived entities (ADR-0018)"}`,
 { status: 409, headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runDeleteEntity(client, { id: "boardgame:foo" })) as {
 ok: boolean;
 error: string;
 message: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("must archive before delete");
 expect(got.message).toContain("/archive first");
 });

 test("hard-destroy on archived entity returns deleted=true", async () => {
 const client = clientWith(
 () =>
 new Response(
 JSON.stringify({ ok: true, id: "boardgame:foo", deleted: true }),
 { headers: { "Content-Type": "application/json" } },
 ),
 );
 const got = (await runDeleteEntity(client, { id: "boardgame:foo" })) as {
 deleted: boolean;
 };
 expect(got.deleted).toBe(true);
 });

 test("404 still throws (non-409 errors keep the YaadIndexError shape)", async () => {
 const client = clientWith(
 () =>
 new Response(`{"error":"not_found","message":"already gone"}`, {
 status: 404,
 headers: { "Content-Type": "application/json" },
 }),
 );
 await expect(runDeleteEntity(client, { id: "boardgame:gone" })).rejects.toThrow(
 YaadIndexError,
 );
 });
});

describe("set_operator_fill tool", () => {
 test("happy path forwards fields verbatim to operator-fill endpoint", async () => {
 let seenUrl = "", seenBody = "";
 const client = clientWith((u, init) => {
 seenUrl = u;
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({ ok: true, entity: { id: "boardgame:foo", kind: "boardgame" }, gaps: [] }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runSetOperatorFill(client, {
 id: "boardgame:foo",
 fields: { rating: 9, owned: true },
 })) as { ok: boolean };
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/boardgame%3Afoo/operator-fill");
 expect(JSON.parse(seenBody)).toEqual({ rating: 9, owned: true });
 expect(got.ok).toBe(true);
 });

 test("missing id returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}", { headers: { "Content-Type": "application/json" } });
 });
 const got = (await runSetOperatorFill(client, { fields: { rating: 9 } })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("missing fields returns invalid_argument", async () => {
 const client = clientWith(() => new Response("{}"));
 const got = (await runSetOperatorFill(client, { id: "boardgame:foo" })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 });

 test("array-shape fields rejected (must be object)", async () => {
 const client = clientWith(() => new Response("{}"));
 const got = (await runSetOperatorFill(client, {
 id: "boardgame:foo",
 fields: [9, true],
 })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 });

 test("409 deferred_requires_unfilled bubbles as YaadIndexError", async () => {
 const client = clientWith(
 () =>
 new Response(
 `{"error":"deferred_requires_unfilled","message":"x"}`,
 { status: 409, headers: { "Content-Type": "application/json" } },
 ),
 );
 await expect(
 runSetOperatorFill(client, {
 id: "boardgame:foo",
 fields: { rating: { defer: true } },
 }),
 ).rejects.toThrow(YaadIndexError);
 });
});

describe("defer_gap tool", () => {
 test("happy path POSTs {<field>: {defer: true}}", async () => {
 let seenBody = "";
 const client = clientWith((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({ ok: true, entity: { id: "boardgame:foo", kind: "boardgame" }, gaps: [] }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runDeferGap(client, { id: "boardgame:foo", field: "played" });
 expect(JSON.parse(seenBody)).toEqual({ played: { defer: true } });
 });

 test("missing field returns invalid_argument", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runDeferGap(client, { id: "boardgame:foo" })) as {
 ok: boolean;
 error: string;
 };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });
});

describe("edges tool", () => {
 test("happy path forwards entity_id + edge_types + direction", async () => {
 let seenUrl = "";
 const client = clientWith((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({
 ok: true,
 edges: [{ type: "designed_by", from_id: "boardgame:foo", to_id: "person:bar" }],
 next_cursor: null,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 const got = (await runEdges(client, {
 entity_id: "boardgame:foo",
 edge_types: ["designed_by"],
 direction: "out",
 })) as { ok: boolean; edges: Array<{ type: string }> };
 expect(seenUrl).toContain("entity_id=boardgame%3Afoo");
 expect(seenUrl).toContain("edge_types=designed_by");
 expect(seenUrl).toContain("direction=out");
 expect(got.ok).toBe(true);
 expect(got.edges).toHaveLength(1);
 });

 test("missing entity_id returns invalid_argument without calling the API", async () => {
 let called = false;
 const client = clientWith(() => {
 called = true;
 return new Response("{}");
 });
 const got = (await runEdges(client, {})) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 expect(called).toBe(false);
 });

 test("non-array edge_types rejected", async () => {
 const client = clientWith(() => new Response("{}"));
 const got = (await runEdges(client, {
 entity_id: "boardgame:foo",
 edge_types: "designed_by",
 })) as { ok: boolean; error: string };
 expect(got.error).toBe("invalid_argument");
 });

 test("invalid direction rejected with hint", async () => {
 const client = clientWith(() => new Response("{}"));
 const got = (await runEdges(client, {
 entity_id: "boardgame:foo",
 direction: "sideways",
 })) as { ok: boolean; error: string; message: string };
 expect(got.error).toBe("invalid_argument");
 expect(got.message).toContain("out, in, both");
 });

 test("direction omitted falls back to client default (no direction param in URL)", async () => {
 let seenUrl = "";
 const client = clientWith((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ ok: true, edges: [], next_cursor: null }),
 { headers: { "Content-Type": "application/json" } },
 );
 });
 await runEdges(client, { entity_id: "person:foo" });
 expect(seenUrl).not.toContain("direction=");
 });
});
