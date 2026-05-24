# ADR-0028: Multi-instance plugins — config schema, dispatch, entity source

## Status

Proposed 2026-05-24. Pre-release; no migration. Schema is the new shape from v0.11 onward.

## Depends on

- [ADR-0005](./0005-plugin-lifecycle.md) — plugin invocation model (subprocess-per-request, JSON over stdio, `--init` capabilities).
- [ADR-0006](./0006-plugin-discovery-config-allowlist.md) — config-allowlist + `url_patterns` routing. This ADR extends the per-plugin entry with an `instances:` array.
- [ADR-0021](./0021-daemon-owns-slug.md) — `kind: "source"` + daemon-derived `<source_namespace>:<slug>` ID + entity `source:` field shape this ADR generalizes from `<plugin>` to `<plugin>/<instance>`.
- [ADR-0022](./0022-plugin-command-protocol.md) — `commands: [...]` + `<plugin>: !<command>` invocation sigil. **Amended** here: §4 below extends the grammar with `<plugin>/<instance>: !<command>` for instance-scoped invocation and defines bare-plugin fan-out semantics.
- [ADR-0023](./0023-unified-plugin-response-protocol.md) — NDJSON streaming response wire. Unchanged: per-instance invocations stream the same way.
- [ADR-0026](./0026-yaad-github-plugin.md) — already specifies a multi-instance pattern at the plugin design level (one binary, configurable base URL + repo list); this ADR formalizes the daemon-side mechanism that the yaad-github §7 pattern relies on.

## Context

The plugin allowlist in `config.yaml` keys plugins by `name`, so the same plugin binary cannot be loaded multiple times with different config. This blocks three concrete use-cases on operators' v0.11 wish list:

- Indexing two Gmail accounts (operator's personal + an assistant address).
- Indexing repos across two GitHub identity contexts — different PATs for personal repos vs. an org's repos when fine-grained-PAT scope prevents a single token from covering both.
- Any future plugin where the same binary needs N independent runtime configs (a second yaad-github pointed at a GHES install, two yaad-wikipedia instances on different language editions, etc.).

ADR-0005 (plugin lifecycle), ADR-0022 (command protocol), and the entity `source:` frontmatter all assume a single-instance-per-plugin model. The fix generalizes plugin identity from `name` to `(name, instance)`:

- **Plugin** = binary + capability set, declared once via `--init`.
- **Instance** = runtime config variant of that capability set.

Capabilities (`canonical_kinds_emitted`, `url_patterns`, `commands`, the `config:` JSON-schema) are plugin-scoped and stable across instances. The per-instance variation is bounded to `env:` and `config:` values.

ADR-0026 §7 already names this pattern at the design level for yaad-github; that ADR's `repos:` + `base_url:` per-instance shape implicitly assumes the mechanism this ADR specifies.

Seven core design questions plus one capability-flag refinement are settled in [#241](https://github.com/yaad-index/yaad-index/issues/241) (locked-decisions comment [4529471833](https://github.com/yaad-index/yaad-index/issues/241#issuecomment-4529471833) and the follow-up *"plugin self-declares multi-instance support"* comment). The decisions below are the locked outcome.

## Decision

### 1. Config schema — `plugins[*].instances[]`

Each plugin entry in the operator config grows an optional `instances:` array:

```yaml
plugins:
  - name: gmail
    path: /usr/local/lib/yaad-index/plugins/yaad-gmail
    instances:
      - name: personal
        env:
          YAAD_GMAIL_ACCOUNT: ops@example.com
          YAAD_GMAIL_APP_PASSWORD_REF: gmail-personal
      - name: assistant
        env:
          YAAD_GMAIL_ACCOUNT: assistant@example.com
          YAAD_GMAIL_APP_PASSWORD_REF: gmail-assistant
  - name: github
    path: /usr/local/lib/yaad-index/plugins/yaad-github
    instances:
      - name: personal
        config:
          repos: [acme-user/repo-a, acme-user/repo-b]
        env:
          YAAD_GITHUB_TOKEN_REF: github-personal
      - name: acme-org
        config:
          repos: [acme-org/*]
        env:
          YAAD_GITHUB_TOKEN_REF: github-acme
```

**Validation rules:**

- Each `instances[*].name` must be unique within the plugin. Operator-config load fails with a clear error on duplicates.
- Names follow the same shape rules as plugin names (alphanumeric + hyphen + underscore, no slash — the slash is reserved for the `<plugin>/<instance>` invocation/source syntax).
- Absent / missing `instances:` block ⇒ implicit single instance named `default`. The operator never has to write `instances: [{name: default}]`; the loader synthesizes it.
- An empty `instances: []` is a config error (no instances ⇒ plugin would be inert; the operator probably meant to delete the plugin entry).
- `env:` and `config:` values are per-instance. Glob patterns are valid inside routing-field values (e.g. `repos: [acme-user/*]`); their semantics are plugin-defined (yaad-github expands the glob at runtime via the upstream `/orgs/{owner}/repos` API per ADR-0026 §3).
- **Plugin self-declaration gate** — operator-config load also enforces the plugin's `supports_instances` capability per §9 below. A plugin with `supports_instances: false` may have 0 or 1 instance entries: 0 entries (absent block) synthesizes the implicit `default`; 1 explicit entry keeps whatever `name` the operator wrote (no coercion to `default`). 2+ entries fail-fast at config load with a clear error. The flag constrains **cardinality** (≤ 1), not **naming** — `source: bgg/personal` is valid provenance when the operator wrote `instances: [{name: personal, ...}]` on a `supports_instances: false` plugin. This is the validation entry point that consumes the §9 flag.

`instances[*].enabled: bool` (default `true`) controls per-instance activation — see §7.

### 2. Plugin loader — `--init` once per plugin

`--init` runs **once per plugin binary**, NOT per instance. The capability cache key in `plugins.capabilities_cache` stays `(plugin name)` — no schema change to that table.

Capabilities (`canonical_kinds_emitted`, `canonical_edge_types_emitted`, `url_patterns`, `commands`, the `config:` JSON-schema, the new `instance_routing` block in §3) are plugin-level facts. The per-instance `env:` and `config:` values are runtime parameters passed when the daemon actually invokes the plugin (subprocess spawn time), not at capability discovery.

Consequence: adding / removing / editing an instance does NOT re-probe `--init`. Only changing the plugin binary itself (`path:`) triggers a re-probe per ADR-0006.

### 3. URL dispatch — `instance_routing` block in `--init` (nullable)

When an inbound URL matches a plugin's `url_patterns`, the daemon must pick which instance handles it. Plugins that opt into multi-instance support (§9, `supports_instances: true`) declare their routing strategy in their `--init` capabilities; plugins that don't opt in MAY omit the block entirely (`instance_routing: null` is valid and is the default expectation for single-instance plugins like yaad-wikipedia / yaad-bgg).

```json
{
  "name": "github",
  "url_patterns": ["^https?://github\\.com/[^/]+/[^/]+/pull/\\d+", ...],
  "instance_routing": {
    "strategy": "glob_match",
    "config_field": "repos",
    "match_template": "{owner}/{repo}"
  }
}
```

**Mechanics:**

- The daemon extracts named fields (`{owner}`, `{repo}`) from the matched URL using the plugin's existing `url_patterns` regex group captures.
- It then formats `match_template` with the extracted fields and glob-matches the result against each enabled instance's `config[<config_field>]` list (in config-file declaration order).
- **First-match wins.** If two instances declare overlapping globs (`acme-user/*` in instance A and `acme-user/repo-a` in instance B), the first-declared instance wins. The daemon emits a startup warning when it detects overlapping coverage at config-load time.
- **Unmatched URL: fail fast.** If no enabled instance's globs match the URL's extracted fields, ingest is rejected with a `400` response shaped `{instance: "unrouted", url: "<the url>", message: "no instance's <config_field> glob matches"}`. The misconfiguration surfaces at ingest time — the operator (or agent) sees the exact URL that didn't match and adds the missing glob entry. Routing the URL to an arbitrary first-declared instance would silently misattribute the resulting entity's `source:` field to an instance that doesn't actually own that scope; a fail-by-default contract preserves provenance correctness. An opt-in permissive-fallback knob (e.g. `unmatched_url_strategy: first-declared`) is deferred to a future ADR if a real workload needs it.

`strategy: "glob_match"` is the only strategy v1 defines. Plugins with `supports_instances: true` MUST declare an `instance_routing` block if their primary invocation path is URL-shape (yaad-github); plugins whose primary path is command-shape (yaad-gmail — instances dispatched only via `!fetch` fan-out per §4) MAY omit `instance_routing` even when `supports_instances: true`, and the daemon then refuses URL-shape ingest for that plugin with a clear "plugin advertises no URL routing" error.

Plugins with `supports_instances: false` always have `instance_routing: null`; URL dispatch on those plugins runs against the implicit `default` instance without a routing scan.

Future routing strategies (exact-match, regex, hash-of-field) can land as additional `strategy:` values without breaking the contract.

### 4. Command dispatch — amends ADR-0022 grammar

ADR-0022 currently defines `<plugin>: !<command>` as the operator-/agent-callable command surface. This ADR extends the grammar with an instance-scope qualifier:

- **`<plugin>/<instance>: !<command>`** — instance-scoped invocation. The named instance MUST exist and be `enabled: true`; the daemon rejects unknown / disabled instance names at routing time.
- **`<plugin>: !<command>`** (bare, no slash) — fans out across all `enabled: true` instances of the plugin, **serially**, in config-file declaration order.

**Fan-out semantics (serial, ordered):**

- Each instance's run completes (stream-to-end + exit) before the next instance starts.
- Logs are linear and instance-attributed (`[gmail/personal] _summary: {ingested: 12}` then `[gmail/assistant] _summary: {ingested: 3}`).
- Total latency = sum of per-instance latencies. This is intentional for v1: determinism + log clarity beat throughput for the bulk-fetch use case. Parallel optimization can land if a real workload demands it.
- If one instance's run errors (non-zero exit or stream error per ADR-0023), the fan-out continues to the next instance; the per-instance error is logged + reported in the aggregate response, not propagated as a fan-out abort.

Single-implicit-instance plugins (no `instances:` block in the config) keep working unchanged under the bare form — the daemon synthesizes `<plugin>/default` internally and the fan-out has cardinality 1. Operators can also write `<plugin>/default: !<command>` explicitly; the shape is uniform.

The sigil-stripped command name still exact-matches against the plugin's `commands` list per ADR-0022; no per-instance command vocabulary divergence.

### 5. Entity `source:` field — always slash form

Today: entity frontmatter has `source: <plugin-name>` indicating which plugin emitted the entity.

**New shape:** `source: <plugin-name>/<instance-name>` always. Implicit-single-instance plugins get `source: <plugin-name>/default`. There is **no bare `source: <plugin-name>` shape** — the parser has one shape, not two.

**Multi-source overlap:** when the same entity is ingested by multiple instances (e.g. the same PR matches both `github/personal` and `github/acme-org` because of overlapping `repos:` globs, or the same URL is routed twice), the field becomes an array:

```yaml
source: [github/personal, github/acme-org]
```

**Implications:**

- **Refresh ownership:** any source in the array may refresh; first-listed wins by default. The first-listed source's `env:` + `config:` is used for subsequent fetches.
- **Cleanup on instance removal:** deleting an instance from config marks single-source entities (`source: [github/personal]` only) for cleanup per §8; multi-source entities have that one source removed from the array but the entity itself stays alive under the remaining sources.
- **Audit:** provenance is answerable directly from frontmatter (no DB lookup needed to determine which instance touched a given entity).

The single-source string vs multi-source array distinction is the same shape generalization canonical-ID resolution (ADR-0017) already supports for cross-source identity. Consumers (search filters, UI surfaces) treat the single-string and single-element-array cases identically.

### 6. Instance runtime state — `(plugin, instance)` composite key

Per-instance runtime state (poll cursors, last-fetched markers, per-instance cache warmups) is keyed by `(plugin, instance)` in the DB.

**Storage shape:** extend the existing `plugin_state` table (or whichever existing per-plugin runtime-state surface the codebase uses; see implementation surface §3 below for the exact migration call) with an `instance_name` column. New composite primary key `(plugin_name, instance_name, key)`. Existing single-instance state rows synthesize `instance_name = 'default'` at load time for the back-compat path; new rows always write the explicit instance name.

Plugin authors continue to interact with runtime state through the existing per-plugin state API; the daemon scopes the read/write to the invoking instance transparently.

### 7. Per-instance `enabled: false`

`instances[*].enabled: false` (default `true`) temporarily disables an instance without removing its config:

- **No URL routing** — disabled instances are invisible to the §3 first-match scan.
- **No command dispatch** — both fan-out (§4 bare form) and instance-scoped (`<plugin>/<disabled-instance>: !<cmd>`) skip / reject.
- **No scheduled refresh** — if a future workflow / cron fires the plugin's `!fetch`, disabled instances are excluded.
- **Config retained, runtime state retained** — re-enabling the instance later picks up where it left off (poll cursors etc. intact).
- **`/v1/plugins` surfaces it** — each plugin row's `instances` field includes the disabled instance with `enabled: false` so the operator can see the full configured set, not just the active subset.

### 8. Cache invalidation — scope follows data scope

Three cache surfaces, three invalidation rules:

- **Plugin `--init` capabilities cache** — plugin-scoped (per §2). Adding / removing / editing an instance does NOT invalidate. Only `path:` change re-probes (per ADR-0006).
- **Routing / URL-pattern compilation cache** — recompiled when any instance's routing-field (e.g. `repos:`) changes. Detection: config-file mtime + content-hash diff at next routing-table refresh.
- **Instance removal** — daemon detects the instance is gone from the next config reload, marks its runtime-state rows `archived: true` (per ADR-0018's archive-not-delete principle), and removes the instance from routing. Operator can re-add the instance later (state un-archives) or explicitly purge with a CLI command. Per §5, single-source entities owned only by the removed instance follow the existing archive-on-source-loss path; multi-source entities just lose that one entry from their `source:` array.

### 9. Plugin self-declares multi-instance support — `supports_instances: bool`

Plugin `--init` capabilities gain a `supports_instances: bool` field. **Default: `false`.** A plugin opts in to multi-instance config by explicitly setting `supports_instances: true`. The default-false posture means existing single-instance plugins keep working without code changes; plugin authors take an explicit action when their data shape genuinely supports independent runtime contexts.

**Expected per current plugins:**

| Plugin | `supports_instances` | Reason |
|---|---|---|
| yaad-wikipedia | `false` | Public API, no auth context, no per-instance scope |
| yaad-bgg | `false` | Single API key for all reads |
| yaad-gmail | `true` | Per-account auth (IMAP credentials per inbox) |
| yaad-github | `true` | Per-PAT auth, per-org/user repo coverage |

**Daemon enforcement at config load (composes with §1 validation):**

- `supports_instances: false` + 0 instance entries (absent block) ⇒ OK. Loader synthesizes the implicit `default` instance per §1.
- `supports_instances: false` + 1 explicit instance entry ⇒ OK. The instance keeps the `name` the operator wrote (`default`, `personal`, or anything else valid per §1's name-shape rules). `source:` field becomes `<plugin>/<that-name>`. The `supports_instances: false` flag constrains cardinality, not naming.
- `supports_instances: false` + 2 or more instance entries ⇒ fail-fast at startup with: *"plugin `<name>` does not support multi-instance config; reduce `instances:` to one entry or remove the block."* The daemon does not start.
- `supports_instances: true` + any instance count (0, 1, or many) ⇒ validated by the rest of this ADR's rules. `instance_routing` is required for URL-shape plugins as covered in §3.

The `instance_routing` block from §3 is **nullable** in the capability surface. Plugins with `supports_instances: false` MUST set `instance_routing: null` (or omit the field). Plugins with `supports_instances: true` MUST set `instance_routing` to a non-null shape if they accept URL-shape ingest; command-only plugins MAY leave it null per §3.

This refinement closes the "what stops an operator from configuring multiple instances of a plugin that doesn't actually scope its data per instance?" gap. Without the flag, yaad-bgg with two operator-declared instances would silently double-write the same entities into one DB row; with the flag, the daemon refuses to start and the operator fixes the config.

## Surface changes (API)

- **`/v1/plugins`** — each plugin row exposes `instances: [{name, enabled}, ...]`. Single-implicit plugins still surface `instances: [{name: "default", enabled: true}]` (the parser's one-shape rule from §5 applies here too).
- **`/v1/ingest`** — successful responses include the routed instance name. On the §3 fail-fast path (no enabled instance's globs match the URL), the response is a `400` with body `{instance: "unrouted", url: "<the url>", message: "no instance's <config_field> glob matches"}` so the caller (operator CLI / agent) sees exactly which URL failed routing and can fix the missing glob.
- **`/v1/needs-fill`, `/v1/structure`** — unchanged. The fill / structure surface is per-plugin (capability-scoped), not per-instance.

## Out of scope (initial)

- **Per-instance plugin binaries.** Single `path:` per plugin entry is intentional; running two versions of the same plugin side-by-side is a separate axis (operator can ship two plugin entries with different `name:` + same binary if they really need to, but the supported pattern is one binary per logical plugin).
- **Cross-instance reference resolution.** An entity emitted by `github/personal` referencing one emitted by `github/acme-org` already resolves correctly because canonical IDs (ADR-0017) are source-agnostic. No new mechanism needed.
- **Parallel fan-out.** §4 commits to serial for v1; parallel is a future ADR with explicit ordering / log-interleaving / per-instance-rate-limit semantics.
- **Per-instance JSON-schema variants.** All instances of a plugin share the same `config:` schema; a plugin can't declare "instance personal has schema X but instance acme-org has schema Y." Schema variance per instance would be a `--init`-per-instance design and is rejected by §2.
- **Per-instance command operator-only flag.** ADR-0022's per-command `operator_only` is plugin-scoped (it's a `commands[].operator_only` field in `--init`). Per-instance overrides ("personal can call `!fetch` but acme-org needs operator-claim") are deferred — if the use case lands, it's a small extension on top of this ADR's mechanism.

## Implementation surface

This ADR ships in 5 cuts (mirroring ADR-0027's cadence):

1. **Cut 1 — config schema + loader + `supports_instances` enforcement.** `plugins[*].instances[]` parsing in `internal/config/`, validation rules from §1 (unique names, `default` synthesis for missing block, error on empty array), the loader change in §2 (capability cache stays plugin-scoped), AND the §9 `supports_instances` capability-flag read + config-load gate (fail-fast on `false`-plus-multi-entry combination). Default `supports_instances: false` preserves existing single-instance plugin behavior; the existing four shipped plugins will set the flag explicitly in a follow-up cut alongside their other capability declarations. No dispatch / source-field changes yet. Smallest surface; lands the data shape the rest of the cuts build on.

2. **Cut 2 — entity `source:` field + slash-form everywhere.** Update the vault writer / reader to emit + parse the `<plugin>/<instance>` slash form per §5, including the multi-source array shape. Update reindex to materialize the slash form on next walk. Update `/v1/plugins` per the surface-changes section. Single-implicit-instance plugins flip from `source: gmail` to `source: gmail/default` on this cut.

3. **Cut 3 — URL dispatch via `instance_routing`.** Wire the `instance_routing` block in `--init` capabilities through the plugin loader; implement glob-match strategy per §3 with first-match-wins across claimed globs, overlap-warning at config-load time, and **unmatched-URL fail-fast** (reject ingest with `400 {instance: "unrouted", url, message}` per §3). Update the existing URL routing path in `internal/api/` to consult the per-instance routing table. Pair with an `instance_routing`-bearing example plugin (yaad-github fits naturally per ADR-0026 §7).

4. **Cut 4 — command dispatch grammar + fan-out + runtime state.** Extend the command parser to recognize `<plugin>/<instance>` invocation per §4; implement serial fan-out for bare-plugin invocations; add the per-instance composite-key migration to the runtime-state table per §6. ADR-0022 inline amendment note linking back to this ADR's §4.

5. **Cut 5 — `enabled: false` flag + cache invalidation + docs.** Wire `instances[*].enabled` through all dispatch / routing / refresh paths per §7. Implement the three cache-invalidation rules per §8 (mtime + content-hash diff on instance config change; archive-on-removal). Update `docs/configs.md` with the `instances:` block + worked example; update `docs/plugin-flow.md` with the `instance_routing` capability + the command grammar extension; refresh `AGENTS.md` reference if needed.

Cuts 3 and 4 are the largest; cuts 1, 2, and 5 are mostly mechanical once the shape is fixed.

## Consequences

**Positive:**
- Operators can index two Gmail accounts / two GitHub identity contexts / N runtime variants of any plugin without forking the binary.
- Capability surface stays plugin-scoped — the per-instance variation is bounded to `env:` + `config:`, so the plugin's contract with the daemon doesn't multiply by instance count.
- Slash-form `source:` shape unifies single- and multi-instance audit paths; consumers learn one shape.
- ADR-0026 §7's multi-instance design lands a daemon-side mechanism it was already implicitly assuming.

**Negative:**
- Operator config gets one level deeper for any plugin that needs multiple instances. The implicit-`default` synthesis keeps the single-instance case free of ceremony, but the operator who graduates from one instance to two has to learn the `instances:` shape.
- Entity `source:` field changes shape (string-or-array) for the multi-source case. Consumers (search, UI) need to handle both; the array case is rare but real.
- Serial fan-out for bare-plugin commands means the operator who runs `gmail: !fetch` against three accounts pays 3× latency. Acceptable for v1 (mostly background poll work, not user-blocking); parallel optimization is a known future extension.
- `instance_routing` is one more `--init` capability field plugin authors need to know about; existing single-instance plugins (yaad-wikipedia, yaad-bgg) skip it and stay simple.

## References

- ADR-0006 — config-allowlist + URL-pattern routing (the per-plugin shape this ADR extends).
- ADR-0022 — plugin command-protocol (grammar amended by §4).
- ADR-0023 — NDJSON streaming response wire (unchanged but referenced by §4 fan-out).
- ADR-0026 §7 — yaad-github's multi-instance design pattern (uses this ADR's mechanism).
- Issue [#241](https://github.com/yaad-index/yaad-index/issues/241) — locked-decisions comment [4529471833](https://github.com/yaad-index/yaad-index/issues/241#issuecomment-4529471833) + the follow-up `supports_instances` comment.
