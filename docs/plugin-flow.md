# Plugin contract

Plugin-author-seat reference for writing a yaad-index plugin: the subprocess invocation model, capabilities + envelope wire shapes, cache TTL contract, and cross-references to the ADRs that govern each piece.

Companion docs split the same surface by audience:

- [`docs/ingest.md`](./ingest.md) — agent-facing view (what a `POST /v1/ingest` caller sees, response envelopes, debug paths).
- [`docs/index-flow.md`](./index-flow.md) — index-internal flows (reindex, vault → DB derivation, atomic upsert invariants).
- [`docs/configs.md`](./configs.md) + [`AGENTS.md`](../AGENTS.md) — operator config (plugin allowlist, `cache_ttl_seconds`, auth).

This doc is a **living reference** (not an ADR). ADRs record decisions; this map describes how those decisions compose at runtime from the plugin author's seat. When an ADR or PR changes the plugin contract, the same change should update this doc.

## 1. Lifecycle

A plugin is a binary in the operator's `~/.config/yaad-index/config.yaml` `plugins:` allowlist (ADR-0006). The daemon spawns it as a subprocess once per invocation; the plugin reads its inputs, writes outputs, and exits.

Three invocation modes a plugin binary MUST support:

### `<binary> --version`

Cheap probe. Stdout is the version string (matching `Capabilities.Version` from `--init`). Exit 0 fast (≤ 2s). Run at every daemon startup as the cache-key probe. Plugins predating `--version` fall through to a full `--init` on every start (the cache short-circuit doesn't fire).

### `<binary> --init`

Full handshake. Stdout MUST be a valid JSON capabilities document (shape in §3). Exit 0. The daemon caches the JSON in the `plugin_capabilities` table keyed by `(name, version)`; subsequent startups skip `--init` when the cheap probe's version string matches the cached row. Operator force-refresh: `yaad-index plugins clear-cache [--name <plugin>]`.

### `<binary>` (fetch path)

The ingest path. Per ADR-0023 the wire shape is **NDJSON on stdout** — one JSON envelope per line, flushed after each line. Stdin carries the per-request `{"operation": "ingest", "url": "..."}` payload; the daemon's per-fetch timeout (default 60s — `subprocess.DefaultFetchTimeout`, operator-overridable per plugin via the `fetch_timeout` field on the config's `plugins:` entry) bounds the call.

Two envelope categories on the wire:

- **Source envelopes** — `{"ok": true, "structured": {...}, "raw_content": "...", "attachments": [...], "notations": [...], "aliases": [...]}`. One per emitted entity. URL-shape plugins emit exactly one; command-shape plugins (`yaad-gmail !fetch`) emit N. Wire-format detail in §2.
- **Control envelopes** — `{"_error": "<code>", "_error_message": "..."}` for per-envelope skips, `{"_summary": {...}}` for aggregate stats. See §5.

The daemon's `Plugin.Fetch` (single-envelope) wraps `Plugin.Stream` (N-envelope) for the URL-shape compatibility surface; both consume the same NDJSON wire.

**ADRs:** [ADR-0005](../adr/0005-plugin-lifecycle.md) (subprocess invocation), [ADR-0006](../adr/0006-plugin-discovery-config-allowlist.md) (config-allowlist + first-match-wins registration), [ADR-0023](../adr/0023-unified-plugin-response-protocol.md) (NDJSON wire).

## 2. Source envelope shape

The per-source NDJSON line:

```json
{
  "ok": true,
  "structured": {
    "kind": "source",
    "name": "Susanna Clarke",
    "data": { "lang": "en", "wikidata_q": "Q278391" },
    "edges": [
      { "type": "authored", "name": "Piranesi", "kind": "book" }
    ],
    "gaps": { "summary": "Two-sentence biography." }
  },
  "raw_content": "# Susanna Clarke\n\nAuthor of...",
  "raw_content_truncated": false,
  "notations": [
    "https://en.wikipedia.org/wiki/Susanna_Clarke",
    "wikipedia: Susanna Clarke"
  ],
  "aliases": [ "S. Clarke", "author: Piranesi" ],
  "attachments": [
    { "role": "thumb", "uri": "file:///stage/abc.jpg", "extension": "jpg" }
  ],
  "provenance": [
    { "source": "wikipedia", "fetched_at": "2026-05-20T10:00:00Z", "ok": true }
  ]
}
```

Field-by-field (mirrors the Go `FetchResult` struct in `internal/plugins/plugin.go` — the daemon decodes NDJSON line-by-line into this shape):

### `structured.kind`

Always `"source"` per ADR-0023. Source-type identity (e.g., `source-type:wikipedia-article-record`) is materialized by the daemon from the `is_a` edge, NOT typed into `kind` by the plugin. Plugins predating the unified protocol emitted plugin-specific kind names; the current contract is `"source"` uniformly.

### `structured.name`

Descriptive human-readable name. The daemon owns slug derivation per ADR-0021 (`canonical.SlugFromName` — NFC normalize, lowercase ASCII, hyphenate). **Plugins MUST NOT emit a pre-formed slug.** Per-plugin slug overrides are not part of the contract; if a plugin needs a specific slug it emits a descriptive name and accepts the daemon's deterministic derivation.

### `structured.data`

Metadata map (not the body). Per #125 the substance of source-shape entities — raw article body, email content, etc. — lives in `raw_content`, NOT under `data.body`. `data` carries structured-metadata fields: language, upstream IDs, dates, anything queryable. The daemon writes this verbatim to vault frontmatter `data:`.

**Reserved keys** — the daemon's vault → DB projection owns:

- `summary` — `vault.Entity.Summary` (filled by agent, not plugin).
- `tags` — `vault.Entity.Tags`.
- `comments_text` — `\n`-joined projection of note threads, FTS-only.

A plugin emitting these as plugin-extracted fields gets clobbered by the projection (`internal/api/fill.go::vaultEntityDataForDB`) on every re-ingest + fill cycle. Treat them as reserved.

### `structured.edges`

Typed relationships emitted alongside the source entity, as `{type, name, kind}` per ref. The daemon resolves each `(name, kind)` to a canonical-label edge target via `canonical.EnsureLabelRow` — auto-materializing the thin canonical-label row when absent, gated by the operator's `canonical_kinds:` + `canonical_edge_types:` config (ADR-0016). Plugins MUST declare `canonical_kinds_emitted` + `canonical_edge_types_emitted` in their `--init` capabilities so the operator sees the discoverability-gap warning at startup for any kind / type they haven't enabled.

### `structured.gaps`

`{field-name → AI-prompt-string}` per ADR-0002. The agent's AI reads the prompt and produces the value via `POST /v1/entities/{id}/fill`. Plugins emit the gap set the upstream couldn't fill; the agent closes them. Empty / absent → `complete` state (no gaps to fill).

### `raw_content`

The article body / email content / source substance, as Markdown. The daemon wraps it between `<!-- yaad:plugin start -->` / `<!-- yaad:plugin end -->` markers in the vault file body per ADR-0015. **Plugins MUST NOT emit the literal marker substrings themselves** — `vault.ErrPluginEmittedMarker` rejects the write.

`raw_content_truncated: true` signals the plugin trimmed the body (e.g., Wikipedia article body exceeds the plugin's character cap). The daemon surfaces this on the ingest response so callers know the entity body isn't complete.

### `notations`

Every input form the plugin knows resolves to this entity — full URL, mobile-subdomain URL, shorthand `<plugin>: <id>`, etc. The daemon writes these to `entity_notations` after a successful fetch; subsequent ingests of any equivalent form short-circuit the plugin spawn via the lookup-first cache (covered in [`docs/ingest.md`](./ingest.md) §2).

**Self-roundtrip invariant.** The ORIGINATING notation MUST appear first in the list — the orchestrator matches on exact-string equality, so the input form must be present at index 0 for the next call's lookup to register on the cache.

### `aliases`

Alternative-label list per ADR-0011 (incl. §"Generalization (#3)"). Two coexisting shapes:

- Bare strings (`"S. Clarke"`) — Obsidian wikilink targets.
- Typed prefixes (`"author: Piranesi"`) — agent reverse-lookup hints, `<edge-type>: <label>` shape.

Order doesn't matter; the daemon merges with ADR-0011's title-synthesized alias at vault-write time, dedupes, and mirrors the merged list into `entity_aliases` via `store.ReplaceAliases`. `alias_kind` is derived at write time from the operator's `canonical_edge_types:` registry — entries whose `<prefix>` is a registered edge type land as `typed`; the rest land as `bare`. `/v1/search` queries against this table so substring-matches on alias text surface the owning entity.

### `attachments`

Per-source binary blob list per ADR-0014. Each entry: `{role, uri, extension}`. `file://` URIs MUST point inside the operator's `attachments_staging_dir` (the daemon hardlinks the staged file into the vault then deletes the staging source on success). Re-fetch comparison is a pure string compare on `(role, uri)` against the freshest provenance — when the URI hasn't changed the daemon skips the re-fetch.

### `provenance`

One `ProvenanceEntry` stamped by the plugin: `{source, fetched_at, ok}`. If a plugin omits provenance, the daemon synthesizes a fallback entry naming the plugin so reindex always has at least one row. Plugins SHOULD emit it themselves so `fetched_at` reflects the upstream's wall-clock, not the orchestrator's.

## 3. Capabilities document (`--init` output)

```json
{
  "name": "wikipedia",
  "version": "1.2.3",
  "url_patterns": [
    "^https?://[a-z]+\\.wikipedia\\.org/wiki/.+",
    "(?i)^wikipedia:\\s*\\S+"
  ],
  "entity_kinds": [
    {
      "name": "wikipedia",
      "description": "Wikipedia article entity",
      "default_ttl_days": 7,
      "snippet_fields": ["extract", "description"]
    }
  ],
  "edge_kinds": [
    { "name": "is_about", "from_kind": "wikipedia", "to_kind": "person" }
  ],
  "canonical_kinds_emitted": ["person", "city", "country", "book", "boardgame"],
  "canonical_edge_types_emitted": ["is_about"],
  "resolves_canonical_kinds": ["person", "city", "country", "book", "boardgame"],
  "supports_search": true,
  "commands": [],
  "source_namespace": "wikipedia",
  "cache_ttl_seconds": 604800,
  "date_fields": {
    "due_date": "due_on",
    "occurred_at": "occurred_on"
  }
}
```

- **`name`** — plugin identifier. Operator's config `name:` MUST match.
- **`version`** — string the `--version` probe also returns. Bumping it invalidates the cached capabilities row.
- **`url_patterns`** — regexes the daemon's registry matches against. First-match-wins across all registered plugins (ADR-0006).
- **`entity_kinds[]`** — `{name, description, default_ttl_days?, snippet_fields?}`. The `name` is what `structured.kind` would name if the protocol allowed plugin-emitted kinds — today plugins emit `"source"`, but the entity-kinds list still drives the daemon's snippet-field lookup + drift-counter attribution.
- **`edge_kinds[]`** — `{name, from_kind, to_kind, description?}`. Declared edge types this plugin emits in `structured.edges`. The daemon's drift-counter (`/v1/cv-status`) attributes drops against this list.
- **`canonical_kinds_emitted[]`** / **`canonical_edge_types_emitted[]`** — names canonical-shape kinds / edge types this plugin MAY emit. Drives the startup warning when the operator's `canonical_kinds:` config doesn't enable a kind the plugin emits.
- **`resolves_canonical_kinds[]`** — subset of `canonical_kinds_emitted` for which this plugin can resolve a free-text name to a concrete canonical entity per #304 (e.g. "Brass" → `boardgame:brass-birmingham`). Plugins opt in when they have a name-search primitive (wikidata search, BGG search-by-name, gmail query). Empty / absent → plugin claims no resolver responsibility; auto-mode edges targeting kinds in `canonical_kinds_emitted` without a matching resolver claim fall through to legacy edge-write (no name-resolution attempt, no disambiguation task). Subset constraint enforced at config-load — declaring an entry for a kind not in `canonical_kinds_emitted` is fail-fast (typo gate).
- **`supports_search: true`** — opt-in to the `POST /v1/search/upstream` fan-out surface. Default false; explicit opt-in.
- **`commands[]`** — command-shape invocation names per ADR-0022 (e.g. `yaad-gmail` declares `["fetch"]`, invoked via `gmail: !fetch`).
- **`source_namespace`** — the `<source_namespace>` prefix on emitted entity ids (`<source_namespace>:<slug>`).
- **`cache_ttl_seconds`** — plugin-level TTL override. See §4.
- **`date_fields`** — per-ADR-0025 cut 2: map of frontmatter field name → canonical edge type. When the daemon's shape-scan finds a `day:YYYY-MM-DD` value in a declared field, the declared edge type (e.g. `due_on`, `occurred_on`, `is_about_day`, a plugin-custom name) is emitted INSTEAD OF the baseline `references_day`. Fields NOT in `date_fields` still flow through the shape-scan and get the baseline edge — plugin authors only opt in for fields where richer typing matters. Empty / absent → every `day:`-shaped frontmatter value gets the baseline. See [`docs/date-entities.md`](./date-entities.md) for the full vocabulary + the no-double-edge rule.

The per-fetch subprocess timeout is operator-side config (not plugin-declared) — see the `fetch_timeout` field on the operator's `plugins:` config entry in [`docs/configs.md`](./configs.md). Default `subprocess.DefaultFetchTimeout` (60s) applies when the operator hasn't set it.

### 3.1 Multi-instance capability surface (ADR-0028)

Plugins whose data shape supports N independent runtime contexts (per-account auth, per-PAT repo coverage, etc.) opt in via two additional `--init` fields:

```json
{
  "name": "github",
  "version": "0.4.0",
  "url_patterns": [
    "^https?://github\\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\\d+"
  ],
  "commands": ["fetch"],
  "supports_instances": true,
  "instance_routing": {
    "strategy": "glob_match",
    "config_field": "repos",
    "match_template": "{owner}/{repo}"
  }
}
```

- **`supports_instances: bool`** (default `false`) — explicit opt-in for the per-ADR-0028 multi-instance contract. Plugins that don't opt in keep working as single-instance under the implicit-`default` synthesis. The daemon fail-fasts at config load when an operator declares 2+ instances for a `supports_instances: false` plugin (the plugin's data shape doesn't support independent runtime contexts and the operator config would silently break).
- **`instance_routing`** (nullable) — required when `supports_instances: true` AND the plugin accepts URL-shape ingest. Carries the routing strategy the daemon uses to pick which instance handles a URL:
  - `strategy: "glob_match"` (v1 only) — extract named capture groups from the matched `url_patterns` regex, format `match_template`, glob-match the result against each enabled instance's `config[<config_field>]` list in declaration order; first-match wins. URLs with no matching glob reject with `400 {instance: "unrouted", url, message}` per ADR-0028 §3 fail-fast — no silent fallback.
  - `config_field` — the per-instance config key whose value list the daemon glob-matches against (e.g. `repos` for yaad-github).
  - `match_template` — formatted from the URL's named captures (e.g. `{owner}/{repo}` for yaad-github). Required placeholders that the URL pattern doesn't capture cause an error at routing time (plugin-author bug).

Plugins whose primary path is command-shape (e.g. yaad-gmail — instances dispatched only via `!fetch` fan-out) MAY leave `instance_routing` null; URL ingest then rejects for that plugin with a clear "plugin advertises no URL routing" error.

### 3.2 Command grammar extension (ADR-0028 §4)

ADR-0022's `<plugin>: !<command>` grammar gains an instance qualifier:

- **`<plugin>/<instance>: !<command>`** — instance-scoped invocation. Routes to the single named instance. Unknown instance name → `404 unknown_instance`. Disabled instance (per ADR-0028 §7 `enabled: false`) → `400 instance_disabled` (4xx not 5xx — operator config state, not transient outage; matches the §3 `unrouted_url` wire shape).
- **`<plugin>: !<command>`** (bare, no slash) — fans the command out **serially** across every enabled instance of the plugin in operator-config declaration order. Each instance's run completes (stream-to-end + exit per ADR-0023) before the next starts; logs are linear and instance-attributed. Per-instance errors are recorded in the aggregate response and do NOT abort the walk.

Aggregate response shape for bare-plugin fan-out across 2+ instances:

```json
{
  "ok": true,
  "state": "fan_out",
  "plugin": "gmail",
  "result": [
    {"instance": "personal", "state": "complete", "entity_id": "gmail:msg-abc"},
    {"instance": "work", "state": "failed", "error": "auth refused"}
  ]
}
```

Single-instance plugins (1 configured instance OR instance-scoped form against 1 instance) collapse to the regular single-attempt response shape — no `fan_out` wrapper. Back-compat for the common single-instance deployment.

## 4. Cache TTL — three-level resolution

Per-entity cache freshness resolves at fetch time across three input layers, narrowest-wins:

| Layer | Field | Effect |
|---|---|---|
| Per-fetch | `FetchResult.CacheTTLSeconds` (pointer; nil = no override) | Overrides plugin + operator levels for THIS entity. |
| Plugin-level | `Capabilities.CacheTTLSeconds` (per the `--init` document) | Default for every entity emitted by this plugin. |
| Operator-level | `cache_ttl_seconds` in `yaad-index.yaml` | Daemon-wide default. |

**Sentinel rules** (identical at every layer):

- `0` → no opinion (fall through to the next layer).
- positive integer → N seconds.
- negative integer → infinite (cache hits forever — the legacy pre-TTL behavior).

The resolver walks layers per-fetch in `internal/api/ingest_tracker.go::resolveCacheExpires`; the first non-zero value wins. With every layer at `0`, freshness is unbounded.

**Per-fetch override use-case.** A plugin that fetched a stale-by-design page (Wikipedia article that wasn't edited in 5 years) can opt the entity into a longer cache window than the plugin default; an event-stream plugin that pulls a calendar invite can opt into a tight window.

**ADRs:** none yet (the TTL surface is plugin-flow-doc-locked; an ADR would land if the contract grows).

## 5. Control envelopes

Per ADR-0023 the wire interleaves source envelopes with control packets:

- **`{"_error": "<code>", "_error_message": "..."}`** — per-envelope skip. The daemon logs it + continues consuming the stream. The plugin's process exit code stays 0; one bad envelope mid-stream doesn't fail the whole call.
- **`{"_summary": {...}}`** — aggregate stats. Plugin-defined fields; the daemon logs them at the end of the stream for the operator. Used by streaming plugins to report counts (envelopes-emitted, upstream-errors-suppressed, etc.).

## 6. Subprocess environment

The daemon extends the parent process environment with these yaad-index-specific variables before spawn:

| Variable | Set to | Plugin contract |
|---|---|---|
| `YAAD_TIMEZONE` | Operator's configured IANA timezone (e.g. `America/Los_Angeles`, `UTC` default). | Plugins SHOULD read this and apply when stamping `provenance.fetched_at` so timestamps stay consistent with the operator's expectation. Plugins predating this variable ignore it; their `time.Now().UTC()` output continues to work — UTC is comparable against operator-TZ values via the offset on each ISO-with-TZ string. |

Boilerplate for a Go plugin:

```go
loc := time.UTC
if tz := os.Getenv("YAAD_TIMEZONE"); tz != "" {
    if l, err := time.LoadLocation(tz); err == nil {
        loc = l
    }
}
now := time.Now().In(loc)
```

## 7. Authoring resources

- **Reference plugin:** `yaad-index/yaad-wikipedia` — full source. Reading its `--init` output and ingest path is the fastest onboarding path.
- **Test fixture:** `internal/plugins/fixture/` — in-memory `Plugin` impl used by the daemon's own tests. Mirrors the contract without a subprocess.
- **Subprocess driver:** `internal/plugins/subprocess/subprocess.go` — the daemon side. Reading `Plugin.Stream` clarifies exactly what the NDJSON consumer expects.

## 8. ADR map

| Surface | ADRs |
|---|---|
| Subprocess invocation | [ADR-0005](../adr/0005-plugin-lifecycle.md) |
| Discovery / first-match-wins | [ADR-0006](../adr/0006-plugin-discovery-config-allowlist.md) |
| Vault → DB derivation | [ADR-0008](../adr/0008-vault-as-source-of-truth.md) |
| Aliases (vault layer) | [ADR-0011](../adr/0011-vault-file-aliases-from-titles.md) |
| Canonical-kind contract | [ADR-0013](../adr/0013-canonical-kind-owns-gap-contract.md) |
| Attachments contract | [ADR-0014](../adr/0014-plugin-attachment-contract.md) |
| Plugin-body markers | [ADR-0015](../adr/0015-plugin-body-markers.md) |
| Canonical-kind activation + auto-materialize | [ADR-0016](../adr/0016-canonical-kind-defaults.md) |
| Canonical id (clean slug) | [ADR-0017](../adr/0017-canonical-id-clean-slug.md) |
| Daemon owns slug derivation | [ADR-0021](../adr/0021-daemon-owns-slug.md) |
| Command-shape plugins | [ADR-0022](../adr/0022-plugin-commands.md) |
| Unified NDJSON wire | [ADR-0023](../adr/0023-unified-plugin-response-protocol.md) |

## Maintenance discipline

This doc is a living plugin-author reference. When a PR changes the plugin contract — new capability, new envelope field, removed-or-renamed protocol surface — the same PR should update the relevant section here. Don't let it become a frozen snapshot. ADRs continue to record decisions; this is the orientation map plugin authors hit first.
