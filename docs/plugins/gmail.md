# yaad-gmail

Gmail extractor plugin. Lives in this monorepo at `cmd/yaad-gmail/`. **Command-shape plugin** per [ADR-0022](../../adr/0022-plugin-commands.md) — Gmail messages don't have a yaad-index-dispatchable URL form, so the binary surfaces `fetch` as a named command invoked via `gmail: !fetch` (operator-only-claim) rather than a URL match. Each invocation connects to Gmail via IMAP + Gmail's `X-GM-LABELS` extension, walks the un-ingested set, emits per-message NDJSON envelopes, and exits. State lives entirely on Gmail (in the configured `ingested_label`); restart-safe by design.

For the plugin protocol contract this implements, see [`docs/plugin-flow.md`](../plugin-flow.md). For the agent-/operator-facing view of `/v1/ingest` against a command-shape plugin, see [`docs/ingest.md`](../ingest.md).

## Build

```sh
make build              # → bin/yaad-gmail alongside bin/yaad-index, etc.
```

Drop the binary somewhere the daemon can read + execute it.

## Register with the daemon

```yaml
# ~/.config/yaad-index/config.yaml
plugins:
  - name: gmail
    path: /home/operator/.local/bin/yaad-gmail
    fetch_timeout: 5m        # optional; daemon default is 60s (subprocess.DefaultFetchTimeout)
```

After editing the config, restart the daemon. On startup it calls `yaad-gmail --version` (cheap probe); on cache-miss / version-change it falls through to `yaad-gmail --init` to refresh the capabilities row in `plugin_capabilities`. Subsequent starts where the version matches skip the full `--init` (see [`docs/plugin-flow.md`](../plugin-flow.md) §1).

`fetch_timeout` is the wall-clock budget the daemon enforces per `yaad-gmail fetch` invocation. The default suits a small steady inbox; operators with large backlogs (initial sync, busy mailboxes) should raise it — a 137-envelope fetch measured ~10s in repro, so a 5-minute budget holds roughly 30× headroom. When exceeded, the subprocess is `SIGKILL`'d and the error surfaces as `fetchTimeout=<value> exceeded`. Accepts any `time.ParseDuration` shape (`30s`, `5m`, `1h`).

## Invocation

Operator-side: `gmail: !fetch` as the `url` field of `POST /v1/ingest`, or via the CLI:

```sh
yaad-index command gmail fetch
```

The `!` sigil declares the command-shape input per [ADR-0022](../../adr/0022-plugin-commands.md). The daemon validates the operator-only claim, dispatches to `yaad-gmail`, and streams the NDJSON envelopes back. The daemon does NOT have a polling scheduler — operators (or operator-driven automation, e.g. an operator-claim cron) trigger the fetch cycle on whatever cadence they want.

## Auth + configuration

Auth shape: **IMAP + app-specific password + Gmail's `X-GM-LABELS` IMAP extension**. No OAuth, no Gmail API, no XOAUTH2.

Operator config flows in via env vars (yaad-index inherits env into the subprocess per [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md)):

| Env var | Default | Purpose |
|---|---|---|
| `YAAD_GMAIL_ACCOUNT` | (required) | Gmail account address (e.g. `you@gmail.com`). |
| `YAAD_GMAIL_APP_PASSWORD` | (required) | Gmail app-specific password (16-char generated). |
| `YAAD_GMAIL_INGESTED_LABEL` | `yaad-ingested` | Label written via X-GM-LABELS after successful ingest. Empty string disables. |
| `YAAD_GMAIL_SKIP_LABEL` | `yaad-skip` | Label that, when present on a message, blocks ingest. Empty string disables. |
| `YAAD_GMAIL_IMAP_HOST` | `imap.gmail.com` | IMAP host override (test fixtures). |
| `YAAD_GMAIL_IMAP_PORT` | `993` | IMAP port override. |

App passwords are generated at https://myaccount.google.com/apppasswords (2-factor required). Empty `YAAD_GMAIL_INGESTED_LABEL` means EVERY invocation re-ingests every message — useful only when the operator wants zero-state-on-Gmail.

Also honored from the daemon-injected environment (per [`docs/plugin-flow.md`](../plugin-flow.md) §6): `YAAD_TIMEZONE` — when set, used to stamp `provenance.fetched_at` so timestamps land in the operator's expected TZ.

## Capabilities document (`yaad-gmail --init`)

```json
{
  "name": "gmail",
  "version": "<buildinfo.Version>",
  "url_patterns": [],
  "entity_kinds": [{ "name": "source" }],
  "edge_kinds": [],
  "canonical_kinds_emitted": ["email", "email-address", "label"],
  "canonical_edge_types_emitted": ["bcc", "cc", "from", "is_a", "is_about", "tagged_as", "to"],
  "source_namespace": "gmail",
  "commands": ["fetch"]
}
```

`url_patterns: []` confirms this is a command-shape-only plugin; the daemon never dispatches `/v1/ingest` to this binary via URL match. The default-mode invocation (no `--command` arg) runs a full poll cycle and exits — kept for the bare-`yaad-gmail` CLI test path; production traffic flows through `--command fetch` from the daemon's subprocess driver.

Per [ADR-0021](../../adr/0021-daemon-owns-slug.md) the `entity_kinds[].name` is the universal `"source"` value; every yaad-gmail envelope carries `structured.kind: "source"`. Source-type identity (`source-type:gmail`) materializes on the `is_a` edge.

## Wire shape — `yaad-gmail --command fetch`

NDJSON on stdout, one envelope per line (per [ADR-0023](../../adr/0023-unified-plugin-response-protocol.md)). One per matched message; the cycle terminates with an optional `_summary` packet, then exit 0.

```json
{
  "ok": true,
  "structured": {
    "kind": "source",
    "name": "Sample subject line",
    "data": {
      "message_id": "<sample-message-id@example.test>",
      "date": "2026-05-20T10:00:00Z",
      "subject": "Sample subject line",
      ...
    },
    "edges": {
      "is_a":      [ { "name": "gmail",                 "kind": "source-type" } ],
      "is_about":  [ { "name": "gmail-sample-msg-id",   "kind": "email" } ],
      "from":      [ { "name": "alice_at_example_dot_test", "kind": "email-address" } ],
      "to":        [ { "name": "bob_at_example_dot_test",   "kind": "email-address" } ],
      "cc":        [ { "name": "carol_at_example_dot_test", "kind": "email-address" } ],
      "tagged_as": [ { "name": "inbox", "kind": "label" } ]
    },
    "provenance": [ { "source": "gmail", "fetched_at": "2026-05-20T10:00:00Z", "ok": true } ]
  },
  "raw_content": "<RFC-822 body, plain-text part preferred, HTML fallback>"
}
```

Followed by a control packet at end of stream:

```json
{ "_summary": { "matched": 12, "ingested": 12, "skipped": 0, "errors": 0 } }
```

## Bidirectional label flow + restart-safety

Labels function as both the control surface AND the durable state mechanism. No client-side state file, no UID persistence, no UIDVALIDITY tracking. Restart-safe by design — the next invocation re-runs the same search predicate and finds the same un-ingested set.

Per cycle:

1. **Search**: IMAP `X-GM-RAW "-label:<ingested_label> -label:<skip_label>"` against `INBOX` + `[Gmail]/Sent Mail`.
2. **Read** Gmail labels per matching message via `UID FETCH (X-GM-LABELS)` → emit `tagged_as` edges to `label:<label-slug>` canonical entities. The control-plane labels (`ingested_label`, `skip_label`) are NEVER surfaced as edges — they're filtered out at the assembly layer.
3. **Write** `ingested_label` via `UID STORE +X-GM-LABELS` immediately after successful ingest. Marks the message as seen.
4. **Refetch on label-removal**: an operator removes the `ingested_label` on Gmail-side → the next invocation's search predicate matches the message again → re-ingest.
5. **Skip-label**: messages carrying `skip_label` are excluded by the search predicate. Operator adds this label on Gmail to block ingest.

## Source slugs + canonical edges

Source-shape entity ID: `gmail:<source-slug>` where `<source-slug>` = `<subject-slug>-<message-id-slug>` from the RFC-822 `Message-ID:` header (NOT the IMAP UID — Message-ID is globally unique + stable across mailbox moves).

Canonical kinds + slugs:

- `email:gmail-<message-id-slug>` — the `is_about` target.
- `email-address:<addr-slug>` — `_at_` for `@`, `_dot_` for `.`. Example: `alice.user@example.test` → `email-address:alice_dot_user_at_example_dot_test`.
- `label:<label-slug>` — `_slash_` for `/` to preserve hierarchy. Example: `Inbox/Active` → `label:inbox_slash_active` (distinct from flat `Inbox Active` → `label:inbox-active`).

Edges from `gmail:<source-slug>`:

| Edge type | Target | Multiplicity | Notes |
|---|---|---|---|
| `is_about` | `email:gmail-<message-id-slug>` | 1 | Canonical-axis edge. |
| `is_a` | `source-type:gmail` | 1 | Universal source-type per [ADR-0021](../../adr/0021-daemon-owns-slug.md). |
| `from` | `email-address:<addr-slug>` | 1 | Sender. |
| `to` | `email-address:<addr-slug>` | many | Each To-header recipient. |
| `cc` | `email-address:<addr-slug>` | many | Each Cc-header recipient. |
| `bcc` | `email-address:<addr-slug>` | many | **Sent-folder only** (inbound never carries visible BCC). |
| `tagged_as` | `label:<label-slug>` | many | One per Gmail label (control-plane labels filtered). |

`email-address` and `label` ship as bare canonical-label entities — no plugin-defined gaps. Gap schema design lands in dedicated follow-up issues.

## Development

From the monorepo root:

```sh
make help              # list targets
make check             # vet + build + test + fmt + lint + tidy
go test ./cmd/yaad-gmail/... ./internal/gmail/...
```

The plugin's library lives at `internal/gmail/` (IMAP transport, parsing, slug derivation); the binary lives at `cmd/yaad-gmail/`.

## Status

Gmail integration live. Threading (`X-GM-THRID`), IMAP IDLE / push-watch, and label gap schemas are deferred follow-ups.

## References

- [`docs/plugin-flow.md`](../plugin-flow.md) — plugin-author-seat reference for the protocol contract.
- [`docs/ingest.md`](../ingest.md) — agent-/operator-facing view of `/v1/ingest`.
- [`docs/plugins/gmail-skill.md`](./gmail-skill.md) — agent-facing skill doc for working with Gmail entities.
- [ADR-0006](../../adr/0006-plugin-discovery-config-allowlist.md) — config allowlist + first-match-wins.
- [ADR-0008](../../adr/0008-vault-as-source-of-truth.md) — vault-canonical persistence.
- [ADR-0021](../../adr/0021-daemon-owns-slug.md) — daemon owns slug derivation; universal `"source"` kind.
- [ADR-0022](../../adr/0022-plugin-commands.md) — command-shape plugin protocol.
- [ADR-0023](../../adr/0023-unified-plugin-response-protocol.md) — unified NDJSON wire.
- Gmail's `X-GM-LABELS` IMAP extension — https://developers.google.com/workspace/gmail/imap/imap-extensions
- [`emersion/go-imap`](https://github.com/emersion/go-imap) — IMAP transport library used.
