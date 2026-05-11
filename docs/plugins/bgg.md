# yaad-bgg

> ⚠️ **DESIGN IN FLUX — WE WILL BREAK THINGS.**
>
> This plugin tracks yaad-index's iterating plugin / cache / API surface and is **NOT stable**. Wire shapes, the `--init` capabilities document, slug derivation, and operator-facing flags may change without notice or migration path until a `stable` flag is set on a future yaad-index release.

BoardGameGeek extractor plugin for [yaad-index](https://github.com/yaad-index/yaad-index).

`yaad-bgg` is a standalone CLI binary that implements the subprocess plugin protocol from yaad-index's [ADR-0006](https://github.com/yaad-index/yaad-index/blob/main/adr/0006-plugin-discovery-config-allowlist.md). It matches `boardgamegeek.com/boardgame/<id>` URLs (plus the `bgg: <id>` shorthand) and — once PR-B lands — fetches the boardgame record via the [`fzerorubigd/bggo`](https://github.com/fzerorubigd/bggo) client.

## Status

This is **PR-A: scaffold** of a two-PR series.

PR-A wires:

- The `--init` capabilities handshake (yaad-index's startup probe).
- The `--version` cache-key probe.
- The fetch dispatch shell (request validation + URL/shorthand matching). The actual BGG client integration + boardgame parsing return `not implemented` and land in **PR-B**.

PR-B will add: `bggo` dependency, real BGG fetch (list + get-by-id), boardgame frontmatter (`publisher`, `designed_by`, `artist_by`, `aliases`), thumbnail download alongside the entity file, and the online `BGG_API_KEY`-gated test.

## Build

```sh
go build -o yaad-bgg .
```

The binary is the only artifact; there are no shared libraries or runtime config files. Drop the binary somewhere yaad-index can read and execute it.

## Register with yaad-index

`yaad-bgg` is invoked subprocess-per-request — yaad-index spawns the binary, writes a JSON request to stdin, reads the JSON response from stdout. Discovery is via yaad-index's config allowlist (no PATH search; absolute paths only):

```yaml
# ~/.config/yaad-index/config.yaml
plugins:
 - name: bgg
 path: /home/operator/.local/bin/yaad-bgg
```

The `name` is what shows up in provenance entries and `/v1/kinds`; the `path` must be absolute (relative paths and `~/` shell expansion are rejected at config-load time per ADR-0006).

After editing the config, restart yaad-index. On startup yaad-index calls `yaad-bgg --init`, which prints the capabilities document to stdout. From PR-B onwards, every `/v1/ingest` call whose URL matches one of the patterns is routed to a fresh `yaad-bgg` subprocess.

## Capabilities document

`yaad-bgg --init` (PR-A) writes:

```json
{
 "name": "bgg",
 "version": "0.1.0",
 "url_patterns": [
 "^https?://(www\\.)?boardgamegeek\\.com/boardgame/[0-9]+(/.*)?$",
 "(?i)^bgg:\\s*(\\S.*)$"
 ],
 "entity_kinds": [
 {"name": "boardgame", "default_ttl_days": 365}
 ],
 "edge_kinds": [],
 "canonical_kinds_emitted": ["boardgame"],
 "cache_ttl_seconds": 31536000
}
```

Two input shapes are accepted:

- **Canonical URL**: `https://boardgamegeek.com/boardgame/<id>[/<slug>]`. Search / forum / list URLs fall through.
- **Shorthand**: `bgg: <id>` (case-insensitive on the prefix). PR-A accepts the captured suffix verbatim; PR-B will resolve numeric-id vs name-search.

The `cache_ttl_seconds: 31536000` (365 days) declaration participates in yaad-index's three-level cache resolution at the plugin level (per yaad-index the source issue + matching the contract from. BGG metadata cadence is essentially static once a game ships, so a yearly default is the right hands-off contract.

## Configuration

`BGG_API_KEY` is the env var operators set to supply the BoardGameGeek API key. PR-A reads it but does not consume the value; PR-B will require it non-empty for any fetch path (fail-closed; no fallthrough to anonymous BGG access). yaad-index spawns this binary subprocess-per-request and inherits its environment, so an env var is the natural integration point — the index's config allowlist takes a path only.

## Development

```sh
make help # list targets
make check # vet + build + test (-race) + fmt-check + lint + tidy-check
make fmt # format in place (gofumpt + goimports + go mod tidy)
```

`make check` is what CI runs (see `.github/workflows/ci.yml`).

## References

- yaad-index `docs/plugin-flow.md` — canonical contract for the plugin protocol.
- yaad-index [ADR-0006](https://github.com/yaad-index/yaad-index/blob/main/adr/0006-plugin-discovery-config-allowlist.md) — config allowlist + first-match-wins dispatch.
- yaad-index [ADR-0008](https://github.com/yaad-index/yaad-index/blob/main/adr/0008-vault-as-source-of-truth.md) — vault-canonical persistence + canonical kinds operator-config layer.
- yaad-index [ADR-0013](https://github.com/yaad-index/yaad-index/blob/main/adr/0013-cv-registry.md) — canonical-kind registry (CV).
- yaad-index issue — three-level cache TTL hierarchy this plugin participates in.
- [`fzerorubigd/bggo`](https://github.com/fzerorubigd/bggo) — BGG client lib (PR-B dep).
- License: Apache 2.0. See `LICENSE`.
