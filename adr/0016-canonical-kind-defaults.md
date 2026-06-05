# ADR-0016 — Canonical-kind defaults: built-in gaps + 4-layer override + plugin-driven activation

**Status:** Accepted (2026-06-05; proposed 2026-05-06)
**Date:** 2026-05-06
**Depends on:** [ADR-0008](./0008-vault-as-source-of-truth.md), [ADR-0013](./0013-canonical-kind-owns-gap-contract.md)
**Amends:** ADR-0013 §1 (canonical_kinds config shape)

## Context

ADR-0013 §1 established the canonical-kinds registry: each kind owns its own gap-set (the field names the AI fills) plus an instruction (the operator-supplied prose telling the AI how to fill). Today the operator config requires the explicit shape:

```yaml
canonical_kinds:
 person:
 gaps:
 name: "Full name"
 tags: "Topic tags relevant"
 summary: "One-paragraph summary"
 birth_date: "Birth date if known"
 instruction: "Fill the gaps using only the supplied clean_content..."
 boardgame:
 gaps:
 name: "Game title"
 tags: "..."
 summary: "..."
 instruction: "..."
```

Three problems surface in practice:

1. **Boilerplate**: every kind repeats `name` / `tags` / `summary` (which are required for DB-backed search anyway). The operator types the same three gap descriptions per kind, with the same instruction prose.
2. **Plugin redundancy**: plugins already declare their canonical kinds in their Capabilities document (per ADR-0013). The operator then has to RE-enable them in config — duplicated declaration that adds no information.
3. **No discoverable defaults**: a new operator has no built-in starting point. They have to read ADR-0013 to know which gap names yaad-index actually uses.

The current shape works (it's correct per ADR-0013), but it's the wrong ergonomic surface for an evolving system where plugins arrive with their own kinds and the operator wants to lightly tune.

## Decision

**Canonical-kind config becomes a four-layer merge for GAPS only: code defaults → plugin declarations → operator-defaults (root) → operator per-kind. Code provides built-in `name`, `tags`, and `summary` gaps for every canonical kind. Plugins activate their declared kinds automatically and may add gaps. Operator config carries two scopes: a top-level `canonical_kinds_defaults:` block applies to every kind, and per-kind blocks under `canonical_kinds:` override scope-of-one.**

**The full `instruction` struct (both `text` and `enabled`) is operator-config-only. Code-default instruction is empty + disabled. Plugins MUST NOT propose instruction text or enable. The operator is the sole source of the prose the AI receives and the gate that decides whether AI fill engages. Reasoning: if plugins could propose text, a plugin author could ship `text: "bad prose"` while the operator enables AI fill globally; per-type plugin text would then override the operator's root text via the layer order, end-running the operator's agency over what reaches the AI. Closing this surface entirely requires removing instruction from the plugin and code layers, not just the `enabled` flag.**

### 1. Built-in defaults (code)

yaad-index defines, in code, three gaps that every canonical kind has by default:

```go
var defaultGaps = map[string]GapSpec{
 "name": {Type: "string", Description: "The name of the entity."},
 "tags": {Type: "[]string", Description: "Relevant tags for this entity."},
 "summary": {Type: "string", Description: "A short prose summary."},
}
```

These gaps:

- Are present for every canonical kind, no matter what plugin or operator config says.
- **Cannot be removed.** They are the columns the DB indexes for search; removing them would break `/v1/search`.
- Can have their `Description` overridden by plugin or operator.

The default `instruction` is empty:

```go
var defaultInstruction = InstructionSpec{
 Enabled: false,
 Text: "",
}
```

Both fields are empty + disabled. The operator config is the SOLE layer that sets either `text` or `enabled`; code provides no instruction prose, plugins are forbidden from declaring instruction (per §2 below). When AI fill is engaged for a kind whose effective instruction text is empty, the daemon refuses to issue an LLM call and logs an INFO line ("instruction.enabled but text is empty for kind X — set instruction.text in operator config"). Empty-text + enabled is a configuration mistake, not a usable AI prompt.

### 2. Plugin declaration

Plugins continue to declare their canonical kinds in the Capabilities handshake (per ADR-0013). The Capabilities shape grows to include per-kind gap and instruction-text proposals:

```json
{
 "name": "bgg",
 "canonical_kinds_emitted": ["boardgame"],
 "canonical_kinds_extras": {
 "boardgame": {
 "gaps": {
 "summary": { "description": "Short summary of gameplay." },
 "year": { "type": "number", "description": "Year of publication." },
 "weight": { "type": "number", "description": "BGG complexity weight." }
 }
 }
 }
}
```

A plugin's canonical kinds are AVAILABLE in the registry the moment the plugin is enabled in the operator's `plugins:` config — no second declaration needed under `canonical_kinds:`. The operator can disable a plugin-declared kind, override its descriptions, or add fields, but the kind doesn't need to appear in operator config to be usable.

A plugin MUST NOT include `instruction` in its capabilities at all. Neither `text` nor `enabled` is plugin-controllable. The daemon ignores any `instruction` field in `canonical_kinds_extras` and logs a WARN naming the plugin (`plugin "X" attempted to set instruction for kind=Y; ignored — instruction is operator-only`). The full instruction struct belongs to operator config (§3 below).

### 3. Operator config — root-level defaults + per-kind blocks

The operator config exposes two distinct top-level keys: `canonical_kinds_defaults:` (root-scoped overrides applying to every kind) and `canonical_kinds:` (a map keyed by kind name). They are siblings, not nested. This avoids the parser-ambiguity that would arise if `defaults:` were a reserved key inside the `canonical_kinds:` map: a kind named "defaults" would collide with the override block.

```yaml
# Root-level overrides apply to EVERY canonical kind (code defaults +
# plugin declarations). Useful when the operator wants to tune the
# instruction text or add a globally-applicable gap.
canonical_kinds_defaults:
 instruction:
 enabled: true
 text: "Fill carefully. Cite sources where possible."
 gaps:
 external_url:
 type: string
 description: "Authoritative URL for this entity."

# Per-kind blocks add or override on top of root + plugin + code.
canonical_kinds:
 person:
 gaps:
 birthdate:
 type: date
 description: "Birth date in YYYY-MM-DD if known."
 instruction:
 enabled: true
 text: "Use only verifiable biographical sources."

 boardgame:
 instruction:
 enabled: false # operator opts out for this kind specifically
```

Both `canonical_kinds_defaults:` and `canonical_kinds:` are optional. The minimal valid config omits both: yaad-index uses code defaults plus whatever the active plugins declare.

### 4. Merge precedence

For each canonical kind, the daemon computes the effective gap-set at registry-load time as:

1. Start with `defaultGaps` (always: name/tags/summary).
2. Layer in plugin-declared additions/overrides (plugins enabled in `plugins:` order; later-loaded plugins override earlier ones for shared fields, with a WARN log per conflict).
3. Layer in operator `canonical_kinds_defaults:` (root-level top-level key). Adds gaps that apply to every kind; overrides any same-name gap from layer 1 or 2.
4. Layer in operator `canonical_kinds.<kind>:` (per-kind, sibling map). Adds kind-specific gaps; overrides any same-name gap from layers 1-3.

**Instruction does not merge across these four layers.** Code default is empty; plugins are forbidden from declaring instruction; the only layers that contribute to the effective `instruction.text` and `instruction.enabled` are operator-defaults (root) and operator per-kind. Per-kind operator config wins over operator-defaults for the kinds it covers; root applies to every other kind. The instruction merge is two-layer (operator-only) while gaps are four-layer.

### 5. Conflict resolution

When two plugins declare the same canonical kind (e.g., yaad-wikipedia and yaad-bgg both emit `person`):

- **Gap additions**: union. wikipedia's `birth_year` + bgg's `bgg_id` both become available on `person`.
- **Gap-description conflicts** (both plugins describe `summary` differently): last-loaded plugin wins per `plugins:` config order. Daemon emits WARN per conflict (`plugin "X" overrode plugin "Y"'s description for kind=person field=summary; consider explicit operator override`). Operator override (layer 3 or 4) supersedes either plugin's choice.
- **Instruction**: never enters plugin-side conflict resolution because plugins can't declare instruction at all. Effective instruction is operator-only (root or per-kind).

The WARN-and-prompt-for-explicit-override pattern surfaces ambiguity to the operator without blocking server start. If the operator wants determinism on a contested gap description, they set the value explicitly in their config; the conflict-warning goes away.

### 6. Non-removable rule

**Nothing is removable in v1.** No layer can remove a gap that another layer has declared. The merge is purely additive: each layer adds new gaps, overrides existing descriptions, but never drops fields.

`name`, `tags`, `summary` have an extra structural reason for non-removability: they are the columns the DB indexes for search; removing them would silently break the `/v1/search` API. But v1's general rule applies to every gap, not just those three — once a layer (code, plugin, or operator) declares a field, downstream layers cannot delete it.

If removal becomes necessary later (e.g., a plugin author wants to deprecate a field, or an operator wants to opt out of a plugin-declared gap), a future ADR introduces a removal mechanism (explicit `remove: true` flag, null-as-sentinel, etc.). v1: no removal, only add + override-description.

### 6.5 Plugin capabilities consistency

A plugin's `canonical_kinds_extras` may only reference kinds the plugin also lists in `canonical_kinds_emitted`. If `extras` declares a kind absent from `emitted`, the daemon ignores that extras entry and logs a WARN naming the plugin and the dangling kind. This handles the case where a plugin author copy-paste-typoes the kind name in extras while the canonical declaration is correct: the working declaration wins, the typo doesn't silently introduce a kind into the registry.

### 7. Typed gaps

Gap shape supports two forms: shorthand for string types, long form for typed fields.

**Shorthand** (string-typed, just description):

```yaml
gaps:
 name: "The name of the entity."
```

Equivalent to:

```yaml
gaps:
 name:
 type: string
 description: "The name of the entity."
```

**Supported types** (v1):

- `string` (default if shorthand or omitted)
- `bool`
- `number` (numeric, integer or float)
- `date` (YYYY-MM-DD)
- `datetime` (RFC3339)
- `[]string` (free-form array of strings; used by the built-in `tags` gap)

**Out of scope (v1)**: arbitrary array-of-typed-X (e.g., `[]date`, `[]number`). If a plugin needs that, they can use `[]string` and parse on retrieval, OR file a follow-up to extend type system. `tags` is the only built-in `[]string` for v1.

**Type semantics in v1**: types are descriptive, not enforced.

- At capabilities-load + config-load: the type label is stored alongside the gap in the registry.
- At AI-fill prompt construction: the type is passed to the AI as a format hint (e.g., `birthdate (type: date, format YYYY-MM-DD)`). The AI is expected to honor the hint; the daemon doesn't validate the response shape.
- At store time: values are persisted into the entity's `data` JSON column as their JSON-native form (string, number, bool, array). `date` and `datetime` are JSON strings (no native JSON date type) — the daemon stores whatever the AI emitted without parsing or normalizing.
- For `name`, `tags`, `summary`: the daemon enforces shape (string and `[]string` respectively) since these are DB-indexed columns. Type mismatch on these three is a hard error at store time.

Strict format validation, range checks, and type-coerced storage are deferred to a future ADR if real query needs surface (e.g., the typed-search-API discussed in §8).

### 8. Search implications

`name`, `tags`, `summary` are the gaps the DB indexes for `/v1/search`. Other gaps are stored in the entity's `data` JSON column and are searched only as substring against the JSON blob (the existing `Search` implementation does `WHERE id LIKE ? OR data LIKE ?`).

Plugin authors and operators who want a field searchable as more than substring (e.g., `birthdate BETWEEN 1990-01-01 AND 2000-12-31`) need a separate API extension (`search_by_field(kind, field, op, value)`). That's its own ADR / issue, not in scope here.

## Alternatives considered

### A. Keep the current explicit-everything shape

Rejected. Demonstrated ergonomic problem: operator types the same name/tags/summary block per kind, and plugin-declared kinds have to be re-enabled in config. The work surface this creates is real friction in practice.

### B. Code-only defaults with no operator override

Rejected. Operator needs to:
- Tune instruction text (different domains have different fill styles)
- Toggle `instruction.enabled` per kind (some kinds shouldn't get AI fill)
- Add domain-specific gaps without modifying the daemon binary

A code-only contract makes yaad-index too rigid.

### C. Plugin-controllable instruction (text or enabled)

Rejected. AI fill is an operational cost (LLM calls, token spend, possible privacy implications) AND a behavioral attack surface (plugin author writes "something bad" as instruction text, operator-enables-globally, plugin's per-type text wins via override order). Closing only the `enabled` flag is insufficient: a plugin proposing text plus an operator who toggles enabled hands the AI plugin-controlled prose. Closing the entire instruction struct (text + enabled) is the load-bearing move. Operator owns end-to-end.

### D. Sentinel-based field removal (`-` or null)

Deferred to v2. Removal introduces complexity (which layer's field is being removed? does removal cascade across layers? what if a field is required by another layer?) that v1 doesn't need. v1 is purely additive: more layers add more gaps and override descriptions, never remove.

### E. Explicit `enabled: false/true` per kind to disable plugin-declared kinds

Considered, partially adopted. The `instruction.enabled` flag is the operator's primary gate; if the operator doesn't want AI fill on a kind, they set `enabled: false` per kind. Whether to ALSO support disabling a kind's MATERIALIZATION (i.e., the kind's stubs don't appear in the DB at all even if plugins emit them) is an open question carried forward — not in scope here.

## Consequences

### Daemon work

- Define `defaultGaps` (name/tags/summary) and `defaultInstruction` constants in `internal/config`.
- Implement the four-layer merge at registry-load time. The merged registry is the source of truth for `/v1/needs_fill`, AI-fill prompts, and DB column generation.
- Capabilities parser learns to read plugin's `canonical_kinds_extras` for GAP additions/overrides only.
- Reject any plugin-supplied `instruction` field (text or enabled) with WARN naming the plugin.
- WARN on plugin-vs-plugin same-field description conflicts.
- Validate per-kind blocks: name/tags/summary cannot be `null`-or-equivalent; descriptions for the three are constrained to "override-only", never "remove".

### Plugin SDK / contract

- Plugin Capabilities document gains a `canonical_kinds_extras` map (optional). Plugins emitting only kinds with default gaps don't need it.
- Plugin authors document the kinds they emit with proposed descriptions; operators can ignore or override.
- yaad-bgg + yaad-wikipedia: existing plugins update their Capabilities to use the new shape (additive, non-breaking — old shapes still parse if the daemon backward-compat handles `canonical_kinds_emitted` without `extras`).

### Operator surface

- Existing operator configs continue to work (the explicit-everything shape remains valid; the merge just produces the same effective registry).
- Operators tuning their config can simplify drastically: an empty `canonical_kinds:` block (or omitting it) gives them code defaults + active-plugin kinds. Per-kind blocks add only overrides + extras.
- New operators have a discoverable starting point: name/tags/summary are always there, plugin-declared kinds light up automatically.

### Migration

yaad-index is pre-tagged-release. Existing configs with explicit-everything shape continue to work. Operators wanting the simpler shape rewrite their config; the daemon doesn't enforce the new shape.

When yaad-index tags v1, this ADR's migration section will need filling in. Until then, no migration is mandatory.

## Open questions

Carried forward, not blockers:

1. **Disabling kind materialization** — operator wants a plugin-declared kind to NOT appear in the DB at all. Today the only knob is `instruction.enabled`. Whether to add a separate `materialize: false` per kind is a follow-up.
2. **Field removal** — for when a plugin author or operator wants to drop a non-required field. v2 concern.
3. **Search-by-typed-field API** — `search_by_field(kind, field, op, value)` for typed range queries. Separate API extension, separate ADR.
4. **Capabilities-document versioning** — when the Capabilities shape evolves further, plugins on older shapes need to keep working. Today the daemon parses what it knows and ignores unknown fields; that's fine for additive growth but a real schema-version contract may be needed eventually.
