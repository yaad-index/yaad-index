# ADR-0033: Entity re-key primitive (`store.RenameEntity`) + UGC move/rename lifecycle

## Status

Proposed (2026-06-05). Documents the as-built design shipped in #425 —
`store.RenameEntity`, the in-place UGC move, and the UGC rename API + MCP
tools. No code change accompanies this ADR.

## Depends on

- [ADR-0008](./0008-vault-as-source-of-truth.md) (vault-as-source-of-truth) —
  the vault-first / store-second write ordering and the reindex-reconciles
  recovery story.
- [ADR-0011](./0011-vault-file-aliases-from-titles.md) (vault-file aliases) —
  the alias table the bare-old-slug guarantee writes into.
- [ADR-0012](./0012-user-generated-content.md) (user-generated content) — the
  UGC entity shape + section-edit machinery the move/rename operations extend;
  this ADR adds the move/rename lifecycle ADR-0012 lacked.
- [ADR-0021](./0021-daemon-owns-slug.md) (daemon-owns-slug) — the
  `<kind>:<slug>` id shape a rename re-keys.

## Context

An entity's ID is derived from its slug, and the slug from its title (ADR-0021).
Before #425 there was no way to change an entity's title without destroying and
recreating it — which would orphan every edge, provenance row, notation, and
alias that referenced the old ID. UGC entities (ADR-0012), whose titles
operators routinely edit, had no rename path at all; the design lived only in
the MCP tool descriptions and inline handler comments — no ADR captured it.

#425 shipped three things:

1. `store.RenameEntity` — a transactional re-key primitive at the store layer.
2. An **in-place move** for UGC — relocate the vault file to a subfolder
   without changing the ID.
3. A **rename** for UGC — retitle → new slug → new ID, re-keyed via
   `store.RenameEntity`.

This ADR captures that design: the re-key invariant, the alias guarantee + the
conflict rule, the move-vs-rename split, the write ordering + recovery, and the
known concurrency limitation.

## Decision

### 1. `store.RenameEntity` — transactional re-key

`RenameEntity(ctx, oldID, newID string, newData map[string]any) error` re-points
every entity-ID-referencing row from `oldID` to `newID` inside a **single
transaction** (`internal/store/rename.go`). SQLite carries no `ON UPDATE
CASCADE` on the entity-ID foreign keys, so each referencing table is re-pointed
by hand, in FK-safe order:

1. **entities** — a new row is inserted *first* (carrying `kind`, `created_at`,
   `gap_call_done_at`, `gap_state`, `archived_at` forward; `data` replaced with
   `newData`; `updated_at` stamped now), so every subsequent re-point targets a
   live row. The old row is deleted **last**, when nothing references it.
2. **edges** — both `from_id` and `to_id` are re-pointed; an entity that is both
   ends of a self-edge re-keys cleanly.
3. **provenance** — `target_entity_id` (entity-target rows) plus
   `target_edge_from` / `target_edge_to` (edge-target rows).
4. **entity_notations** — `entity_id` re-pointed. The FK is `ON DELETE CASCADE`,
   so re-pointing *before* the old-row delete is what preserves them.
5. **entity_aliases** — re-homed to `newID` (see §2).

The whole sequence commits atomically; any error rolls the transaction back,
leaving the store at its pre-rename state. `reindex_files` is intentionally
**not** re-keyed — that per-file bookkeeping self-heals on the next reindex walk.

The call returns `ErrNotFound` (404) when `oldID` has no row, `ErrAliasConflict`
(409) on a foreign-owned slug (§2), and `nil` on success.

### 2. The bare-old-slug alias guarantee + the foreign-owned-slug 409

**Old references keep resolving.** After a rename, `<kind>:<old-slug>` continues
to resolve to the renamed entity: the re-key ensures the bare old slug is
registered as an alias (`alias_kind = bare`) pointing at `newID`. If the old
entity already carried aliases, they are re-homed to `newID` and the bare-slug
alias rides along; if it had none, the bare slug is inserted for `oldID` before
the re-home so the back-reference survives. The upsert is guarded by a
`WHERE entity_aliases.entity_id = excluded.entity_id` clause, so it never
hijacks a bare slug that already belongs to a different entity.

**Foreign-owned slug → 409.** A rename is **rejected before any mutation**
(returns `ErrAliasConflict`, surfaced as HTTP 409) when the old bare slug is
already owned as an alias by a *different same-kind* entity — re-homing it would
steal another entity's reference. The check is kind-scoped: a same-named slug
under a different kind is harmless (the alias resolver is kind-scoped) and does
not block. Because the check runs first, a 409 leaves both vault and store
untouched. The store re-validates this invariant inside the transaction as a
defensive backstop; the API enforces it up front via the alias resolver.

### 3. Move vs rename — id-stable vs id-changing

Two UGC operations, deliberately split by whether they change the entity ID:

| | **move** (`move_user_content`) | **rename** (`rename_user_content`) |
|---|---|---|
| Entity ID | **unchanged** (id-stable) | **changes** (id-changing) |
| Input | target `subfolder` (omit → flat) | `new_title` (slugified server-side) |
| Vault | file relocates to the subfolder | file renamed to the new slug |
| Store | none — the ID is path-independent | `store.RenameEntity` re-keys the row |
| Route | `POST /v1/user-content/{id}/move` | `POST /v1/user-content/{id}/rename` |

A **move** changes only where the file sits on disk; the entity ID is
location-independent, so no store mutation is needed and edges / provenance / id
are untouched. A **rename** changes the title, hence the slug, hence the ID — so
it must re-key, which is the whole reason `store.RenameEntity` exists. Both are
idempotent: moving to the current subfolder, or renaming to a title that
slugifies to the current slug, is a 200 no-op.

### 4. Vault-first / store-second ordering + recovery

The rename handler writes the **vault first**, the **store second**
(`internal/api/user_content_rename.go`):

1. `vault.Writer.RenameUserContentSlug` writes the new `.md`, moves any sidecar,
   and removes the old `.md` — ordered *write-new → move-sidecar → remove-old*
   so a crash never destroys the source before the destination lands; each step
   rolls back on failure, leaving the vault at its pre-rename state.
2. `store.RenameEntity` mirrors the re-key into the DB.

This honors ADR-0008: **the vault is the source of truth.** If the vault write
fails, the store is never touched and the error surfaces synchronously. If the
vault write succeeds but the store re-key fails, the response is a 500 that
explicitly tells the caller the DB view lagged — and the next reindex walk
re-derives the store from the already-renamed vault. There is no separate
reconciliation path; reindex is the eventual-consistency surface, the same
contract entity creation uses.

## Consequences / known limitations

- **Write-lock scope on rename (#429).** The handler holds the write lock on the
  **old** ID for the duration of the rename. Once `store.RenameEntity` commits,
  the new ID is live but was never under that lock, so a request addressing the
  new ID directly is not serialized against the in-flight rename. The
  new-ID-already-exists pre-check narrows but does not close the window. Tracked
  as #429; acknowledged here as a known concurrency limitation, not a solved
  problem.
- **Check-to-commit window on the foreign-slug guard.** The foreign-owned-slug
  check (§2) runs before the transaction; a concurrent alias write could in
  principle land between the check and the old-row delete. The store's
  in-transaction re-validation is the backstop, with the API pre-check as the
  primary defense. No row-level pessimistic lock is taken.
- **`reindex_files` not re-keyed.** Deliberate — it self-heals on the next walk
  — but the store's file-bookkeeping view briefly references the old path until
  then.

## Alternatives considered

- **Archive + recreate under the new ID.** Rejected: it orphans (or forces
  manual re-creation of) every edge, provenance row, and alias, and breaks old
  references. The transactional re-key preserves the full relationship graph in
  one step — the entire point of the primitive.
- **Store-first, vault-second.** Rejected: it violates ADR-0008's
  vault-as-source-of-truth contract and would leave the authoritative vault
  stale if the second step failed, with no recovery path — reindex derives the
  store *from* the vault, not the reverse.
