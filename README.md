# yaad-index

> ⚠️ **DESIGN IN FLUX — WE WILL BREAK THINGS.**
>
> This project is iterating on its plugin / cache / API surface and is **NOT stable**. Schemas, wire shapes, plugin contracts, and CLI flags may change without notice or migration path until a `stable` flag is set on a future release. Operators and downstream consumers should treat any version of these interfaces as ephemeral. Backward-compatibility shims you might expect (e.g., plugins-predating-legacy fallback, deprecated-but-supported config keys) may be removed at any time.

Personal knowledge index for AI agents. Caches structured and unstructured sources (web pages, emails, board games, code repos, calendars, …) into a queryable graph that agents read via HTTP — not files.

## Why "yaad"

**یاد** (*yaad*) is the Persian word for memory — specifically the remembering kind: recollection, the act of holding something in mind, the thing someone leaves behind when they're gone. It's a soft word. Old game pieces, their color worn off, that you keep because they were your mother's are a *yaad* of her. A photograph of your father, smiling, is a *yaad*. The vault this tool sits on top of, accumulating years of a person's thinking, is also a *yaad*.

The tool is named for what it does: it holds onto the things you've encountered — the games you played, the people you met, the articles you read, the emails that mattered — so you don't have to re-meet them every time you want to remember. It's memory as a thing you keep, not a thing you re-fetch.

Pronounced roughly like English *"yod"* (as in the Hebrew letter) but with a slightly longer vowel — *yaahd*. Short *a* would also be understood in most Persian dialects.

The name was chosen deliberately rather than descriptively: `yaad-index` is more honest than `knowledge-graph-tool-47` about what the thing actually is.

The tool is built for both humans and AIs to use. Its HTTP API exposes the same queries the CLI does — an AI agent and a person are equally first-class users of the knowledge graph. Not AI-as-bolt-on, not AI-as-main-act: equal access to the same surface.

It's also an experiment: this project is being built *with* an AI (Yaad, which the tool is named for), not just *for* one. The ADR review history in this repo shows that across design decisions — the AI is a contributor, not a tool.

## Status

Pre-alpha rewrite. An earlier prototype lived under a file-first design; this repo starts over from an **AI-first, remote-API** premise. See [ADR-0001](./adr/0001-fresh-rewrite-ai-first-remote-api.md) for the rewrite rationale.

## What it does (planned)

1. **Ingest.** A collector fetches a source — a board-game page, a blog post, an email — and parses it into entities and edges. Structured sources are parsed deterministically; unstructured sources are queued for AI extraction.
2. **Index.** Entities land in a per-vault SQLite store with stable canonical IDs. Markdown vaults remain authoritative for human-edited content; the index is the agent-facing view.
3. **Serve.** Everything reachable via HTTP. Local agents and remote agents call the same endpoints. No file-based fallback.
4. **Query.** Hits the local index — never the original sources. Cache-first by design.

## What it isn't

- **Not a live-state tool.** yaad-index holds knowledge that changes slowly. Sub-second freshness belongs in MCP servers fronting live systems.
- **Not a multi-tenant service in v1.** One vault per server instance.
- **Not a CLI-first tool.** The CLI is a thin client over the API.
- **Not authz-enforced in v1.** Network topology is the trust model. See [ADR-0001](./adr/0001-fresh-rewrite-ai-first-remote-api.md).

## Architecture sketch

```
agent (claude / cli / curl)
 │
 │ HTTP /v1/...
 ▼
┌──────────────────┐ ┌────────────────────┐
│ yaad-index │◀───────▶│ AI extractor │ (unstructured ingest)
│ server │ │ (per-deployment) │
└──────────────────┘ └────────────────────┘
 │
 ├─ collectors (per source: bgg, web, gmail, …)
 ├─ entity store (SQLite, per-vault)
 └─ markdown vault (authoritative for human edits)
```

Full design lives in [`adr/`](adr/). For the API surface itself, see [ADR-0002](./adr/0002-api-surface.md). For implementation-detail locks, see [ADR-0003](./adr/0003-cli-library-kong.md) (CLI library) and [ADR-0004](./adr/0004-logging-library-slog.md) (logging library).

## Quick start (when v1 ships)

```bash
# Install (once released)
go install github.com/yaad-index/yaad-index/cmd/yaad-index@latest

# Run the server
yaad-index serve

# From an agent or curl, ingest a source
curl -X POST http://localhost:7433/v1/ingest \
 -H 'Content-Type: application/json' \
 -d '{"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham"}'

# Fetch the resulting entity
curl http://localhost:7433/v1/entities/boardgame:brass-birmingham
```

## Repo layout

- `adr/` — Architecture Decision Records. Read these before making design changes.
- `cmd/yaad-index/` — server binary entry point.
- `internal/api/` — v1 HTTP handlers (`GET /v1/kinds` today; more endpoints land per [ADR-0002](./adr/0002-api-surface.md)).

## Building and running

```bash
make build # → ./yaad-index
./yaad-index serve --bind localhost:7433 # or set $YAAD_INDEX_BIND
curl http://localhost:7433/v1/kinds # returns the canonical kinds payload
```

`--db-path` (env `YAAD_INDEX_DB_PATH`, default `~/.local/share/yaad-index/yaad.db`) selects the SQLite file. The path's parent directory and the database itself are created on first run; schema migrations apply automatically. The server uses [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — pure Go, no CGO required.

The server logs structured JSON to stderr (per [ADR-0004](./adr/0004-logging-library-slog.md)) and shuts down gracefully on `SIGINT` / `SIGTERM`.

## Docker

A multi-stage `Dockerfile` at the repo root builds a runnable image with bundled plugin binaries (currently `yaad-wikipedia`).

```bash
make docker-build # → yaad-index:latest, tracks yaad-wikipedia main
make docker-build TAG=v1.2.3 # → yaad-index:v1.2.3
make docker-build YAAD_UID=$(id -u) YAAD_GID=$(id -g) # for non-1000 hosts (macOS etc)
make docker-build YAAD_WIKIPEDIA_REF=5407e26f24c2 # pin plugin to a specific SHA
```

`make docker-build` uses BuildKit (`DOCKER_BUILDKIT=1`) and forwards your ssh-agent (`--ssh default`) so the build can clone the private `yaad-wikipedia` plugin source. Have a running ssh-agent with your GitHub key loaded — the same setup you use to clone `yaad-index` itself. When the plugin repos go public, this requirement drops.

The container's `yaad` user defaults to **uid 1000 / gid 1000**, the conventional first-user uid on debian/ubuntu/most modern distros. This matches the typical `id -u` of the host operator running the build, so bind-mounted host files (`yaad-index.db`, `vault/`) are writable from inside the container without manual chmod. If your host uid differs (e.g. macOS, multi-user shared boxes), override at build time per the third example above.

The `yaad-wikipedia` plugin ref defaults to **`main`** so successive `make docker-build` runs pick up plugin updates as they land. The Makefile auto-injects a cachebust value (current timestamp) when `YAAD_WIKIPEDIA_REF=main`, so docker re-clones the plugin every build instead of reusing a stale layer. Pin to a SHA via `YAAD_WIKIPEDIA_REF=<sha>` for reproducible builds — the cachebust switches to a stable `pinned-<sha>` value and successive pinned-builds reuse the layer cache. Trade-off: tracking `main` defeats reproducibility; accepted while plugins iterate fast, revisit when the cadence settles.

The image bakes:

- `/usr/local/bin/yaad-index` — server binary
- `/usr/local/lib/yaad-index/plugins/yaad-wikipedia` — bundled plugin
- `/etc/yaad-index/config.yaml.example` — starter config

Three mount points; two are required, one is optional:

| Mount | Required | Behavior if not mounted |
|------------------------------------|----------|--------------------------------------------------------------------------------------|
| `/etc/yaad-index/config.yaml` | **yes** | Container fails fast on startup with a helpful error pointing at the example |
| `/data/vault` | **yes** | Container fails fast on startup; vault is the source of truth per ADR-0008 |
| `/data/index.db` | no | SQLite creates an ephemeral file at `/data/index.db`; lost on container restart |

The config file accepts a `cache_ttl_seconds` knob (default `0` = disabled, cache hits forever; positive values bound the lookup-first cache freshness window in seconds). See [`docs/plugin-flow.md`](docs/plugin-flow.md) §2b for the full TTL semantics.

### Setup

```bash
cp config.yaml.example yaad-index.yaml # edit if needed
touch yaad-index.db # create the file before first run
mkdir -p vault # if you don't already have one
```

The `touch` step matters: docker bind-mounts treat a missing host path as a directory and create one — if `yaad-index.db` doesn't exist on the host, you'll get a directory mount and the SQLite open will fail. The default `YAAD_UID=1000` matches most host operators' uid; the bind-mounted file is then writable without chmod. On non-1000 hosts, build with `YAAD_UID=$(id -u) YAAD_GID=$(id -g)` per the section above, OR `chmod a+rw yaad-index.db && chmod a+rwX vault/` if rebuilding isn't convenient.

### Run

```bash
docker run -d \
 -v ./yaad-index.yaml:/etc/yaad-index/config.yaml \
 -v ./vault:/data/vault \
 -v ./yaad-index.db:/data/index.db \
 -p 7433:7433 \
 yaad-index:latest
```

Copy `config.yaml.example` from this repo as your starting point — the baked example points at the bundled plugin path and `/data/vault`. Adding a new bundled plugin is a small Dockerfile addition: one parallel build stage cloning the plugin source, one COPY into the runtime image, one entry in the example config.

### Operator commands

After a plugin upgrade, if the plugin's Capabilities surface (URL patterns, entity kinds, frontmatter-edge mappings, etc.) changes but the plugin author forgot to bump `--version`, the daemon's startup cache will trust the stale row and never see the new fields. Recover with:

```bash
yaad-index plugins reprobe # force re-probe every plugin
yaad-index plugins reprobe --name bgg # or just one
```

This clears the cached `plugin_capabilities` row and re-runs `--init`. Restart the daemon afterwards so the running registry picks up the fresh capabilities. The companion `yaad-index plugins clear-cache` drops cached rows without re-probing — useful when you want the next server start to do the work.

## Development

```bash
git clone git@github.com:yaad-index/yaad-index.git
cd yaad-index
make check # full verification: vet, build, test, fmt, lint, go mod tidy
make install-hooks # (optional) wire the checks as a pre-commit block
```

`make check` runs the same chain CI runs. `make install-hooks` generates `.githooks/pre-commit` and points `git.core.hooksPath` at it, so commits that fail `make githook-check` (fmt-check + vet + lint + tidy-check) are rejected locally before they reach CI. See `make help` for every target.

## License

Apache 2.0. See [LICENSE](./LICENSE).
