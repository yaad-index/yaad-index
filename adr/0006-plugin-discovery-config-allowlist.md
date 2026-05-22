# ADR-0006 — Plugin discovery: config allowlist (supersedes ADR-0005's PATH scan)

**Status:** Accepted (2026-04-30)
**Date:** 2026-04-30
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md), [ADR-0002](./0002-api-surface.md), [ADR-0005](./0005-plugin-lifecycle.md)
**Supersedes:** the *Discovery* section of ADR-0005 only. Invocation (subprocess-per-request, JSON over stdio), the `--init` capabilities document, the request/response protocol, and the cache + freshness rules from ADR-0005 are unchanged.

## Context

ADR-0005 commits to plugins as separate executables invoked subprocess-per-request. For *discovery* — the question of which executables the server is willing to spawn — it picks **git-style PATH scan**: the server walks `$PATH` for binaries named `yaad-*` and treats every match as a registered plugin.

A pre-implementation review (the operator, on the in-tree-Go-plugin attempt that drifted away from ADR-0005's subprocess decision) raised the security cost of that choice:

- **Anything on PATH that matches `yaad-*` runs.** A drive-by `yaad-attacker` binary anywhere on the user's PATH (a malicious `~/.local/bin/yaad-foo` bundled with an unrelated tool, a typo-squatted package, an `npm install` side-effect of a dev tool) becomes part of the trust boundary the moment yaad-index starts.
- **The trust boundary is implicit and shifts under the operator.** Adding a new entry to `$PATH` for unrelated reasons silently expands what yaad-index will execute. The decision of "which plugins is yaad-index allowed to invoke" becomes coupled to the user's general shell environment.
- **Promotion-by-naming is too easy.** A plugin a user previously experimented with and then forgot, but whose binary still lives somewhere on PATH, gets re-registered on every server restart with no opt-in.

The git analogy that motivated the PATH scan in ADR-0005 doesn't carry the same risk weight: `git foo` is invoked by the user explicitly, so the user is the trust check. yaad-index runs as a daemon that fetches arbitrary URLs in response to AI-agent requests; nothing in the request flow asks the operator to vouch for the binary that ends up running.

## Decision

**Discovery is by an explicit config allowlist — an ordered list of `{name, path}` entries. No PATH search.**

The operator names every plugin yaad-index is allowed to invoke. A binary not in the config is not in the registry, regardless of whether it exists on the filesystem or matches the `yaad-*` naming convention.

### Config file

A new YAML file, default location `~/.config/yaad-index/config.yaml` (overridable via `--config` / `YAAD_INDEX_CONFIG`):

```yaml
plugins:
 - name: wikipedia
 path: /home/operator/.local/bin/yaad-wikipedia
 - name: bgg
 path: /home/operator/code/yaad-bgg/yaad-bgg
 - name: web
 path: /opt/yaad/yaad-web
```

- **`plugins` is a list, not a map.** List order is the dispatch priority: when two plugins' `url_patterns` both match a URL, the earlier entry wins (see *Conflict resolution* below). A map shape was rejected because Go's randomised map iteration would scramble priority across server restarts.
- **`name`** labels provenance, log lines, and the future plugin-management surface. Free-form, alphanumeric + dashes. Names must be unique within the file; duplicates are rejected at config load time.
- **`path` is an absolute path.** Relative paths and bare binary names are **rejected** at config load time (`fail-fast` — the server doesn't start if the config is invalid). No PATH search, no `~` expansion-then-resolve, no inheriting from `$PATH`. The operator types the full path or doesn't run the plugin.
- An empty or missing `plugins:` list is allowed; yaad-index runs with zero plugins (every URL falls through to the no-plugin-matched 422 path; see ADR-0002).

The config file may grow other top-level keys later (`store: { path: ... }`, `bind: ...` migrating off CLI flags, etc.). For ADR-0006 only `plugins:` is committed.

### Capability hydration

At startup, for every entry in `plugins`:

1. Server `stat`s the path. Missing file → fail-fast with the offending path + plugin name.
2. Server `exec`s `<path> --init`, reads JSON capabilities from stdout (per ADR-0005's existing `--init` shape), enforces a 5-second wall-clock timeout (matches the per-fetch timeout from ADR-0005's request protocol).
3. The capabilities document populates the same url-pattern → plugin map and kinds catalog ADR-0005 already specifies.

A failed `--init` (non-zero exit, malformed JSON, timeout) → fail-fast on that plugin. v1: the server doesn't start. (A more lenient "skip plugins that fail --init, log + serve the rest" behaviour can land later under explicit operator opt-in; defaulting to fail-fast forces operators to notice broken configs.)

### Per-plugin config delivery

Per the 2026-05-22 amendment (#192), each plugin entry MAY carry a structured `config:` sub-block:

```yaml
plugins:
  - name: github
    path: /usr/local/lib/yaad-index/plugins/yaad-github
    config:
      repos: [acme/proj, beta/widget]
      recent_days: 7
      base_url: https://api.github.com
```

**Arbitrary YAML structure.** The block accepts scalars, lists, nested maps — whatever the plugin's schema declares. The plugin owns its config shape; the daemon doesn't impose a flat-scalar restriction.

**Single JSON env var.** At subprocess spawn time the daemon JSON-marshals the entire `config:` block and delivers it as a single env var named `YAAD_PLUGIN_CONFIG`. Plugins read it on startup with one `os.Getenv` + `json.Unmarshal` into their own struct. The uniform env-var name is intentional — every plugin reads the same name; per-subprocess env isolation keeps the value scoped to its target.

**Plugin declares its schema.** Each plugin's `--init` capabilities document MAY include a `config_schema` field carrying a JSON Schema document (the plugin embeds it verbatim). At registry-load time the daemon validates the operator's `config:` block against the schema and fails fast on mismatch — operators see the violation in the startup log, not at first ingest. Plugins without a declared schema get their config passed through unvalidated (skip-validate, still-marshal).

**Daemon-injected fields.** The daemon writes reserved `_`-prefixed keys into the JSON payload before delivery. v1 injects exactly one:

- `_name` — the entry's `name:` value, so multi-instance plugins (e.g. `github-personal` / `github-work`) read their instance identity without operator-side duplication.

Operator keys starting with `_` are rejected at config load (a defensive guard against shadowing daemon-injected fields). Future iterations may add additional daemon-injected fields under the same `_`-prefix convention (e.g. `_version`, `_position`) without per-field design decisions.

**Affordance — instance-keyed validation profiles.** Because daemon-injected fields land in the same JSON payload the validator runs against, `_name` is visible to JSON Schema's conditional constructs (`if` / `then` / `anyOf` / `oneOf`). A plugin can therefore declare N validation profiles in a single schema and key them on instance identity — e.g. "if `_name == "github-work"`, then `base_url` is required". This is an emergent property of the design (we inject `_name` to remove operator-side duplication; the validator naturally sees it), not a planned feature; documenting it here so operators discover the affordance from the spec rather than from grep.

**Secrets stay in env-passthrough.** The `config:` block lands in operator yaml (typically committed to ops/SCM); secrets like API tokens should NOT live there. Operators expose secrets via the daemon's process environment (docker `-e`, systemd `EnvironmentFile`, etc.); the daemon passes its env to subprocesses by default, so the plugin reads `os.Getenv("YAAD_GITHUB_TOKEN")` directly. The two channels are explicit: `config:` for structured non-secret values; daemon-env for secrets.

### Re-discovery

Same as ADR-0005: registry built once at startup, refreshable via `POST /v1/plugins/refresh` (admin endpoint owned by a future plugin-management ADR). Adding a plugin is now: edit config + refresh. **Not:** drop a binary somewhere and hope.

### No-plugin-matched dispatch

The dispatcher walks registered plugins in registration order and routes the request to the first plugin whose compiled `url_patterns` accept the input. If no plugin claims it:

- **`POST /v1/ingest` returns `422 unsupported_url`** with the canonical envelope (per [ADR-0002](./0002-api-surface.md)):
 ```json
 { "ok": false, "error": "unsupported_url", "message": "no plugin handles URL <X>" }
 ```
- The status is **422, not 400**: the request is well-formed; the server's currently-loaded plugin set just doesn't cover this URL family. 400 is reserved for client-side validation failures (URL malformed, scheme not http/https, `wait_seconds` out of range).
- The status is **422, not 404**: 404 is reserved for "the resource you asked for doesn't exist." The URL might point at a real document — yaad-index just isn't configured to ingest it.
- The shorthand input shape (e.g. `wikipedia: <topic>` from `yaad-wikipedia`) follows the same dispatch — if no plugin's `url_patterns` include the shorthand regex, 422.

The capability advertisement → 422 path forms a single contract: a plugin advertises which input shapes it claims via `url_patterns` in `--init`, and yaad-index turns "no advertisement matches" into a 422 the operator can act on. Adding a plugin (or extending an existing plugin's `url_patterns`) is the canonical fix.

A future "plugin management" surface may surface the unsupported-URL count as a metric (so operators can spot URL families their agents are asking for that no plugin handles), but v1 just emits the 422 and trusts the operator to inspect logs.

### Disambiguation responses

When a plugin's upstream produces multiple plausible candidates for one input — Wikipedia's `Go` page being the canonical case (the programming language, the board game, the verb, …) — the plugin returns a `FetchResult` with `Options` populated instead of a single `Entity`. yaad-index surfaces this as a **200 response with `state: "disambiguation"`** and the options array inline (per [ADR-0002](./0002-api-surface.md)). The caller picks one option's `url` and re-invokes `POST /v1/ingest` to fetch that specific candidate.

#### Architecture: state synthesis is server-side, not plugin-emitted

The plugin contract is "emit data, let yaad-index label." A plugin's `FetchResult` takes one of two shapes — `Entity` (with optional `Gaps` for needs_fill) OR `Options` (for disambiguation). **Two paths, not three.** yaad-index synthesizes the wire-level `state` from which field is set:

| populated field | synthesized state | HTTP status |
|-----------------------------|-------------------|-------------|
| `Options` (≥1) | `disambiguation` | 200 |
| `Entity` + `Gaps` (≥1) | `needs_fill` | 202 |
| `Entity` only | `complete` | 200 |
| (all empty) | `not_found` | 404 |

Plugin authors thus never type the literal strings `"complete"`, `"needs_fill"`, or `"disambiguation"`. The protocol vocabulary lives in yaad-index, not in every plugin binary.

#### Wire shape for the options object

`options` is a `{<id> → {label, summary?}}` object. The map key carries the plugin's canonical id directly — no separate `id` field on the value. No `url` field either: callers re-invoke ingest via the plugin's shorthand input shape (see "Shorthand-by-id contract" below), keyed off the option's id.

```json
"options": {
 "Go_(programming_language)": {"label": "Go (programming language)", "summary": "Open-source language by Google"},
 "Go_(game)": {"label": "Go (game)", "summary": "Ancient Chinese strategy board game"}
}
```

- map key — the plugin's canonical id. Opaque to the caller; treated as a stable handle.
- `label` — human-readable name. Plugin-provided, NOT derived from the id.
- `summary` — a few words to a short sentence helping an agent picking among options disambiguate semantically. Plugin SHOULD include but MAY omit (`omitempty` on the wire).

Keep options lightweight: don't return the full upstream document in each option. The caller will re-invoke ingest with the picked id, which is when the plugin does the real fetch.

#### Shorthand-by-id contract

**Any plugin that emits `Options` MUST also support shorthand input keyed by the option id**, of the form `<plugin>: <id>`. This closes the disambiguation loop without a separate `url` field on each option: the caller picks an id from the options map, builds `<plugin>: <id>`, and POSTs that as the `url` field on a fresh `/v1/ingest` request. yaad-index's dispatcher routes the shorthand back to the same plugin, which resolves the id to the canonical upstream URL and fetches.

Concrete example: yaad-wikipedia's existing `wikipedia: <topic>` shorthand satisfies this contract — the option key (`Go_(programming_language)`) is the canonical wiki path slug, so `wikipedia: Go_(programming_language)` resolves to `https://en.wikipedia.org/wiki/Go_(programming_language)` per the existing shorthand-resolution rule. A future plugin emitting `Options` would add an analogous shorthand if it doesn't already have one — for instance, a `bgg` plugin might support `bgg: 224517` as the by-id form.

This is what makes single-option responses ("is this what you meant?") work: the caller confirms by re-ingesting via the shorthand, which is structurally identical to the multi-option case (pick the only key) — there's no separate confirmation API, just the same re-ingest path.

#### Invariants

- **Stateless plugin.** Plugin holds nothing between calls — no callback ids, no session tokens, no "remember which option I offered last time." Every `POST /v1/ingest` is independent. Plugins are subprocesses (per ADR-0005); statelessness is structural, not a guideline.
- **Idempotent.** Same input → same output. Two ingest calls with the same URL return the same disambiguation list (modulo upstream changing between fetches, which is the same caveat that applies to every plugin response).
- **Caller-decides.** The caller picks one option, ignores all of them, or does something else entirely (e.g. asks the user). yaad-index doesn't auto-resolve, doesn't track which option was picked, doesn't retry.
- **Single-option allowed.** `len(options) == 1` is valid — an "is this what you meant?" confirmation. NOT auto-resolved to that one option. A plugin that wants auto-resolution for a single match should populate `Entity` directly; emitting one option signals "I'm not confident enough to decide for you."
- **Empty options ≠ disambiguation.** A plugin that returns `Options: []` (explicitly empty) is structurally indistinguishable from `Options: nil` — both shapes mean "no candidates," not "disambiguation with zero." yaad-index treats an all-empty `FetchResult` (no Entity, no Options, no Gaps) as `not_found` 404. Disambiguation never carries zero.

#### Considered alternatives

- **HTTP 300 Multiple Choices.** Rejected: semantic mismatch. RFC 9110 §15.4.1 defines 300 as "the same resource available in multiple representations" (e.g. content-language variants of the same article). Disambiguation is "candidate resources for ambiguous input" — different resources, not different representations of one. 200 + an options array makes the schema-level shape explicit instead of overloading the status code.
- **HTTP 422 with options inside the error body.** Rejected: structurally awkward. 4xx responses in this API carry the `{"ok": false, "error": ..., "message": ...}` envelope (per [ADR-0002](./0002-api-surface.md) §"Error envelope"); options would have to ride in `error.context` or similar, which conflates "well-formed alternative results" with "request rejected." Disambiguation is a successful outcome — the plugin DID find candidates, the caller just has to pick. 200 reflects that.
- **Plugin-emitted `state` field.** Rejected during the design review. Asking the plugin to literal-string `"state": "disambiguation"` couples the plugin binary to the host's protocol vocabulary; a future v2 protocol that renames states or adds new ones forces a coordinated bump of every plugin. Server-side state synthesis from populated-field shape keeps the plugin contract "what data did you find?" and the protocol vocabulary "how does the host label that?" cleanly separated.
- **Plugin-held callback state.** Rejected: breaks subprocess statelessness. A protocol where the plugin returns a `disambiguation_id` and the caller re-invokes with `{disambiguation_id, picked_option}` would force the plugin to remember offered options across calls. Subprocess-per-request invocation (ADR-0005) makes that storage the host's problem at best, the plugin's process-level problem at worst. The current shape — caller re-invokes with the picked option's `url` — keeps every call independent.

### What stays from ADR-0005

Unchanged:

- Invocation: subprocess-per-request, JSON over stdio.
- `--init` capabilities document shape.
- `--fetch <url>` (or stdin-JSON-request) request protocol and the response shape.
- Cache + freshness rules (per-kind TTL declared by plugin, overridable).
- Conflict resolution on overlapping URL patterns: registration order wins (here: positional order of the `plugins:` list in the config file — operators control priority by reordering entries).
- Entities are kind-scoped (one `person:tolkien` globally, multiple plugins contribute provenance).

## Consequences

### Positive

- **Trust boundary is explicit.** "Which plugins can yaad-index invoke" is a question with a one-file answer. Operators can audit it; agents can't influence it.
- **No PATH coupling.** Adding a binary to `~/.local/bin` for an unrelated tool can't promote it into yaad-index's runtime. Operators don't have to think about PATH hygiene to keep yaad-index safe.
- **Reproducible deployments.** Two machines with the same config file behave the same regardless of their respective `$PATH` values.
- **Test seam.** Tests register plugins via in-memory configs (no temp files) without needing a writable PATH.

### Negative / costs

- **Operator overhead per plugin.** Adding a plugin is now an explicit edit to the config file rather than a `cp` to `~/.local/bin`. For a single-user personal tool that's marginal; the security gain is worth the friction.
- **Less discoverable.** A user who installs `yaad-bgg` via `go install` and expects it to "just work" is going to be confused. README + the future plugin-management ADR need to spell out the config step.
- **No first-class "find plugins for me" mode.** Future ADR can add an `--scan-and-suggest` mode that walks PATH and prints config suggestions without auto-registering — keeps the security boundary while restoring some of the ergonomics.

### Migration from ADR-0005

ADR-0005's *Discovery* section is superseded. Implementations should not read `$PATH`. The `yaad-*` binary-name convention can stay as a soft convention (humans recognise the executables as yaad-index plugins), but the convention is no longer load-bearing for runtime discovery. The `plugins.<binary_name>.path` config override mentioned at the end of ADR-0005's Discovery section is now the *only* registration mechanism, not a fallback.

ADR-0005's other decisions (invocation, request protocol, cache, conflict resolution) are intact. A future PR may add a "Superseded by ADR-0006" annotation to ADR-0005's Discovery section to keep ADR-0005 self-consistent for readers.

## Open questions

- **Config validation strictness.** Should yaad-index reject relative paths verbatim, or `os.UserHomeDir`-resolve `~/`? Current decision: full absolute paths only, no expansion. Revisitable if it becomes annoying.
- **Multi-config-file merging.** A single user file is enough for v1. If an operator wants to ship a system-wide allowlist + per-user additions later, that's a future ADR.
- **Plugin-binary integrity.** v1 trusts the path the operator names — no checksum verification, no signature check. A future ADR can add `sha256:` per-plugin in the config if a real attack model demands it. The concrete threat model that would justify it is the symlink vector: an attacker who can write to a directory the configured path *resolves through* (e.g., a writable parent directory that contains a symlink the operator named) can swap the binary out from under yaad-index without touching the config. A `sha256:` field would close that gap by failing fast when the binary's content drifts. Today the file-system permissions on the resolved path are the integrity boundary.

## Action items if approved

1. Implement the config loader (`internal/config/`) — parses YAML, validates `plugins.*` values are absolute paths and the files exist + are executable, returns a typed config struct.
2. Implement the subprocess plugin substrate (`internal/plugins/subprocess/`) — wraps a binary path in the `Plugin` interface from ADR-0005's invocation section, calling `--init` for capabilities and `--fetch <url>` (or the stdin-JSON-request shape ADR-0005 specifies) for ingestion.
3. Wire `cmd/yaad-index/main.go` to load the config at startup, hydrate the registry from `plugins:`, and pass the registry into `api.NewHandler`.
4. Drop the in-tree `internal/plugins/wikipedia/` from any branch; Wikipedia ships as a standalone repo (`yaad-wikipedia`) that builds to a `yaad-wikipedia` binary implementing the protocol from ADR-0005's request section.
5. Annotate ADR-0005's Discovery section with a "**Superseded by ADR-0006**" line so a reader of ADR-0005 alone doesn't follow stale guidance.
6. README + INSTALL.md updates: how to write a config, how to register a plugin, the security rationale for full-path-only.

## Revisions

### 2026-05-22 — structured `config:` block + JSON-Schema validation (#192)

The original ADR's `config:` sub-block was an undeclared open question — the implementation that landed via #7 supported only flat scalar values converted to per-key env vars (`<PLUGIN_UPPER>_<KEY_UPPER>`). yaad-github's `repos: [list]` requirement surfaced that the scalar shape was too narrow.

Changes in this amendment (added as the "Per-plugin config delivery" section above):

- **Arbitrary YAML structure** in the `config:` block — scalars, lists, nested maps all accepted. Plugin owns its schema.
- **Single uniform env var.** Daemon JSON-marshals the whole block and delivers via `YAAD_PLUGIN_CONFIG`. Plugin reads with one `os.Getenv` + `json.Unmarshal`.
- **JSON Schema validation.** Plugin's `--init` capabilities grow a `config_schema` field (JSON Schema draft 2020-12). Daemon validates the operator's `config:` against the schema at registry-load time + fails fast on mismatch. Plugins without a declared schema skip validation.
- **Daemon-injected fields.** Reserved `_`-prefix convention; v1 injects `_name`. Operator keys starting with `_` rejected at Load.
- **Scalar → per-key env var conversion REMOVED.** The original #7 convention (`<PLUGIN_UPPER>_<KEY_UPPER>` per scalar key) goes away. Existing plugins (yaad-bgg, yaad-gmail, yaad-wikipedia, yaad-github) migrate to read JSON config + declare their schema in subsequent per-plugin PRs. Operators in the migration window can keep secrets at the daemon-process env layer (env-passthrough remains the only secrets channel).
