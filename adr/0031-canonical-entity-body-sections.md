# ADR-0031: Operator-authored body sections on canonical entities

## Status

Accepted (2026-06-05; proposed 2026-06-01). Implements #388. Amends the ADR-0012 anchor model: UGC
markdown sections may now anchor to a canonical entity ID, not only to a
`user-content:<slug>` ID.

## Depends on

- ADR-0008 (canonical kinds + slug rules) — the canonical-label entity shape.
- ADR-0011 (alias synthesis) — the canonical-label materialize path this reuses.
- ADR-0012 (user-generated content) — the section-edit machinery + the
  operator-equality auth rule (§Auth, amended #377) this inherits verbatim.

## Context

Canonical thin-edge entities (e.g. `boardgame:moon-colony-bloodbath`,
`person:alex-example`) today carry structured `data` (canonical-kind gaps),
typed `edges`, `provenance`, and append-only structured `notes`. They have
**no free-form markdown body surface**.

UGC entities (`user-content:<slug>`) do have one — the section-level edit
surface from ADR-0012. But that surface is gated on an ID prefix: the section
tools (`list`/`get`/`add`/`edit`/`rename`/`delete_user_content_section`) reject
any ID that does not start with `user-content:`.

Operators want to write longer-form prose directly on a canonical page ("my take
on this boardgame", "what I know about this person beyond the structured
fields") without standing up a parallel `user-content:` entity that duplicates
the canonical slug and fragments the content across two IDs. The current
workarounds — `add_note` (structured per-note, not free-form markdown) or a
UGC edge-linked to the canonical (indirection + a second page) — don't match the
"write a body on this page" mental model.

The content is still UGC semantically (operator-authored markdown sections);
only the **anchor** changes — from a `user-content:` ID to a canonical entity ID.

## Decision

### 1. `ugc: true` frontmatter flag gates section editability

Section editability is gated on a typed top-level frontmatter flag `ugc: true`,
not on the `user-content:` ID prefix. The flag is a sibling of `kind` / `source`
in the frontmatter — **not** a `data` gap — so it never appears in the canonical
data namespace, `needs_fill`, or gap-driven surfaces.

The six section tools operate on any entity that is **effectively UGC** —
`ugc: true` in frontmatter **or** `kind == user-content` (see §2 for why the kind
check stays):

- **Thin-edge canonicals** — the daemon stamps `ugc: true` when the entity's
  vault file is materialized (§3). Section tools then operate on them via the flag.
- **Plugin-emitted source entities** (`bgg:<slug>`, `gmail:<id>`,
  `github:owner/repo#N`, `wikipedia:X`) — plugins never emit `ugc: true` and their
  kind is not `user-content`, so the daemon never stamps it. Section tools refuse
  them.
- **Pure UGC** (`user-content:<slug>`) — editable via its `kind == user-content`
  (the flag is *implicit by kind*, and always has been). New creates also stamp
  `ugc: true` explicitly going forward, but the kind check is what keeps every
  pre-existing UGC file editable with no migration.

This cleanly separates: plugin source entities = plugin-owned structured data +
immutable plugin body (`clean_content`); thin-edge canonicals = operator-authored
body welcome; pure UGC = unanchored operator content.

### 2. Gate broadens from ID prefix to flag-or-kind

`loadUserContentVaultEntity` (the shared loader behind every section tool) stops
gating on the `user-content:` ID **prefix** and instead loads the entity's vault
file and accepts it when **either** `ve.UGC == true` **or**
`ve.Kind == "user-content"`. When neither holds the loader returns the same
`invalid_argument` 400 envelope as today, with the message reworded to name the
actual condition (the entity does not accept user-content sections) rather than
the now-incorrect "id must start with \"user-content:\"".

**Why the kind check stays (backward-compat, zero migration).** Every UGC vault
file created before this change carries `kind: user-content` but **not**
`ugc: true` (the flag didn't exist). Flipping the gate to `ve.UGC` *alone* would
make every pre-existing UGC file unreadable/uneditable by the section tools until
something restamped it — and there is deliberately no migration tooling (§Out of
scope). The kind check is the standing implicit-UGC signal (the issue's own
"implicit by kind today"); keeping it as one arm of the OR preserves all existing
UGC unconditionally. The flag is therefore **additive** — it admits canonical
kinds into the editable set, it does not replace the UGC kind signal. A reindex
pass restamps `ugc: true` onto old UGC files opportunistically, but nothing
depends on it having run.

### 3. Stamping: daemon-side only, never plugin-emitted

`ugc: true` is stamped by the daemon at exactly two sites; plugins have no
ability to set it:

- **UGC create** — `handleUserContentCreate` sets the flag on the vault entity
  it writes (makes the previously-implicit-by-kind flag explicit).
- **Canonical-label materialize** — when a canonical thin-edge's vault file is
  written (`NewCanonicalLabelEntity` → `WriteCanonicalLabelWithCommit`, the
  operator-fill auto-materialize path), the daemon stamps `ugc: true` for
  canonical kinds (kinds in the operator's `canonical_kinds` registry).

A plugin source entity's vault file is written without the flag, so its kind is
never editable as UGC.

### 4. Section-write auto-materializes the canonical vault file (the empty-page case)

A canonical thin-edge only gets a vault file on its **first fill** — a brand-new
thin row (`data: null`, no vault file) has nothing for the section loader to
read. Requiring a prior fill before an operator can write a body would break the
"write a body on this page" model and would block the #388 test case (a canonical
`boardgame:` that exists only as a `data: null` row).

So a section write (`add` / `edit` / `rename` / `delete`) on a **canonical
kind** with no vault file auto-materializes the vault file first — reusing the
existing `NewCanonicalLabelEntity` + `WriteCanonicalLabelWithCommit` flow from
the operator-fill path, stamping `ugc: true` per §3 — then applies the section
edit. This reuses a proven materialize path rather than inventing a second one.
The materialize stamps only `ugc: true`; the operator/owner is stamped by the
section write itself per §5 (first-write-claims-ownership), so a write through
this path both materializes the file and claims ownership in one operation.

Read-only section tools (`list` / `get`) on a never-materialized canonical
return an empty section set (no body yet); they do not materialize a file for a
read.

The alternative — refuse section writes until the entity has been filled once —
was rejected: it adds a non-obvious ordering prerequisite, breaks the operator
mental model, and fails the issue's own migration test case without a manual
pre-step.

### 5. Auth + provenance: operator-equality with first-write ownership

Writes are **operator-only**, using the same operator-equality core UGC uses
(ADR-0012 §Auth, amended #377): a caller may mutate sections only when the
caller's JWT operator pair-claim equals the entity's stored `operator`;
cross-operator writes return 403 `operator_mismatch`. Section content carries the
same `author` (JWT subject) / `operator` (pair-claim) stamping UGC sections use.

**Ownership source — the one divergence from UGC.** A pure UGC entity gets its
`operator` stamped at create, so UGC's rule rejects an empty stored `operator`
outright. A canonical thin-edge has no operator-driven create step: its `Data`
starts empty (`NewCanonicalLabelEntity`), and the §3 `ugc:true` materialize stamp
adds no operator. Applying UGC's empty-operator rejection verbatim would 403
*every* section write on a freshly-materialized canonical — the entity would be
flagged editable yet be permanently unwritable. So canonical bodies need an
explicit ownership source.

**Decision: first-write-claims-ownership.** When a section write lands on a
`ugc:true` entity whose stored `operator` is empty, the write is allowed and the
caller's operator is stamped as the entity's `operator` in that same write; every
subsequent write is plain operator-equality. This is safe precisely because
canonical bodies are a brand-new surface — an empty stored `operator` can only
mean "no body author yet," never "a body owned by an operator we can't identify"
(the case UGC's rejection guards against on legacy rows). There is no
pre-existing operator-owned canonical body to protect.

**Where the operator is stamped:** on the **first body write** (the
section-add/edit that observes an empty stored `operator`), binding ownership to
the authoring act — **not** at the §3/§4 `ugc:true` materialize sites, which can
run in system / reindex / fill contexts with no caller operator to attribute.
The materialize stamps therefore leave `operator` empty by design; the first body
author claims it.

**v1 ownership is single-operator.** The first body author owns the canonical
body; a different operator editing it gets 403 `operator_mismatch`, exactly as
UGC. Multi-operator shared canonical bodies are out of scope for v1 (in a
single-operator deployment all agents share the operator pair-claim, so the
claim-and-equality dance is a no-op in practice). No new permission surface is
introduced; agent-writes-with-permission across operators is out of scope for v1
(§Out of scope).

### 6. Coexistence invariant: canonical thin-edges carry no plugin body

A canonical thin-edge's body is operator-authored only — its `clean_content`
(plugin body) is always empty, because the plugin-emitted body lives on the
corresponding **source** entity (`bgg:<slug>`), not on the canonical thin-edge.
Operator body and plugin body therefore never collide on the same vault file, so
no body-merge / precedence rule is needed. This invariant is what makes §1's
"operator body welcome on canonicals" safe.

### 7. What stays unchanged

- Pure UGC (`user-content:<slug>`) behavior — same tools, same section model,
  same auth. The gate broadens (flag-**or**-kind, §2); existing UGC keeps passing
  via `kind == user-content` with no migration, new UGC additionally carries the
  explicit flag.
- Plugin source entities stay read-only bodies; reindex re-derives them.
- Edges, notes, gap_state, provenance, and the section containment model
  (ADR-0012 §Update semantics) are untouched.

## Consequences

### Positive

- Operators write prose on the page they already think of as the entity, with no
  parallel UGC and no edge indirection.
- One gate (`ugc: true` **or** `kind == user-content`, §2) admits both canonical
  thin-edges and pure UGC into the editable set without per-tool kind branching,
  and broadens the existing surface rather than replacing it — no behavior
  regresses for current UGC.
- The flag is daemon-controlled, so plugin source entities stay immutable by
  construction — a plugin cannot opt its source body into operator edits.

### Negative

- The section loader now reads the flag from the vault file rather than
  short-circuiting on an ID prefix, so a refused non-UGC entity costs a vault
  read before the 400. Acceptable: the path is operator-interactive, not hot.
- A new top-level frontmatter field (`ugc`) must round-trip through the vault
  marshal/unmarshal; hand-edited vault files that drop the flag lose editability
  until reindex/re-materialize restamps it (consistent with the ADR-0008
  hand-edit-then-reindex contract).

### Migration

**No UGC restamp needed.** Pre-existing `user-content:` vault files (created
before the flag existed, carrying `kind: user-content` but no `ugc: true`) stay
editable through the §2 kind arm of the gate, so the gate flip ships with zero
data migration. A reindex pass restamps the explicit flag opportunistically, but
nothing depends on it.

Existing UGCs whose content belongs on a canonical thin-edge (e.g.
`user-content:moon-colony-bloodbath`, whose canonical `boardgame:` counterpart
exists as a `data: null` row) are relocated by the **operator manually** —
moving the body sections into the canonical and dropping the UGC. **No automated
migration tooling is built**, and none is planned. The feature-side change (this
ADR + its impl) is all that lands; it makes the canonical target writable so the
manual relocation is possible.

## Out of scope

- Automated migration tooling (UGC → canonical body) — operators relocate
  existing UGC content manually; no tooling is built or planned.
- Agent-writes-with-permission on canonical bodies — v1 is operator-only.
- Multi-operator shared canonical bodies — v1 is single-owner (first body author
  claims ownership per §5); a second operator editing gets 403.
- Multi-source body merge / precedence — moot under §6 (canonical thin-edges
  carry no plugin body).
- Stamping `ugc` into the DB mirror for query ("list entities that accept UGC") —
  the gate reads the vault file; a DB-side index can follow if a query need
  appears.

## Implementation cuts

- **Cut 1 (this ADR).** Lock the flag model, the flag-or-kind gate, materialize-
  on-section-write, stamping sites, auth + first-write ownership, the backward-
  compat path, and the coexistence invariant.
- **Cut 2.** Implementation: the `ugc` vault frontmatter field (round-trip), the
  gate broadening in `loadUserContentVaultEntity` (`ve.UGC || kind ==
  user-content`), the two `ugc:true` stamp sites, the section-write
  auto-materialize, the first-write operator stamp (§5, claim-on-empty-operator),
  MCP tool doc + SKILL.md updates, and e2e tests (including a pre-existing-UGC
  file with no flag staying editable, the `boardgame:` empty-page write path, and
  the first-write ownership claim).
