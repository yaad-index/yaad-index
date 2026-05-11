# ADR-0005 — Plugin lifecycle

**Status:** Accepted (2026-04-28). Discovery section superseded by ADR-0006 (config allowlist replaces PATH scan). Single-entity response shape (the "Plugin response — single shape" section below) superseded by ADR-0023 (NDJSON streaming, one envelope per line). Invocation, `--init`, cache rules unchanged.
**Date:** 2026-04-28
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md), [ADR-0002](./0002-api-surface.md)

## Context

ADR-0002 forward-references "a future ADR on plugin lifecycle" in several places. This is that ADR.

The yaad-index server is the only HTTP-facing thing in the architecture. Agents talk to the server. The server, in turn, dispatches ingestion work to *plugins* — separate processes that each know how to fetch from one source family (boardgamegeek.com, gmail, generic web pages, …) and produce entities + edges.

Three intertwined questions need answering before any implementation:

1. **Invocation model.** How does the server engage a plugin: HTTP, long-running stdio, subprocess-per-request, in-process?
2. **Discovery.** How does the server know which plugins exist and what they handle?
3. **Cache & freshness.** When does the server skip the plugin and use the cache, and how does it know when the cache is stale?

This ADR commits to one answer per question.

## Decision

### Invocation: subprocess-per-request, JSON over stdio

Plugins are separate executables. When the server needs to ingest from a source, it forks the plugin binary, writes a JSON request to its stdin, reads a JSON response from its stdout, and the plugin exits. One process per request. No daemons, no ports, no health-check fans, no plugin sidecars to operate.

This deliberately breaks from the everything-HTTP commitment in [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md). That commitment is about *external* surface: agents (current and future, on this host or elsewhere) all reach yaad-index through HTTP. Server-to-plugin is *internal* infra; uniformity there is not load-bearing for the agent contract. The cost of HTTP-everywhere internally would be:

- Each plugin a long-running daemon (5+ services for a typical install).
- Port management or unix-socket routing.
- Plugin restart-on-update orchestration.
- Health checks.

For a personal-scale tool, those costs aren't worth the marginal uniformity.

**Promotion path:** if one plugin grows performance demands that subprocess cold-start hurts, the migration is from "subprocess per request" to "long-lived stdio subprocess" (same JSON-over-stdio protocol, just keep the process alive between requests). HTTP-as-internal can wait for a real reason; v1 doesn't have one.

### Discovery: git-style PATH scan + `--init` introspection

> **Superseded by [ADR-0006](./0006-plugin-discovery-config-allowlist.md).** The PATH-scan mechanism described below was rejected on security grounds (anything matching `yaad-*` on PATH would auto-register, expanding the trust boundary implicitly). Discovery is now an explicit config allowlist. The `--init` introspection sub-decision and the `yaad-*` binary-name convention survive — only the discovery mechanism changed.

Plugins are binaries on `$PATH` named `yaad-<name>` (e.g., `yaad-bgg`, `yaad-web`, `yaad-gmail`). This mirrors git's command discovery — `git foo` exec's `git-foo` from PATH; `yaad-index` dispatches to `yaad-bgg` by the same convention.

At server startup, the server scans `$PATH` for binaries matching `yaad-*`. For each match, it invokes the binary with a single `--init` flag:

```
$ yaad-bgg --init
```

The plugin writes a JSON capabilities document to stdout, then exits 0. Example:

```json
{
 "name": "bgg",
 "version": "0.1.0",
 "url_patterns": [
 "^https?://(www\\.)?boardgamegeek\\.com/boardgame/.*",
 "^https?://(www\\.)?boardgamegeek\\.com/boardgameperson/.*"
 ],
 "entity_kinds": [
 { "name": "boardgame", "default_ttl_days": 30 },
 { "name": "person", "default_ttl_days": 90 }
 ],
 "edge_kinds": [
 { "name": "designed_by", "from_kind": "boardgame", "to_kind": "person" },
 { "name": "published_by", "from_kind": "boardgame", "to_kind": "company" }
 ]
}
```

The server uses this to build:

1. **url-pattern → plugin** map (for URL-based ingest, regex match against `url_patterns`).
2. **kinds catalog** (the union of all plugins' `entity_kinds` and `edge_kinds`, with each kind's `source_plugins` list — what `GET /v1/kinds` returns).

**Entities are kind-scoped, not plugin-scoped.** `person:tolkien` is one entity globally. If both `yaad-bgg` (Tolkien-as-game-designer) and `yaad-books` (Tolkien-as-author) produce data about the same person, both contribute *provenance entries* to the same entity row — they don't fork the entity into `bgg/person:tolkien` vs `books/person:tolkien`. The merge story stays clean.

**Ingest dispatch is URL-based.** A `POST /v1/ingest` request must include a URL; the server matches it to a plugin via the url-pattern map. **ID-only ingest is rejected** in v1 (returns 400; "ingest needs a URL; ID-only requests are cache reads via GET"). This sidesteps the question "if multiple plugins produce `person`, who handles `POST /v1/ingest {id: 'person:tolkien'}` ?" — the agent provides the URL, the URL pattern picks the plugin.

A future ADR can add a "reverse-resolver" mode (plugins declare `id_pattern` matching, server picks one to resolve ID → URL) when an actual flow demands it. v1 doesn't.

**Conflict resolution (URL patterns):** if two plugins claim overlapping URL patterns, the first one in PATH order wins. This is git's behavior. A config override (`plugins.<binary_name>.path: /explicit/path`) is available for cases where PATH order isn't what the operator wants.

**Re-discovery:** the registry is built once at startup and on demand via `POST /v1/plugins/refresh` (an admin endpoint — out of scope to fully spec here, the future plugin-management ADR owns it). Adding a new plugin = drop binary in PATH + refresh.

**Why git-style over manifest:** no parallel source of truth. The plugin binary + its `--init` output is the single declaration of what it does. No risk of a stale manifest claiming kinds the binary doesn't actually support.

### Cache & freshness: per-kind TTL declared by plugin, overridable

Each entity kind has a default TTL declared in the plugin's `--init` output (`default_ttl_days`). The server uses that TTL when deciding whether a cached entity is fresh enough to return inline or whether to spawn the plugin for a re-fetch.

```
ingest flow:
 1. parse the request (URL or canonical ID)
 2. resolve to a kind
 3. look up the entity in the index
 4. if found AND latest provenance.fetched_at + TTL_days > now → return inline (cache hit)
 5. else → dispatch to plugin
```

`force_refetch=true` (from ADR-0002's ingest request) bypasses step 4.

**TTL overrides:** the plugin declares a default; the operator can override per-kind via config (`ttl.<kind>: <days>`). A future ADR will add a `PUT /v1/kinds/<name>/ttl` admin endpoint for runtime overrides without restart. Not in v1.

### Request protocol

When the server dispatches a request, it forks the plugin and writes JSON to stdin:

```json
{
 "operation": "ingest",
 "url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
 "hint": "boardgame",
 "force_refetch": false
}
```

The plugin reads stdin until EOF, does its work, and writes a JSON response to stdout, then exits.

> **⚠️ Superseded by [ADR-0023](./0023-unified-plugin-response-protocol.md):** the single-entity response clause documented below is replaced by NDJSON streaming on stdout (one envelope per line, plugin exits when done). The field shape of an individual envelope (`structured`, `edges`, `raw_content`, `gaps`, `provenance`) carries forward unchanged; only the multiplicity contract changed (1 → N | 0). Read ADR-0023 before applying the rules in this section.

**Plugin response — single shape** (exit 0):

```json
{
 "ok": true,
 "structured": {
 "id": "boardgame:brass-birmingham",
 "kind": "boardgame",
 "data": { ...whatever fields the plugin could deterministically fill from the source... },
 "provenance": [{
 "source": "bgg:224517-2026-04-28",
 "fetched_at": "2026-04-28T18:30:00Z",
 "ok": true
 }]
 },
 "edges": [
 { "type": "designed_by", "from": "boardgame:brass-birmingham", "to": "person:martin-wallace" }
 ],
 "raw_content": "...verbatim source text/html/markdown for the agent's AI to read...",
 "gaps": ["summary", "tags", "complexity_assessment"]
}
```

The plugin is a **dumb fetcher**. Its job:

1. Fetch the source.
2. Fill whatever entity fields it can deterministically extract (structured collectors fill many; unstructured collectors fill few).
3. Hand back the raw content for whoever is going to AI-extract the rest.
4. Declare the gaps — the field names it couldn't fill that the agent's AI is expected to produce.

**Field optionality:**
- `structured.data` is **optional** — a fully unstructured plugin (e.g., a generic web fetcher) may omit it entirely; the agent's AI fills everything from `raw_content`.
- `raw_content` is **optional** — a fully structured plugin (e.g., BGG XML) with `gaps: []` may omit it; nothing left to AI-extract.
- `gaps` is **optional** — empty or omitted means the entity is complete; server skips the needs-fill response and stores immediately.
- `edges` is **optional** — plugins that don't extract relationships just omit the array.

A response with no `gaps` and a complete `structured` is the structured-collector fast path. A response with empty `structured.data` and full `raw_content` + a comprehensive `gaps` list is the pure-unstructured path.

**Conflicting fields when multiple plugins contribute:** two plugins (e.g., bgg and books) both emit a value for the same field on the same entity (e.g., `data.summary` on `person:tolkien`). v1 policy: **last-write-wins** at the field level, with the older value preserved in the provenance entry of its writing plugin. Future ADR can introduce per-field merge strategies (e.g., "first-non-empty", "longer-text-wins") if the use case demands.

**Plugins do not run AI.** The AI is the calling agent's job (see "AI feedback loop" below). This keeps the server (and plugins) free of AI client deps, prompt management, and key handling. The agent already has its model; yaad-index doesn't compete.

**The agent's response to the plugin** comes wrapped through the server's API (next section), not directly stdio-back to the plugin.

**Failure response (exit non-zero, or exit 0 with `ok: false`):**

```json
{
 "ok": false,
 "error": "fetch_failed",
 "message": "BGG returned 503 after 3 retries"
}
```

The server records a failure provenance entry against any pre-existing placeholder entity and surfaces the error to the agent.

**Stderr is not load-bearing.** Plugins log diagnostics to stderr; the server forwards stderr to its own log stream tagged with the plugin name. The protocol is stdin → stdout JSON only.

### AI feedback loop (server ↔ agent, *not* server ↔ plugin)

The plugin gives the server "what it could fill + gaps + raw content." The server then has a choice:

1. **No gaps** → the entity is complete. Server stores it, returns the full entity to the agent inline (200 status: `complete`).
2. **Gaps present** → server cannot complete the entity by itself (it has no AI). It returns a **needs-fill response** to the agent, with the partial entity + raw content + gap list + a one-shot `fill_token`.

The agent then runs AI on the raw content + partial-context, fills the gap fields, and POSTs the result back to a new endpoint (specified in the ADR-0002 amendment that lands alongside this one):

```
POST /v1/entities/<id>/fill
{
 "fill_token": "...",
 "fields": {
 "summary": "Heavy economic euro by Martin Wallace...",
 "tags": ["heavy-euro", "economic", "industrial-revolution"],
 "complexity_assessment": "..."
 }
}
```

The server validates the token, merges the filled fields into the partial entity, writes to vault + index, and returns the now-complete entity. Future agents asking for this entity get the merged version from cache (no plugin call, no AI call) — the AI work was paid once, served to many.

**Why agent-as-AI, not server-as-AI:**
- yaad-index has zero AI dependencies. No model selection, no key management, no prompt-template versioning at the server level.
- The agent already has its AI client. The agent picks the model. The agent owns the cost.
- "AI-first" properly: agents ARE AI; the server is the index they share.
- Different agents can fill the same gap differently (better or worse). The server stores what comes back; provenance entries on the entity carry which agent's fill produced which fields.

**Cache flow with the fill loop:**

```
1. agent → POST /v1/ingest {url}
2. server checks cache:
 - cache hit + fresh → return inline (200, entity)
 - else continue
3. server forks plugin → reads response
4. server merges plugin output into a partial entity
5. if no gaps → store + return inline (200, entity)
6. if gaps → store partial + provenance, return needs-fill (202, partial + raw + gaps + token)
7. agent runs AI, POSTs /v1/entities/<id>/fill {token, fields}
8. server merges, stores complete, returns final entity
```

`force_refetch=true` from ADR-0002 forces step 3 (re-runs the plugin) even on cache hit.

**Token semantics:**
- One-shot, time-limited (lean: 5 min TTL, configurable).
- Bound to the specific (entity_id, fill_session) — can't use it to write to another entity.
- Reused token returns 410 Gone after first use or expiration.

**On token expiration:** the agent re-calls `POST /v1/ingest` with the same URL and `force_refetch=true` (or just the URL — idempotent ingest will reuse the cached partial entity if not yet expired by TTL, returning a fresh fill_token without re-running the plugin). The `force_refetch=true` path is the unambiguous recovery: re-runs the plugin, generates a new partial + new token. No new endpoint needed for "lost token" — re-ingest is the recovery.

The full endpoint shape lives in the ADR-0002 amendment. This ADR pins the *flow*; that ADR pins the *surface*.

### Subprocess timeouts

Two distinct timeouts:

- **`--init` timeout: 5 seconds.** Plugins must report capabilities fast or be considered broken. A hung `--init` would otherwise stall server startup; 5s is generous for a JSON-write-and-exit. On timeout the server kills the process (SIGTERM, then SIGKILL after 1s grace), logs the failure, and excludes the plugin from the registry.
- **Plugin ingest-request timeout: `wait_seconds + 10s` of grace, capped at 310s.** The agent's request specifies `wait_seconds` (ADR-0002, default 60s, max 300s). Since the plugin is a fetcher only (no AI), this timeout is bound by network fetch time + parsing — typically << 60s. The 10s tail covers writes happening after the server already responded async. If the plugin still hasn't completed past `wait_seconds + 10s`, the server kills it (TERM-then-KILL) and writes a failure provenance entry against the placeholder entity (per ADR-0002's always-write-placeholder invariant). The agent gets a 504 if it was still waiting; otherwise the failure is recorded and the next entity-poll surfaces it.

 Note: AI extraction time isn't part of the plugin timeout — that work happens on the agent's side, after the plugin has already completed and the server has already returned the needs-fill response. The agent can take as long as it needs; the `fill_token` TTL (5 min default) is the only time bound on that side.

Both kill paths log the plugin name + the request that was in flight. No silent zombies.

## Consequences

### Positive

- **No always-on services.** Memory is held only during a request. A laptop running yaad-index doesn't accrue 5 idle daemons.
- **No port management.** No socket files, no per-plugin listen addresses, no port collisions on shared dev machines.
- **Trivial plugin authoring.** A plugin is a binary that reads JSON from stdin and writes JSON to stdout, with a `--init` mode. Any language. Drop in PATH, refresh, done.
- **Plugin self-describes.** The `--init` output is the single source of truth for what the plugin does. No manifest drift.
- **Updates are atomic.** Replace the binary, refresh the registry. No process to coordinate restart against.

### Negative / costs

- **Subprocess cold-start cost per request.** Realistic for Go binaries: ~5–20ms per fork+exec on Linux. Acceptable for a tool whose other costs (HTTP fetch, AI extraction) are 100ms–60s. If a plugin grows hot enough that fork cost shows up in profiles, promote it to long-lived stdio (same protocol).
- **Server runs plugin with its own privileges.** v1 personal-tool: fine. A future ADR will add isolation (per-plugin user, namespaces, container) if/when yaad-index runs in a less-trusted context. Out of scope here.
- **Cross-plugin orchestration is manual.** If plugin A needs an entity that plugin B produces, the server has to be the conductor (call B, then A). The protocol doesn't let plugins call other plugins directly. Acceptable: it forces the server to remain the only state-mutator, which keeps reasoning simple.
- **Conflict resolution by PATH order is implicit.** Operators who haven't thought about which `yaad-bgg` is on PATH first can get surprising behavior. The config override is the explicit-path escape hatch.

## Alternatives considered

- **HTTP plugins (long-running services).** Rejected (see Invocation): operational weight not worth uniformity at v1 scale.
- **In-process Go plugins.** Rejected: forces all plugins to be Go, can't be replaced without server rebuild, plugin crashes take the server down.
- **Manifest files** (e.g., `~/.yaad/plugins/bgg.toml`). Rejected: parallel source of truth that drifts. `--init` is more honest.
- **MCP-style stdio with a long-running subprocess.** A plausible later promotion target, not a v1 starting point. Adds plugin lifecycle (ready/idle/exit) that the simpler one-shot model doesn't need.
- **Server-owned AI extractor** (server runs OpenAI/Claude calls itself). Rejected: yaad-index becomes responsible for model choice, key management, prompt versioning, cost accounting — all of which the calling agent already handles for itself. The agent-as-AI model keeps yaad-index as a pure index/cache layer.
- **Plugin-owned AI** (each plugin runs its own AI calls). Rejected: every plugin reimplements AI client integration; key management leaks across N processes.
- **Namespaced kinds per plugin** (`bgg/person`, `books/person`). Rejected: forks the same person across rows, breaks the cross-source merge story. Unified kind + per-plugin provenance is cleaner.

## Open questions

None at this time. (The `--init` timeout is pinned at 5s under "Subprocess timeouts" above; discovery scope is the full `$PATH` per "Discovery" above.)

## Action items if approved

1. Define the `--init` JSON schema in `internal/plugin/init.go` (or equivalent).
2. Define the request/response JSON schemas (`internal/plugin/protocol.go`): one operation in v1 (`ingest`).
3. Implement the discovery scan + registry build at server startup.
4. Implement the dispatch path: kind/URL → plugin lookup → fork+exec with stdin/stdout JSON.
5. Implement the cache-first ingest flow (steps 1–5 in "Cache & freshness") in the `POST /v1/ingest` handler.
6. Author a stub `yaad-bgg` that satisfies `--init` + a single ingest request, end-to-end test the full flow.
