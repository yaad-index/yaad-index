# ADR-0022: Plugin command-protocol — `commands` field, `!` invocation sigil, in-memory job system, routing-time validation, CLI surface

## Status

Proposed 2026-05-10. Response-shape clause (the "Response shape: multi-envelope for command-shape" subsection under Decision §1) superseded by ADR-0023 (NDJSON streaming, one envelope per line, applied uniformly to URL-shape and command-shape responses). Commands field, `!` invocation sigil, in-memory job system, routing-time validation, and CLI surface remain in force.

Amended 2026-05-22: per-command `operator_only` flag on `CommandSpec` replaces the blanket-operator-only-on-command-shape rule. The original §5.3 wording is preserved below as the *CLI-side* contract (cron + manual operator invocations) and the *daemon-side* ingest gate is now per-command, defaulting to agent-callable. §1 was extended to document the long-form CommandSpec wire shape; §5.3 was rewritten to reflect the per-command rule.

## Depends on

- [ADR-0005](./0005-plugin-lifecycle.md) — plugin invocation model (subprocess-per-request, JSON over stdio, `--init` capabilities). The command-protocol extends that contract with a parallel invocation shape.
- [ADR-0006](./0006-plugin-discovery-config-allowlist.md) — config-allowlist discovery + `url_patterns` routing. The command-protocol adds a sibling `commands` field with its own routing path.

## Context

Plugins today produce entity references from URL-shape inputs: a plugin advertises `url_patterns: [...]` in its `--init` capabilities document, the daemon walks the registered plugins in registration order on every ingest, and the first plugin whose pattern matches handles the request. The model fits sources where every artifact has a stable URL identity — Wikipedia articles, BGG boardgame pages — and breaks when the source has no URL form yaad-index can dispatch against.

yaad-gmail is the motivating case: Gmail messages don't have a URL the daemon can route on. The plugin's job is poll-driven (walk Gmail's IMAP-side un-ingested set, emit per-message envelopes back to the daemon) — there is no input shape an agent or external scheduler can express as `gmail:<url>`. The plugin needs an imperative invocation shape.

Three design pressures decided the resulting protocol:

1. **The advertised name and the invocation form should differ.** A plugin advertises bare command names in its capabilities (e.g. `commands: ["fetch"]`); callers invoke them with a sigil (`gmail: !fetch`). Mixing the sigil into the advertised name pulls operator-side syntax into the plugin's own contract.

2. **Plugin invocations need cycle-level idempotency.** A misconfigured cron plus a slow Gmail poll could spawn overlapping yaad-gmail subprocesses; the daemon should collapse identical concurrent invocations to a single job. This applies to every plugin invocation, not just commands — overlapping URL-pattern fetches on the same input also benefit from collapse.

3. **Routing-time validation is cheaper than spawn-then-fail.** A `gmail: !sync` request when yaad-gmail declares only `commands: ["fetch"]` should reject before the subprocess fork. Same for URL-shape requests whose pattern doesn't match the named plugin's `url_patterns`.

The CLI is the operator's primary surface for one-off command invocations + automated scheduling (external cron). It needs subcommand support for both invocation shapes and operator-attributed authentication.

## Decision

### 1. `commands: [...]` field on `--init` capabilities

Plugins advertise the imperative commands they expose by extending the `--init` capabilities document with a `commands` field, parallel to `url_patterns`:

```json
{
 "name": "gmail",
 "version": "0.3.0",
 "url_patterns": [],
 "entity_kinds": [{"name": "source"}],
 "edge_kinds": [],
 "canonical_kinds_emitted": ["email", "email-address", "label"],
 "canonical_edge_types_emitted": ["bcc", "cc", "from", "is_a", "is_about", "tagged_as", "to"],
 "source_namespace": "gmail",
 "commands": ["fetch"]
}
```

- **`commands` is a list of bare strings *or* objects.** Per the 2026-05-22 amendment, each entry may be either a bare-string shorthand (`"fetch"`) or the long-form object (`{"name":"delete-all","operator_only":true}`). The bare-string shorthand decodes with `operator_only=false` — the default agent-callable shape. The long-form object is required when the per-command operator-only gate must be engaged (see §5.3 below). No leading `!` in either form — the sigil is a property of the invocation syntax, not the advertised vocabulary. Empty / absent `commands` is valid and back-compatible — plugins that only do URL-shape ingest (yaad-wikipedia, yaad-bgg) advertise nothing here and the daemon's command-routing path never reaches them.
- **Names follow the same shape rules as `url_patterns` entries on the operator side**: the daemon parses + persists the list verbatim alongside `url_patterns` in its capability cache. No deduplication, no canonicalization beyond what the plugin emits.
- **Plugins receive a command invocation via a subprocess flag** (per ADR-0005's existing subprocess-per-request shape). The wire format mirrors `--init` and `--version`: `<plugin-binary> --command <name>`. The plugin's `--command` handler runs the imperative work and writes a structured response to stdout.

#### Response shape: multi-envelope for command-shape

> **⚠️ Superseded by [ADR-0023](./0023-unified-plugin-response-protocol.md):** the `envelopes: [...]` array shape documented in this subsection is replaced by NDJSON streaming on stdout (one envelope per line, plugin exits when done). The unification covers both URL-shape and command-shape responses uniformly; the URL-vs-command distinction stays at the *invocation* layer (`!` sigil) only. Per-envelope errors move to inline `_error` lines; aggregate stats move to an optional `_summary` packet. Read ADR-0023 before applying the rules in this subsection.

Command-shape invocations differ from URL-shape on the response wire. URL-shape is **1:1** (one URL input → one `Entity` or `Options` per ADR-0005's existing `FetchResult` contract). Command-shape is **1:N** (one command invocation → N source emissions; yaad-gmail's `fetch` produces one envelope per un-ingested message, possibly hundreds per cycle).

The command-shape response envelope is therefore a list-shape:

```json
{
 "ok": true,
 "envelopes": [
 { "id": "gmail:<source-slug-1>", "kind": "source", ... },
 { "id": "gmail:<source-slug-2>", "kind": "source", ... }
 ],
 "ingested": 2,
 "errors": []
}
```

`envelopes` is the list of source-shape entity emissions; the daemon iterates and routes each through the same persist-to-vault path the URL-shape ingest uses. `ingested` is the operator-facing count. `errors` carries per-message non-fatal failures (transient parse errors, etc.); a fully-failed cycle returns `ok: false` with `error` + `message` populated and an empty `envelopes`.

This is a NEW wire shape over what ADR-0005 currently documents (which is single-entity-per-request). The command-shape response shape is committed to ADR-0022 explicitly; ADR-0005's response section may be re-amended later if URL-shape ever grows multi-envelope support, but URL-shape is intentionally single-entity for now.

### 2. `<plugin>: !<command>` invocation syntax

The daemon's input parser recognizes two invocation shapes against the registered plugin set:

- **URL-shape** — `<plugin>: <pattern>` (existing — covers full URLs, plugin-shorthand inputs like `wikipedia: Tehran`).
- **Command-shape** — `<plugin>: !<command>` (new — `gmail: !fetch`, future `bgg: !sync` if BGG ever grows imperative commands).

The `!` is the discriminator. Anything beginning with `<plugin>: !` routes through command-dispatch; everything else routes through URL-dispatch. The two paths are separate at the daemon level; a plugin's `commands` list and `url_patterns` list don't have to overlap (and typically won't — a plugin is usually URL-shape OR command-shape, not both).

The sigil lives in the **invocation surface** only:

- Operator types `gmail: !fetch` (or `yaad-index command gmail fetch`, see CLI section).
- Plugin advertises bare `"fetch"` in `commands`.
- Daemon strips the `!` at parse time, looks up the bare name against the plugin's `commands` list, dispatches.

This separation lets the protocol grow other sigils later (e.g. `?` for query-shape) without breaking plugin-side contracts.

### 3. In-memory job system — covers ALL plugin invocations

The daemon maintains an in-memory map of active plugin subprocesses keyed on the invocation identity:

- **URL-shape key**: `<plugin>:<pattern>` (e.g. `bgg:https://boardgamegeek.com/boardgame/...`).
- **Command-shape key**: `<plugin>:<command>` (e.g. `gmail:fetch`).

The map's value is the job state: a unique job-id (UUID; surfaced to the caller), the subprocess handle (`*exec.Cmd`), the invocation key, started-at timestamp, and optionally a last-checked-at for the sanity-poll path.

#### Dispatch contract

On every plugin invocation (URL-shape or command-shape):

1. Daemon parses the input + resolves the plugin + computes the invocation key.
2. Daemon checks the job map.
 - **Existing entry**: return the existing job-id with no new spawn (idempotent on already-running).
 - **No entry**: spawn the subprocess, insert a new entry, return the new job-id.

The dispatch contract is the same whether the request comes through `POST /v1/ingest` (URL-shape from agent), through the `command` HTTP endpoint (command-shape from CLI), or through any future routing surface. Concurrent identical dispatches collapse to a single subprocess + a single job-id; the agent flow that previously spawned overlapping yaad-bgg fetches on the same URL now collapses cleanly.

#### Cleanup contract

Two paths, primary + backstop:

1. **Primary: subprocess-exit detection.** Each spawned subprocess has a goroutine that calls `cmd.Wait()`. When `Wait` returns (subprocess exited — successful, errored, or signal-killed), the goroutine removes the entry from the job map. This is automatic + synchronous-to-exit; every plugin invocation lifecycle ends with map-removal as a deterministic event.

2. **Backstop: 5-second sanity poll.** A separate goroutine sweeps the map every 5 seconds, checks each entry's subprocess for liveness via `Process.Signal(syscall.Signal(0))` (or platform equivalent), and removes entries whose subprocess is no longer alive. This catches the rare case where the primary path missed the exit signal (zombie process, lost-goroutine scenarios). The 5-second cadence is a sanity check, not the load-bearing cleanup — primary path handles 99% of cases.

The sanity poll is intentionally coarse. A finer cadence would consume polling cost without proportional benefit; the cleanup-at-exit path is the contract, and the poll is the belt-and-suspenders.

#### Restart kills jobs

The job system is in-memory only; no persistence. Daemon restart drops the entire job map, killing tracked subprocesses (the daemon-as-parent process exit signals the kernel-level subprocess teardown). Acceptable in v1 for two reasons:

- Plugin invocations are designed restart-tolerant. yaad-gmail's bidirectional label flow keeps state on Gmail, not in the daemon's job map; the next poll cycle re-runs the same search and finds the same un-ingested set. URL-shape fetches are stateless on the daemon side (cache lookup happens at ingest, not at job track).
- Persisting job state across restart adds storage + recovery complexity that doesn't pay back the rare-case "job was running when daemon restarted" scenario. The agent or operator re-invokes if needed.

A future ADR may add `/v1/jobs/<id>` lookup or cancel-running-job; out of scope here.

### 4. Routing-time validation — reject fast before spawn

The daemon validates the input shape against the named plugin's declared capabilities BEFORE spawning the subprocess + BEFORE the in-memory job system check:

- **URL-shape requests** (`<plugin>: <pattern>`): the **full input string** (including the `<plugin>:` namespace prefix) must regex-match at least one of the plugin's compiled `url_patterns` entries. Mismatch → 400 with `{ok: false, error: "invalid_input", message: "<reason>", field: "input"}`. No subprocess, no job entry.

 The `url_patterns` entries are full regular expressions that match the entire input string, not namespace-only prefixes. yaad-wikipedia's patterns illustrate the shape: `^https?://[a-z]{2,3}(\.m)?\.wikipedia\.org/wiki/.+` (matches the full URL form) and `(?i)^wikipedia:\s*(\S.*)$` (matches the full shorthand form including the `wikipedia:` prefix). A full-input regex match decides whether to route to the plugin AND whether the input is shape-valid in one pass.

 Inputs that match the plugin namespace (`bgg:`) but fail the full regex (e.g. `bgg: not-a-url-shape-i-handle`) reject at the daemon's routing layer. Inputs that match the regex pass through to the subprocess; the plugin owns the second-layer semantic validation.

- **Command-shape requests** (`<plugin>: !<command>`): the bare `<command>` (sigil-stripped, namespace-stripped) must exact-match a string in the plugin's `commands` list. Mismatch → same 400 envelope.

 Exact-match is the contract — `gmail: !sync` against a plugin advertising `commands: ["fetch"]` rejects without spawn. Substring / prefix matches are not respected; the bare command name is the whole identity.

Validation runs before the job-map check. A repeated invalid invocation never inserts a job-map entry; the dispatch costs are bounded by the parse + lookup, not by spawn-then-fail.

The plugin still owns deep validation (semantic-level checks the daemon can't make). Routing-time validation is the cheap shape-check; plugin-side is the second layer.

### 5. CLI surface — `command` + `fetch` subcommands; operator-only auth

The single `yaad-index` binary gains two subcommands alongside `serve`, `reindex`, `plugins clear-cache`, `plugins reprobe`:

- `yaad-index command <plugin> <cmd>` — dispatches a command-shape invocation. `yaad-index command gmail fetch` is equivalent to a request body of `gmail: !fetch`.
- `yaad-index fetch <plugin> <pattern>` — dispatches a URL-shape invocation. `yaad-index fetch bgg "ticket to ride"` is equivalent to `bgg: ticket to ride`.

#### Wire format: CLI → daemon

Both subcommands concatenate the parsed args into the existing single-string input shape `<plugin>: <suffix>` (URL-shape: `<plugin>: <pattern>`; command-shape: `<plugin>: !<cmd>`) and POST it as the `input` field on the daemon's existing dispatch endpoint. No new request shape; the CLI is a typed wrapper around the same wire format an agent would POST directly.

Concretely:
- `yaad-index fetch bgg "ticket to ride"` → `POST /v1/ingest {"url": "bgg: ticket to ride"}` (or whichever endpoint the daemon's dispatch path uses for URL-shape; the existing `/v1/ingest` is the natural target).
- `yaad-index command gmail fetch` → `POST` to the daemon's command-dispatch endpoint with `{"input": "gmail: !fetch"}` (the daemon discriminates by the `!` sigil; the command-dispatch endpoint can be the same `/v1/ingest` parser-wise OR a sibling `/v1/command` — the implementation issues / decide).

The reconstruction is mechanical (string concatenation); the CLI does not invent a new wire format. This keeps the daemon's dispatch contract single-shaped: every dispatch sees `<plugin>: <suffix>` regardless of how it arrived.

Both subcommands talk to the running daemon over HTTP (same pattern as `reindex`). External cron uses `yaad-index command gmail fetch` to schedule polling — no special cron-vs-CLI auth path; the daemon sees the same HTTP request shape regardless of the caller.

Output: command-shape prints the returned job-id (caller can later inspect via the daemon's logs/metrics or the future `/v1/jobs/<id>` surface); fetch-shape prints the success envelope. Non-zero exit on dispatch failure (daemon not running → connection-refused, validation failure → 400 surfaced as exit 1).

#### Authentication: per-command operator-only flag (2026-05-22 amendment)

The original `#### Authentication: operator-claim-only` decision read every command-shape dispatch as an operator action and rejected pair-claim tokens at the daemon's gate with a blanket `operator_only_required` error. That rule is too strict for agent flows: an agent invoking a benign `gmail: !fetch` — the motivating case for this amendment, since MCP-routed agent dispatch hit the blanket-reject — wants to dispatch the same imperative the operator's cron does, and the audit trail already distinguishes them via JWT.sub.

The amendment moves the gate from "all command-shape" to "per-command, declared on the CommandSpec":

- Each `CommandSpec` carries an optional `operator_only: bool` field (default false → agent-callable).
- The daemon's `/v1/ingest` command-shape gate looks up the dispatched command's spec on the named plugin and:
  - Pair-claim tokens (Subject ≠ Operator, distinct subject + operator claims, per ADR-0019) → **accepted** when `operator_only=false`, **403 operator_only_required** when `operator_only=true`.
  - Operator-only tokens (Subject == Operator) → **accepted** for every command regardless of the flag.
  - Anonymous dev-mode claims (`auth.required=false`) → **accepted** for every command (matches the URL-shape gate's permissive shape so dev-mode behavior doesn't regress).
- Routing-time validation (§4) still runs first; an unknown plugin or unknown command rejects at the routing layer before the auth gate evaluates the flag.
- Provenance audit trail: JWT.sub continues to carry the invoking subject (`sub=<agent>` for pair-claims, `sub=<operator>` for operator-only). No provenance shape change.

The CLI surface (§5's `yaad-index command` / `yaad-index fetch`) is unaffected by the per-command flag: the CLI mints operator-only tokens (or carries an operator-claim-bearing JWT) and POSTs to the same `/v1/ingest` endpoint as any other caller. The per-command flag only changes what *non-operator* tokens may invoke. Operator-only tokens retain full access to every command, so the CLI-as-operator-shell semantic is preserved.

**Plugin-side migration.** Plugins predating this amendment emit `commands: ["fetch"]` (bare-string array) and decode with `operator_only=false` on every entry — agent-callable by default. Plugins that want to gate a destructive command emit the long-form object explicitly (`{"name":"delete-all","operator_only":true}`). No plugin in the current set declares an operator-only command; the default-callable shape unblocks the agent flows that motivated the amendment without ceremony.

**Wire-shape back-compat.** The daemon's `Capabilities.Commands` decoder accepts both wire shapes (string or object) via a custom JSON UnmarshalJSON on `CommandSpec`. Plugin binaries that emit only bare strings (yaad-gmail, yaad-github at amendment time) need no rebuild — the daemon reads their existing capabilities document unchanged.

**Operator-facing surface.** `GET /v1/plugins` now mirrors the CommandSpec shape: each command serializes as a bare string when `operator_only=false` (preserving the pre-#107 wire shape) and as the long-form object when `operator_only=true`. SKILL.md generators that walk `/v1/plugins` therefore see the per-command flag exactly when it's set.

## Out of scope (for this ADR)

- **Per-plugin command declarations.** Each plugin is responsible for its own `commands` list. yaad-gmail declares `["fetch"]` (separate issue); future plugins declare what they need.
- **Job persistence across daemon restart.** v1 is in-memory; restart kills jobs. A future ADR may add persistence + `/v1/jobs/<id>` lookup if the use case surfaces.
- **Cancel-running-job.** No `DELETE /v1/jobs/<id>` surface. The subprocess runs to completion or the daemon kills it on restart.
- **Plugin sigils beyond `!`.** Future shapes (`?` for query, `@` for ...) are intentionally not specified. The discriminator architecture leaves room; this ADR commits to `!` only.
- **CLI auth-token rotation / lifecycle.** Token issuance + rotation are separate plumbing (not in this ADR's scope).

## Consequences

### Positive

- **Plugin-side simplicity.** A plugin author adds one field to `--init`, implements a `--command <name>` handler, and the daemon's dispatch + job-tracking + validation come for free. No per-plugin job-tracking code, no per-plugin idempotency logic.
- **CLI parity with daemon-side dispatch.** Operators get the same invocation surface their automation uses. External cron + manual ad-hoc invocation share one entry point.
- **Idempotency-by-default for all invocations.** URL-shape fetches that previously could spawn overlapping yaad-bgg subprocesses on the same URL now collapse to a single job. Free regression-fix on the existing path.
- **Routing-time validation cheapens the misconfigured-input path.** A typo in a cron line (`gmail: !syncc` vs `gmail: !sync`) rejects in microseconds, not after a 5-second subprocess timeout.

### Trade-offs

- **In-memory job state is lost on restart.** A long-running yaad-gmail poll that was mid-cycle when the daemon restarted gets re-invoked on the next cron tick (which is fine for poll-shape; less fine for hypothetical long-running blocking commands — none today). Documented as acceptable.
- **The `!` sigil is a hard syntactic split.** A plugin can't have a single namespace for both URL-shape and command-shape invocations; the discriminator forces explicit choice at the call site. Considered the smaller cost vs sigil-less ambiguity ("does `gmail: foo` mean a URL-shape `foo` or a command-shape `foo`?"). Operators benefit from the visible discriminator.
- **The 5-second sanity poll has a worst-case 5-second-stale window.** A subprocess that exited immediately after the primary-path goroutine missed its `Wait` return (rare; effectively a goroutine bug) appears in the job map for up to 5 seconds. Idempotency-by-key returns the stale job-id during that window — acceptable for the rare-case scenario; the next sanity sweep clears it.

### Plugin-side migration

- **No-commands plugins** (yaad-wikipedia, yaad-bgg today) need zero changes — the absent `commands` field is back-compatible.
- **yaad-gmail** declares `commands: ["fetch"]` + adds the `--command fetch` subprocess flag. Reuses the existing poll-cycle code; the wire-routing is the only new path.
- **The job system applies retroactively** to URL-shape dispatches. yaad-bgg + yaad-wikipedia get job-tracking + idempotency-protection without any plugin-side change — the daemon owns it.

## Implementation cascade

The implementation lands across separate issues (this ADR is the foundation; not the implementation):

- **** — daemon-side `commands` field plumbing in capability cache + invocation parser for `<plugin>: !<command>`.
- **** — in-memory job system covering all invocations.
- **** — routing-time validation.
- **** — CLI subcommands `command` + `fetch`.
- **** — CLI auth (operator-claim-only).
- **** — yaad-gmail declares `commands: ["fetch"]` + adds the handler.

This ADR is the contract; the implementation issues cite it.
