# ADR-0026: yaad-github plugin — hybrid URL+command invocation, multi-instance pattern, split PR/Issue namespaces

## Status

Proposed 2026-05-21.

## Depends on

- [ADR-0005](./0005-plugin-lifecycle.md) — plugin invocation model.
- [ADR-0006](./0006-plugin-discovery-config-allowlist.md) — config-allowlist + URL-pattern routing.
- [ADR-0008](./0008-vault-as-source-of-truth.md) — operator's `canonical_kinds:` + `canonical_edge_types:` config gates emission.
- [ADR-0021](./0021-daemon-owns-slug.md) — `kind: "source"` + daemon-derived `<source_namespace>:<slug.Slug(name)>` ID + `edges` block map-keyed-by-type.
- [ADR-0022](./0022-plugin-command-protocol.md) — `commands: [...]` + `<plugin>: !<command>` invocation sigil.
- [ADR-0023](./0023-unified-plugin-response-protocol.md) — NDJSON streaming response wire (one envelope per line).

## Context

Operators using yaad-index for daily work need first-class GitHub visibility. The motivating ask: surface every PR and issue the operator is involved in across configured repos as canonical entities in the graph, so "what am I dealing with right now" reads from the index instead of from a six-tab GitHub spree.

The plugin needs to do two things on different axes:

1. **Single-item ingest by URL or shorthand** — same shape as yaad-wikipedia. Agent says `ingest("https://github.com/owner/repo/pull/181")` or `ingest("github:owner/repo#181")`, plugin fetches that single PR/issue.
2. **Bulk fetch across all configured repos** — same shape as yaad-gmail. Operator says `github: !fetch`, plugin walks every configured repo, fetches everything the operator is involved in, emits one envelope per item.

Both shapes are needed. URL-shape handles "look up this specific thing"; command-shape handles "give me the state of my world."

A second axis is the deployment context. An operator may run two yaad-index instances side-by-side — for example one personal and one for a work setup — pointing at different GitHub deployments (`github.com` and a GHES install). The same plugin binary should serve both, with config supplying the differentiation: token, base URL, repo list. No two-binary maintenance burden, and no data crossover between instances.

A third axis is graph shape. PRs and issues are different enough that conflating them into one entity-kind muddies queries (`is this PR mergeable?` makes no sense as a question about an issue) but related enough that a deeply-split design with separate emission paths is over-engineering for v1.

The remaining design tensions — repo discovery, comments-as-entities, closed-item lifecycle — are settled below.

## Decision

### 1. Hybrid trigger: URL/shorthand inputs + `github: !fetch` command

Plugin advertises both `url_patterns` and `commands` in its `--init` capabilities:

```json
{
  "name": "github",
  "version": "<buildinfo.Version>",
  "url_patterns": [
    "^https?://github\\.com/[^/]+/[^/]+/pull/\\d+",
    "^https?://github\\.com/[^/]+/[^/]+/issues/\\d+",
    "(?i)^github:\\s*[^/]+/[^/]+#\\d+"
  ],
  "entity_kinds": [
    {"name": "source"}
  ],
  "edge_kinds": [],
  "canonical_kinds_emitted": ["github-pr", "github-issue", "repository", "github-user"],
  "canonical_edge_types_emitted": ["is_a", "in_repo", "authored_by", "involves", "assigned_to", "reviewed_by"],
  "source_namespace": "<see §2>",
  "commands": ["fetch"],
  "supports_search": false,
  "cache_ttl_seconds": 900
}
```

**URL-shape inputs (single-item):**
- `https://github.com/owner/repo/pull/123` → fetches PR-123
- `https://github.com/owner/repo/issues/456` → fetches issue-456
- `github:owner/repo#123` → shorthand, resolves to canonical URL

**Command-shape (bulk):**
- `github: !fetch` → walks every configured repo, emits per-item NDJSON
- `github: !fetch <owner/repo>` → single-repo full pass

Per ADR-0022, the bulk command requires an operator-only JWT claim. URL-shape inputs are agent-callable per the existing ADR-0005 fetch contract.

### 2. Split namespaces: `github-pr` + `github-issue`

PRs and issues are emitted as two separate canonical kinds, not one parameterized `github-item` kind.

**Reasoning:** Queries read cleaner. `list_entities(kind:"github-pr")` is directly expressive; `list_entities(kind:"github-item") + filter(data.type=="pr")` adds query overhead. The doubled config surface (operator opts into each kind separately in `canonical_kinds:`) is the price; the price is cheap.

**Source namespace choice.** Since a single plugin emits both PRs and issues, `source_namespace` in `--init` needs to be either:

- **Option A:** A single namespace (e.g. `github`) — the daemon then has no clean way to derive the canonical-kind from the namespace alone, so the plugin must put the kind into the entity's `name` or `data.type` and the daemon ingester must use that to decide which canonical-kind to emit.
- **Option B:** Per-emission namespace — plugin returns `source_namespace: "github-pr"` or `source_namespace: "github-issue"` PER ENVELOPE, overriding the `--init` advertised value. This requires a per-envelope `source_namespace` field in the wire shape, which ADR-0021 does NOT currently spec (`--init` is single-namespace per plugin).

**Decision: Option A** — single `source_namespace: "github"` advertised, plugin emits the discriminator via `structured.kind` semantically AND via canonical-kind emission in the `edges` block. The daemon's existing canonical-kinds gating handles the rest. If ADR-0021 ever grows per-envelope namespace support, the decision is worth revisiting.

Entity IDs: `github:<owner>_<repo>_pr_<num>` and `github:<owner>_<repo>_issue_<num>` (daemon-derived via `slug.Slug(name)`). The PR-vs-issue distinction lives in the slug.

### 3. Repo configuration — explicit list in plugin config

Operator declares the repo set explicitly:

```yaml
plugins:
  - name: github
    path: /home/operator/.local/bin/yaad-github
    config:
      repos:
        - acme-org/project-a
        - acme-org/project-b
        - someuser/dotfiles
```

No auto-discovery in v1. A future `auto_discover_orgs: [acme-org, someuser]` flag could discover-within-org boundaries without polluting from random OSS comments, but that's v2+.

Empty or absent `repos:` is a config error at daemon startup (per ADR-0006's strict-validation pattern).

### 4. `involves:` scope = full GitHub broad meaning

For each configured repo, the plugin's bulk-fetch query is GitHub's `is:open involves:<operator-login> repo:<owner>/<name>` plus a closed-since-last-sync sweep. `involves:` covers author + assignee + mentioned + commenter + reviewer. Matches a typical coordinator+reviewer involvement pattern; explicit narrower scopes are deferred to a future config knob if the surface gets too noisy.

The `<operator-login>` token is derived from the authenticated PAT's user (one `GET /user` call on first startup, cached in plugin state).

### 5. Comments — counts on parent, not entities

PR and issue entities carry comment metadata in `structured.data`:

```json
"data": {
  "number": 181,
  "type": "pr",
  "comment_count": 5,
  "comment_count_unread_since_last_sync": 2,
  "last_comment_at": "2026-05-20T16:43:32Z",
  ...
}
```

Comments are not emitted as separate `github-comment` entities. The graph stays small; the cost is no "all comments by a given user" or "comments mentioning a specific ADR" queries. Threshold-based comment-promotion (>200 chars OR contains code-block OR first-from-new-author) is a v2 design discussion.

### 6. Closed-item lifecycle — archive flag, not TTL

Closed PRs and issues remain in the graph indefinitely. A `data.archived: true` flag flips after 30 days of no state-change on a closed item. Default queries skip archived; agents can pass `include_archived: true` to surface them.

The preservation cost is near-zero (the entities don't go away, just get a flag). The query-surface cost is contained (one filter clause in the default query path).

### 7. Multi-instance pattern: same binary, configurable base URL

The plugin reads its API base URL from `YAAD_GITHUB_BASE_URL` (env), defaulting to `https://api.github.com`. Operators can register two plugin instances pointing the same binary at different GitHub deployments:

```yaml
plugins:
  - name: github-personal
    path: /home/operator/.local/bin/yaad-github
    env:
      YAAD_GITHUB_BASE_URL: https://api.github.com
      YAAD_GITHUB_TOKEN: <personal PAT>
    config:
      repos: [acme-org/project-a, someuser/dotfiles]
  - name: github-work
    path: /home/operator/.local/bin/yaad-github
    env:
      YAAD_GITHUB_BASE_URL: https://ghes.example.com/api/v3
      YAAD_GITHUB_TOKEN: <work PAT>
    config:
      repos: [team/service-a, team/service-b]
```

The instances are independent: separate tokens, separate URL bases, separate repo lists. Each instance handles inputs matching its `name:` prefix in the URL/command — `github-personal: !fetch` vs `github-work: !fetch`.

The plugin's URL-pattern matching needs a small adjustment: instead of hardcoding `github.com` in the patterns, the plugin's `--init` interpolates `YAAD_GITHUB_BASE_URL`'s host portion:

```python
host = urlparse(os.environ["YAAD_GITHUB_BASE_URL"]).hostname  # github.com OR ghes.example.com
url_patterns = [
    f"^https?://{re.escape(host)}/[^/]+/[^/]+/pull/\\d+",
    f"^https?://{re.escape(host)}/[^/]+/[^/]+/issues/\\d+",
    f"(?i)^{name}:\\s*[^/]+/[^/]+#\\d+",  # shorthand uses the instance name, not "github"
]
```

So `github-work: ghes-org/repo#42` shorthand resolves through the work instance, and `github-personal: acme-org/project-a#181` through the personal instance. The shorthand sigil prefix discriminates instances cleanly.

### 8. Auth + secrets

- **Env var name:** `YAAD_GITHUB_TOKEN` — distinct from the `gh` CLI's `GITHUB_TOKEN` to avoid scope confusion.
- **Delivery path:** the value reaches the plugin subprocess via the operator's `plugins[].env:` block (per [ADR-0006](./0006-plugin-discovery-config-allowlist.md)) — that is the single canonical mechanism. The `env:` example in §7 above shows the literal placeholder for illustration; operators MUST NOT commit a real PAT inline. Recommended pattern is to either reference an out-of-tree env-file (e.g. via a shell-expansion / secrets-manager indirection the operator wires up themselves, or via a deployment-time substitution) or to use a per-instance variant such as `~/.config/yaad-index/github.env` that the daemon sources before spawning the plugin. The plugin itself reads only the env vars passed in; how those vars get into the subprocess env is operator-discretionary.
- **Required scopes:** `repo` (private repo read) + `read:org` (org membership visibility). Never logged.

## Consequences

### Graph shape

After full sync, the canonical-kinds layer carries:

- `github-pr:<owner>_<repo>_pr_<num>` for each PR
- `github-issue:<owner>_<repo>_issue_<num>` for each issue
- `repository:<owner>_<repo>` for each repo touched
- `github-user:<login>` for each author / assignee / reviewer / commenter / mentioned-in

Edge density: ~5-8 edges per PR/issue entity (`is_a` + `in_repo` + `authored_by` + `involves` + 0-3 reviewer/assignee edges).

### Query expressivity

Agent prompts can ask:

- "what PRs am I currently reviewing?" → `list_entities(kind:"github-pr")` + filter on `edges.reviewed_by` containing the operator's GitHub login.
- "what's open against the <project> repo?" → `edges(entity_id:"repository:<owner>_<project>", direction:"in", edge_types:"in_repo")` filtered to PRs+issues in open state.
- "any PRs from a specific GitHub user this week?" → cross-edge from `github-user:<login>` via `authored_by`.

### Multi-instance separation

The personal and work graphs are entirely disjoint. The same plugin binary serves both; the configs differ; the data never crosses. Each instance's per-config `repos:` list, env-supplied token, and base URL keep the boundary at the plugin-invocation layer rather than at the daemon's data layer.

### Operator config burden

The split-namespace decision (§2) requires the operator to opt into BOTH `github-pr` AND `github-issue` in `canonical_kinds:`. Forgetting one means PRs OR issues silently won't surface. The `/v1/cv-status` endpoint (per ADR-0002) will surface this drift; the operator should run it after first sync to verify.

### Out of scope (v1)

- Webhooks / push updates (poll-only).
- Posting comments / replies (read-only API).
- `/notifications` API surface.
- Discussions.
- Actions / workflow runs.
- Comments-as-entities (threshold-based v2).
- Auto-discover (v2 `auto_discover_orgs:`).

These are deliberately deferred — the read-only-snapshot graph needs to prove useful before push-based or write surfaces get added.

## Alternatives considered

- **Single `github-item` kind** (rejected) — query muddying outweighs the doubled-config cost.
- **Option B per-envelope `source_namespace`** (deferred, not rejected) — would let the plugin advertise `github-pr` or `github-issue` on a per-envelope basis without needing the canonical-kind discriminator inside `structured.kind` or `data.type`. Cleaner separation at the source layer. Blocked today because ADR-0021's wire spec is single-namespace-per-plugin via `--init`. If ADR-0021 grows per-envelope namespace, this ADR's §2 choice is worth revisiting.
- **Auto-discover by default** (rejected for v1) — noisy from drive-by OSS comments. Deferred to v2 `auto_discover_orgs:` flag.
- **TTL-delete closed items** (rejected) — preservation cost is zero; archive-flag gives the same query-surface cleanliness without losing history.
- **Separate plugin binary per GitHub instance** (rejected) — maintenance burden. Multi-instance via base URL env reuses the same pattern ADR-0006 already supports (multiple plugin instances, each with its own config + env).

## Migration / backward compatibility

Greenfield. No prior github plugin to migrate from. The `repository` canonical kind doesn't currently exist in the system; this ADR introduces it. The 5 new edge types (`in_repo`, `authored_by`, `involves`, `assigned_to`, `reviewed_by`) are also new — operators must enable them in `canonical_edge_types:` config.
