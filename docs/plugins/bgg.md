# yaad-bgg

BoardGameGeek extractor plugin. Lives in this monorepo at `cmd/yaad-bgg/`; the daemon spawns it subprocess-per-request via the operator's `plugins:` allowlist (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md)). Matches `boardgamegeek.com/boardgame/<id>` URLs plus the `bgg: <id>` shorthand and fetches the boardgame record via the [`fzerorubigd/bggo`](https://github.com/fzerorubigd/bggo) client.

For the plugin protocol contract this implements, see [`docs/plugin-flow.md`](../plugin-flow.md).

## Build

The plugin is one of several binaries in the monorepo:

```sh
make build              # → bin/yaad-bgg alongside bin/yaad-index, etc.
```

Drop the resulting binary somewhere the daemon can read + execute it.

## Register with the daemon

```yaml
# ~/.config/yaad-index/config.yaml
plugins:
  - name: bgg
    path: /home/operator/.local/bin/yaad-bgg
```

The `name` field is what surfaces in provenance entries + `/v1/plugins`; the `path` MUST be absolute (per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — no `$PATH` search, no `~/` expansion). After editing the config, restart the daemon. On startup the daemon calls `yaad-bgg --version` (cheap probe), and on cache-miss or version-change calls `yaad-bgg --init` to refresh the capabilities row.

## Capabilities document (`yaad-bgg --init`)

```json
{
  "name": "bgg",
  "version": "<buildinfo.Version>",
  "url_patterns": [
    "^https?://(www\\.)?boardgamegeek\\.com/boardgame/[0-9]+(/.*)?$",
    "(?i)^bgg:\\s*(\\S.*)$"
  ],
  "entity_kinds": [
    { "name": "source", "default_ttl_days": 365 }
  ],
  "edge_kinds": [],
  "canonical_kinds_emitted": ["boardgame", "person", "company"],
  "canonical_edge_types_emitted": ["is_a", "is_about", "designed_by", "artist_by", "published_by"],
  "cache_ttl_seconds": 31536000,
  "source_namespace": "bgg",
  "canonical_kinds_extras": {
    "boardgame": {
      "gaps": {
        "rating":            { "type": "int",  "description": "How do you rate this on a 1-10 scale?", "range": [1, 10], "fill_strategy": "operator" },
        "owned":             { "type": "bool", "description": "Do you own this?",                       "fill_strategy": "operator" },
        "want":              { "type": "bool", "description": "Do you want this?",                       "fill_strategy": "operator" },
        "played":            { "type": "bool", "description": "Have you played this?",                  "fill_strategy": "operator" },
        "knows_how_to_play": { "type": "bool", "description": "Do you know how to play this?",          "fill_strategy": "operator" }
      }
    }
  }
}
```

Per [ADR-0021](../../adr/0021-daemon-owns-slug.md), `entity_kinds[].name` is the universal `"source"` value — every yaad-bgg envelope carries `structured.kind: "source"`. Source-type identity (`source-type:bgg-record`) materializes on the `is_a` edge the daemon resolves from the canonical-edge-types contract, not from a per-plugin kind name.

Per [ADR-0016](../../adr/0016-canonical-kind-defaults.md), the five operator-strategy gaps on `boardgame` are declared at the plugin layer (Layer 2 of the gap-merge chain). The agent-fill path won't attempt them (no `clean_content` signal); the operator-fill endpoint accepts them via `POST /v1/entities/{id}/operator-fill`. The operator can override any of these via `canonical_kinds.boardgame.gaps.<field>` in `yaad-index.yaml` — the operator layer wins.

The plugin-level `cache_ttl_seconds: 31536000` (365 days) participates in the three-level cache resolution (see [`docs/plugin-flow.md`](../plugin-flow.md) §4). BGG metadata cadence is essentially static once a game ships; the yearly default is the right hands-off contract.

## Ingest wire shape

Two input shapes match the registered URL patterns:

- **Canonical URL** — `https://boardgamegeek.com/boardgame/<id>[/<slug>]`. Search / forum / list URLs fall through.
- **Shorthand** — `bgg: <id>` (case-insensitive). Numeric ids resolve directly; name suffixes route through BGG search (a 1-vs-N split: 1 hit → fetch + emit, N hits → disambiguation envelope per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md)).

The plugin reads `{"operation": "ingest", "url": "..."}` from stdin and writes one JSON envelope on stdout (URL-shape, single source per call). Mutually-exclusive top-level shapes:

```json
{
  "ok": true,
  "structured": {
    "kind": "source",
    "name": "Sample Boardgame",
    "data": { "year": 2018, "min_players": 2, "max_players": 4, ... },
    "edges": {
      "is_a":         [ { "name": "bgg-record",      "kind": "source-type" } ],
      "is_about":     [ { "name": "Sample Boardgame", "kind": "boardgame" } ],
      "designed_by":  [ { "name": "A. Designer",     "kind": "person" } ],
      "artist_by":    [ { "name": "B. Artist",       "kind": "person" } ],
      "published_by": [ { "name": "Acme Games",      "kind": "company" } ]
    },
    "provenance": [ { "source": "bgg", "fetched_at": "2026-05-20T10:00:00Z", "ok": true } ]
  },
  "raw_content": "# Sample Boardgame\n\n![thumb](thumb.jpg)\n\n<description prose>\n",
  "notations": [
    "<input url verbatim>",
    "https://boardgamegeek.com/boardgame/99999999",
    "bgg: 99999999"
  ],
  "aliases": ["Sample Boardgame Alt-Name"],
  "attachments": [
    { "role": "thumb", "uri": "file:///<stage>/<random>.jpg", "extension": "jpg" }
  ]
}
```

Or, on disambiguation (name shorthand hits N candidates):

```json
{
  "ok": true,
  "options": {
    "99999999": { "label": "Sample Boardgame (2018)" },
    "99999998": { "label": "Sample Boardgame (2007)" }
  }
}
```

Field semantics per the slim plugin-author reference at [`docs/plugin-flow.md`](../plugin-flow.md) §2:

- **`structured.name`** — descriptive title; the daemon slugifies per [ADR-0021](../../adr/0021-daemon-owns-slug.md) to derive the entity id (`bgg:<slug>`).
- **`structured.data`** — metadata only (year, player count, weight, etc.); raw description prose lives in `raw_content`.
- **`structured.edges`** — keyed by edge type, value is a list of `{name, kind}` descriptive refs. The daemon resolves each to a canonical-label edge via `canonical.EnsureLabelRow`, gated by the operator's `canonical_kinds:` + `canonical_edge_types:` config.
- **`raw_content`** — markdown body (title H1, thumb image embed, description prose). The daemon wraps it between `<!-- yaad:plugin start/end -->` markers per [ADR-0015](../../adr/0015-plugin-body-markers.md).
- **`notations[]`** — every input form that resolves to this entity. The originating notation comes first (cache self-roundtrip invariant — see [`docs/plugin-flow.md`](../plugin-flow.md) §2 `notations`).
- **`attachments[]`** — the boardgame thumbnail per [ADR-0014](../../adr/0014-plugin-attachment-contract.md). `file://` URI points into the operator's attachments-staging directory; the daemon hardlinks into the vault then deletes the stage source.

## Configuration

`BGG_API_KEY` is the env var operators set to supply the BoardGameGeek API key. yaad-bgg spawns subprocess-per-request and inherits the daemon's environment, so the env var is the natural integration point. The plugin fails closed when the key is empty (no anonymous-BGG fallback).

### Optional per-game collection enrichment (#282)

When `BGG_USERNAME` + `BGG_PASSWORD` are both set, the plugin additionally calls `/xmlapi2/collection?id=<game>&stats=1&showprivate=1` after the public `/thing` fetch and merges the operator's per-game collection fields onto the canonical row:

| `data` field | Source |
|---|---|
| `operator_status` | `<status>` flags as a flat list (`["own"]`, `["own", "played"]`, etc.) |
| `operator_rating` | `<stats><rating value=...>` rounded to int 1-10; absent when unrated |
| `operator_num_plays` | `<numplays>` when > 0 |
| `operator_comment` | `<comment>` when non-empty |
| `operator_price_paid` + `operator_price_currency` | `<privateinfo>` `pricepaid` + `pp_currency` |
| `operator_acquisition_date` | `<privateinfo>` `acquisitiondate` (ISO date) |
| `operator_acquired_from` | `<privateinfo>` `acquiredfrom` |
| `operator_inventory_location` | `<privateinfo>` `inventorylocation` |
| `operator_private_comment` | `<privatecomment>` child element |

If either env var is missing, enrichment is silently off and the plugin behaves as the `/thing`-only legacy path. The credentials path also requires `YAAD_PLUGIN_DATA_DIR` (the daemon-managed per-instance dir from [#284](https://github.com/yaad-index/yaad-index/issues/284)) — the plugin persists a session-cookie jar at `<dataDir>/session.json` so subsequent subprocess invocations skip the Login round-trip. Without the data dir the cookie jar can't persist, so enrichment falls back off with a stderr WARN.

**Recommended operator setup** with [#256](https://github.com/yaad-index/yaad-index/issues/256) `${NAME}` expansion:

```yaml
# /etc/yaad-index/yaad-index.env (mode 0600)
BGG_API_KEY=ghp_xxx
BGG_USERNAME=operator-handle
BGG_PASSWORD=operator-secret

# config.yaml (mode 0644)
plugins:
  - name: bgg
    path: /opt/yaad/yaad-bgg
    instances:
      - name: default
        env:
          BGG_USERNAME: ${BGG_USERNAME}
          BGG_PASSWORD: ${BGG_PASSWORD}
```

**Failure modes:**

- Bad credentials at Login → stderr WARN, fall back to `/thing`-only result for this fetch and for the rest of the subprocess lifetime.
- Mid-session 401 (server-side session invalidation while the client-side cookie `Expires` is in the future) → silent re-login + retry once; one WARN log line. If the retry also fails, fall back to `/thing`-only for this fetch.
- Game not in operator's collection → `/thing` result lands unchanged, no `operator_*` fields.
- Network / transport error on `/xmlapi2/collection` → stderr WARN, fall back to `/thing`-only for this fetch.

## Development

From the monorepo root:

```sh
make help              # list targets
make check             # vet + build + test + fmt + lint + tidy
make build             # build all binaries including bin/yaad-bgg
go test ./cmd/yaad-bgg/... ./internal/bgg/...  # plugin + library tests
```

The plugin's library lives at `internal/bgg/`; the binary lives at `cmd/yaad-bgg/`. The split keeps the BGG client + parsing logic library-testable without subprocess wiring.

## References

- [`docs/plugin-flow.md`](../plugin-flow.md) — plugin-author-seat reference for the protocol contract.
- [`docs/ingest.md`](../ingest.md) — agent-facing view of what happens when a `POST /v1/ingest` call hits this plugin.
- [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — config allowlist + first-match-wins.
- [ADR-0008](../../adr/0008-vault-as-source-of-truth.md) — vault-canonical persistence + canonical kinds.
- [ADR-0013](../../adr/0013-canonical-kind-owns-gap-contract.md) — canonical-kind owns the gap contract.
- [ADR-0014](../../adr/0014-plugin-attachment-contract.md) — attachment delivery (thumbnail).
- [ADR-0015](../../adr/0015-plugin-body-markers.md) — body marker pairs.
- [ADR-0016](../../adr/0016-canonical-kind-defaults.md) — canonical-kind activation + 4-layer gap merge.
- [ADR-0021](../../adr/0021-daemon-owns-slug.md) — daemon owns slug derivation; plugins emit descriptive names.
- [ADR-0023](../../adr/0023-unified-plugin-response-protocol.md) — unified NDJSON wire (yaad-bgg is single-envelope URL-shape).
- [`fzerorubigd/bggo`](https://github.com/fzerorubigd/bggo) — BGG XMLAPI2 client library.
