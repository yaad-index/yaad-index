# yaad-gmail

> ⚠️ **DESIGN IN FLUX — WE WILL BREAK THINGS.**
>
> This plugin tracks yaad-index's iterating plugin / cache / API surface and is **NOT stable**. Wire shapes, the `--init` capabilities document, slug derivation, the Notations contract, and operator-facing flags may change without notice or migration path until a `stable` flag is set on a future yaad-index release. Treat any version of these interfaces as ephemeral.

Gmail extractor plugin for [yaad-index](https://github.com/yaad-index/yaad-index).

`yaad-gmail` is a standalone CLI binary that implements the subprocess plugin protocol from yaad-index's [ADR-0006](https://github.com/yaad-index/yaad-index/blob/main/adr/0006-plugin-discovery-config-allowlist.md). It connects to Gmail via IMAP using an app-specific password + Gmail's `X-GM-LABELS` IMAP extension, polls inbox + sent folders for un-ingested messages, parses RFC-822 headers, and emits source-shape entities + canonical edges (`email`, `email-address`, `label`) into yaad-index. State lives entirely on Gmail (in the configured `ingested_label`); restart-safe by design.

## Build

```sh
go build -o yaad-gmail .
```

The binary is the only artifact; there are no shared libraries or runtime config files. Drop the binary somewhere yaad-index can read and execute it.

## Register with yaad-index

`yaad-gmail` is invoked subprocess-per-poll-cycle — yaad-index's scheduler dispatches one invocation per configured polling interval, the binary connects to Gmail, walks the un-ingested set, emits per-message envelopes, and exits. Discovery is via yaad-index's config allowlist (no PATH search; absolute paths only):

```yaml
# operator config
plugins:
 - name: gmail
 path: /absolute/path/to/yaad-gmail
```

After editing the config, restart yaad-index. On startup yaad-index calls `yaad-gmail --init` to discover the plugin's capabilities, then dispatches per-poll-cycle invocations per the configured cadence.

## Auth + configuration

Auth shape: **IMAP + app-specific password + Gmail's `X-GM-LABELS` IMAP extension**. No OAuth, no Gmail API, no XOAUTH2.

Operator config flows in via env vars (yaad-index inherits env into the subprocess per ADR-0006):

| Env var | Default | Purpose |
|----------------------------------|-----------------|--------------------------------------------------------|
| `YAAD_GMAIL_ACCOUNT` | (required) | Gmail account address (e.g. `you@gmail.com`). |
| `YAAD_GMAIL_APP_PASSWORD` | (required) | Gmail app-specific password (16-char generated). |
| `YAAD_GMAIL_INGESTED_LABEL` | `yaad-ingested` | Label written via X-GM-LABELS after successful ingest. Empty string disables. |
| `YAAD_GMAIL_SKIP_LABEL` | `yaad-skip` | Label that, when present on a message, blocks ingest. Empty string disables. |
| `YAAD_GMAIL_IMAP_HOST` | `imap.gmail.com`| IMAP host override (test fixtures). |
| `YAAD_GMAIL_IMAP_PORT` | `993` | IMAP port override. |

App passwords are generated at https://myaccount.google.com/apppasswords (2-factor required). Empty `YAAD_GMAIL_INGESTED_LABEL` means EVERY poll re-ingests every message — useful only when the operator wants no-state-on-Gmail.

## Capabilities document

`yaad-gmail --init` writes:

```json
{
 "name": "gmail",
 "version": "0.2.0-dev",
 "url_patterns": [],
 "entity_kinds": [{"name": "source"}],
 "edge_kinds": [],
 "canonical_kinds_emitted": ["email", "email-address", "label"],
 "canonical_edge_types_emitted": ["bcc", "cc", "from", "is_a", "is_about", "tagged_as", "to"],
 "source_namespace": "gmail"
}
```

URL patterns are empty: Gmail messages don't have a yaad-index-dispatchable URL form. The plugin is poll-driven, not URL-driven; yaad-index never dispatches a `/v1/ingest` to this binary via URL match. The default-mode invocation runs a full poll cycle.

## Bidirectional label flow + restart-safety

Labels function as both the control surface AND the durable state mechanism. No client-side state file, no UID persistence, no UIDVALIDITY tracking. Restart-safe by design — the next polling cycle re-runs the same search predicate and finds the same un-ingested set.

Per cycle:

1. **Search**: IMAP `X-GM-RAW "-label:<ingested_label> -label:<skip_label>"` against `INBOX` + `[Gmail]/Sent Mail`.
2. **Read** Gmail labels per matching message via `UID FETCH (X-GM-LABELS)` → emit `tagged_as` edges to `label:<label-slug>` canonical entities. The control-plane labels (`ingested_label`, `skip_label`) are NEVER surfaced as edges — they're filtered out at the assembly layer.
3. **Write** `ingested_label` via `UID STORE +X-GM-LABELS` immediately after successful ingest. Marks the message as seen.
4. **Refetch on label-removal**: an operator removes the `ingested_label` on Gmail-side → the next poll cycle's search predicate matches the message again → re-ingest.
5. **Skip-label**: messages carrying `skip_label` are excluded by the search predicate. Operator adds this label on Gmail to block ingest.

## Source slugs + canonical edges

Source-shape entity ID: `gmail:<source-slug>` where `<source-slug>` = `<subject-slug>-<message-id-slug>` from the RFC-822 `Message-ID:` header (NOT the IMAP UID — Message-ID is globally unique + stable across mailbox moves).

Canonical kinds + slugs:

- `email:gmail-<message-id-slug>` — the `is_about` target.
- `email-address:<addr-slug>` — `_at_` for `@`, `_dot_` for `.`. Example: `Eli.Rubigd@Gmail.COM` → `email-address:eli_dot_rubigd_at_gmail_dot_com`.
- `label:<label-slug>` — `_slash_` for `/` to preserve hierarchy. Example: `Job Search/Active` → `label:job-search_slash_active` (distinct from flat `Job Search Active` → `label:job-search-active`).

Edges from `gmail:<source-slug>`:

| Edge type | Target | Multiplicity | Notes |
|-------------|----------------------------------|--------------|----------------------------------------|
| `is_about` | `email:gmail-<message-id-slug>` | 1 | Canonical-axis edge. |
| `is_a` | `source-type:gmail` | 1 | Universal source-type per ADR-0021. |
| `from` | `email-address:<addr-slug>` | 1 | Sender. |
| `to` | `email-address:<addr-slug>` | many | Each To-header recipient. |
| `cc` | `email-address:<addr-slug>` | many | Each Cc-header recipient. |
| `bcc` | `email-address:<addr-slug>` | many | **Sent-folder only** (inbound never carries visible BCC). |
| `tagged_as` | `label:<label-slug>` | many | One per Gmail label (control-plane labels filtered). |

`email-address` and `label` ship as bare canonical-label entities — no plugin-defined gaps. Gap schema design lands in dedicated follow-up issues.

## Development

```sh
make check
```

Runs `go vet`, `go build`, `go test -race`, `gofumpt -l` (fmt-check), `goimports -l`, `golangci-lint run`, and `go mod tidy -diff`.

```sh
make install-hooks
```

Installs a pre-commit hook that runs the `githook-check` chain (fmt-check + vet + lint + tidy-check) on every commit.

## Status

Gmail integration live. Threading (X-GM-THRID), IMAP IDLE / push-watch, and label gap schemas are deferred follow-ups.

## References

- yaad-index — host daemon. [ADR-0006](https://github.com/yaad-index/yaad-index/blob/main/adr/0006-plugin-discovery-config-allowlist.md) (subprocess plugin protocol), [ADR-0021](https://github.com/yaad-index/yaad-index/blob/main/adr/0021-daemon-owns-slug.md) (universal source kind + canonical-label edges).
- yaad-wikipedia — reference plugin (URL-driven).
- yaad-bgg — second reference plugin (URL-driven).
- Gmail's `X-GM-LABELS` IMAP extension — https://developers.google.com/workspace/gmail/imap/imap-extensions
- emersion/[go-imap/v2](https://github.com/emersion/go-imap) — IMAP transport library used.
