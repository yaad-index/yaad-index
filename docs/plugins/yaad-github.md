# yaad-github

GitHub extractor plugin. Lives in this monorepo at `cmd/yaad-github/`; the daemon spawns it subprocess-per-request via the operator's `plugins:` allowlist (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md)). **Hybrid URL + command shape** per [ADR-0026](../../adr/0026-yaad-github-plugin.md): single PR / issue URL ingest plus the bulk `github: !fetch` command that walks every configured repo.

For the plugin protocol contract this implements, see [`docs/plugin-flow.md`](../plugin-flow.md). For the agent-facing `/v1/ingest` view, see [`docs/ingest.md`](../ingest.md). For the closed-item lifecycle workflow pattern this plugin's `data.state` feeds, see [`docs/workflows.md`](../workflows.md) + [ADR-0024](../../adr/0024-workflows-and-tasks.md) `entity_updated` trigger.

## Build

```sh
make build-plugins              # → plugins/yaad-github alongside the other bundled plugins
```

Drop the binary somewhere the daemon can read + execute it. The container image (`make docker-build`) bundles `yaad-github` at `/usr/local/lib/yaad-index/plugins/yaad-github`.

## Register with the daemon

```yaml
# ~/.config/yaad-index/config.yaml
plugins:
  - name: github
    path: /home/operator/.local/bin/yaad-github
    env:
      YAAD_GITHUB_TOKEN: <personal access token>
      YAAD_GITHUB_REPOS: "acme-org/project-a,acme-org/project-b,someuser/dotfiles"
      YAAD_GITHUB_RECENT_DAYS: "7"           # optional; default 7

canonical_kinds:
  github-pr:
    description: "GitHub pull request"
  github-issue:
    description: "GitHub issue"
  repository:
    description: "Code repository (GitHub repo, etc.)"
  github-user:
    description: "GitHub user account"

canonical_edge_types:
  - in_repo
  - authored_by
  - involves
  - assigned_to
  - reviewed_by
```

The `name` field is what surfaces in provenance entries + the URL / command sigils (`github:owner/repo#42`, `github: !fetch`); the `path` MUST be absolute (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — no `$PATH` search, no `~/` expansion). After editing the config, restart the daemon — startup calls `yaad-github --version` then falls through to `--init` on cache-miss.

**Both `github-pr` and `github-issue` must be enabled in `canonical_kinds:`.** Forgetting one means PRs OR issues silently won't surface in the canonical layer. The `/v1/cv-status` endpoint surfaces this drift; run it after first sync to verify.

## Auth + secrets

| Env var | Required | Purpose |
|---|---|---|
| `YAAD_GITHUB_TOKEN` | yes | GitHub Personal Access Token. Scopes: `repo` (private repo read) + `read:org` (org membership visibility). |
| `YAAD_GITHUB_REPOS` | yes | Comma-separated `owner/repo` list — every repo the bulk sweep walks. Whitespace around entries trimmed. Empty or absent → startup fails fast. |
| `YAAD_GITHUB_RECENT_DAYS` | no | N-day rolling window for the closed-item sweep. Default `7`. Positive integers only (`1`, `30`, `365` etc.); zero / negative / non-integer values fail fast. |
| `YAAD_GITHUB_BASE_URL` | no | API base URL. Default `https://api.github.com`; override for GHES (e.g. `https://ghes.example.com/api/v3`). See [§7 multi-instance](#multi-instance-deployments). |
| `YAAD_GITHUB_INSTANCE_NAME` | no | The plugin's operator-side `name:` config — interpolates into the shorthand URL pattern + envelope notations for multi-instance setups. |

The token reaches the plugin subprocess via the operator's `plugins[].env:` block (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md)). Operators MUST NOT commit a real PAT inline — reference an out-of-tree env-file via the operator's secrets-manager wiring, or use a per-instance env file the daemon sources before spawning the plugin.

## Invocation

### URL / shorthand (single-item ingest)

Three input forms the daemon dispatches to this plugin:

```
https://github.com/owner/repo/pull/123     → fetches PR-123
https://github.com/owner/repo/issues/456   → fetches issue-456
github:owner/repo#123                      → shorthand resolves to canonical URL
```

Agent-side: `POST /v1/ingest` with the URL or shorthand as the `url` field. Standard daemon dispatch — same shape as wikipedia / bgg.

### `github: !fetch` (bulk command)

Operator-only-claim per [ADR-0022](../../adr/0022-plugin-command-protocol.md). Walks every repo in `YAAD_GITHUB_REPOS` and emits one envelope per matched item:

```sh
yaad-index command github fetch
```

Or via `POST /v1/ingest` with `url: "github: !fetch"`. Two per-repo searches run:

1. `is:open involves:<operator-login> repo:<owner>/<name>` — every currently-open PR / issue the operator is involved in.
2. `is:closed involves:<operator-login> repo:<owner>/<name> updated:>=<N-days-ago>` — every closed PR / issue with upstream activity in the `YAAD_GITHUB_RECENT_DAYS` window.

`involves:` covers author + assignee + mentioned + commenter + reviewer. The `<operator-login>` token is derived from the authenticated PAT's user (one `GET /user` call per invocation, cached for the process lifetime).

**Closure-window boundary.** Closed items older than `YAAD_GITHUB_RECENT_DAYS` with no subsequent upstream activity won't surface in the closed sweep — that's the intended cold-set per the workflow-owns-state design. A long-quiet closure is genuinely settled. Operators with a narrow window should know this trade-off; the default 7 days covers typical review cycles.

The bulk path streams one envelope per item via NDJSON per [ADR-0023](../../adr/0023-unified-plugin-response-protocol.md) and terminates with a `_summary` control packet.

## Canonical kinds + edges

Emitted in the canonical layer:

| Kind | Pattern | Notes |
|---|---|---|
| `github-pr` | `github-pr:<owner>_<repo>_pr_<num>` | Pull request. Carries `data.state` (`open`/`closed`) + `data.merged` (bool) for the lifecycle workflow path. |
| `github-issue` | `github-issue:<owner>_<repo>_issue_<num>` | Issue. `data.state` only (no merged flag). |
| `repository` | `repository:<owner>_<repo>` | Code repository. Materialized as a thin canonical-label row by `in_repo` edges. |
| `github-user` | `github-user:<login>` | Per-login user — author, assignee, reviewer, commenter, mentioned-in. Thin label rows. |

Edges emitted:

| Edge type | From → To | Notes |
|---|---|---|
| `is_a` | item → `source-type:github-record` | Universal source-shape per [ADR-0021](../../adr/0021-daemon-owns-slug.md). |
| `in_repo` | item → repository | Every PR / issue ↔ its parent repo. |
| `authored_by` | item → github-user | Item creator. |
| `involves` | item → github-user | Every login the `involves:` search surfaced. |
| `assigned_to` | item → github-user | Assignee(s); only emitted when set. |
| `reviewed_by` | item → github-user | PR reviewers (requested or completed); PR-only. |

Edge density: ~5-8 edges per item.

## Closed-item lifecycle — workflow-driven archive

The plugin emits truth (`structured.data.state`) and the operator configures workflows that wrap the archive lifecycle per [ADR-0024](../../adr/0024-workflows-and-tasks.md)'s `entity_updated` + `archive_entity` / `restore_entity` primitives. The plugin does NOT call `archive_entity` itself — archive policy lives entirely in operator-authored workflows.

**Three workflow files** for the v1 pattern (mirror pair + initial-closed-ingest sibling):

### 1. Archive on state-change to `closed`

```markdown
---
name: github-archive-on-state-change
version: 1
status: active
---

# Archive github items when upstream reports them closed

```yaml
allowed_plugins:
  - github

trigger:
  type: entity_updated
  match:
    field_changed: data.state
    canonical_kind: [github-pr, github-issue]

condition: 'entity.data.state == "closed"'

dedup:
  key: 'workflow + entity.id'
  policy: skip

actions:
  - archive_entity:
      target: '{{ entity.id }}'
      reason: 'github-state-closed'
\```
```

### 2. Restore on state-change to `open`

Mirror of (1): same trigger + match shape, `condition: 'entity.data.state == "open"'`, action calls `restore_entity` with `reason: 'github-state-reopened'`. Idempotent at the engine layer — re-firing on the same state is a no-op.

### 3. Archive on initial-closed-ingest

```markdown
---
name: github-archive-on-initial-closed
version: 1
status: active
---

# Archive github items first-ingested as closed

The closed-window sweep surfaces items the index never saw as
open. These fire `entity.created` with `data.state == "closed"`;
the state-change workflow doesn't catch them.

```yaml
allowed_plugins:
  - github

trigger:
  type: entity_created
  match:
    canonical_kind: [github-pr, github-issue]

condition: 'entity.data.state == "closed"'

dedup:
  key: 'workflow + entity.id'
  policy: skip

actions:
  - archive_entity:
      target: '{{ entity.id }}'
      reason: 'github-state-closed-initial'
\```
```

Default `/v1/search`, `/v1/list-entities`, etc. already skip archived rows; agents that want them back pass `include_archived=true` per the existing [ADR-0018](../../adr/0018-archive-replaces-delete.md) endpoint contract. Queries like "what did I merge last week" need the flag explicitly — the default active-set view shows open items only.

## Multi-instance deployments

Operators running multiple GitHub instances side-by-side (personal `github.com` + a GHES install, say) reuse the same binary with per-instance env vars:

```yaml
plugins:
  - name: github-personal
    path: /home/operator/.local/bin/yaad-github
    env:
      YAAD_GITHUB_INSTANCE_NAME: github-personal
      YAAD_GITHUB_BASE_URL: https://api.github.com
      YAAD_GITHUB_TOKEN: <personal PAT>
      YAAD_GITHUB_REPOS: "acme-org/project-a,someuser/dotfiles"
  - name: github-work
    path: /home/operator/.local/bin/yaad-github
    env:
      YAAD_GITHUB_INSTANCE_NAME: github-work
      YAAD_GITHUB_BASE_URL: https://ghes.example.com/api/v3
      YAAD_GITHUB_TOKEN: <work PAT>
      YAAD_GITHUB_REPOS: "team/service-a,team/service-b"
```

Instance separation is plugin-config-layer; the daemon's data layer is shared. Each instance handles inputs matching its `name:` prefix in the URL / command — `github-personal: !fetch` vs `github-work: !fetch`, shorthand `github-work: ghes-org/repo#42`, etc.

## Wire shape

Single-item URL ingest reads `{"operation": "ingest", "url": "..."}` from stdin and writes one source-shape envelope on stdout. Bulk-command (`--command fetch`) reads no stdin and streams N envelopes followed by a `_summary` packet.

```json
{
  "ok": true,
  "structured": {
    "kind": "source",
    "name": "acme_proj_pr_42",
    "data": {
      "number": 42,
      "type": "pr",
      "state": "open",
      "title": "Refactor the foo to use bar",
      "comment_count": 5,
      "last_comment_at": "2026-05-21T16:43:32Z",
      "merged": false
    },
    "edges": {
      "is_a":         [ { "name": "github-record", "kind": "source-type" } ],
      "in_repo":      [ { "name": "acme_proj",   "kind": "repository" } ],
      "authored_by":  [ { "name": "alice",       "kind": "github-user" } ],
      "involves":     [ { "name": "alice",       "kind": "github-user" }, { "name": "bob", "kind": "github-user" } ],
      "reviewed_by":  [ { "name": "bob",         "kind": "github-user" } ]
    },
    "provenance": [
      { "source": "github", "fetched_at": "2026-05-22T09:00:00Z", "ok": true }
    ]
  },
  "raw_content": "<PR body markdown verbatim>",
  "notations": [
    "https://github.com/acme/proj/pull/42",
    "github:acme/proj#42"
  ],
  "cache_ttl_seconds": 900
}
```

`data.state` is the lifecycle anchor §"Closed-item lifecycle" workflows read. The bulk path's `_summary` control packet at end-of-stream carries `repos`, `emitted`, `errors`, and `duration_ms` for the run.

## Development

From the monorepo root:

```sh
make help                  # list targets
make build-plugins         # → plugins/yaad-github (+ siblings)
go test -race ./cmd/yaad-github/... ./internal/github/...
```

The plugin's library lives at `internal/github/` (auth, fetch, search, envelope, repo-list / recent-days parsing); the binary lives at `cmd/yaad-github/`.

## Notes

- Bulk-fetch path constructs ONE shared `*github.Client` per invocation and reuses it across every search + per-item fetch — `http.Client.Timeout` (default 30s) enforces the per-request budget, the outer context enforces the 10min run budget.
- Items appearing in both the open and closed-recent search results (state flipped mid-sweep) emit only once — dedup is on `(owner, repo, kind, number)`.
- The closed-recent search uses GitHub Search's native `updated:>=<date>` operator — stateless, no last-sync cursor on the plugin or daemon side.

## References

- [`docs/plugin-flow.md`](../plugin-flow.md) — plugin-author-seat reference for the protocol contract.
- [`docs/ingest.md`](../ingest.md) — agent-facing `/v1/ingest` flow.
- [`docs/workflows.md`](../workflows.md) — workflow engine + the `entity_updated` trigger pattern the lifecycle workflows above use.
- [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — config allowlist + URL-pattern routing.
- [ADR-0018](../../adr/0018-archive-replaces-delete.md) — archive lifecycle the closed-item workflows wrap.
- [ADR-0021](../../adr/0021-daemon-owns-slug.md) — daemon owns slug derivation; universal `"source"` kind.
- [ADR-0022](../../adr/0022-plugin-command-protocol.md) — command-shape plugin protocol.
- [ADR-0023](../../adr/0023-unified-plugin-response-protocol.md) — NDJSON streaming + `_summary` / `_error` control packets.
- [ADR-0024](../../adr/0024-workflows-and-tasks.md) — workflow engine + `entity_updated` trigger + `archive_entity` / `restore_entity` actions.
- [ADR-0026](../../adr/0026-yaad-github-plugin.md) — full plugin design (hybrid invocation, multi-instance, split PR/issue namespaces).
