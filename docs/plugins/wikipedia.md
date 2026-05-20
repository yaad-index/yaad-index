# yaad-wikipedia

Wikipedia extractor plugin. Lives in this monorepo at `cmd/yaad-wikipedia/`; the daemon spawns it subprocess-per-request via the operator's `plugins:` allowlist (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md)). Matches any URL on `*.wikipedia.org/wiki/...` plus the `wikipedia: <topic>` shorthand, fetches the article summary via Wikipedia's REST API, and returns an ADR-0021 source-shape envelope the daemon persists.

For the plugin protocol contract this implements, see [`docs/plugin-flow.md`](../plugin-flow.md). For the agent-facing `/v1/ingest` view, see [`docs/ingest.md`](../ingest.md).

## Build

```sh
make build              # → bin/yaad-wikipedia alongside bin/yaad-index, etc.
```

Drop the binary somewhere the daemon can read + execute it.

## Register with the daemon

```yaml
# ~/.config/yaad-index/config.yaml
plugins:
  - name: wikipedia
    path: /home/operator/.local/bin/yaad-wikipedia
```

The `name` field is what surfaces in provenance entries + `/v1/plugins`; the `path` MUST be absolute (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — no `$PATH` search, no `~/` expansion). After editing the config, restart the daemon. On startup it calls `yaad-wikipedia --version` (cheap probe); on cache-miss / version-change it falls through to `yaad-wikipedia --init` to refresh the capabilities row (see [`docs/plugin-flow.md`](../plugin-flow.md) §1).

## Capabilities document (`yaad-wikipedia --init`)

```json
{
  "name": "wikipedia",
  "version": "<buildinfo.Version>",
  "url_patterns": [
    "^https?://[a-z]{2,3}(\\.m)?\\.wikipedia\\.org/wiki/.+",
    "(?i)^wikipedia:\\s*(\\S.*)$"
  ],
  "entity_kinds": [
    { "name": "source", "default_ttl_days": 7 }
  ],
  "edge_kinds": [],
  "canonical_kinds_emitted": [
    "album", "anime", "artwork", "boardgame", "book", "business", "city",
    "comic", "country", "film-series", "movie", "organization", "person",
    "podcast", "school", "software", "tv-show", "video-game"
  ],
  "canonical_edge_types_emitted": ["is_about", "is_a"],
  "supports_search": true,
  "source_namespace": "wikipedia-article",
  "cache_ttl_seconds": 31536000
}
```

Two input shapes match the registered URL patterns:

- **Canonical URL** — `https://<lang>.wikipedia.org/wiki/<title>` (and the `m.` mobile variant). API and edit URLs fall through.
- **Shorthand** — `wikipedia: <topic>` (case-insensitive prefix). The plugin resolves the shorthand to a canonical URL using the configured language (default `en`, override via `--lang` / `YAAD_WIKIPEDIA_LANG`). Spaces become underscores per Wikipedia's URL convention; non-ASCII is percent-encoded; parens stay literal. Real ambiguity is signalled by Wikipedia's own `summary.type == "disambiguation"` response, which triggers a search-fallback that surfaces candidate Options.

Both shapes produce the same `data.url` for the same article — the canonical URL — so an entity reached via shorthand is structurally indistinguishable from one reached via URL.

Per [ADR-0021](../../adr/0021-daemon-owns-slug.md) the `entity_kinds[].name` is the universal `"source"` value — every yaad-wikipedia envelope carries `structured.kind: "source"`. Source-type identity (`source-type:wikipedia-article`) materializes on the `is_a` edge.

`canonical_kinds_emitted` + `canonical_edge_types_emitted` declare the cross-source canonical layer the plugin MAY emit; the operator's `canonical_kinds:` / `canonical_edge_types:` config decides which actually materialize. Declaring them here surfaces the discoverability gap in startup logs when a plugin emits a kind the operator hasn't enabled (per [ADR-0008](../../adr/0008-vault-as-source-of-truth.md)).

`supports_search: true` opts the plugin into the `POST /v1/search/upstream` fan-out surface — the daemon's federated-search dispatcher invokes `yaad-wikipedia` with `operation: "search"` for any opted-in plugin. The plugin delegates to Wikipedia's action API `?list=search`.

The plugin-level `cache_ttl_seconds: 31536000` (365 days) participates in the three-level cache resolution (see [`docs/plugin-flow.md`](../plugin-flow.md) §4). Wikipedia article cadence is slow enough that a yearly default is a sensible hands-off contract.

## Wire shape

The plugin reads `{"operation": "ingest", "url": "..."}` from stdin and writes one JSON envelope on stdout (URL-shape, single source per call). Two mutually-exclusive top-level shapes — the `structured` envelope on a single-match fetch, or the `options` envelope on Wikipedia-side disambiguation.

### Single-match envelope

```json
{
  "ok": true,
  "structured": {
    "kind": "source",
    "name": "Sample Article Title",
    "data": {
      "title": "Sample Article Title",
      "lang": "en",
      "url": "https://en.wikipedia.org/wiki/Sample_Article_Title"
    },
    "edges": {
      "is_a":     [ { "name": "wikipedia-article", "kind": "source-type" } ],
      "is_about": [ { "name": "Sample Article Title",     "kind": "person" } ]
    },
    "provenance": [
      { "source": "wikipedia", "fetched_at": "2026-05-20T10:00:00Z", "ok": true }
    ]
  },
  "raw_content": "Sample Article Title is a fictional placeholder for an article body…",
  "gaps": {
    "summary":     "One-paragraph narrative summary of the article.",
    "tags":        "Short label set distilled from the article's categories…",
    "birth_date":  "Date of birth (YYYY or YYYY-MM-DD).",
    "birth_place": "City / region / country of birth.",
    "occupation":  "One or two short labels (e.g. 'composer, conductor')."
  },
  "notations": [
    "https://en.wikipedia.org/wiki/Sample_Article_Title",
    "https://en.m.wikipedia.org/wiki/Sample_Article_Title",
    "wikipedia: Sample Article Title"
  ],
  "aliases": ["Sample Article Title", "Sample Title (alt)"],
  "cache_ttl_seconds": 31536000
}
```

Field-by-field (per [`docs/plugin-flow.md`](../plugin-flow.md) §2):

- **`structured.kind`** — always `"source"` per [ADR-0021](../../adr/0021-daemon-owns-slug.md). The plugin does NOT emit a pre-formed entity id; the daemon derives it from `<source_namespace>:<slug.Slug(name)>` (`wikipedia-article:<slug>`).
- **`structured.name`** — descriptive article title; daemon slugifies.
- **`structured.data`** — metadata: `title`, `lang`, `url`. The article body lives in `raw_content`, NOT under `data.body` (per #125 source-shape body inversion).
- **`structured.edges`** — keyed by edge type; each value is a list of `{name, kind}` descriptive refs the daemon resolves to canonical-label edges via `canonical.EnsureLabelRow`. Universally emits `is_a` to `source-type:wikipedia-article` per [ADR-0021](../../adr/0021-daemon-owns-slug.md); on a Wikidata Q-id match (Q-id table below) emits `is_about` to the matched canonical-kind label.
- **`raw_content`** — full article body in plaintext, fetched via MediaWiki action API (`/w/api.php?action=query&prop=extracts&explaintext=1`). Load-bearing for the agent fill flow: the agent's AI reads this to derive the gap field values. A failed action-API call leaves `raw_content` empty without failing the whole fetch — the source entity still lands.
- **`gaps`** — `{field-name → AI-prompt}` map. yaad-wikipedia declares `summary` + `tags` universally and merges in kind-specific gaps from `wikipedia.kindGaps(<kind>)` when a canonical kind is detected (see Q-id pipeline below).
- **`notations`** — every input form yaad-wikipedia knows resolves to this entity. Originating notation first (self-roundtrip invariant — see [`docs/plugin-flow.md`](../plugin-flow.md) §2 `notations`); the example above is URL-form, so the canonical desktop URL leads. A shorthand-form request reorders the array so `"wikipedia: Sample Article Title"` is at index 0.
- **`aliases`** — alternative-label list (per [ADR-0011](../../adr/0011-vault-file-aliases-from-titles.md) + #3). Currently bare strings; typed-prefix shape `<edge-type>: <label>` is allowed but yaad-wikipedia doesn't emit any typed aliases today.
- **`cache_ttl_seconds`** — per-fetch override (pointer-shape on the wire; the plugin currently mirrors the plugin-level default but the per-fetch slot is reserved for specialization, e.g. shorter TTL for freshly-edited articles).

### Disambiguation envelope

```json
{
  "ok": true,
  "options": {
    "Sample Topic (occupation A)": { "label": "Sample Topic (occupation A)", "summary": "Description A" },
    "Sample Topic (occupation B)": { "label": "Sample Topic (occupation B)", "summary": "Description B" }
  }
}
```

`options` is populated when the article URL resolves to Wikipedia's own disambiguation page (`summary.type == "disambiguation"`); yaad-wikipedia falls back to a search across the article's title and surfaces the candidates as Options. The agent picks one key and re-invokes ingest with the `wikipedia: <key>` shorthand.

## Wikidata Q-id pipeline

The canonical-kind detection chain runs on every successful article fetch:

1. The Wikipedia summary endpoint returns `wikibase_item: <Q-id>` alongside the article body (when one is associated).
2. `fetchKindByQID` calls Wikidata's EntityData API for that Q-id, decodes only the `claims["P31"]` ("instance of") entries, and matches the P31 values against the plugin's `kindByQID` lookup table. The plugin ships 18 Wikidata-domain mappings; the **operator's `canonical_kinds:` config gates which of those actually materialize**. Adding interest in a new domain = enabling the kind in operator config; no plugin change needed.
3. On a match, the plugin emits an `is_about` edge from the source-shape entity to the canonical-label target. Universally, every source emission carries an `is_a` edge to `source-type:wikipedia-article` per [ADR-0021](../../adr/0021-daemon-owns-slug.md).
4. Failures along the chain — empty `wikibase_item`, Wikidata API error, P31 doesn't match the table — are logged to stderr and the source-shape article still lands without a canonical layer.

### Currently mapped Q-id → canonical kind

| Q-id | Wikidata label | Canonical kind |
|---|---|---|
| Q5 | human | `person` |
| Q515 | city | `city` |
| Q6256 | country | `country` |
| Q571 | book | `book` |
| Q1004 | comics | `comic` |
| Q11424 | film | `movie` |
| Q24856 | film series | `film-series` |
| Q5398426 | television series | `tv-show` |
| Q1107 | anime | `anime` |
| Q482994 | album | `album` |
| Q24634210 | podcast show | `podcast` |
| Q7889 | video game | `video-game` |
| Q131436 | board game | `boardgame` |
| Q838948 | work of art | `artwork` |
| Q43229 | organization | `organization` |
| Q4830453 | business | `business` |
| Q3914 | school | `school` |
| Q7397 | software | `software` |

The plugin authors this mapping from Wikidata's P31 hierarchy. The table is intentionally a superset / menu — operators turn on the kinds they care about via `canonical_kinds:`. New entries are an additive change; an operator who hasn't enabled the new kind sees no change in behavior.

The decoder is narrowly scoped to the P31 claim only — other Wikidata properties (P18 image filename, P569 birth-date time-object, P625 coordinates) carry foreign datavalue shapes that would crash a single-pass decode of the entire claims map. The outer claims map is parsed as `map[string]json.RawMessage` and only `P31` is strict-unmarshaled into the entity-id-shape struct.

## User-Agent

Wikimedia's [User-Agent policy](https://meta.wikimedia.org/wiki/User-Agent_policy) requires identifying the client. By default `yaad-wikipedia` sends:

```
yaad-wikipedia/<version> (; contact: yaad-index@yaad-index.invalid)
```

Override two ways:

- **`--user-agent <string>`** CLI flag (highest precedence).
- **`YAAD_WIKIPEDIA_USER_AGENT`** environment variable. The daemon inherits its environment into spawned subprocesses, so setting the env var in the service unit (or shell) reaches the plugin.

Supply the full UA string yourself; the value is used verbatim.

## Shorthand language

The shorthand input form (`wikipedia: <topic>`) needs to know which Wikipedia to ask. Default is English (`en`). Override:

- **`--lang <code>`** CLI flag.
- **`YAAD_WIKIPEDIA_LANG`** environment variable (same env-passthrough story as the User-Agent override).

Full URL inputs are unaffected — the URL's host already names the language.

Also honored from the daemon-injected environment (per [`docs/plugin-flow.md`](../plugin-flow.md) §6): `YAAD_TIMEZONE` — when set, used to stamp `provenance.fetched_at` so timestamps land in the operator's expected TZ.

## Upstream search

`POST /v1/search/upstream` fans out across plugins with `supports_search: true`. yaad-wikipedia's contribution: the action-API `?list=search` endpoint. The daemon invokes the plugin with `{"operation": "search", "query": "..."}`; the plugin returns a candidates list the daemon merges into the federated response.

## Development

From the monorepo root:

```sh
make help              # list targets
make check             # vet + build + test + fmt + lint + tidy
go test ./cmd/yaad-wikipedia/... ./internal/wikipedia/...
```

The plugin's library lives at `internal/wikipedia/` (REST + action-API clients, Wikidata Q-id mapping, slug derivation); the binary lives at `cmd/yaad-wikipedia/`.

## Notes

- One-shot per request: no daemon, no warm cache, no connection pool inside `yaad-wikipedia`. The daemon's `entity_notations` lookup cache handles repeat-fetch suppression — once an article is ingested, equivalent input forms (URL / shorthand / mobile-subdomain) hit the cache without re-spawning this plugin until the operator's `cache_ttl_seconds` expires.
- Entity ids are `wikipedia-article:<slug>` without a language prefix, so the same slug across two language Wikipedias would collide. Acceptable for v1's English-bias smoke; the prefix decision is deferred to a future schema change.

## References

- [`docs/plugin-flow.md`](../plugin-flow.md) — plugin-author-seat reference for the protocol contract.
- [`docs/ingest.md`](../ingest.md) — agent-facing `/v1/ingest` flow.
- [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — config allowlist + first-match-wins + disambiguation protocol.
- [ADR-0008](../../adr/0008-vault-as-source-of-truth.md) — vault-canonical persistence + canonical-kinds operator config.
- [ADR-0011](../../adr/0011-vault-file-aliases-from-titles.md) — vault aliases.
- [ADR-0013](../../adr/0013-canonical-kind-owns-gap-contract.md) — canonical-kind owns the gap contract.
- [ADR-0015](../../adr/0015-plugin-body-markers.md) — body marker pairs.
- [ADR-0016](../../adr/0016-canonical-kind-defaults.md) — canonical-kind activation + 4-layer gap merge.
- [ADR-0021](../../adr/0021-daemon-owns-slug.md) — daemon owns slug derivation; universal `"source"` kind.
- [ADR-0023](../../adr/0023-unified-plugin-response-protocol.md) — unified NDJSON wire (yaad-wikipedia is single-envelope URL-shape).
- Wikimedia [User-Agent policy](https://meta.wikimedia.org/wiki/User-Agent_policy).
