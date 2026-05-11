import { describe, expect, test } from "bun:test";

import { YaadIndexClient, YaadIndexError } from "../src/client/yaad_index.js";

function fakeFetch(
 responder: (url: string, init: RequestInit) => Response | Promise<Response>,
) {
 return async (input: string | URL, init: RequestInit = {}) => {
 const url = typeof input === "string" ? input : input.toString();
 return await responder(url, init);
 };
}

describe("YaadIndexClient", () => {
 test("ingest POSTs to /v1/ingest with the URL in the body", async () => {
 let seenUrl = "";
 let seenBody = "";
 let seenMethod = "";
 let seenAuth: string | null = null;
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenBody = String(init.body ?? "");
 seenMethod = init.method ?? "GET";
 seenAuth =
 (init.headers as Record<string, string> | undefined)?.["Authorization"] ?? null;
 return new Response(
 JSON.stringify({ state: "complete", entity: { id: "wikipedia:foo", kind: "wikipedia-article" } }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.ingest("https://en.wikipedia.org/wiki/Foo");
 expect(seenUrl).toBe("http://yaad-index.test/v1/ingest");
 expect(seenMethod).toBe("POST");
 expect(JSON.parse(seenBody)).toEqual({ url: "https://en.wikipedia.org/wiki/Foo" });
 expect(seenAuth).toBeNull();
 expect(got.state).toBe("complete");
 expect(got.entity?.id).toBe("wikipedia:foo");
 });

 test("getEntity GETs /v1/entities/<id>?with_edges=* so all outgoing edges expand inline", async () => {
 let seenUrl = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test/",
 fetchImpl: fakeFetch((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ id: "wikipedia:martin-wallace", kind: "wikipedia-article" }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.getEntity("wikipedia:martin-wallace");
 expect(seenUrl).toBe(
 "http://yaad-index.test/v1/entities/wikipedia%3Amartin-wallace?with_edges=*",
 );
 expect(got.kind).toBe("wikipedia-article");
 });

 test("fill POSTs to /v1/entities/<id>/fill with {fields}", async () => {
 let seenUrl = "";
 let seenBody = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({ ok: true, entity: { id: "person:martin-wallace", kind: "person" }, gaps: [] }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.fill("person:martin-wallace", {
 birth_date: "1957",
 occupation: "board-game designer",
 });
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/person%3Amartin-wallace/fill");
 expect(JSON.parse(seenBody)).toEqual({
 fields: { birth_date: "1957", occupation: "board-game designer" },
 });
 expect(got.ok).toBe(true);
 expect(got.gaps).toEqual([]);
 });

 test("listEntities hits /v1/search?kind=<kind>&limit=100 (URL-encoded)", async () => {
 let seenUrl = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ ok: true, results: [], total: 0, limit: 100, offset: 0 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 await client.listEntities("wikipedia-article");
 expect(seenUrl).toBe(
 "http://yaad-index.test/v1/search?kind=wikipedia-article&limit=100",
 );
 });

 test("listEntities throws YaadIndexError(400) when kind is empty", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(() => {
 throw new Error("fetch should not be called when kind is empty");
 }),
 });
 await expect(client.listEntities("")).rejects.toBeInstanceOf(YaadIndexError);
 });

 test("authToken is sent as Authorization: Bearer when configured", async () => {
 let seenAuth = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 authToken: "secret-token",
 fetchImpl: fakeFetch((_u, init) => {
 seenAuth =
 (init.headers as Record<string, string> | undefined)?.["Authorization"] ?? "";
 return new Response(JSON.stringify({ state: "complete" }), {
 headers: { "Content-Type": "application/json" },
 });
 }),
 });
 await client.ingest("https://example.test/foo");
 expect(seenAuth).toBe("Bearer secret-token");
 });

 test("non-2xx response throws YaadIndexError carrying status + body", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(`{"error":"unsupported_url","message":"no plugin matches"}`, {
 status: 422,
 headers: { "Content-Type": "application/json" },
 }),
 ),
 });
 let caught: unknown;
 try {
 await client.ingest("not-a-real-url-shape");
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 const err = caught as YaadIndexError;
 expect(err.status).toBe(422);
 expect(err.message).toContain("unsupported_url");
 });

 test("baseUrl trailing slashes are normalized", async () => {
 let seenUrl = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test///",
 fetchImpl: fakeFetch((u) => {
 seenUrl = u;
 return new Response(JSON.stringify({}), {
 headers: { "Content-Type": "application/json" },
 });
 }),
 });
 await client.getEntity("foo");
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/foo?with_edges=*");
 });

 test("reindex POSTs to /v1/reindex with no body when mode is omitted", async () => {
 let seenUrl = "";
 let seenMethod = "";
 let seenBody: string | undefined;
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 seenBody = init.body === undefined || init.body === null ? undefined : String(init.body);
 return new Response(
 JSON.stringify({
 mode: "incremental",
 scanned: 12,
 skipped: 8,
 parsed: 4,
 entities_created: 2,
 entities_updated: 1,
 entities_deleted: 0,
 edge_rows_written: 5,
 started_at: "2026-05-07T16:00:00Z",
 finished_at: "2026-05-07T16:00:00.123Z",
 duration_ms: 123,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.reindex();
 expect(seenUrl).toBe("http://yaad-index.test/v1/reindex");
 expect(seenMethod).toBe("POST");
 expect(seenBody).toBeUndefined();
 expect(got.mode).toBe("incremental");
 expect(got.entities_created).toBe(2);
 });

 test("reindex POSTs {mode: 'full'} when mode is explicit", async () => {
 let seenBody = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 mode: "full",
 scanned: 100,
 skipped: 0,
 parsed: 100,
 entities_created: 0,
 entities_updated: 100,
 entities_deleted: 0,
 edge_rows_written: 0,
 started_at: "2026-05-07T16:01:00Z",
 finished_at: "2026-05-07T16:01:02Z",
 duration_ms: 2000,
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.reindex("full");
 expect(JSON.parse(seenBody)).toEqual({ mode: "full" });
 expect(got.mode).toBe("full");
 expect(got.parsed).toBe(100);
 });

 test("reindex propagates a 404 (vault.path unconfigured) as YaadIndexError", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(`{"error":"not_found","message":"reindex unregistered: vault.path is empty"}`, {
 status: 404,
 headers: { "Content-Type": "application/json" },
 }),
 ),
 });
 let caught: unknown;
 try {
 await client.reindex();
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 expect((caught as YaadIndexError).status).toBe(404);
 });

 test("getEntitiesBatch POSTs {ids} to /v1/entities/batch and surfaces missing[] verbatim", async () => {
 let seenUrl = "";
 let seenMethod = "";
 let seenBody = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 entities: [
 { id: "person:martin-wallace", kind: "person", data: { name: "Martin Wallace" } },
 ],
 missing: ["person:never-existed"],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.getEntitiesBatch([
 "person:martin-wallace",
 "person:never-existed",
 ]);
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/batch");
 expect(seenMethod).toBe("POST");
 expect(JSON.parse(seenBody)).toEqual({
 ids: ["person:martin-wallace", "person:never-existed"],
 });
 expect(got.ok).toBe(true);
 expect(got.entities).toHaveLength(1);
 expect(got.missing).toEqual(["person:never-existed"]);
 });

 test("getEntitiesBatch passes with_edges through to the request body when set", async () => {
 let seenBody = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((_u, init) => {
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({ ok: true, entities: [], missing: [] }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 await client.getEntitiesBatch(["person:a"], ["is_about"]);
 expect(JSON.parse(seenBody)).toEqual({
 ids: ["person:a"],
 with_edges: ["is_about"],
 });
 });

 test("getEntitiesBatch surfaces 400 too_many_ids as YaadIndexError", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(
 `{"error":"too_many_ids","message":"ids exceeds maximum of 100 (got 150)"}`,
 { status: 400, headers: { "Content-Type": "application/json" } },
 ),
 ),
 });
 let caught: unknown;
 try {
 await client.getEntitiesBatch(Array.from({ length: 150 }, (_, i) => `kind:item-${i}`));
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 const err = caught as YaadIndexError;
 expect(err.status).toBe(400);
 expect(err.message).toContain("too_many_ids");
 });

 test("getKinds GETs /v1/kinds and returns entity_kinds + edge_kinds verbatim", async () => {
 let seenUrl = "";
 let seenMethod = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 return new Response(
 JSON.stringify({
 ok: true,
 entity_kinds: [
 { name: "boardgame", description: "BGG boardgame", source_plugins: ["bgg"] },
 { name: "wikipedia-article", description: "Wikipedia article", source_plugins: ["wikipedia"] },
 ],
 edge_kinds: [
 {
 name: "is_about",
 description: "Article is about a canonical entity",
 from_kind: "wikipedia-article",
 to_kind: "person",
 source_plugins: ["wikipedia"],
 },
 ],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.getKinds();
 expect(seenUrl).toBe("http://yaad-index.test/v1/kinds");
 expect(seenMethod).toBe("GET");
 expect(got.ok).toBe(true);
 expect(got.entity_kinds).toHaveLength(2);
 expect(got.edge_kinds).toHaveLength(1);
 expect(got.edge_kinds[0]?.from_kind).toBe("wikipedia-article");
 });

 test("getPlugins GETs /v1/plugins and returns per-plugin Capabilities subset verbatim", async () => {
 let seenUrl = "";
 let seenMethod = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 return new Response(
 JSON.stringify({
 ok: true,
 plugins: [
 {
 name: "wikipedia",
 version: "0.6.0",
 url_patterns: ["^https?://[a-z]{2,3}\\.wikipedia\\.org/wiki/.+"],
 commands: [],
 entity_kinds: [{ name: "source", description: "wikipedia source" }],
 edge_kinds: [],
 source_namespace: "wikipedia",
 },
 {
 name: "gmail",
 version: "0.4.0",
 url_patterns: [],
 commands: ["fetch"],
 entity_kinds: [{ name: "source" }],
 edge_kinds: [],
 source_namespace: "gmail",
 },
 ],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.getPlugins();
 expect(seenUrl).toBe("http://yaad-index.test/v1/plugins");
 expect(seenMethod).toBe("GET");
 expect(got.ok).toBe(true);
 expect(got.plugins).toHaveLength(2);
 expect(got.plugins[0]?.name).toBe("wikipedia");
 expect(got.plugins[0]?.url_patterns).toHaveLength(1);
 expect(got.plugins[0]?.commands).toEqual([]);
 expect(got.plugins[1]?.name).toBe("gmail");
 expect(got.plugins[1]?.commands).toEqual(["fetch"]);
 expect(got.plugins[1]?.url_patterns).toEqual([]);
 expect(got.plugins[1]?.source_namespace).toBe("gmail");
 });

 test("archiveEntity POSTs /v1/entities/<id>/archive and returns the archive envelope", async () => {
 let seenUrl = "";
 let seenMethod = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 return new Response(
 JSON.stringify({ ok: true, id: "boardgame:foo", archived: true }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.archiveEntity("boardgame:foo");
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/boardgame%3Afoo/archive");
 expect(seenMethod).toBe("POST");
 expect(got.ok).toBe(true);
 expect(got.archived).toBe(true);
 });

 test("restoreEntity POSTs /v1/entities/<id>/restore and returns archived=false", async () => {
 let seenUrl = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ ok: true, id: "boardgame:foo", archived: false }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.restoreEntity("boardgame:foo");
 expect(seenUrl).toBe("http://yaad-index.test/v1/entities/boardgame%3Afoo/restore");
 expect(got.archived).toBe(false);
 });

 test("archiveEntity throws YaadIndexError on 404 not_found", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(`{"error":"not_found","message":"no entity with id boardgame:nope"}`, {
 status: 404,
 headers: { "Content-Type": "application/json" },
 }),
 ),
 });
 let caught: unknown;
 try {
 await client.archiveEntity("boardgame:nope");
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 expect((caught as YaadIndexError).status).toBe(404);
 });

 test("archiveEntity / restoreEntity throw on missing id without calling fetch", async () => {
 let called = false;
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(() => {
 called = true;
 return new Response("", { status: 200 });
 }),
 });
 let aErr: unknown, rErr: unknown;
 try { await client.archiveEntity(""); } catch (e) { aErr = e; }
 try { await client.restoreEntity(""); } catch (e) { rErr = e; }
 expect(aErr).toBeInstanceOf(YaadIndexError);
 expect(rErr).toBeInstanceOf(YaadIndexError);
 expect(called).toBe(false);
 });

 test("deleteEntity returns the upstream envelope verbatim on 409 archive-first", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(
 `{"ok":false,"error":"must archive before delete","message":"POST /v1/entities/boardgame:foo/archive first; DELETE only destroys archived entities (ADR-0018)"}`,
 { status: 409, headers: { "Content-Type": "application/json" } },
 ),
 ),
 });
 const got = (await client.deleteEntity("boardgame:foo")) as {
 ok: boolean;
 error: string;
 message: string;
 };
 // Surfaced verbatim — NOT thrown — so the agent can branch on `error`.
 expect(got.ok).toBe(false);
 expect(got.error).toBe("must archive before delete");
 expect(got.message).toContain("/archive first");
 });

 test("deleteEntity hard-destroy on archived entity returns deleted=true", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(
 JSON.stringify({ ok: true, id: "boardgame:foo", deleted: true }),
 { headers: { "Content-Type": "application/json" } },
 ),
 ),
 });
 const got = (await client.deleteEntity("boardgame:foo")) as {
 ok: boolean;
 id: string;
 deleted: boolean;
 };
 expect(got.deleted).toBe(true);
 expect(got.id).toBe("boardgame:foo");
 });

 test("deleteEntity still throws on 404 / 503 (non-409 errors)", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(`{"error":"not_found","message":"already gone"}`, {
 status: 404,
 headers: { "Content-Type": "application/json" },
 }),
 ),
 });
 let caught: unknown;
 try {
 await client.deleteEntity("boardgame:gone");
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 expect((caught as YaadIndexError).status).toBe(404);
 });

 test("deleteUserContent surfaces 409 verbatim too", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(
 `{"ok":false,"error":"must archive before delete","message":"POST /v1/entities/user-content:foo/archive first; DELETE only destroys archived entities (ADR-0018)"}`,
 { status: 409, headers: { "Content-Type": "application/json" } },
 ),
 ),
 });
 const got = (await client.deleteUserContent("user-content:foo")) as {
 ok: boolean;
 error: string;
 };
 expect(got.ok).toBe(false);
 expect(got.error).toBe("must archive before delete");
 });

 test("setOperatorFill POSTs /v1/entities/<id>/operator-fill with fields verbatim", async () => {
 let seenUrl = "";
 let seenMethod = "";
 let seenBody = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u, init) => {
 seenUrl = u;
 seenMethod = init.method ?? "GET";
 seenBody = String(init.body ?? "");
 return new Response(
 JSON.stringify({
 ok: true,
 entity: { id: "boardgame:foo", kind: "boardgame" },
 gaps: ["want"],
 }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 const got = await client.setOperatorFill("boardgame:foo", {
 rating: 9,
 played: { defer: true },
 summary: null,
 });
 expect(seenUrl).toBe(
 "http://yaad-index.test/v1/entities/boardgame%3Afoo/operator-fill",
 );
 expect(seenMethod).toBe("POST");
 // Body is the fields object verbatim — no wrapping under a `fields:` key.
 expect(JSON.parse(seenBody)).toEqual({
 rating: 9,
 played: { defer: true },
 summary: null,
 });
 expect(got.ok).toBe(true);
 });

 test("setOperatorFill 409 deferred_requires_unfilled bubbles as YaadIndexError", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(
 `{"error":"deferred_requires_unfilled","message":"field \\"rating\\" is filled; cannot mark deferred"}`,
 { status: 409, headers: { "Content-Type": "application/json" } },
 ),
 ),
 });
 let caught: unknown;
 try {
 await client.setOperatorFill("boardgame:foo", {
 rating: { defer: true },
 });
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 expect((caught as YaadIndexError).status).toBe(409);
 expect((caught as YaadIndexError).message).toContain(
 "deferred_requires_unfilled",
 );
 });

 test("setOperatorFill 403 agent_not_allowed bubbles as YaadIndexError", async () => {
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(
 () =>
 new Response(
 `{"error":"agent_not_allowed","message":"operator-fill is operator-only"}`,
 { status: 403, headers: { "Content-Type": "application/json" } },
 ),
 ),
 });
 let caught: unknown;
 try {
 await client.setOperatorFill("boardgame:foo", { rating: 9 });
 } catch (e) {
 caught = e;
 }
 expect(caught).toBeInstanceOf(YaadIndexError);
 expect((caught as YaadIndexError).status).toBe(403);
 });

 test("edges builds the expected URL with entity_id, edge_types, direction", async () => {
 let seenUrl = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ ok: true, edges: [], next_cursor: null }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 await client.edges("boardgame:foo", ["designed_by", "authored_by"], "both");
 expect(seenUrl).toBe(
 "http://yaad-index.test/v1/edges?entity_id=boardgame%3Afoo&edge_types=designed_by%2Cauthored_by&direction=both",
 );
 });

 test("edges with no edge_types and default direction omits those params", async () => {
 let seenUrl = "";
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch((u) => {
 seenUrl = u;
 return new Response(
 JSON.stringify({ ok: true, edges: [], next_cursor: null }),
 { headers: { "Content-Type": "application/json" } },
 );
 }),
 });
 await client.edges("person:martin");
 expect(seenUrl).toBe(
 "http://yaad-index.test/v1/edges?entity_id=person%3Amartin",
 );
 });

 test("edges throws YaadIndexError on missing id without calling fetch", async () => {
 let called = false;
 const client = new YaadIndexClient({
 baseUrl: "http://yaad-index.test",
 fetchImpl: fakeFetch(() => {
 called = true;
 return new Response("{}");
 }),
 });
 await expect(client.edges("")).rejects.toBeInstanceOf(YaadIndexError);
 expect(called).toBe(false);
 });
});
