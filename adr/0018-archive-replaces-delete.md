# ADR-0018: Archive replaces delete; deletion becomes a separate explicit destroy path

## Status

Accepted 2026-06-06 (proposed 2026-05-08).

## Context

Today's daemon offers two destructive endpoints:

- `DELETE /v1/entities/{id}` — purges a plugin or canonical entity
 from DB and vault.
- `DELETE /v1/user-content/{id}` — purges a UGC entity from DB and
 vault.

For the present plugins (wiki, bgg), destructive delete is recoverable
in principle: the source artifact stays on Wikipedia / BGG, so a
re-ingest reconstructs the entity. The cost is the canonical-side
interpretive work (alias-merges, frontmatter-derived edges, manual
gap-fills), which is non-trivial but bounded.

The present plugin shape is also low-risk for accidental destruction:
no thread structure, no attachments referenced from elsewhere, no
plugin emits content that downstream plugins depend on by reference.

Two upcoming shapes break that:

1. **Email plugin (planned).** Pulls inbox messages with attachments
 and threading. A deletion of one message can break thread-context
 for siblings, orphan attachments referenced by replies, and
 permanently destroy data the daemon may be the only copy of (the
 email client also retains, but the daemon-side enrichment notes,
 tags, edges to other canonical entities are unique). Deletion is
 not recoverable from the source side beyond the raw message body.

2. **User-content as the operator's notebook.** UGC entities are
 operator-created and operator-edited. Deletion has no source-side
 recovery path. A typo in a delete call, a misclick in a future UI,
 or a naive bulk operation can destroy notes the operator built
 over weeks or months.

Even today's plugin entities benefit from archive over delete:
canonical merges (`person:martin-wallace` with eleven provenance
sources from this week's ingest) represent interpretive work that
reproduces from sources but at a cost. An archive layer with
restore is cheap insurance.

The vault is also git-tracked, which provides recovery for vault
files but not for the DB rows + edges. Git-recover-the-file is a
poor substitute for an explicit archive that re-binds DB and vault
together on restore.

## Decision

Replace the destructive `DELETE` paths with an archive lifecycle.
Permanent destruction remains available as a separate, explicit
operator-only action.

### Entity lifecycle

- **active** — default state on creation.
- **archived** — reachable by id but hidden from default surfaces;
 edges to/from archived entities still resolve, with the archived
 flag visible to consumers that ask.
- **destroyed** — DB row and vault file gone. Reachable only via git
 history of the vault repo (and even that not for the DB-only data
 like edge weight, alias synthesis, ingest provenance timestamps).

### Schema

Add a single column to the entities table:

```sql
ALTER TABLE entities ADD COLUMN archived_at TIMESTAMPTZ NULL;
CREATE INDEX entities_archived_at_idx ON entities (archived_at);
```

`NULL` = active. Non-NULL = archived at that timestamp. Pre-v1, no
migration of existing data; the column starts NULL across the board.

### Endpoints

**Replaces today's `DELETE /v1/entities/{id}` and
`DELETE /v1/user-content/{id}`:**

- `POST /v1/entities/{id}/archive` — set `archived_at = NOW()`, move
 vault file to `_archive/<kind>/<slug>.md`.
- `POST /v1/entities/{id}/restore` — clear `archived_at`, move vault
 file back to `<kind>/<slug>.md`.

(Same shape for user-content — same path or a parallel
`POST /v1/user-content/{id}/archive` / `restore`. Implementation
detail; one path with a kind-aware handler is simpler.)

**Default-filtered surfaces:**

- `GET /v1/list_entities` — filters `archived_at IS NULL` by default.
 `?include_archived=true` opts in. `?archived_only=true` returns
 only archived.
- `GET /v1/search` — same defaults.
- `GET /v1/entities/{id}` — returns archived entities normally
 (lookup by id is exempt from the default-hide; archived entities
 are reachable, just not surfaced).
- Edge expansion responses — include an `archived: true` flag on any
 endpoint that points at an archived entity. Consumers decide
 whether to follow.

**Permanent destroy (replaces the destructive shape today's DELETE
implements):**

- `DELETE /v1/entities/{id}` — only valid on archived entities.
 Returns `409 Conflict` with a hint to call `/archive` first if the
 entity is active. On an archived entity, the call hard-deletes:
 row gone from DB, file gone from `_archive/<kind>/<slug>.md`.
 Operator-only auth tier.

The state-machine enforces the two-step path: archive first, then
delete. There is no single-call destroy of an active entity. This
makes the safety property structural rather than convention-based —
no magic confirmation flag, no opt-in skip; the only path to destroy
is through archive.

### Vault representation

Archived entities live at `vault/_archive/<kind>/<slug>.md` (mirrors
the active layout one level down). Ranges:

- The `_archive/` directory is excluded from the default vault
 reader (search, list, ingest-walk).
- Archive moves are committed by the auto-commit pipeline as a
 separate commit shape (`archive:` prefix, parallel to today's
 `ingest:` and `fill:`).
- Restore moves the file back, committed with `restore:` prefix.

### Edge handling

Edges referencing archived entities are retained in the database.
Graph traversal (the `?with_edges=` flag, the upcoming
`GET /v1/edges`) returns archived endpoints with the
`archived: true` flag visible. Default consumers can choose to
follow or skip; the archive state is information, not invisibility.

When a canonical entity is archived but source-shape entities still
emit edges pointing at it, the canonical stays archived (single
operator action). New ingests do not auto-restore. The operator
restores intentionally.

### Attachments and ownership cascade

Some entity kinds carry attachments — files the daemon stores
locally on the entity's behalf. BGG entities carry a thumbnail image.
Email entities (planned) carry message bodies and per-attachment
binaries. Wikipedia entities may carry locally-cached images.

Attachments are **owned by their parent entity**, not addressable as
standalone entities. The unit of archive and delete is the parent
plus all its attachments, cascading.

Concrete shape:

- Each entity carries an `attachments: [{name, kind, path, content_type, bytes}, ...]` manifest field listing what it owns.
- Archive cascades: each attachment file moves to `vault/_archive/<parent-kind>/<parent-slug>/<attachment-name>` (kept under the parent's archive folder so they live and die together).
- Restore cascades: attachments move back alongside their parent.
- Delete cascades: no attachment survives its parent.
- API: there is no `POST /v1/attachments/{id}/archive` or `DELETE /v1/attachments/{id}`. Attachments are read-only via `GET /v1/entities/{id}/attachments/{name}`. The only path to mutate the manifest is via the parent entity's edit flow (the plugin's re-ingest, or the user-content edit endpoint).
- For BGG specifically: today's `data.thumbnail_url` field becomes a manifest entry pointing at a daemon-stored local copy rather than a remote URL. The image is fetched once at ingest, stored under the entity, and deleted/archived alongside it.

**Sharing policy: no shared attachments.** If two parent entities want to reference the same logical asset (e.g., two BGG entries with the same thumbnail URL), each parent owns its own copy. Storage cost is bounded for the assets we care about (thumbnails KB, email attachments tracked per-message); refcount semantics interact awkwardly with restore (if I restore an entity and the asset's refcount went to zero in between, the file is gone and restore can't recover it). Copy-on-ingest is simpler and the bounds are acceptable.

The aggregate-root pattern: parent entity is the unit of operation. Attachments exist *because* of the parent and only as long as the parent does. This prevents orphan-attachment hygiene problems and gives the operator a single mental model: archive/delete a thing, all its parts go with it.

### Bulk archive (future)

`POST /v1/archive/bulk` with a predicate (kind, source-plugin,
date-range, etc.) for shapes like "archive all email from sender X."
Not in initial scope; comes with the email plugin.

### Archive lifetime

For v1: archived = forever, no auto-purge. Plugin-specific purge
policies (email after 90 days?) come later as a configurable
per-plugin setting. Initial implementation does not auto-destroy.

## Consequences

### Positive

- **Accidental-loss insurance.** UGC pages, canonical merges, plugin
 enrichments — all recoverable from a single archive call. The
 failure mode of "deleted what I shouldn't have" becomes "find it in
 archive, restore."
- **Email plugin viability.** The eventual email plugin can lean on
 archive as the default disposal action without breaking thread
 context. Attachments referenced by other messages stay reachable.
- **Audit trail.** `archived_at` timestamp + commit history give a
 clean operator-action audit. Today's hard delete leaves no
 daemon-side trace beyond the absence of a row.
- **Search hygiene retained.** Default search/list still hides
 archived entities, so the operator surface stays clean. The
 hide-from-default property is what destructive delete provided
 today; archive preserves it.

### Negative

- **DB growth.** Archived entities still take up rows. For the
 current plugins, this is bounded by the operator's ingest volume.
 For the eventual email plugin with attachments, growth is
 unbounded without a purge policy. Per-plugin lifetime settings
 defer this; v1 accepts the cost.
- **Edge resolution complexity.** Consumers (mcp, daemon-internal
 graph traversal) need to handle the `archived: true` flag. Most
 callers will want to skip; some (audit, restore-flow tooling) will
 want to see. New surface area for edge consumers.
- **Two endpoints for one logical "remove" verb.** Operators need to
 know the difference between archive (default) and permanent destroy
 (only on already-archived entities). Documentation tax, but the
 state-machine error message on `DELETE` of an active entity is its
 own self-documenting hint.

### Neutral

- The vault `_archive/` folder is git-tracked, so even archived
 files have a recovery path from git history if the archive itself
 is later destroyed.
- Existing destructive `DELETE` callers (any operator scripts, the
 MCP delete tool) need to migrate. Pre-v1, this is acceptable
 breakage.

## Implementation

Filed as a series of small reviewable PRs against yaad-index:

1. **Schema migration** — add `archived_at` column + index.
2. **Archive / restore endpoints + default filtering** — handler
 logic + vault file moves + commit shape.
3. **Edge response `archived` flag** — propagate through expand
 responses.
4. **DELETE endpoint state-machine** — accept only on archived
 entities; reject DELETE on active entities with `409 Conflict` +
 archive-first hint.
5. **MCP tool layer** — add `archive_entity` / `restore_entity`
 tools; deprecate or rewrite `delete_entity`.
6. **Attachment manifest + cascade** — `attachments[]` on entities,
 archive/restore/delete walk the manifest. BGG plugin update to
 emit thumbnail as a managed attachment instead of a remote URL.
 Wikipedia attachment-fetching can land later when local-image
 caching is wanted.

Companion plugin and consumer changes (mcp, scripts, ui-if-any) land
after the daemon-side surface is stable.

## Out of scope

- Per-plugin auto-purge policies (email 90-day, etc.). Lands with
 the email plugin or as a follow-up ADR.
- Bulk archive operations. Lands with the email plugin or as a
 follow-up.
- A first-class trash UI in the daemon. The archive endpoints are
 the API; a UI is optional.
- Encryption of archived data. The archive is for accidental-loss
 recovery, not adversarial-destruction protection.
