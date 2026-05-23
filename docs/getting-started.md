# Getting started

A walkthrough for a new operator coming to yaad-index fresh. Goes from `git clone` to a running daemon, a first ingest, and a first agent connection — about an hour of work end-to-end.

The deep-dive reference docs ([ingest.md](ingest.md), [fill-gap.md](fill-gap.md), [configs.md](configs.md), [workflows.md](workflows.md), [tasks.md](tasks.md), [`mcp/SKILL.md`](../mcp/SKILL.md)) assume the daemon is already running. This doc gets you there.

## 1. Prereqs + clone + build

You need Go 1.22+ on `$PATH`. SQLite is bundled via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO).

```bash
git clone git@github.com:yaad-index/yaad-index.git
cd yaad-index
make build           # → ./yaad-index
```

`make build` produces the daemon binary. The bundled plugin sources live alongside `cmd/yaad-index/` in this monorepo (`cmd/yaad-wikipedia/`, `cmd/yaad-bgg/`, `cmd/yaad-gmail/`, `cmd/yaad-github/`); build any plugin you need with:

```bash
go build -o ./yaad-wikipedia ./cmd/yaad-wikipedia
```

Confirm the daemon binary works:

```bash
./yaad-index --help
```

You should see the subcommand list (`serve`, `issue-token`, `keygen`, `command`, `fetch`, `reindex`, `plugins`, etc.).

## 2. Mint an operator token

The daemon's `/v1/...` routes are gated by JWT. The `auth.required=false` dev-mode bypass exists (set `YAAD_INDEX_AUTH_REQUIRED=false` or `auth.required: false` in the config), but the production path is operator-issued tokens.

Generate the keypair under `./keys/`:

```bash
./yaad-index keygen --keys-dir ./keys
```

Mint an operator-only token (Subject == Operator, full access to CLI dispatch + command-shape ingest):

```bash
./yaad-index issue-token --operator $USER --operator-only --keys-dir ./keys > op.jwt
```

The token lands in `op.jwt`. Default TTL is 90 days; pass `--ttl 24h` for a shorter window.

## 3. Run the daemon

The minimal config doesn't even need a YAML file — defaults work for a first run. Start the daemon with the keypair you just generated:

```bash
./yaad-index serve --keys-dir ./keys --bind localhost:7433
```

You should see structured-JSON log lines on stderr ending with `msg="listening"`. The SQLite file lands at `~/.local/share/yaad-index/yaad.db` by default; override with `--db-path` or `YAAD_INDEX_DB_PATH`. The vault directory defaults to `./vault/` — create it (`mkdir vault`) if it isn't there yet.

Sanity-probe the daemon:

```bash
curl -H "Authorization: Bearer $(cat op.jwt)" http://localhost:7433/v1/kinds
```

A JSON response listing canonical kinds (`boardgame`, `person`, ...) means the daemon is up + your token works.

## 4. First ingest

The wikipedia plugin (`yaad-wikipedia`) is the lowest-friction first ingest: no API key, no auth dance, just hit a Wikipedia URL. Build it from `cmd/yaad-wikipedia/` (step 1 above) and add it to your config:

```yaml
# yaad-index.yaml
plugins:
  - name: wikipedia
    path: ./yaad-wikipedia    # or an absolute path
```

Restart `serve` with `--config yaad-index.yaml`. Then ingest a URL:

```bash
curl -X POST http://localhost:7433/v1/ingest \
     -H "Authorization: Bearer $(cat op.jwt)" \
     -H 'Content-Type: application/json' \
     -d '{"url": "https://en.wikipedia.org/wiki/Tehran"}'
```

The response carries `{"ok": true, "entity_id": "wikipedia:tehran", ...}`. Fetch the entity:

```bash
curl -H "Authorization: Bearer $(cat op.jwt)" \
     http://localhost:7433/v1/entities/wikipedia:tehran
```

You should see the full entity shape — kind, data, provenance, edges, plus the vault-merged body (clean_content, summary, tags) once the agent fill cycle lands. For first ingest, the body fields will be empty until an agent fills the gaps (next step).

## 5. Vault + Obsidian

Open the vault directory in [Obsidian](https://obsidian.md/) (point Obsidian at `./vault/` as a new vault). You should see `wikipedia/tehran.md` — the entity's source-of-truth file. The frontmatter carries the canonical fields (`kind`, `data`, `notations`, `provenance`); the body has `## Notes` and `## Edges` sections that mirror the same data in human-readable form.

Hand-edits to `wikipedia/tehran.md` survive reindex (see [`docs/index-flow.md`](index-flow.md)). Obsidian renders wikilinks (`[[person:martin-wallace]]`) and lets you walk the canonical graph by clicking through.

## 6. First workflow

Workflows fire on entity events and trigger actions (write notes, dispatch plugins, file tasks). The full vocabulary is in [`docs/workflows.md`](workflows.md); the smallest possible workflow writes a note when an entity is created.

Create `vault/workflows/welcome.md`:

```yaml
---
name: welcome
match:
  event: entity.created
  kind: wikipedia
actions:
  - add_note:
      content: "welcome — first ingest landed via the getting-started walkthrough"
---
```

Restart `serve` so the workflow loader picks up the file. Next time you ingest a wikipedia entity, the workflow fires and appends a note. Check with:

```bash
curl -H "Authorization: Bearer $(cat op.jwt)" \
     "http://localhost:7433/v1/entities/wikipedia:<some-other-page>" \
  | jq .notes
```

## 7. First agent connection

The daemon's MCP surface lives at `/mcp` over Streamable HTTP. Mint a pair-claim token for the agent:

```bash
./yaad-index issue-token --operator $USER --agent claude-code --keys-dir ./keys > agent.jwt
```

Configure your agent (Claude Code, Claude Desktop, Cursor — anything that speaks Streamable-HTTP MCP). For Claude Code, add to your config:

```json
{
  "mcpServers": {
    "yaad-index": {
      "type": "http",
      "url": "http://localhost:7433/mcp",
      "headers": { "Authorization": "Bearer <paste-agent.jwt-here>" }
    }
  }
}
```

Restart the agent. The MCP tool surface (`get_entity`, `list_entities`, `ingest`, `add_note`, ...) becomes available; the agent can now fetch + write through the daemon.

Verify by asking the agent to call `get_entity(id="wikipedia:tehran")` — it should round-trip the entity you ingested in step 4.

## What to read next

- [`docs/ingest.md`](ingest.md) — the URL-shape + command-shape ingest contract, cache behavior, force-refetch, what each plugin's URL patterns accept.
- [`docs/fill-gap.md`](fill-gap.md) — the agent-fill loop: how `needs-fill` surfaces gap fields, how `POST /v1/entities/{id}/fill` writes them back to vault.
- [`docs/configs.md`](configs.md) — full operator-config reference: plugin allowlist, canonical_kinds, auth, cache TTLs, the YAML→subprocess env channel.
- [`docs/workflows.md`](workflows.md) — workflow event/action vocabulary: match shapes, action types, runtime semantics.
- [`docs/tasks.md`](tasks.md) — the agent task surface (the workflow `file_task` action lands here).
- [`docs/date-entities.md`](date-entities.md) — day entities + canonical edge vocab (`due_on` / `occurred_on` / `is_about_day` / `references_day`) + the `is_journal` filter for journal-shaped days.
- [`mcp/SKILL.md`](../mcp/SKILL.md) — the MCP tool reference an agent sees, with per-tool usage patterns.
- [`docs/plugin-flow.md`](plugin-flow.md) — for plugin authors: the `--init` capability shape, `--command` / `--version` flags, NDJSON response protocol.
- [`docs/plugins/`](plugins/) — per-plugin operator notes (wikipedia, bgg, gmail, github).
