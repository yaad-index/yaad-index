# yaad-wikipedia

> ⚠️ **DESIGN IN FLUX — WE WILL BREAK THINGS.**
>
> This plugin tracks yaad-index's iterating plugin / cache / API surface and is **NOT stable**. Wire shapes, the `--init` capabilities document, slug derivation, the Notations contract, and operator-facing flags may change without notice or migration path until a `stable` flag is set on a future yaad-index release. Treat any version of these interfaces as ephemeral. This plugin tracks the current contract; the contract may change. Backward-compatibility shims you might expect (e.g., omittable Notations field, deprecated-but-supported flags) may be removed at any time.

Wikipedia extractor plugin for [yaad-index](https://github.com/yaad-index/yaad-index).

`yaad-wikipedia` is a standalone CLI binary that implements the subprocess plugin protocol from yaad-index's [ADR-0006](https://github.com/yaad-index/yaad-index/blob/main/adr/0006-plugin-discovery-config-allowlist.md). It matches any URL on `*.wikipedia.org/wiki/...`, fetches the article summary via Wikipedia's REST API, and returns a structured entity yaad-index can persist.

## Build

```sh
go build -o yaad-wikipedia .
```

The binary is the only artifact; there are no shared libraries or runtime config files. Drop the binary somewhere yaad-index can read and execute it.

## Register with yaad-index

`yaad-wikipedia` is invoked subprocess-per-request — yaad-index spawns the binary, writes a JSON request to stdin, reads the JSON response from stdout. Discovery is via yaad-index's config allowlist (no PATH search; absolute paths only):

```yaml
# ~/.config/yaad-index/config.yaml
plugins:
 - name: wikipedia
 path: /home/operator/.local/bin/yaad-wikipedia
```

The `name` is what shows up in provenance entries and `/v1/kinds`; the `path` must be absolute (relative paths and `~/` shell expansion are rejected at config-load time per ADR-0006).

After editing the config, restart yaad-index. On startup yaad-index calls `yaad-wikipedia --init`, which prints the capabilities document (name, version, URL patterns, entity kinds) to stdout. From then on, every `/v1/ingest` call whose URL matches one of the patterns is routed to a fresh `yaad-wikipedia` subprocess.

## Capabilities document

`yaad-wikipedia --init` writes:

```json
{
 "name": "wikipedia",
 "version": "0.1.0",
 "url_patterns": [
 "^https?://[a-z]{2,3}(\\.m)?\\.wikipedia\\.org/wiki/.+",
 "(?i)^wikipedia:\\s*(\\S.*)$"
 ],
 "entity_kinds": [
 {"name": "wikipedia-article", "default_ttl_days": 7}
 ],
 "edge_kinds": [],
 "canonical_kinds_emitted": ["album", "anime", "artwork", "boardgame", "book", "business", "city", "comic", "country", "film-series", "movie", "organization", "person", "podcast", "school", "software", "tv-show", "video-game"],
 "canonical_edge_types_emitted": ["is_about", "is_a"]
}
```

Two input shapes are accepted:

- **Canonical URL**: `https://<lang>.wikipedia.org/wiki/<title>` (and the `m.` mobile variant). API and edit URLs fall through.
- **Shorthand**: `wikipedia: <topic>` (case-insensitive on the prefix). The plugin resolves the shorthand to a canonical URL using the configured language (default `en`, override via `--lang` / `YAAD_WIKIPEDIA_LANG`). Spaces in the topic become underscores per Wikipedia's URL convention; non-ASCII characters are percent-encoded; parens stay literal. Shorthand inputs go through the same URL fetch path as URL inputs — there is no pre-emptive search step (per / PR ); real ambiguity is signalled by Wikipedia's own `summary.type == "disambiguation"` response, which triggers the search-fallback that surfaces candidate Options.

Both shapes produce the same `data.url` for the same article — the canonical URL — so an entity reached via shorthand is structurally indistinguishable from one reached via URL.

`canonical_kinds_emitted` + `canonical_edge_types_emitted` declare the cross-source canonical kinds the plugin MAY emit alongside its source-shape Wikipedia article (see "What yaad-wikipedia emits per Fetch" below + the Wikidata Q-id pipeline section). yaad-index gates these by the operator's `canonical_kinds:` / `canonical_edge_types:` config — declaring them here makes the discoverability gap visible in startup logs when a plugin emits a kind the operator hasn't enabled.

## Fetch protocol

For every matching ingest request, yaad-index spawns the binary and writes a JSON request to stdin. The `url` field accepts either the full URL or the shorthand:

```json
{"operation": "ingest", "url": "https://en.wikipedia.org/wiki/Susanna_Clarke"}
```

```json
{"operation": "ingest", "url": "wikipedia: Susanna Clarke"}
```

`yaad-wikipedia` resolves the shorthand to a canonical URL (using `--lang` / `YAAD_WIKIPEDIA_LANG`), fetches the REST summary, optionally walks the article body via the action API, and (when the article carries a Wikidata Q-id matching one of the canonical kinds) resolves the Q-id to a canonical-kind stub. Errors (upstream 404, upstream 5xx, decode failures, context cancellation) write to stderr with the URL in the message; the binary exits non-zero. yaad-index's subprocess wrapper translates non-zero exit into a `fetch_failed` envelope on `/v1/ingest`.

The upstream HTTP fetch is capped at 5 seconds. The whole process invocation is capped at 10 seconds (the outer cap protects against stuck stdin).

## What yaad-wikipedia emits per Fetch

The JSON `yaad-wikipedia` writes to stdout maps onto yaad-index's `FetchResult` shape (`internal/plugins/plugin.go`). The example below corresponds to the URL-form request (`https://en.wikipedia.org/wiki/Susanna_Clarke`); the response shape is identical for the shorthand-form request, except the `notations` array is reordered so the originating input is first — see the note below the example. On a successful single-match fetch:

```json
{
 "ok": true,
 "structured": {
 "id": "wikipedia:susanna-clarke",
 "kind": "wikipedia-article",
 "data": {
 "title": "Susanna Clarke",
 "lang": "en",
 "url": "https://en.wikipedia.org/wiki/Susanna_Clarke"
 },
 "provenance": [
 {"source": "wikipedia:susanna-clarke", "fetched_at": "2026-05-03T09:00:00Z", "ok": true}
 ]
 },
 "raw_content": "Susanna Clarke is an English author…",
 "gaps": {
 "summary": "One-paragraph narrative summary of the article.",
 "tags": "Short label set distilled from the article's categories…",
 "birth_date": "Date of birth (YYYY or YYYY-MM-DD).",
 "birth_place": "City / region / country of birth.",
 "occupation": "One or two short labels (e.g. 'composer, conductor')."
 },
 "notations": [
 "https://en.wikipedia.org/wiki/Susanna_Clarke",
 "https://en.m.wikipedia.org/wiki/Susanna_Clarke",
 "wikipedia: Susanna Clarke"
 ],
 "canonical_entities": [
 {"id": "person:susanna-clarke", "kind": "person", "data": {"name": "Susanna Clarke"}}
 ],
 "canonical_edges": [
 {"type": "is_about", "from": "wikipedia:susanna-clarke", "to": "person:susanna-clarke"}
 ]
}
```

What each field carries:

- **`structured`** — the source-shape entity. The id is `wikipedia:<slug>` (slug derived from the URL-decoded article title); kind is always `wikipedia-article`. `data.url` is the canonical desktop URL regardless of input form.
- **`raw_content`** — the full article body in plaintext, fetched via the MediaWiki action API (`/w/api.php?action=query&prop=extracts&explaintext=1`). Load-bearing for the agent fill flow: the agent's AI reads this to derive the gap field values. A failed action API call leaves `raw_content` empty without failing the whole fetch — the source entity still lands.
- **`gaps`** — `{field-name → AI-prompt}` map. yaad-wikipedia declares `summary` + `tags` universally and merges in kind-specific gaps from `wikipedia.kindGaps(<kind>)` when a canonical kind is detected (see the Q-id pipeline below). The agent fills these via `POST /v1/entities/{id}/fill`.
- **`notations`** — every input form yaad-wikipedia knows resolves to this entity (per a prior PR). yaad-index writes these to its `entity_notations` lookup cache so subsequent ingests of any equivalent shape short-circuit the plugin spawn. Order: **originating notation first** (the input that triggered this fetch — self-roundtrip invariant per yaad-index a prior PR's lookup-first contract), then the remaining derived forms (canonical desktop URL, mobile-subdomain URL, `wikipedia: <human-title>` shorthand). The example above is for URL-form input, so the canonical desktop URL leads. For the shorthand-form request (`wikipedia: Susanna Clarke`), the same article would emit:

 ```json
 "notations": [
 "wikipedia: Susanna Clarke",
 "https://en.wikipedia.org/wiki/Susanna_Clarke",
 "https://en.m.wikipedia.org/wiki/Susanna_Clarke"
 ]
 ```

 Deduplicated; missing forms are dropped silently. The originating-first invariant is what makes a self-roundtrip register a hit on yaad-index's lookup-first cache: the cache matches on exact-string equality, so the input form must be present at index 0 for the same input to short-circuit on the next ingest.
- **`canonical_entities`** + **`canonical_edges`** — the cross-source canonical layer. When the article's Wikidata Q-id resolves to a known canonical kind, yaad-wikipedia emits the canonical stub (`<kind>:<slug>` with `data.name` from the article title) plus an `is_about` edge from the source-shape entity to the stub. Operator-config-gated by yaad-index's `canonical_kinds:` / `canonical_edge_types:` settings — kinds the operator hasn't enabled materialize as nothing.

Plus the disambiguation shape (mutually exclusive with `structured`):

```json
{
 "ok": true,
 "options": {
 "Martin Wallace (game designer)": {"label": "Martin Wallace (game designer)", "summary": "British board-game designer"},
 "Martin Wallace (American football)": {"label": "Martin Wallace (American football)", "summary": "American football quarterback"}
 }
}
```

`options` is populated when the article URL resolves to Wikipedia's own disambiguation page (`summary.type == "disambiguation"`); yaad-wikipedia falls back to a search across the article's title and surfaces the candidates as Options. The agent picks one option's key and re-invokes ingest with the `wikipedia: <key>` shorthand.

## Wikidata Q-id pipeline

The canonical-kind detection chain runs on every successful article fetch:

1. The Wikipedia summary endpoint returns `wikibase_item: <Q-id>` alongside the article body (when one is associated with the article).
2. `fetchKindByQID` calls Wikidata's EntityData API for that Q-id, decodes only the `claims["P31"]` ("instance of") entries, and matches the P31 values against the plugin's `kindByQID` lookup table. The plugin ships 18 Wikidata-domain mappings covering people, places, written/visual works, film, TV, music, podcasts, games, organizations, software, and schools; the **operator's `canonical_kinds:` config gates which of those actually materialize**. Adding interest in a new domain = enabling the kind in operator config; no plugin change needed.
3. On a match, the plugin emits a `<kind>:<slug>` canonical stub plus an `is_about` edge from the source-shape entity to the stub. Universally, every source emission carries an `is_a` edge to `source-type:wikipedia-article` (per ADR-0021).
4. Failures along the chain — empty `wikibase_item`, Wikidata API error, P31 doesn't match the table — are logged to stderr (per a prior PR) and the source-shape article still lands without a canonical layer.

### Currently mapped Q-id → canonical kind

The 18 mappings below are verified against Wikidata. The plugin emits an `is_about` edge whenever a P31 claim hits one of these Q-ids; the operator's `canonical_kinds:` config decides which actually materialize.

| Q-id | Wikidata label | Canonical kind |
|-----------|-------------------|----------------|
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

The plugin authors this mapping from Wikidata's P31 hierarchy. Per, the table is intentionally a superset / menu — operators turn on the kinds they care about via `canonical_kinds:`. New entries added to the table are an additive change; an operator who hasn't enabled the new kind sees no change in behavior.

The decoder is narrowly scoped to the P31 claim only — other Wikidata properties like P18 (image filename, bare string), P569 (birth-date, time-object), P625 (coordinates, globecoordinate object) carry foreign datavalue shapes that would crash a single-pass decode of the entire claims map. Per a prior PR (the decode fix that made the chain reliable), the outer claims map is parsed as `map[string]json.RawMessage` and only `P31` is strict-unmarshaled into the entity-id-shape struct.

## User-Agent

Wikimedia's [User-Agent policy](https://meta.wikimedia.org/wiki/User-Agent_policy) requires identifying the client. By default `yaad-wikipedia` sends:

```
yaad-wikipedia/0.1.0 (; contact: yaad-index@yaad-index.invalid)
```

Operators can override this two ways:

- **`--user-agent <string>`** CLI flag (highest precedence).
- **`YAAD_WIKIPEDIA_USER_AGENT`** environment variable. This is the natural integration point under yaad-index, whose config allowlist takes only a path — the index inherits its environment into spawned subprocesses, so setting the env var in your service unit (or shell) reaches the plugin without changes to yaad-index.

Either way, supply the full UA string yourself; the value is used verbatim.

## Shorthand language

The shorthand input form (`wikipedia: <topic>`) needs to know which Wikipedia to ask. By default it's English (`en`). Override:

- **`--lang <code>`** CLI flag.
- **`YAAD_WIKIPEDIA_LANG`** environment variable (same env-passthrough story as the User-Agent override).

Full URL inputs are unaffected — the URL's host already names the language. The lang setting only applies to shorthand.

## Development

```sh
make help # list targets
make check # vet + build + test (-race) + fmt-check + lint + tidy-check
make fmt # format in place (gofumpt + goimports + go mod tidy)
```

`make check` is what CI runs (see `.github/workflows/ci.yml`).

## Notes

- One-shot per request: there's no daemon, no warm cache, no connection pool inside `yaad-wikipedia`. yaad-index's `entity_notations` lookup cache (per yaad-index) handles repeat-fetch suppression — once an article is ingested, equivalent input forms (URL / shorthand / mobile-subdomain) hit the index without re-spawning this plugin until the operator's configured `cache_ttl_seconds` expires.
- Entity ids are `wikipedia:<slug>` without a language prefix, so `wikipedia:go-programming-language` would collide between English and another language sharing the slug. This is acceptable for v1's English-bias smoke; the prefix decision is being deferred to a future schema change.
- License: Apache 2.0. See `LICENSE`.

## References

- yaad-index `docs/plugin-flow.md` — canonical contract for the plugin protocol (subprocess invocation, FetchResult shape, Notations / canonical-axis emission expectations, lookup-first cache architecture).
- yaad-index [ADR-0006](https://github.com/yaad-index/yaad-index/blob/main/adr/0006-plugin-discovery-config-allowlist.md) — config allowlist + first-match-wins dispatch + disambiguation protocol.
- yaad-index [ADR-0008](https://github.com/yaad-index/yaad-index/blob/main/adr/0008-vault-as-source-of-truth.md) — vault-canonical persistence model + canonical kinds operator-config layer (governs how this plugin's canonical_entities / canonical_edges materialize).

Recent PRs in this repo:

- — search-then-fetch architecture for shorthand inputs (later refined by).
- — stderr instrumentation on the canonical-axis emission path (made silent failures visible in `docker logs`).
- — Wikidata decode fix: scoped to `claims["P31"]` only so foreign-shape properties (P18 string, P569 time-object, P625 coordinates) don't crash the decoder.
- — `Article.Notations` emission for yaad-index's lookup-first cache.
- — removed shorthand → search-first; canonical parens-form titles now flow straight through URL fetch.
