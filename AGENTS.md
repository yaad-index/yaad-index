# AGENTS.md

> ⚠️ **DESIGN IN FLUX — WE WILL BREAK THINGS.**
>
> This project is iterating on its plugin / cache / API surface and is **NOT stable**. Schemas, wire shapes, plugin contracts, and CLI flags may change without notice or migration path until a `stable` flag is set on a future release. Operators and downstream consumers should treat any version of these interfaces as ephemeral. Backward-compatibility shims you might expect (e.g., plugins-predating-legacy fallback, deprecated-but-supported config keys) may be removed at any time. When you see "backwards-compat" framing in a doc, treat it as descriptive of *today*, not durable.

Launch-pad for agents working on yaad-index. This file is a signpost
index, not a manual — each entry points at the canonical doc that
holds the detail.

If you're new to this repo: read [README.md](README.md) for the
human-facing project overview, then come back here for the agent-
oriented map of where conventions live.

## Architecture overview

**The markdown vault is the source of truth for entity state. The
SQLite database is a derived index, regenerable from the vault via
`yaad-index reindex`.** Every state mutation writes the vault first,
then mirrors the change to the DB; a vault-write failure aborts the
request and the DB stays untouched (no partial state).

The high-level data flow:

```
agent / operator
 │
 ▼
 POST /v1/ingest POST /v1/entities/{id}/fill
 POST /v1/entities/{id}/notes (or hand-edit a `*.md` file
 in the vault)
 │
 ▼
 internal/api ──── vault.Writer ────► <vault>/<kind>/<slug>.md ◄── source of truth
 │ (atomic temp+rename) │
 ▼ │
 store.Store │
 (entities / edges / provenance / │
 reindex_files bookkeeping) ◄── reindex regenerates
 │ (CLI: yaad-index reindex
 ▼ or HTTP: POST /v1/reindex)
 GET /v1/entities/{id}
 GET /v1/entities/{id}/context?depth=N (Per the prior design,; multi-hop stitch)
 GET /v1/needs-fill (Per the prior design,; batch gap-call queue)
 GET /v1/structure (Per the prior design,; CV registry + plugins + version)
 GET /v1/cv-status (Per the prior design,; canonical-vocab drift)
 GET /v1/search
```

Loss of the DB is recoverable: `yaad-index reindex` walks every
`<kind>/<slug>.md` under `vault.path` and rebuilds rows. Loss of the
vault is **not** recoverable from yaad-index alone — the vault is the
operator's responsibility (Syncthing, Git, backups).

The detailed rationale + each subsection (frontmatter schema, callback
ID = entity ID, canonical-kinds operator config, atomic writes, file
watcher deferred) lives in [ADR-0008](adr/0008-vault-as-source-of-truth.md). Read it before
changing any of the surfaces below.

## Architecture decisions (ADRs)

Every load-bearing technical decision in this codebase has an ADR in
[`adr/`](adr/). Read the relevant one before changing the surface it
governs.

- [ADR-0001 — Fresh rewrite, AI-first remote API](adr/0001-fresh-rewrite-ai-first-remote-api.md). The "why this project
 exists at all" — frames yaad-index as an HTTP knowledge index for
 AI agents on a home network, not a human-facing app.
- [ADR-0002 — v1 API surface](adr/0002-api-surface.md). The `/v1/*` endpoints, request/response shapes,
 long-poll model, error envelope, ingest state machine. Amended by
 [ADR-0008](adr/0008-vault-as-source-of-truth.md) in three places: snippet semantics
 (now from agent-filled `summary`, not query-time substring strip),
 fill-token mechanism (gone — entity ID is the durable callback),
 and the new `/v1/entities/{id}/notes` endpoint.
- [ADR-0003 — CLI library: kong](adr/0003-cli-library-kong.md). Why `cmd/yaad-index/main.go` uses kong for
 command parsing rather than `flag`/`cobra`/etc.
- [ADR-0004 — Logging library: slog](adr/0004-logging-library-slog.md). Standard-library `log/slog` everywhere; no third-
 party logger.
- [ADR-0005 — Plugin lifecycle](adr/0005-plugin-lifecycle.md). The original plugin invocation model
 (subprocess-per-request, JSON over stdio, `--init` capabilities,
 per-kind TTL cache). **The Discovery section is superseded by
 ADR-0006**; everything else (invocation, request protocol, cache,
 conflict resolution) is still authoritative.
- [ADR-0006 — Plugin discovery via config allowlist](adr/0006-plugin-discovery-config-allowlist.md). Replaces ADR-0005's
 PATH scan with an explicit `~/.config/yaad-index/config.yaml`
 allowlist. Fail-fast on broken configs, ordered list = dispatch
 priority. Extended with the **disambiguation protocol**:
 plugins emit data (Entity / Gaps / Options); yaad-index labels.
 Plugins emitting `Options` MUST also accept `<plugin>: <id>`
 shorthand input — see the "Shorthand-by-id contract" subsection.
- [ADR-0007 — Testing library: testify](adr/0007-testing-library-testify.md). Adopts `github.com/stretchr/testify` for
 the `require` / `assert` split + standardised diagnostics. Test-only
 dep — never imported by production code. See the **Testing** section
 below for the conventions.
- [ADR-0008 — Vault as source of truth (DB as derived index)](adr/0008-vault-as-source-of-truth.md). The
 architectural redesign that makes the markdown vault authoritative
 and the SQLite DB regenerable. Introduces the `vault.path` config
 key, the `canonical_kinds` / `canonical_edge_types` operator-config
 layer, notes as a first-class entity field, the
 `POST /v1/reindex` admin endpoint, and the entity-ID-as-durable-
 callback agent-fill model. Amends ADR-0002 in the three places
 noted above.
- [ADR-0009 — Provenance reconciliation (vault canonical, reindex re-derives)](adr/0009-provenance-reconciliation.md). Closes
 the half-migrated provenance state from ADR-0008's rollout: the
 vault file's frontmatter `provenance:` list is the canonical
 snapshot; reindex re-derives the DB-side rows on every walk via
 `store.ReplaceProvenance`. Names the sequential-failure window
 between `Replace` and ingest-path `Append` calls and accepts the
 trade-off that the next reindex pass cleans it up.
- [ADR-0010 — Row-level idempotency for vault-derived DB tables](adr/0010-row-level-idempotency-for-derived-tables.md). Tightens
 the `provenance` table with two partial UNIQUE indexes (fetch path +
 fill path, each `WHERE NOT NULL` to dodge SQLite's NULL-distinctness
 in unique indexes). `AppendProvenance` becomes silent on duplicate
 insert via `ON CONFLICT DO NOTHING`. Closes the duplicate-row
 outcome from ADR-0009 §Race at the storage layer.
- [ADR-0011 — Vault file aliases synthesized from entity titles](adr/0011-vault-file-aliases-from-titles.md). yaad-index
 synthesizes an `aliases:` frontmatter field from each entity's
 human-readable title (`data.title` for source-shape, `data.name`
 for canonical-shape) at vault-write time. Slug-based filenames
 stay; aliases become a navigation overlay so Obsidian wikilinks
 via natural names resolve. Reindex re-synthesizes; user-edits to
 aliases are not preserved.
- [ADR-0012 — User-generated content as a first-class entity source](adr/0012-user-generated-content.md). UGC
 entities (operator-created notes) become a first-class kind in
 the daemon's data model alongside plugin-emitted entities. The
 `user_content` plugin-shape gives operators a persistent
 vault-and-DB surface for hand-written pages without going through
 a fetch/ingest plugin.
- [ADR-0013 — Canonical-kind owns gap contract; plugin emits edge-keyed data; index extracts edges](adr/0013-canonical-kind-owns-gap-contract.md). The
 canonical-kind registry owns the gap vocabulary (which fields
 agents fill, what their semantics are, which prompt the agent
 reads). Plugins emit edge-keyed data; the index walks plugin
 output and extracts edges into the canonical-shape stubs. Closes
 the question of who-owns-what between plugin payloads and
 canonical-side fill flows.
- [ADR-0014 — Plugin attachment contract](adr/0014-plugin-attachment-contract.md). Binary-blob
 delivery from plugins to vault via a staged-source + hardlink
 protocol. Plugins write attachments to a per-request staging dir;
 the daemon hardlinks the file into the vault and deletes the
 staged source on success.
- [ADR-0015 — Plugin body markers](adr/0015-plugin-body-markers.md). Marker-pair
 contract (`<!-- yaad-index:plugin-body:start -->` / `:end -->`)
 for plugin-emitted markdown body content. Lets plugins own a
 rendered body section without clobbering operator-edited content
 in the same file.
- [ADR-0016 — Canonical-kind defaults](adr/0016-canonical-kind-defaults.md). Built-in
 gap vocabularies for the standard canonical kinds (person,
 company, boardgame, book, city, country) + a 4-layer override
 model (operator config > plugin capabilities > built-in defaults
 > daemon hardcoded fallback) + plugin-driven activation: a
 canonical kind activates only when at least one plugin emits it.
- [ADR-0017 — Canonical entity IDs use plugin-agnostic clean slug](adr/0017-canonical-id-clean-slug.md). Plugin-emitted
 entities live at two layers: source-shape ID
 (`<plugin>:<slug-with-disambig>`, plugin's preferred form) and
 canonical-shape ID (`<canonical-kind>:<clean-slug>`, plugin-
 agnostic). Canonical-side slugs MUST be clean so cross-plugin
 merges land on the same key. Wikipedia strips parens-disambig at
 canonical emission; bgg drops year suffix. Source-layer slugs
 retain whatever disambig the plugin needs.
- [ADR-0018 — Archive replaces delete; deletion becomes a separate explicit destroy path](adr/0018-archive-replaces-delete.md). Lifecycle:
 active → archived → destroyed. Archive is the default disposal;
 DELETE is only valid on already-archived entities (returns
 `409 Conflict` + archive-first hint otherwise). Vault: archived
 entities move to `_archive/<kind>/<slug>.md`, hidden from default
 search/list. Edges retained with `archived: true` flag.
 Aggregate-root pattern: attachments owned by parent entity, not
 standalone addressable; archive/delete cascades through the
 attachment manifest.
- [ADR-0019 — Operator fills gaps the agent can't (dual-source gaps)](adr/0019-operator-fill.md). Extends
 the existing gap mechanism so the agent fills what it can from
 `clean_content` and the operator fills the rest. Gap state machine:
 unfilled / agent-filled / operator-filled / deferred ("ignore for
 now"). Per-gap `fill_strategy` hint (`agent`/`operator`/`both`,
 default `both`) routes which audience sees the gap. Operator-fill
 via `POST /v1/entities/{id}/operator-fill`. Storage in entity
 `data` for values + parallel `gap_state:` block for write-source +
 defer metadata. Closes the operator-database reframe from the
 2026-05-08 concerns meeting; addresses the cold-reviewer's "would you miss it
 = no" by making yaad-index hold operator-truth, not just external-
 source-truth.
- [ADR-0020 — Search with gap-field predicates](adr/0020-search-gap-predicates.md). Extends
 `GET /v1/search` with repeated `where=` predicates over
 `data.<field>` paths (operator-filled values from ADR-0019
 become first-class filter axes alongside `q` and `kind`) and
 repeated `gap_state=` filters over the ADR-0019 state machine
 (`unfilled` / `agent-filled` / `operator-filled` / `deferred`;
 default excludes `deferred`). Additive: existing callers keep
 working.
- [ADR-0021 — Daemon owns slug derivation; canonical entities are edge-target labels](adr/0021-daemon-owns-slug.md). Moves
 the clean-slug algorithm (ADR-0017) out of plugin code into a
 single daemon-side deterministic function — plugins no longer
 produce slugs. Plugin emission carries a `kind: source` payload
 plus a flat `edges` block keyed by edge type with
 `{name, kind}` targets; source-type information (Wikipedia
 article, BGG record, email) lives as an EDGE on the source
 node, not as a per-plugin entity kind.
- [ADR-0022 — Plugin command protocol](adr/0022-plugin-command-protocol.md). Adds
 a `commands: [...]` field to plugin `--init` capabilities
 (parallel to `url_patterns`), entries either bare-string
 (`"fetch"`, defaults `operator_only=false`) or long-form object
 (`{"name":"delete-all","operator_only":true}`). Invocation
 sigil `!` on the user/agent side (e.g. `gmail: !fetch`) routes
 through an in-memory job system (sigil-stripped bare command
 exact-matched against advertised `commands`; no argument
 grammar is defined in v1); routing-time validation rejects
 unknown commands.
 The plugin handler runs via `<plugin-binary> --command <name>`.
 Response-shape clause superseded by ADR-0023.
- [ADR-0023 — Unified plugin response protocol (NDJSON streaming)](adr/0023-unified-plugin-response-protocol.md). All
 plugin responses — URL-shape AND command-shape — are
 newline-delimited JSON on stdout, one self-contained envelope
 per line, flushed eagerly. Three recognized line shapes:
 entity-emission, `_error` (per-item failure, processing
 continues), `_summary` (terminal counters). Applies uniformly
 across `--fetch` and `--command <name>`; supersedes
 ADR-0022's per-command multi-envelope clause.
- [ADR-0024 — Workflows and tasks](adr/0024-workflows-and-tasks.md). Introduces
 operator-authored **workflows** (markdown files at
 `workflows/<name>.md`, YAML-fenced rules in the body,
 frontmatter for metadata) with trigger / optional
 gap-injection / CEL decision / output. yaad-index ships the
 engine (parser, trigger detector, fill-gap integration via
 ADR-0019, output dispatch); operators write the files.
 Decisions are deterministic CEL by default (agent-free); the
 `context` stanza pre-binds named CEL expressions for DRY +
 readability. Pairs with a first-class **task** entity kind
 surfaced through note/task endpoints.
- [ADR-0025 — Date entities](adr/0025-date-entities.md). Adds
 a daemon-managed `day:<YYYY-MM-DD>` canonical kind (v1.x:
 day only; week / month / year deferred) interpreted in the
 daemon's configured timezone (`timezone:` config knob; falls
 back to host `time.Local`). Auto-creation is reference-driven:
 a shape-scan on every write path emits a `references_day`
 canonical edge for any `day:`-shaped frontmatter value; plugins
 MAY declare `date_fields` in `--init` to promote per-field
 references into typed canonical edges (`due_on`, `occurred_on`,
 `is_about_day`, `ingested_on`). Days carrying operator-written
 journal content set `data.is_journal: true` so consumers can
 filter the journal subset.
- [ADR-0026 — yaad-github plugin (hybrid URL+command invocation)](adr/0026-yaad-github-plugin.md). The
 yaad-github plugin advertises both `url_patterns` (per ADR-0022)
 for ad-hoc PR/issue ingest from links AND a `fetch` command
 (per ADR-0022 + ADR-0023) for periodic per-repo polling.
 Multi-instance pattern: one binary, many configured instances
 (one per repo / org) so per-instance polling cadence and
 credentials are independent. PR and Issue live in split
 canonical namespaces (`github-pr:` / `github-issue:`) — both
 reference the same `repository:` and `github-user:` anchors but
 have distinct lifecycle vocabulary.
- [ADR-0027 — CEL temporal + graph-walk primitives](adr/0027-cel-temporal-graph-primitives.md). Adds
 three categories of primitive to the workflow CEL evaluator
 from ADR-0024: **day helpers** (`today()`, `yesterday()`,
 `tomorrow()` — canonical-ID return `day:YYYY-MM-DD` so the
 result composes directly with `graph.get(today())` and
 `add_canonical_edge.target.name`); **stateless temporal
 arithmetic over day IDs** (`add_days`, `days_between` — take
 day-ID strings, return a day ID / signed int respectively, so
 they pair with the day helpers); **period helpers + their
 dedicated companions** (`this_week()` / `this_month()` /
 `this_year()` return ISO-scalar strings `"2026-W21"` /
 `"2026-05"` / `"2026"` — pure CEL computation, NOT canonical
 IDs — paired with `days_in_week/month/year` for fan-out into
 day-ID lists and with `week_of/month_of/year_of` for the
 reverse projection from a day ID; period scalars do NOT
 resolve through `graph.get` / `add_canonical_edge` and are NOT
 valid inputs to `add_days` / `days_between`); **graph-walk** (`graph.in_edges`,
 `graph.out_edges`, `graph.in_neighbors`,
 `graph.out_neighbors` — return `{items, truncated, total}`
 structs to surface capped-result state). Per-fire caching of
 the current-period helpers (`today` through `this_year`) keeps
 a workflow's view of "now" stable across its evaluation.
 Includes the action-runner kind-prefix strip in
 `vault_writers.go` so `target.name: today()` resolves to
 `day:2026-11-11`, not `day:day-2026-11-11`.
- [ADR-0028 — Multi-instance plugins](adr/0028-multi-instance-plugins.md). Generalizes
 plugin identity from `name` to `(name, instance)` so the same
 binary can be loaded multiple times under different runtime
 config (two Gmail accounts, multiple GitHub identity contexts,
 etc.). Config: `plugins[*].instances[]` with per-instance
 `env:` + `config:`; absent `instances:` synthesizes an
 implicit instance named `default`. Plugin self-declares
 multi-instance support via `supports_instances: bool` in
 `--init` (default `false`); the daemon fail-fasts at config
 load when a `false`-declaring plugin has 2+ instance entries.
 The flag constrains **cardinality** (≤ 1 when `false`), NOT
 naming — a `supports_instances: false` plugin with one explicit
 instance keeps the operator's chosen name (`source: bgg/personal`
 is valid). `--init` runs **once per plugin** — capabilities
 are plugin-scoped; instances are runtime-config variants only.
 URL dispatch: plugins declare a nullable `instance_routing`
 block in `--init` (strategy + config_field + match_template);
 first-match-wins glob across instances. **Unmatched URLs
 fail fast** at ingest with a `400 {instance: "unrouted", url}`
 response — no silent fallback to a first-declared instance, so
 misattribution can't quietly land in `source:`. Command grammar
 (amends ADR-0022): `<plugin>/<instance>: !<cmd>` for
 instance-scoped invocation; bare `<plugin>: !<cmd>` fans out
 **serially** across enabled instances in declaration order.
 Entity `source:` field is always the slash form
 `<plugin>/<instance>` (no bare-plugin shape); multi-source
 overlap promotes the field to an array. Per-instance
 `enabled: false` flag, runtime state composite-keyed by
 `(plugin, instance)`, archive-not-purge on instance removal.
 Pre-release status — no migration.

## Project layout

- `cmd/yaad-index/` — server entry point (kong CLI: `serve`,
 `plugins clear-cache`, `reindex`). Hosts the store + registry +
 vault wiring + canonical-kinds guard bring-up + graceful shutdown.
- `internal/api/` — HTTP handlers, middleware, response shapes,
 long-poll ingest tracker. Note + fill endpoints write vault
 first, then mirror to the DB.
- `internal/vault/` — markdown serialization (Entity ↔ file bytes),
 atomic-write Writer (temp + rename), Reader that merges body
 `## Edges` / `## Notes` into frontmatter on read so hand-edits
 flow through reindex. The frontmatter schema is locked by
 ADR-0008.
- `internal/reindex/` — vault walker that (re)builds the derived
 SQLite rows. Used by `yaad-index reindex` (CLI) and
 `POST /v1/reindex` (HTTP). Two modes: incremental
 (mtime + content-hash per file) and full (drop + rebuild).
- `internal/store/` — SQLite persistence (entities, edges,
 provenance, plugin capabilities cache, reindex bookkeeping).
 Schema migrations live in `internal/store/migrations/`.
- `internal/plugins/` — plugin substrate. The `Plugin` interface
 (`Name` / `Match` / `Fetch` / `Capabilities`) and the `Registry`
 live here; the two concrete implementations are:
 - `internal/plugins/subprocess/` — wraps a binary on disk per
 ADR-0005's invocation model + ADR-0006's allowlist.
 - `internal/plugins/fixture/` — in-memory plugin used by tests.
- `internal/config/` — YAML config loader (per ADR-0006 +
 ADR-0008). Hosts the `CanonicalGuard` value the ingest path
 consults to gate canonical-shape stubs.
- `adr/` — this is where decisions live (see above).

## Configuration

`~/.config/yaad-index/config.yaml` (or wherever `--config` /
`YAAD_INDEX_CONFIG` points). YAML, fail-fast on parse / validation
errors at server start.

```yaml
plugins:
 - name: wikipedia
 path: /home/operator/.local/bin/yaad-wikipedia
 - name: bgg
 path: /home/operator/code/yaad-bgg/yaad-bgg

vault:
 path: /home/operator/notes/vault
 # Auto-commit on write (per yaad-index the source issue); all fields optional.
 # auto_commit: true # nil → auto-detect via .git/
 # auto_commit_debounce_seconds: 0 # 0 = per-operation; >0 = batch window
 # auto_push: false # opt-in `git push` after each commit
 # committer_name: yaad-index
 # committer_email: yaad-index@localhost

canonical_kinds:
 person:
 gaps:
 name: "Full name."
 summary: "One-paragraph summary."
 tags: "Topic tags relevant to this entry."
 birth_date: "Birth date in YYYY-MM-DD if available."
 instruction: "Skip if absent."
 city:
 gaps:
 name: "City name."
 country: "Country it belongs to."
 country:
 gaps:
 name: "Country name."

canonical_edge_types:
 - is_about
 - same_as
 - lives_in

log_level: info

# Pair-claim JWT auth scaffold (per yaad-index a prior PR; a prior PR
# wires the HTTP middleware; a prior PR enforces author validation on
# notes; a prior PR serves /v1/jwks). Operational config — keys_dir lives
# outside the vault by design (vault-readable means agent-readable,
# which the operator explicitly vetoed). All three fields optional; defaults
# are `required: true`, `keys_dir: /etc/yaad-index/keys`,
# `default_ttl: 2160h`.
# auth:
# keys_dir: /etc/yaad-index/keys
# default_ttl: 2160h # Go duration syntax (90 days)
# required: true # set to false for dev-mode bypass (NOT for prod)

# Operator-supplied directive injected on every needs_fill response
# (per ADR-0013 §2 a prior PR). Empty / unset → field omitted on the wire.
# fill_instruction: |
# Extract canonical companions only when the article's text clearly
# supports them. Skip-if-absent on every gap.
```

- **`plugins:`** — ordered list (per ADR-0006); first-match-wins
 dispatch. Path must be absolute, executable, an actual file.
- **`vault.path`** — absolute path to the markdown vault root (per
 [ADR-0008 §"Vault layout"](adr/0008-vault-as-source-of-truth.md)). Required for `POST /v1/ingest`,
 `POST /v1/entities/{id}/fill`, `POST /v1/entities/{id}/notes`,
 and `POST /v1/reindex` to function. Without it, ingest stays
 DB-only and the fill / notes / reindex endpoints return
 503 `vault_required` (or are unregistered, in reindex's case).
- **`vault.auto_commit`** + auto-commit fields — when the vault root
 is a git working tree, yaad-index records every successful write
 as a git commit summarizing the operation (per yaad-index issue
). The vault becomes its own audit log. Tri-state on
 `auto_commit`: nil (default) auto-detects `.git/`; `true` requires
 it (Validate fails fast otherwise); `false` opts out regardless.
 Templates: `ingest: <id>`, `re-ingest: <id> [force_refetch=true|ttl_expired]`,
 `fill: <id> [field1, field2, ...]`, `note: <id> by <author>`.
 `auto_commit_debounce_seconds: N` (>0) collapses bursty writes
 into one rollup commit per N-second window with a summarized
 message (`bulk: ingest 12, fill 3`); 0 (default) is per-operation.
 `auto_push: true` runs `git push` after each commit (best-effort —
 push failures log but don't fail the underlying write).
 `committer_name` / `committer_email` set the git committer
 identity; the per-write author is the calling agent (e.g.
 `agent:bob`). Reindex is intentionally NOT in the operation set —
 reindex is read-only on the vault per ADR-0008. Commit failures
 log but don't fail the surrounding vault write; the file is the
 source of truth, the audit commit is best-effort.
- **`canonical_kinds:`** + **`canonical_edge_types:`** — the
 operator's enabled set of cross-source canonical kinds + edge
 types (per [ADR-0008 §"Canonical kinds (operator config)"](adr/0008-vault-as-source-of-truth.md)
 and [ADR-0013 §1](adr/0013-canonical-kind-owns-gap-contract.md)).
 Empty / missing → no canonical layer materializes; only
 source-shape entities live. Plugins that emit canonical-shape
 stubs declare what they MAY emit via `canonical_kinds_emitted` /
 `canonical_edge_types_emitted` in their `--init` capabilities;
 yaad-index logs a warn line at startup for each emission the
 operator hasn't enabled (so the discoverability gap is visible
 rather than silent).
 - **Schema migration (yaad-index, 2026-05-04).** The
 `canonical_kinds:` shape changed from a string-list (`[person,
 city]`) to a map of per-kind config blocks. **The old shape no
 longer parses.** Each enabled kind now declares its own gap-set
 vocabulary and an optional fill instruction (per ADR-0013 §1):
 ```yaml
 canonical_kinds:
 person:
 gaps:
 name: "Full name."
 summary: "One-paragraph summary."
 instruction: "Skip if absent."
 city:
 gaps:
 name: "City name."
 ```
 Validation rules (fail-fast at server start): kind names match
 `[a-z][a-z0-9_]*`; each kind's `gaps:` must have ≥ 1 entry;
 each gap field name matches the same regex; each gap prompt
 is non-empty; `instruction:` is optional but, when present,
 cannot be whitespace-only. Operators with the old list-shape
 migrate by giving each enabled kind its own per-kind config
 block. The map keys are the enabled-kinds set (semantically
 equivalent to the old list).
 - a prior PR (yaad-index) carries parsing + validation only; the
 `gaps` + `instruction` fields are stored in the Config struct
 but not yet wired through to the `canonical_vocabulary` field
 on `needs_fill` responses. Wiring lands in a prior PR.
- **`log_level`** — slog handler threshold; one of `debug`, `info`
 (default), `warn`, `error`. Empty / missing → info, matching the
 legacy hardcoded value. An unknown string fails server start
 with a clear error per ADR-0006's "operators must notice broken
 configs" rule. The level applies uniformly to both `serve` and
 `reindex` subcommands; a `slog.LevelVar` swap at startup means
 the few pre-config-load lines (`store ready`) emit at info
 regardless, then everything past that point honors the
 operator's setting.
- **`fill_instruction`** — operator-supplied prose surfaced on
 every `needs_fill` ingest response under the `instruction` wire
 field (per [ADR-0013](adr/0013-canonical-kind-owns-gap-contract.md) §2). The agent's AI reads this
 string as a stable directive on how to approach gap filling
 without per-call API surface changes. Resolution order at
 response-build time (per ADR-0013 §2 a prior PR, yaad-index):
 per-kind `canonical_kinds.<kind>.instruction:` wins → global
 `fill_instruction:` next → both unset omits the field. Source-
 shape entities (kind not in the registry) only see the global
 path. The server does NOT compose or post-process the chosen
 value; it's byte-identical to the winning config field. Both
 fields appear on `needs_fill` responses ONLY (not on filled
 responses).
- **`canonical_vocabulary` on the wire** — every `needs_fill`
 response also surfaces the operator's full `canonical_kinds:`
 registry verbatim under `canonical_vocabulary` (per ADR-0013 §2
 a prior PR, yaad-index). Empty / nil registry → field omitted.
 Operator-config-only — plugins never control these contents
 (prompt-injection guardrail per ADR-0013 §2). Agents read this
 alongside the resolved `instruction` when deciding whether to
 extract canonical companions from `clean_content`.
- **Gap-call lifecycle (ADR-0013 §4 + §5, yaad-index).**
 The `needs_fill` payload (gap-call) is bounded to **one per
 fetch-cycle**:
 - Set on a successful fill: any 2xx response from
 `POST /v1/entities/{id}/fill` (full or partial) stamps a
 DB-only `gap_call_done_at` flag on the entity.
 - Suppressed while set: subsequent cache-hit ingests return
 the entity in its current state (`complete` envelope) even
 if some gaps remain unfilled. The AI couldn't satisfy the
 gap-set with the prior `clean_content`; re-prompting against
 the same content is wasteful.
 - Cleared by refetch: `force_refetch=true` on `POST /v1/ingest`
 or a TTL-driven cache fall-through that re-runs the plugin
 rolls the entity into a new fetch-cycle and clears the flag.
 - Direct retry on AI failure: a fill that returns 5xx (network
 error, content-filter block, tool crash) leaves the flag
 unset, so the next direct ingest still returns `needs_fill`.
 - Regen invariant: the flag is **DB-only and intentionally not
 derived from vault**. Wiping the DB and re-running reindex
 restores the entity from vault but leaves the flag NULL —
 gap-call replay against unchanged content is acceptable per
 ADR-0013 §5. **No content-hash-based or persistent attempt-
 tracking** is permitted; that would defeat the regen
 invariant.
- **`auth.keys_dir`** + **`auth.default_ttl`** — pair-claim JWT auth
 scaffold (per [yaad-index](https://github.com/yaad-index/yaad-index/issues/178)
 a prior PR of the auth series). Operational, NOT vault-readable: the
 private key MUST live outside the vault root; agents must not
 be able to trick the index into returning it. Default keys_dir
 is `/etc/yaad-index/keys/` (sibling of `config.yaml`); default
 default_ttl is `2160h` (90 days — values use Go's
 `time.ParseDuration` syntax, so `ns`/`us`/`ms`/`s`/`m`/`h` only;
 no `d` suffix). Path precedence chain (locked):
 **CLI flag > env (`YAAD_INDEX_KEYS_DIR` / `YAAD_INDEX_DEFAULT_TTL`)
 > config file**. The CLI / env layers always win; config is the
 lowest-priority default.

 **Setup runbook:**
 ```sh
 # 1. Generate the keypair (refuses to overwrite without --force).
 yaad-index keygen --keys-dir /etc/yaad-index/keys

 # 2. Issue a token for an operator/agent pair.
 yaad-index issue-token --operator alice --agent the cold-reviewer --ttl 24h
 # → prints the signed JWT on stdout; pipe into a secrets store.
 ```

 a prior PR ships the building blocks (keygen, sign, verify, CLI
 surface). a prior PR wires the HTTP middleware that extracts
 `Authorization: Bearer <token>` and attaches the parsed claim
 to the request context. a prior PR enforces that
 `POST /v1/entities/{id}/notes` `author` matches the JWT's
 `sub`. a prior PR serves `GET /v1/jwks` (RFC 7517) so peer
 agents can verify yaad-index-issued tokens without out-of-band
 key sharing — the route is public, `Cache-Control: public, max-age=3600`,
 single-key v1, registered only when public.pem is readable
 (dev-mode without keys leaves it unregistered).

- **`auth.required`** (added a prior PR) — master switch on the HTTP
 middleware. Default `true`: every protected route demands a valid
 Bearer JWT. Set to `false` (or pass `--auth-required=false` /
 `YAAD_INDEX_AUTH_REQUIRED=false`) for dev-mode bypass; the server
 attaches a synthetic anonymous claim (`sub=anonymous`,
 `operator=none`) and logs a startup warning so running disabled is
 never silent. **Production deployments must leave this at the
 default.** The protected/public route split is documented in
 [ADR-0002 §"Authentication / authorization"](adr/0002-api-surface.md).
 Public routes (`/v1/health`, `/v1/structure`, `/v1/cv-status`,
 future `/v1/jwks`) stay accessible without a token by design —
 system metadata, no vault data.

- **`cache_ttl_seconds`** — bounds the lookup-first ingest cache
 freshness window in seconds . Empty / missing /
 `0` disables the TTL (cache hits forever once registered —
 legacy behavior). `>0` makes a cache hit valid only when the
 entity's freshest non-cache-shaped `provenance.fetched_at` is
 within `now - ttl`; stale hits fall through to the plugin Fetch
 path. `force_refetch=true` always bypasses regardless of TTL.
 See [`docs/plugin-flow.md`](docs/plugin-flow.md) §2a–§2c for the full architecture.

## Reserved data keys

The `data` map on entity wire shapes carries plugin-emitted fields
plus a few **platform-owned** keys derived from top-level vault
frontmatter. Plugins that emit these names natively will be
clobbered by the projection on every vault → DB mirror; treat them
as reserved.

| Key | Source | Introduced |
|-----------------|------------------------------|------------|
| `summary` | `vault.Entity.Summary` | a prior PR (snippet from summary) |
| `tags` | `vault.Entity.Tags` | a prior PR (fill vault-first) |
| `comments_text` | `\n`-joined `vault.Entity.Notes[].Text` (FTS-only — full structured notes live in `vault.Entity.Notes`) | a prior PR (notes endpoint) |

(See `internal/api/fill.go::vaultEntityDataForDB` for the projection
function. Plugins emitting `summary` / `tags` / `comments_text` as
their own `data` keys will overwrite each other on a re-ingest +
fill cycle; pick different names.)

## Operator workflow

What happens when an agent calls `POST /v1/ingest` (per
[ADR-0008](adr/0008-vault-as-source-of-truth.md) flow +
lookup-first cache; full walkthrough in [`docs/plugin-flow.md`](docs/plugin-flow.md) §2):

1. **Lookup-first cache probe.** yaad-index probes
 `entity_notations[req.url]` first (skipped on `force_refetch=true`).
 Hit + within TTL → respond from store + vault, no plugin call;
 the response carries an ephemeral `cache:notations` provenance
 entry so agents distinguish cached from fresh.
2. **Cache miss → plugin dispatch.** Plugin matches the URL;
 yaad-index dispatches to the plugin's subprocess.
3. Plugin returns `FetchResult` with the source-shape entity, any
 `clean_content`, the gap field-name set, every input form that
 resolves to this entity (`Notations []string` — see plugin-flow
 §2a), and (optionally) canonical-shape stubs + edges via
 `CanonicalEntities` / `CanonicalEdges`.
4. yaad-index writes the entity to the vault as `<vault>/<kind>/<slug>.md`
 with full frontmatter (id, kind, plugin, data, provenance,
 notations, gaps, summary, tags, edges, notes). Atomic temp +
 rename — there is never a partially-written destination file.
5. yaad-index mirrors the entity into the DB via `UpsertEntity` +
 `AppendProvenance` + `ReplaceNotations` (the cache pre-
 registration step).
6. Canonical-shape stubs/edges from the plugin filter through the
 `CanonicalGuard` (operator config); survivors land via the same
 path. Filtered drops emit a debug log line.
7. Response carries `state` (`complete` / `needs_fill` / `queued` /
 `disambiguation`) and the entity (with the single-hop body —
 `clean_content`, `summary`, `tags`, `gaps`, `aliases`, `plugin`,
 `notations`, `notes` — via the `mergeVaultEntity` overlay).
 The **entity ID is the durable callback handle**: agents use
 `POST /v1/entities/{id}/fill` later (seconds, hours, days)
 without any server-side token.

Agent fill cycle (`POST /v1/entities/{id}/fill`):

1. Read vault file (canonical state).
2. Validate every submitted field name against the entity's current
 `gaps:` list. Per-call atomic — any submitted field not in the
 set returns 409 `conflict` with `rejected: [...]`; no partial
 success.
3. Write vault first (atomic), then mirror to DB.
4. Partial fills are first-class: an agent can submit a subset of
 gaps; remaining gaps stay open for a future call.

Multi-hop context stitch (`GET /v1/entities/{id}/context?depth=N`,
per yaad-index the source issue):

1. BFS-walk outbound edges from the path id up to `depth` hops
 (server cap 3; `depth=4+` is rejected with `400 invalid_argument`).
2. Cycle detection: each entity appears at most once in the
 response (root + neighbors), keyed by id. Back-edges to
 already-visited entities are dropped silently.
3. Optional `edge_types=A,B` filter walks only the named edge
 types AND surfaces only those edges in the response.
4. Optional `max_results` (default 200, cap 1000) bounds the
 `neighbors` array. Truncation cuts after the last fitting
 entry and sets `truncated: true` so agents distinguish "no
 more results" from "result was clipped."
5. One `GetEdgesForMany` SQL query per BFS depth (frontier-batched)
 plus one `GetEntities` resolve per depth — at depth=3 worst
 case that's 6 SQL round-trips, never N independent point-lookups.
6. Use case: assemble cross-source context (PR + linked Jira ticket
 + linked Confluence doc + canonical process stub) in one
 round-trip without per-hop client loops.

Note append (`POST /v1/entities/{id}/notes`):

1. `{text, author?}` body. Server stamps `date` (UTC, second-
 precision so body and frontmatter round-trip identically).
 Append-only in v1; edit / delete by note ID is a follow-up.
2. Vault frontmatter is canonical; the body `## Notes` section
 is regenerated from the list on every write. Hand-edits to the
 body section flow through the next `vault.Reader.ReadByID` call
 that reads the file (the reader merges body → frontmatter on
 read). Note text is FTS-searchable via the
 `data["comments_text"]` projection.

Reindex (`yaad-index reindex` CLI or `POST /v1/reindex`):

- **Incremental** (default): walks `vault.path`, computes
 `(mtime, content_hash)` per `*.md`, only re-parses changed files.
 Files in bookkeeping that no longer exist on disk produce a
 cascading entity delete (entity row + bidirectional edges +
 provenance).
- **Full** (`--full` flag, or `{"mode": "full"}` in the HTTP body):
 drops every entity / edge / provenance / reindex_files row in a
 single transaction, then walks and parses every file as new.
 Plugin capabilities cache is preserved.

## Build, test, and verify

`Makefile` is the source of truth for everything CI runs:

```sh
make help # list targets
make check # vet + build + race-test + fmt-check + lint + tidy-check
make test # go test -race -timeout 2m ./...
make lint # golangci-lint run ./...
make fmt # gofumpt + goimports + go mod tidy (writes)
make install-hooks # one-time pre-commit hook setup
```

Always run `make check` before pushing. CI runs the same chain split
into separate `test` / `lint` / `build` / `coverage` jobs (see
`.github/workflows/ci.yml`). `make build` produces `./yaad-index`
in CWD (Per the prior design, fix).

Local end-to-end smoke for the plugin loop:

```sh
make build
# Drop a binary path into ~/.config/yaad-index/config.yaml under plugins:
# Add `vault: { path: /absolute/path/to/vault }` for vault-first ingest + fill.
./yaad-index serve # fail-fast on bad config; logs registered plugins
```

## Testing

`testify` (`github.com/stretchr/testify`) is the assertion library
across the test suite — see [ADR-0007](adr/0007-testing-library-testify.md). The split:

- **`require.*`** — setup invariants. Failure aborts the test (DB
 open, JSON decode, slice length before indexing, etc.). Use when
 the test cannot proceed without nil-deref / panic.
- **`assert.*`** — response-shape checks. Failure records but
 continues, so multiple field mismatches all surface. Use for
 per-field response checks where each diagnostic is independently
 useful.

Other conventions:

- Parameter ordering is `(t, want, got)` — testify's convention even
 though the `expected, actual` param names look misleading. Failure
 messages assume this order.
- Skip the `assert.New(t)` / `require.New(t)` shortcut form unless a
 single file is heavy enough to warrant the consistency tradeoff.
 Default to package-level functions (`assert.Equal(t, ...)`).
- Stdlib `t.Helper()` / `t.Parallel()` / `t.Run()` / `t.Cleanup()` /
 `t.Setenv()` / `t.TempDir()` stay — testify only replaces the
 comparison-and-fail pattern, not the test scaffolding.
- testify is test-only; nothing in non-test code imports it, so it
 doesn't ship in `cmd/yaad-index`.

## Operator-only subcommands

Some maintenance lives on the binary, not on the HTTP surface (the v1
API is intentionally unauthenticated; operator concerns shouldn't be
agent-reachable):

- **`yaad-index reindex [--full] [--vault-path <path>]`** — walks the
 markdown vault and rebuilds the derived SQLite rows. Default
 incremental (mtime + content-hash check per file); `--full` drops
 every entity / edge / provenance / reindex_files row in a single
 transaction first. Reads `vault.path` from the config unless
 `--vault-path` is set explicitly. Prints a JSON summary
 (`{scanned, parsed, skipped, entities_created/updated/deleted,
 edge_rows_written, errors, duration_ms}`) to stdout.
 Mirrors `POST /v1/reindex` for HTTP.
- **`yaad-index plugins clear-cache [--name <plugin>]`** — drops cached
 plugin capabilities so the next `serve` re-runs `--init` for the
 affected plugins. With `--name`, only that plugin's row is dropped;
 without, all rows go. The cache is keyed on `(plugin_name, version)`;
 bumping a plugin's version triggers a normal cache miss on next
 start, so manual clearing is only needed when a plugin's
 capabilities move without a version bump (rare). See the source issue for
 the cache design.

## Conventions

- **Universal `state` field on 2xx `/v1/ingest` responses.** Every
 2xx body carries `state` (`complete` / `disambiguation` / `queued` /
 `needs_fill`) — server-side label, not plugin-emitted. The legacy
 `status` field carries the same value for backwards compatibility;
 new code SHOULD read `state`. See ADR-0002.
- **Plugin contract: emit data, let yaad-index label.** A plugin's
 `FetchResult` takes one of two shapes — `Entity` (with optional
 `Gaps`) OR `Options`. Plus optional `CanonicalEntities` /
 `CanonicalEdges` for cross-source identity stubs (ADR-0008). The
 tracker synthesizes the wire-level state from which field is
 populated. Plugin authors don't type protocol state names. See
 ADR-0006 §"Disambiguation responses" + ADR-0008 §"Canonical kinds".
- **Vault-first persistence.** State-mutating endpoints write the
 vault BEFORE updating the DB. A vault-write failure aborts the
 request with 500 `internal_error` and the DB stays untouched.
 Without `vault.path` configured, `POST /v1/entities/{id}/fill` and
 `POST /v1/entities/{id}/notes` return 503 `vault_required`;
 ingest stays DB-only as a backwards-compatible fallback.
- **Error envelope.** Every non-2xx response is
 `{"ok": false, "error": "<code>", "message": "<human-readable>"}`
 per ADR-0002 §"Error envelope". Code set is stable; new codes add,
 existing ones never change meaning. Common codes: `not_found`
 (404), `invalid_argument` (400), `conflict` (409 — fill-conflict
 per ADR-0008), `missing_entity` (422), `unsupported_url` (422),
 `vault_required` (503), `internal_error` (500),
 `collector_unavailable` (502), `collector_timeout` (504).
- **Review gate (standard).** PRs need approval from at least one
 reviewer plus a clean CI run (`test`, `lint`, `build`, `coverage`).
 PR creator merges once review + checks are green (squash-merge,
 delete branch). ADR-grade changes may require additional reviewers
 at the maintainer's discretion.
- **PR body convention.** Reference the issue with `closes #<n>` so
 GitHub auto-closes on merge. Test plan checklist + scope statement
 belong in the body.
- **Fail-on-main test discipline.** New tests for behaviour change
 must fail on main before the change is applied, then pass after.
 Verify both directions before pushing.
- **Mermaid for diagrams in markdown.** When a doc needs a
 diagram — sequence flow, state machine, architecture map, table
 of relationships — use a fenced ` ```mermaid ` block, not ASCII
 art. Two reasons: GitHub renders mermaid natively (so the doc
 stays readable on the web view), and parsers / agents handle the
 structured form more easily than ASCII boxes-and-arrows. Applies
 to `README.md`, `AGENTS.md`, ADR documents, and anything under
 `docs/`. Example: see `docs/plugin-flow.md` "The big picture"
 section.

## Plugin protocol cheat sheet

For the full end-to-end map of every plugin-touching surface (startup
+ cache, ingest, fill, plugin contract), see
[`docs/plugin-flow.md`](docs/plugin-flow.md). The cheat sheet below is
the quick reference; the doc is the orientation map.

For implementers writing a new yaad-index plugin (a separate Go
module, see e.g. yaad-wikipedia):

- The plugin is a binary invoked subprocess-per-request via the
 config allowlist (ADR-0006). Two CLI modes:
 - `<binary> --init` → write the capabilities document as JSON to
 stdout, exit 0. Example shape:

 ```json
 {
 "name": "wikipedia",
 "version": "1.2.3",
 "url_patterns": ["^https?://[a-z]+\\.wikipedia\\.org/wiki/.+", "(?i)^wikipedia:\\s*\\S+"],
 "entity_kinds": [
 {"name": "wikipedia-article", "default_ttl_days": 7, "snippet_fields": ["extract"]}
 ],
 "edge_kinds": [],
 "canonical_kinds_emitted": ["person", "city", "country", "book", "boardgame"],
 "canonical_edge_types_emitted": ["is_about"],
 "supports_search": true
 }
 ```

 - `<binary>` (no args) → read `{"operation": "ingest", "url": ...}`
 from stdin, write a JSON response to stdout, exit 0. On failure,
 write a human-readable message to stderr and exit non-zero.

- **Response shape** for ingest:

 ```json
 {
 "ok": true,
 "structured": { "id": "...", "kind": "...", "data": { ... }, "provenance": [ ... ] },
 "raw_content": "...",
 "raw_content_truncated": false,
 "gaps": { "summary": "...", "tags": "...", "birth_date": "..." },
 "notations": [
 "https://en.wikipedia.org/wiki/Susanna_Clarke",
 "https://en.m.wikipedia.org/wiki/Susanna_Clarke",
 "wikipedia: Susanna Clarke"
 ],
 "aliases": ["Susanna Clarke"],
 "canonical_entities": [
 {"id": "person:susanna-clarke", "kind": "person", "data": {"name": "Susanna Clarke"}, "aliases": ["Susanna Clarke", "wikipedia: Susanna Clarke"]}
 ],
 "canonical_edges": [
 {"type": "is_about", "from": "wikipedia:susanna-clarke", "to": "person:susanna-clarke"}
 ]
 }
 ```

 Per ADR-0006: populate `structured` (with optional `gaps` for
 needs_fill) OR `options` for disambiguation, never both. Empty
 response = the tracker maps to 404 `not_found`.

- **`raw_content`** is the article body the plugin extracted from
 the upstream — plugins emit it under this name; yaad-index
 surfaces it as `clean_content` on the API response (the
 agent-facing wire shape) and on the vault file body. See
 [`docs/plugin-flow.md`](docs/plugin-flow.md) §2 / §2c for the
 rename at the API boundary.
- **`gaps`** is a `{field-name → description}` object. The agent's AI
 reads descriptions to drive the fill; the field-name set is what
 the fill endpoint validates.
- **`options`** is a `{<id> → {label, summary?}}` object keyed by the
 plugin's canonical id. Plugins emitting `options` MUST accept
 `<plugin>: <id>` shorthand input on a follow-up `/v1/ingest` call
 so callers can pick a candidate (ADR-0006 §"Shorthand-by-id
 contract").
- **`canonical_entities`** / **`canonical_edges`** are the optional
 cross-source stubs — yaad-index gates them through the operator's
 `canonical_kinds:` / `canonical_edge_types:` config (ADR-0008).
 Each `canonical_entities[]` entry MAY carry its own `aliases:`
 list (same shape as the article-level `aliases:` below).
- **`notations`** is the cache-key list — every input form the
 plugin knows resolves to this entity (canonical URL, mobile-
 subdomain URL, shorthand `<plugin>: <id>`, etc.). yaad-index
 writes these to the `entity_notations` cache after a successful
 Fetch; subsequent ingests of any equivalent form short-circuit
 the plugin spawn. **The originating notation MUST be the first
 entry** — the orchestrator's lookup-first probe matches on
 exact-string equality, so the input form must be present at
 index 0 for a self-roundtrip to register on the next call. See
 / [`docs/plugin-flow.md`](docs/plugin-flow.md) §2a.
- **`aliases`** is the alternative-label list for Obsidian wikilink
 resolution + agent reverse-lookup. Two shapes coexist in the flat
 list: bare strings (`"Susanna Clarke"`) render as wikilink targets;
 `<edge-type>: <label>` prefixes (`"author_of: Jonathan Strange"`)
 carry a typed reverse-lookup hint that agents filter on.
 Multi-valued; order doesn't matter (yaad-index merges with the
 ADR-0011 title-synthesized alias and dedupes at vault-write
 time). See / [`docs/plugin-flow.md`](docs/plugin-flow.md) §4.
- **`supports_search`** in the `--init` capabilities document
 declares that the plugin opts in to the upstream-search dispatch
 surface (`POST /v1/search/upstream`, planned in a prior PR). When
 yaad-index fans a query out across registered plugins, only those
 with `supports_search: true` are invoked. Default false; explicit
 opt-in. Plugins not opting in are silently skipped on fan-out
 (the local-search path `/v1/search` continues to work for them).

## Where to look first

| Question | Start here |
|--------------------------------------------------|-----------|
| What's the v1 API shape? | [ADR-0002](adr/0002-api-surface.md) + [ADR-0008](adr/0008-vault-as-source-of-truth.md) (amendments) |
| Why is the vault the source of truth? | [ADR-0008](adr/0008-vault-as-source-of-truth.md) |
| How are plugins discovered? | [ADR-0006](adr/0006-plugin-discovery-config-allowlist.md) |
| How are plugins invoked? | [ADR-0005](adr/0005-plugin-lifecycle.md) (still authoritative outside Discovery) |
| How does the full plugin lifecycle compose? | [`docs/plugin-flow.md`](docs/plugin-flow.md) — startup, ingest, fill, contract end-to-end |
| What does index-internal flow look like? | [`docs/index-flow.md`](docs/index-flow.md) — vault as source of truth, reindex, wipe set, upsert invariants |
| How are canonical kinds gated? | [ADR-0008 §"Canonical kinds"](adr/0008-vault-as-source-of-truth.md) + `internal/config/canonical.go` + `cmd/yaad-index/main.go::warnCanonicalEmissionGaps` |
| What does the ingest tracker do? | `internal/api/ingest_tracker.go` |
| How is a `complete` / `needs_fill` / `disambiguation` response built? | `internal/api/ingest.go` |
| How does the fill conflict semantic work? | `internal/api/fill.go` (per-call atomic 409 with `rejected[]`) + [ADR-0008 §"Callback ID = entity ID"](adr/0008-vault-as-source-of-truth.md) |
| How does subprocess invocation work? | `internal/plugins/subprocess/subprocess.go` |
| How does the vault writer / reader work? | `internal/vault/` package doc + `entity.go::package` |
| How does reindex walk the vault? | `internal/reindex/reindex.go` |
| What goes in the config file? | [ADR-0006 §"Config file"](adr/0006-plugin-discovery-config-allowlist.md) + [ADR-0008 §"Canonical kinds"](adr/0008-vault-as-source-of-truth.md) + `internal/config/config.go` |
