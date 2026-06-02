# ADR-0029: Unified fill surface — gap-fill, overwrite, and ad-hoc writes through one endpoint

## Status

Proposed 2026-05-30. Pre-release; no migration window beyond the deprecation marker on `/v1/entities/{id}/operator-fill`.

## Depends on

- [ADR-0008](./0008-vault-as-source-of-truth.md) — vault-as-source-of-truth. Fill writes route through the vault writer + DB mirror per ADR-0008's split. Unchanged.
- [ADR-0013](./0013-canonical-kind-owns-gap-contract.md) — canonical_kinds owns the gap contract. The per-kind gap registry (typed shape, fill_strategy, etc.) still defines the gap surface; this ADR changes only how writes against gaps are dispatched.
- [ADR-0016](./0016-canonical-kind-defaults.md) — plugin-emitted canonical-kind defaults. The plugin's gap registration still seeds the canonical-kind config; the unified endpoint reads the merged spec via `resolveEffectiveGaps`.
- [ADR-0019](./0019-operator-fill.md) — **superseded by this ADR**. The still-load-bearing model (fill_strategy field, gap_state per-field state machine, defer semantics, auto-materialize path, per-field-op map) carries forward unchanged; the endpoint split + caller-identity gate is the part being reversed.
- [ADR-0021](./0021-daemon-owns-slug.md) — daemon-owned slug + auto-materialize. Unchanged: the unified endpoint inherits operator-fill's auto-materialize path for thin-DB-row canonical labels.

## Context

ADR-0019 introduced two parallel fill endpoints:

- `POST /v1/entities/{id}/fill` — the agent-fill path. Routes a per-field key:value map; gates per the gap's `fill_strategy` (rejects `operator`-strategy gaps as `agent_only_field`).
- `POST /v1/entities/{id}/operator-fill` — the operator-correction path. Routes a richer per-field-op map (scalar set / null clear / `{"defer": true/false}` / canonical_type list); gates the inverse direction (`fill_strategy: agent` → `agent_only_field` rejection); also covers ad-hoc writes to fields no workflow registered.

The split conflates **who triggered the write** (operator-via-agent vs. agent-autonomous) with **who executes the write** (always the agent — operators don't run curl). In practice both endpoints route through identical write machinery; the only meaningful difference is which way the `fill_strategy` gate fires. Recent iterations (#350 gap-state-aware total, #353 non-canonical-kind operator-fill) sharpened the operator-fill side to handle the cases the agent-fill side already handled — the gap between the two endpoints has shrunk to the strategy-direction flip.

The "operator-fill" naming further reads as a caller-identity gate. It is not. The strategy field on the gap (per ADR-0019) is the authoritative gate for which trigger-modes are permitted; the endpoint URL adds no security or audit value beyond what the strategy check already provides.

Three call shapes the split handles awkwardly:

1. **Open gap exists for the field.** Both endpoints handle this. The strategy gate fires the same way on both (just inverted).
2. **Field has an existing value (gap already closed or never had one).** Only `/v1/operator-fill` handles this today — `/v1/fill` rejects with `field_not_in_gap_set`. There is no caller-mode reason for the rejection; an agent revising its prior guess should be a first-class flow.
3. **Field is brand-new (no workflow ever registered a gap for it).** Only `/v1/operator-fill` handles this today (post-#353). Ad-hoc property writes belong on the same surface as gap-fills.

The fix is to collapse both endpoints into one and lift the trigger-mode gate to a property of the request (not the URL).

## Decision

### 1. Single endpoint: `POST /v1/entities/{id}/fill`

The unified endpoint absorbs both `/v1/fill` and `/v1/operator-fill`. The request body is the richer per-field-op map ADR-0019 introduced for operator-fill (scalar set / null clear / `{"defer": true/false}` / canonical_type list). The trimmed-down `/v1/fill` body (plain key:value map) is accepted as a special case where every value parses as a scalar set.

### 2. Three-case routing per field

Each field in the request body routes through one of three branches based on the entity's current state:

| Case | Condition | Behavior |
|------|-----------|----------|
| **Open gap** | Field appears in the entity's open `gaps:` list AND has a corresponding `gap_state` or canonical-kind spec | Validate the request's trigger-mode against the gap's `fill_strategy` (see §3). Apply write, close the gap, stamp `gap_state[field] = {source, filled_at}`. |
| **Overwrite** | Field has an existing value (gap was closed or never had one but `data[field]` is set) | Reject with `409 already_filled` unless the request carries `force=true`. With `force=true`, apply the overwrite + re-stamp `gap_state` accordingly. |
| **Ad-hoc** | Field is brand-new — no entry in `data`, no open gap, no `gap_state` entry, no canonical-kind spec | Accept the write. No strategy check (there is no gap to gate against — the trigger IS the authorization). Stamp `gap_state[field] = {source, filled_at}` for audit. |

The router picks the case per-field, not per-request: a single request body MAY combine open-gap fills, overwrites, and ad-hoc writes, each routed independently.

### 3. Strategy gate via trigger-mode (not URL)

The caller's identity claim distinguishes two trigger-modes:

- **operator-trigger** — subject claim equals operator claim (the operator-via-agent shape per ADR-0019 §"Endpoint surface"); maps to "operator-strategy gaps fillable".
- **agent-trigger** — every other authenticated request (agent acting autonomously); maps to "agent-strategy gaps fillable".

A gap with `fill_strategy: either` is fillable under both trigger-modes. The gate fires when the request's trigger-mode doesn't match the gap's allowed set, with the same `400 agent_only_field` / `400 operator_only_field` error codes ADR-0019 defined — only the URL changes.

Ad-hoc writes (no registered gap) require operator-trigger. Agent-trigger ad-hoc writes reject with `400 unknown_field` since there is no gap to authorize the path.

### 4. `defer` absorbed into the unified endpoint

The `{"defer": true/false}` per-field op stays exactly as ADR-0019 defined it. The operator marks a gap as "not-ready, stop surfacing"; the new endpoint accepts the same envelope. Defer requires operator-trigger.

Un-defer (`{"defer": false}`) flips the `deferred` flag back; subsequent fills land normally.

### 5. `/v1/entities/{id}/operator-fill` returns `410 gone`

Pre-release window means no client compatibility window is owed. The deprecated endpoint returns `410 gone` with a Location-pointer header at `/v1/entities/{id}/fill` and a body describing the migration. Tests pin the 410 + Location.

A `308 permanent redirect` was considered. `410 gone` was chosen because:

- Clients written against the operator-fill body shape (per-field-op map) can replay the body verbatim against `/v1/fill` — the request shape doesn't change, only the URL does. A redirect adds a round-trip without semantic benefit.
- `410` carries a clearer "this is gone, migrate" signal than `308` for downstream agents reading status codes deterministically.
- The only two operator-fill callers are the MCP `set_operator_fill` tool and a small set of test fixtures; both will be updated in ADR-0029's Cut 3.

### 6. What stays unchanged

- `GET /v1/needs-fill` — gap-surface listing, including the `total` field per #338 / #350.
- The `fill_strategy` field on gap specs — authoritative gate for trigger-mode.
- The `defer` operation — folded into the unified endpoint per §4.
- The auto-materialize path (per ADR-0021 §3) — fresh `vault.Entity` synthesis for thin-DB-row canonical labels.
- The per-field-op shape — scalar set / null clear / `{"defer": ...}` / canonical_type list.
- The `gap_state` per-field state machine — source / filled_at / deferred / deferred_at semantics carry over verbatim.
- The auto-materialize gate for non-canonical kinds — DB row exists + vault file missing on a non-registry kind still returns `404 not_found` (per #353).

### 7. MCP tool migration

`set_operator_fill(id, fields)` and `fill_gap(id, fields)` both alias to a single `fill_field(id, fields, force?)` tool that maps to the unified endpoint. The aliases stay registered for one minor version after the cut (so workflow YAMLs naming the old tool keep working); a future ADR retires the aliases once usage drops.

The `defer_gap(id, gap)` MCP tool stays unchanged — it's a thin envelope wrapper that retargets to the unified endpoint's defer op.

The workflow-engine `set_property` action is a separate surface (workflow-internal write primitive, not the HTTP fill surface) and is untouched.

## Consequences

### Positive

- Single mental model: "every fill is a fill" — no operator-vs-agent endpoint register.
- The agent's autonomous revise-prior-guess flow (Case 2 in §2) gets a first-class path it lacked under ADR-0019. Agents revising hallucinated values today have to route around the `field_not_in_gap_set` rejection by deferring then re-filling — Case 2 with `force=true` is the direct shape.
- Ad-hoc property writes (Case 3) no longer need a separate endpoint to express. Operators correcting non-gap fields (typo cleanup, ad-hoc canonical_type edge additions) route through the same surface.
- Fewer surfaces to test, document, and reason about.

### Negative

- Existing clients of `/v1/operator-fill` get a `410 gone` immediately on the cut. The MCP layer's `set_operator_fill` tool keeps working via the alias in §7, so the operator-visible surface stays connected. External direct-HTTP callers (none known) must update URL.
- The trigger-mode gate now lives in the unified endpoint's handler rather than being implicit in the URL. The handler MUST read the caller's claim correctly — a misclassified trigger-mode could allow an agent-trigger write to land against an operator-strategy gap (the failure mode the old split structurally prevented). Tests pin the gate's behavior on both directions.
- The `force=true` parameter adds a small risk surface: a caller passing `force=true` unconditionally clobbers existing values without seeing the `409 already_filled` warning. Mitigated by defaulting `force=false` and naming the parameter explicitly.

### Migration

Three-cut implementation:

1. **Cut 1 (this ADR)** — design lock. ADR-0029 written; ADR-0019 stamped SUPERSEDED. No code change. Reviewers can sign off on the unified-fill surface before any code lands.
2. **Cut 2** — code change. Unified `/v1/fill` handler implements the three-case routing + force-gate + defer-absorption + strategy gate. `/v1/operator-fill` returns `410 gone`. Tests cover all three cases + the deprecation path.
3. **Cut 3** — docs + MCP alias. `docs/fill-gap.md` refactored to drop the operator-fill register; MCP `set_operator_fill` aliases to `fill_field`. `defer_gap` retargets to the unified endpoint.

The cut order is load-bearing: the ADR locks the surface before code touches it, the code lands the actual collapse, and docs+MCP fold into the new shape last.

## Open questions

None. The strategy-gate flip on trigger-mode + the three-case routing fully cover ADR-0019's surface; the `410` choice over `308` is locked per §5.

## References

- [ADR-0019](./0019-operator-fill.md) — superseded by this ADR.
- [#355](https://github.com/yaad-index/yaad-index/issues/355) — issue that scoped the collapse.
- [#350 / PR-351](https://github.com/yaad-index/yaad-index/pull/351) — gap-state-aware total (groundwork for treating gap_state as the authoritative gap-presence signal).
- [#353 / PR-354](https://github.com/yaad-index/yaad-index/pull/354) — non-canonical-kind operator-fill (sharpens the gap_state-driven path this ADR generalizes).
