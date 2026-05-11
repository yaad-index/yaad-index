# yaad-gmail — agent skill

`yaad-gmail` is a yaad-index plugin for Gmail-sourced canonical entities. It connects via IMAP + app-password + Gmail's `X-GM-LABELS` IMAP extension, polls inbox + sent folders, and emits source-shape entities (`gmail:<source-slug>`) plus three canonical kinds:

- **`email`** — the message itself. Slug: `gmail-<message-id-slug>`.
- **`email-address`** — one canonical entity per unique address. Slug: `_at_`/`_dot_` encoding.
- **`label`** — one canonical entity per unique Gmail label. Slug: `_slash_` for hierarchy boundaries.

State lives entirely on Gmail (in the `ingested_label`) — no client-side state file, no UID persistence. Restart-safe by design.

## Bidirectional label flow

The Gmail-side labels function as both the control surface AND the durable state mechanism:

- **`yaad-ingested` (default)** — written by the plugin after each successful ingest. Removing it on Gmail triggers re-ingest on the next poll.
- **`yaad-skip` (default)** — when present on a message, blocks ingest. Add it on Gmail to keep something out of the graph.

Both label names are operator-configurable; empty-string disables that slot.

## Edges emitted from `gmail:<source-slug>`

- `is_about` → `email:gmail-<message-id-slug>` (1)
- `is_a` → `source-type:gmail` (1, universal per ADR-0021)
- `from` → `email-address:<addr-slug>` (1)
- `to` → `email-address:<addr-slug>` (many)
- `cc` → `email-address:<addr-slug>` (many)
- `bcc` → `email-address:<addr-slug>` (many, **sent-folder only**)
- `tagged_as` → `label:<label-slug>` (many; control-plane labels filtered)

## Querying the graph

Once the operator's poll cycles have run a few times, the canonical graph carries:

- Every `email-address` you've corresponded with (queryable via `list_entities(kind: "email-address")`).
- Every Gmail label as a canonical entity (`list_entities(kind: "label")`).
- `tagged_as` edges connecting source-shape `gmail:` entities to those labels — `edges(entity_id: "label:job-search_slash_active", direction: "in")` returns every email tagged with that Gmail label.

## Caveats

- BCC edges only emit on messages from `[Gmail]/Sent Mail`. Inbound BCC headers are not surfaced (they're rarely populated reliably; the spec scopes BCC to sent mail).
- Threading (Gmail's `X-GM-THRID`) is not yet exposed.
- The plugin is poll-driven, not URL-driven — agents can't `ingest("gmail:xyz")` to fetch a specific message; the operator's poll schedule drives all ingest.
