# yaad-gmail — agent skill

Agent-facing reference for working with Gmail-sourced entities in the canonical graph. For the plugin-author / operator view (capabilities, invocation, IMAP transport, env vars), see [`docs/plugins/gmail.md`](./gmail.md).

`yaad-gmail` is the yaad-index plugin that connects to Gmail via IMAP + app-password + Gmail's `X-GM-LABELS` IMAP extension, walks inbox + sent folders for un-ingested messages, and emits source-shape entities (`gmail:<source-slug>`) plus three canonical kinds:

- **`email`** — the message itself. Slug: `gmail-<message-id-slug>`.
- **`email-address`** — one canonical entity per unique address. Slug: `_at_`/`_dot_` encoding.
- **`label`** — one canonical entity per unique Gmail label. Slug: `_slash_` for hierarchy boundaries.

State lives entirely on Gmail (in the `ingested_label`) — no client-side state file, no UID persistence. Restart-safe by design.

## Triggering a fetch cycle

`yaad-gmail` is a command-shape plugin per [ADR-0022](../../adr/0022-plugin-commands.md) — Gmail messages don't have a URL form the daemon dispatches against. Agents can't `ingest("gmail:xyz")` to fetch a specific message. The fetch cycle runs on operator demand:

- **`/v1/ingest` with `{"url": "gmail: !fetch"}`** — the `!` sigil declares the command-shape input. Requires an operator-only claim (the JWT subject must equal the operator); agent-on-behalf claims work when the operator delegates that authority. Streams NDJSON envelopes back as the cycle walks the un-ingested set.
- **`yaad-index command gmail fetch`** — CLI equivalent, same operator-only claim gate.

The daemon does NOT have a built-in polling scheduler. Operators (or operator-driven automation, e.g. an operator-claim cron) decide the cadence.

## Bidirectional label flow

Gmail-side labels function as both the control surface AND the durable state mechanism:

- **`yaad-ingested` (default)** — written by the plugin after each successful ingest. Removing it on Gmail triggers re-ingest on the next cycle.
- **`yaad-skip` (default)** — when present on a message, blocks ingest. Add it on Gmail to keep something out of the graph.

Both label names are operator-configurable; empty-string disables that slot.

## Edges emitted from `gmail:<source-slug>`

- `is_about` → `email:gmail-<message-id-slug>` (1)
- `is_a` → `source-type:gmail` (1, universal per [ADR-0021](../../adr/0021-daemon-owns-slug.md))
- `from` → `email-address:<addr-slug>` (1)
- `to` → `email-address:<addr-slug>` (many)
- `cc` → `email-address:<addr-slug>` (many)
- `bcc` → `email-address:<addr-slug>` (many, **sent-folder only**)
- `tagged_as` → `label:<label-slug>` (many; control-plane labels filtered)

## Querying the graph

Once the operator has run a few fetch cycles, the canonical graph carries:

- Every `email-address` you've corresponded with — `list_entities(kind: "email-address")`.
- Every Gmail label as a canonical entity — `list_entities(kind: "label")`.
- `tagged_as` edges connecting source-shape `gmail:` entities to labels — `edges(entity_id: "label:<label-slug>", direction: "in")` returns every email tagged with that Gmail label.
- Reverse lookups via `from` / `to` / `cc` — `edges(entity_id: "email-address:<addr-slug>", direction: "in")` returns every email sent from or received by that address (use `edge_types` to narrow to `from` vs `to` / `cc`).

## Caveats

- **BCC edges only emit on sent-folder messages.** Inbound BCC headers are not surfaced (they're rarely populated reliably; the spec scopes BCC to sent mail).
- **Threading (`X-GM-THRID`) is not yet exposed.** Multi-message threads currently appear as N independent `email` canonical entities, one per `Message-ID`.
- **Agent-fetch is not available** — agents can't direct-fetch one specific message. The operator's command-driven cycle is the only ingest path.

## References

- [`docs/plugins/gmail.md`](./gmail.md) — plugin-author / operator-facing doc (capabilities, env config, IMAP transport).
- [`docs/ingest.md`](../ingest.md) — agent-facing `/v1/ingest` flow.
- [ADR-0021](../../adr/0021-daemon-owns-slug.md) — universal `"source"` kind + daemon-owned slug derivation.
- [ADR-0022](../../adr/0022-plugin-commands.md) — command-shape plugin protocol.
