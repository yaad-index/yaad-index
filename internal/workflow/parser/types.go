// Package parser implements the operator-authored workflow file
// reader for the workflow engine per ADR-0024 §"Workflow". Each
// workflow lives in a markdown file under `vault/workflows/<name>.md`
// (operator-extensible) or the daemon-side workflow config path
// (reserved for system-shipped workflows; vault-only is effective
// in v1.x).
//
// **File shape.** Frontmatter (YAML between `---` fences) carries
// metadata (name, version, status). The body's prose is operator-
// readable documentation. A single YAML code-fence inside the
// body holds the structured rules the engine parses
// (trigger, condition, actions, etc.). The split keeps the
// metadata + the engine config + the human explanation cleanly
// separable.
//
// **Phase 1 scope.** This package parses + validates the schema
// shape. CEL expression syntax validation is deferred to Phase 3
// (when the cel-go integration lands); Phase 1 treats CEL fields
// as opaque strings so workflow authors can ship files now and
// have CEL-syntax errors surface later without re-parsing the
// shell.
//
// Loader / registry / hot-reload concerns live in Phase 1.B (a
// sibling package or sub-package); this file is parse + validate
// for a single document.

package parser

// Workflow is the parsed, validated representation of one operator-
// authored workflow file. All CEL-expression fields are stored as
// opaque strings; the CEL parse step lands in Phase 3.
type Workflow struct {
	// Name is the workflow's unique identifier (from frontmatter).
	// The loader uses this for de-duplication across files; the
	// engine surfaces it in event-tag bookkeeping
	// (`workflow:<name>` source per ADR-0024 §"Internal event
	// bus").
	Name string

	// Version is an operator-managed schema-version marker on the
	// workflow file itself (not the engine's schema). 1 is the
	// only value v1 recognises; the parser preserves whatever
	// the operator sets so a future v2-aware loader can branch.
	Version int

	// Status is the workflow's lifecycle stage. Recognised values
	// (advisory in Phase 1; the engine in later phases gates
	// subscription on this):
	//   - "active" (default) — workflow is live, subscribes to
	//     events, fires on matches.
	//   - "paused" — workflow is parsed + registered but the
	//     engine doesn't subscribe; useful for operator-side
	//     debugging.
	//   - "draft" — workflow is parsed but not registered;
	//     reserved for future-WIP shapes.
	Status string

	// AllowedPlugins is the workflow's import-shape declaration
	// per ADR-0024 §"Workflow declares its plugin scope". Each
	// entry is a plugin name (e.g. `yaad-gmail`); the loader
	// validates each against the live plugin registry at load
	// time and rejects the workflow file if any are absent.
	// Required + non-empty: every workflow declares its scope.
	AllowedPlugins []string

	// Trigger names the event class the workflow subscribes to.
	// Required.
	Trigger Trigger

	// Subject is a CEL template that derives the per-fire
	// subject string. The task path is
	// `tasks/<workflow>-<subject>.md`, so the subject template
	// is what scopes "same situation" together (e.g.
	// `{{ entity.slug }}` keys per-entity, `{{ window.day }}`
	// keys per-day for time-windowed shapes). Optional;
	// workflows that omit it default to `entity.id`.
	Subject string

	// Context is the list of named CEL bindings the engine
	// evaluates BEFORE Condition + before Actions. Used for DRY
	// (binding a sub-expression that appears in multiple places)
	// or readability (giving a graph-walked entity a name).
	// Optional.
	Context []ContextBinding

	// Condition is the CEL predicate that gates whether the
	// workflow's actions fire on a given event. Optional; an
	// empty condition is treated as `true` (always fire on
	// trigger match). When set the predicate evaluates against
	// the trigger entity (as `entity`), any Context bindings,
	// and the standard CEL environment (per the Phase-3
	// decision pipeline).
	Condition string

	// Dedup configures the per-pattern de-duplication: the key
	// that scopes "same situation" and the policy that decides
	// what happens when a duplicate-key event fires. Optional;
	// when omitted the engine uses the default key + policy
	// (per ADR-0024 §"Per-pattern de-duplication").
	Dedup Dedup

	// Actions is the ordered list of action primitives the
	// workflow fires when Condition evaluates true. Required:
	// at least one action (a workflow that does nothing is a
	// file-shape error).
	Actions []Action

	// AddableGaps is the unified vocabulary of gaps the workflow
	// is permitted to inject — covers both trigger-time gap
	// injection AND action-stage `add_gap`. Per ADR-0024 §"Output
	// surface — action vocabulary" the single declaration keeps
	// the workflow's gap-side-effect surface visible at
	// file-read time. Optional; workflows that don't inject
	// gaps omit the field.
	AddableGaps []string

	// AutoArchiveOnDone controls whether tasks spawned by this
	// workflow auto-archive on operator-resolve (per ADR-0018
	// archive lifecycle, default true). A pointer so the parser
	// can distinguish "operator omitted the field" from "operator
	// explicitly set false" — the engine treats nil as the
	// default (true).
	AutoArchiveOnDone *bool

	// Filename is the source file's base name (e.g.
	// `01-classify-linkedin.md`). Stamped by the loader after
	// Parse; not parsed from frontmatter. The engine uses this
	// for the deterministic filename-alphabetical ordering of
	// pass-1 + pass-2 workflows per #169 — operators name files
	// `01-`, `02-` to control evaluation order within an event.
	// Tests that build a Workflow inline can leave this empty;
	// the engine's sort falls back to lexicographic-empty-first
	// in that case.
	Filename string

	// ContentHash is the SHA-256 of the raw bytes Parse decoded
	// from, per #280. The engine's Reconcile compares this
	// against the previously-registered workflow's hash to skip
	// no-op unregister/re-register cycles when the loader's
	// 15s poll re-reads an unchanged file. Stamped by Parse
	// itself so every code path that produces a Workflow gets
	// a stable identity (ParseFile + the loader's reload path
	// both flow through Parse). Tests that build a Workflow
	// inline can leave this empty; the no-op gate just
	// degrades to "always re-register" for those, matching the
	// pre-#280 behavior.
	ContentHash string

	// CatchAll marks the workflow as a pass-2 fallback per #169.
	// Regular workflows (CatchAll=false) run in the per-event
	// pass-1 chain; catch_all workflows run in pass-2 only when
	// no pass-1 workflow claimed the event. Catch-alls must NOT
	// declare a `condition` field — the only allowed scoping is
	// the trigger's kind filter (any further condition collapses
	// the catch-all into a low-priority regular workflow, which
	// is the wrong shape). The loader enforces uniqueness:
	// exactly one catch_all workflow may match a given trigger
	// type + kind combination; the global wildcard slot
	// (kind-empty) is its own unique entry.
	CatchAll bool
}

// Trigger names the event the workflow subscribes to.
type Trigger struct {
	// Type is one of the v1 trigger kinds:
	//   - TriggerTypeEdgeCreated — fires on entity.edge_added
	//     events; commonly used for "tagged-as" patterns.
	//   - TriggerTypeEntityCreated — fires on entity.created
	//     (fresh-ingest only; not re-fetch).
	//   - TriggerTypeFillCompleted — fires on fill.completed;
	//     the dominant shape for "operator answered the
	//     question" round-trips.
	//   - TriggerTypeManual — no event subscription; fires only
	//     via workflow.trigger(name, input).
	Type string

	// Match scopes the trigger within its Type. The per-Type
	// validation rules below name which Match fields are
	// required vs. allowed.
	Match TriggerMatch
}

// TriggerMatch is the discriminated-union-shape match scoper.
// Each Type uses a subset of fields; the parser zeroes the
// non-applicable fields when shape-validating.
type TriggerMatch struct {
	// EdgeType is the canonical edge type filter for
	// TriggerTypeEdgeCreated. Required for edge_created;
	// rejected for other types.
	EdgeType string

	// TargetKind is an optional filter on the edge's target
	// entity kind for TriggerTypeEdgeCreated. Empty = no kind
	// filter (all edges of EdgeType match).
	TargetKind string

	// Kinds is the canonical-kind filter for TriggerTypeEntityCreated
	// + TriggerTypeEntityUpdated. Empty list = no kind filter;
	// non-empty = the event's entity kind must appear in the
	// list. YAML key is `canonical_kind`; the operator may
	// specify a single value (`canonical_kind: github-pr`) or a
	// list (`canonical_kind: [github-pr, github-issue]`); both
	// shapes round-trip into the same []string.
	Kinds []string

	// Gap is the gap-name filter for TriggerTypeFillCompleted.
	// Empty = no gap filter (any fill on any gap matches).
	Gap string

	// Source is the optional Source-tag filter for
	// TriggerTypeFillCompleted — useful for the "act only on
	// operator-authored fills" shape. Recognised values mirror
	// internal/eventbus.Source: "agent", "operator",
	// "workflow:<name>". Empty = no source filter.
	Source string

	// FieldChanged is the dotted-path filter for
	// TriggerTypeEntityUpdated naming which field's delta the
	// workflow cares about (e.g. `data.state`). Required for
	// entity_updated; rejected for other types. v1 matches by
	// exact-string equality against the published event's
	// Field — no globbing, no prefix matching.
	FieldChanged string
}

// ContextBinding is one entry in the `context` list — a named
// CEL pre-binding evaluated before Condition.
type ContextBinding struct {
	// Name is the identifier the binding is exposed as in
	// Condition + action templates. Must be non-empty and
	// unique within the workflow's Context list.
	Name string

	// Via is the CEL expression whose value the engine binds to
	// Name. Required non-empty; Phase 3 will parse/typecheck.
	Via string
}

// Dedup configures per-workflow-pattern de-duplication.
type Dedup struct {
	// Key is the CEL template the engine evaluates to derive
	// the dedup-key string. Default (when the workflow omits
	// the dedup stanza): `workflow + entity.id`.
	Key string

	// Policy selects the engine's behavior when a duplicate-key
	// event fires:
	//   - "update" (default) — modify the existing task with
	//     the new event context; the action-level dedup
	//     decides what actually changes inside the task body.
	//   - "skip" — no-op on duplicates; useful when subsequent
	//     triggers are noise.
	//   - "replace" — close the prior task, create a new one;
	//     useful when each fire is a distinct moment to
	//     surface.
	Policy string
}

// Action is the discriminated-union container for one entry in
// the workflow's `actions` list. Exactly one of the action
// fields is non-nil on a valid Workflow; the parser enforces
// this.
type Action struct {
	TaskAppend        *TaskAppendAction
	AddNote           *AddNoteAction
	PluginDispatch    *PluginDispatchAction
	AddGap            *AddGapAction
	SetProperty       *SetPropertyAction
	AddCanonicalEdge  *AddCanonicalEdgeAction
	ArchiveEntity     *ArchiveEntityAction
	RestoreEntity     *RestoreEntityAction
	ClaimEntity       *ClaimEntityAction
}

// TaskAppendAction is the `task_append` primitive per ADR-0024
// §"Output surface — action vocabulary". Appends a content line
// to a named section of the workflow's task file.
type TaskAppendAction struct {
	// Section is the named section under which Content is
	// appended. Required non-empty.
	Section string

	// Content is the CEL template the engine evaluates to
	// produce the line written to the section. Required
	// non-empty.
	Content string

	// IfAlreadyPresent governs the action-level dedup behavior
	// when Content's expanded string already exists in the
	// target section. Recognised values:
	//   - "skip" (default) — no-op on duplicate line.
	//   - "replace" — rewrite the matching line only (per
	//     ADR-0024's `if_already_present` scope: matching line
	//     only, not the section).
	//   - "append-anyway" — write a duplicate line regardless.
	IfAlreadyPresent string
}

// AddNoteAction is the `add_note` primitive — attaches a
// note to an existing entity via the standard notes
// pathway.
type AddNoteAction struct {
	// Target is the CEL expression that resolves to the
	// target entity's id. Defaults to `entity.id` (the
	// triggering entity) when omitted.
	Target string

	// Content is the CEL template the engine evaluates to
	// produce the note body. Required non-empty.
	Content string

	// Field is the optional per-field scope per #186 (e.g.
	// `birth_date`). Static string — not a CEL template;
	// workflow authors typically scope to a known field
	// rather than computing it. Empty → entity-level note.
	Field string

	// Kind discriminates everyday notes from agent-feedback
	// annotations per #186. Empty / `note` → operator-level
	// commentary (default); `annotation` → agent observation
	// surfaced for operator attention via the read-side kind
	// filter.
	Kind string
}

// PluginDispatchAction is the `plugin_dispatch` primitive — fires
// a plugin command from inside the workflow per ADR-0024
// §"plugin_dispatch execution semantics".
type PluginDispatchAction struct {
	// Plugin is the plugin name to dispatch to. Must appear in
	// the workflow's AllowedPlugins list (enforced at load
	// time).
	Plugin string

	// Command is the plugin-command shorthand (per ADR-0006
	// command syntax). Required non-empty.
	Command string

	// Args is the optional argument map passed to the plugin
	// dispatch. Values are opaque to the parser; the plugin's
	// own contract validates them.
	Args map[string]any

	// TimeoutSeconds is the synchronous-await budget. Defaults
	// to 30s per ADR-0024 §"plugin_dispatch execution
	// semantics".
	TimeoutSeconds int
}

// AddGapAction is the `add_gap` primitive — injects a gap onto
// an entity from the action stage per ADR-0024 §"Constraints on
// add_gap". The Gap value MUST appear in the workflow's
// AddableGaps vocabulary; the parser enforces this.
type AddGapAction struct {
	// Entity is the CEL expression that resolves to the target
	// entity id. Defaults to `entity.id` when omitted.
	Entity string

	// Gap is the gap-field name to inject. Must be a member of
	// the workflow's AddableGaps list.
	Gap string

	// DataSchema is the optional per-key extraction guidance the
	// agent's fill prompt sees for canonical_type gaps carrying
	// the optional per-entry `data` map. Map key = data-field
	// name; map value = natural-language extraction instruction.
	// Persisted on the gap's GapStateEntry; surfaced on
	// `/v1/needs-fill` so the prompt builder includes the
	// instruction set. Empty / nil omits.
	DataSchema map[string]string

	// Type / Description / FillStrategy / Range / MaxLength /
	// Values / Kinds carry the gap's full shape inline per #142.
	// Mirrors the operator-config GapSpec shape. When set, the
	// runner persists them on the gap's GapStateEntry so
	// `/v1/needs-fill` surfaces the workflow-injected gap with
	// its shape (the same metadata an operator-config-registered
	// gap would surface).
	//
	// All fields are optional individually; the parser + loader
	// enforce internal consistency at workflow-load time:
	//   - Type must be one of {string, int, enum, canonical_type}
	//     when set; empty falls through to operator-config or
	//     "string" default.
	//   - Kinds required when Type=="canonical_type", rejected
	//     otherwise.
	//   - Range integer-pair, Type=="int" only.
	//   - MaxLength positive, Type=="string" only.
	//   - Values non-empty, Type=="enum" only and required.
	//   - FillStrategy in {agent, operator, both}; default "both".
	Type         string
	Description  string
	FillStrategy string
	Range        []int
	MaxLength    int
	Values       []string
	Kinds        []string
}

// SetPropertyAction is the `set_property` primitive — writes
// static or CEL-templated values directly into the target
// entity's frontmatter `data` map without going through the
// fill machinery. Suited for derive-from-existing-context
// classifications where an LLM call is overkill.
//
// Merge semantics: per-field overwrite (last-write-wins on key
// collisions with existing `data` entries; other keys are
// preserved). The runner publishes one `fill.completed` event
// per field that lands so downstream workflows can subscribe
// per-field, mirroring the per-gap event shape from the fill
// pathway.
// AddCanonicalEdgeAction is the `add_canonical_edge` primitive
// per #132 — the deterministic-fill counterpart to `add_gap` for
// canonical_type gaps. The workflow declares the edge target
// inline (CEL-rendered name + literal kind) and the runner
// creates the canonical-label edge directly, bypassing the
// agent-fill round-trip. Optional per-entry `data:` map mirrors
// the #119 canonical_type-fill data shape and lands as a
// dataview-inline paragraph on the target canonical entity's
// body, auto-materializing the target vault file if absent per
// ADR-0021 §3.
type AddCanonicalEdgeAction struct {
	// Source is the CEL expression that resolves to the entity
	// the edge originates from. Defaults to `entity.id` (the
	// triggering entity) when omitted.
	Source string

	// EdgeType is the canonical edge type (literal, not CEL).
	// Validated against the daemon's canonical_edge_types
	// allowlist at workflow-registration time, not at action-
	// fire time, so a workflow file with an unknown edge type
	// is rejected on load.
	EdgeType string

	// TargetKind is the canonical kind of the edge target
	// (literal, not CEL). Validated against the daemon's
	// canonical_kinds registry at workflow-registration time.
	TargetKind string

	// TargetName is the CEL expression the runner evaluates to
	// produce the canonical-label name. The daemon slugifies
	// the resolved value via the deterministic clean-slug rule
	// to produce the canonical-label id (`<TargetKind>:<slug>`)
	// per ADR-0021 §1.
	TargetName string

	// Data is the optional per-entry data map: key → CEL
	// expression. The runner evaluates each value and the
	// daemon writes the resolved map as a sorted-key
	// dataview-inline paragraph on the target canonical
	// entity's body per #119. Empty / nil omits the paragraph
	// append (edge-only fire). Duplicate paragraphs (same
	// content hash) dedup at append time.
	Data map[string]string
}

type SetPropertyAction struct {
	// Entity is the CEL expression that resolves to the target
	// entity id. Defaults to `entity.id` (the triggering
	// entity) when omitted.
	Entity string

	// Fields maps field-name → CEL template the runner
	// evaluates to produce the value written to that field.
	// Required non-empty; empty field names rejected at
	// validate time.
	Fields map[string]string
}

// ClaimEntityAction is the `claim_entity` primitive per #169.
// When the workflow's per-event chain runs, a fired
// claim_entity flags the event as claimed and stops further
// workflow dispatch for that event (no remaining pass-1
// workflows fire, no pass-2 catch_all fires). The action
// itself produces no side effect outside the engine's
// in-process queue state.
//
// v1 has no fields — the action is a bare `- claim_entity: {}`
// invocation. A future revision may add a `reason: string` for
// audit purposes if operators want to record why a particular
// workflow claimed (separate from the workflow name, which is
// already tracked in the queue's claimed_by field).
type ClaimEntityAction struct{}

// ArchiveEntityAction is the `archive_entity` primitive per
// #150 — flips an entity into ADR-0018's archived state from
// inside the workflow action vocabulary. Thin adapter onto the
// operator-side archive surface (the same `vault.Writer.ArchiveWithCommit`
// + `store.ArchiveEntity` pair the HTTP handler uses); the
// workflow runner just resolves the target id + reason via CEL,
// hands them to the writer, and writers acquire the per-entity
// write-lock via `AcquireWithTimeout` for async-side contention
// shape per PR-153.
//
// Idempotent: archiving an already-archived entity is a no-op
// (the vault-side already-at-destination short-circuit + the
// store-side `COALESCE(archived_at, ?)` both preserve the
// original archive timestamp). Entity-not-found is a soft skip:
// the runner logs and returns success so the workflow chain
// continues — the entity may have been archived by another
// path before this action fired.
type ArchiveEntityAction struct {
	// Entity is the CEL expression that resolves to the target
	// entity id. Defaults to `entity.id` (the triggering
	// entity) when omitted — same shape as add_note / add_gap /
	// set_property.
	Entity string

	// Reason is the optional CEL expression the runner evaluates
	// to produce a free-form audit string recorded with the
	// archive event. Empty (or empty after render) leaves the
	// workflow name as the implicit source.
	Reason string
}

// RestoreEntityAction is the `restore_entity` primitive — the
// mirror of `archive_entity` per ADR-0024's 2026-05-21
// amendment. Flips an entity back out of ADR-0018's archived
// state from inside the workflow action vocabulary. Same shape
// as ArchiveEntityAction (Entity defaults to `entity.id`, Reason
// is audit-only); same idempotence + soft-skip contract
// (restoring an already-active entity is a no-op; not-found
// is a soft skip).
type RestoreEntityAction struct {
	// Entity is the CEL expression that resolves to the target
	// entity id. Defaults to `entity.id` when omitted.
	Entity string

	// Reason is the optional CEL audit string folded into the
	// restore commit message.
	Reason string
}

// Trigger type constants — the v1 closed set per ADR-0024
// §"Trigger types (v1)". Internal time-based is deferred
// post-v1; external host cron + manual covers the immediate
// need.
const (
	TriggerTypeEdgeCreated    = "edge_created"
	TriggerTypeEntityCreated  = "entity_created"
	TriggerTypeFillCompleted  = "fill_completed"
	TriggerTypeManual         = "manual"
	// TriggerTypeEntityUpdated is the trigger type per ADR-0024's
	// 2026-05-21 amendment: workflows subscribe to per-field data
	// deltas surfaced by ingest re-fetch. Pairs with the
	// `field_changed` Match field — required, names the dotted
	// data path the workflow cares about.
	TriggerTypeEntityUpdated  = "entity_updated"
)

// Dedup policy constants — the v1 closed set per ADR-0024
// §"Per-pattern de-duplication".
const (
	DedupPolicyUpdate  = "update"
	DedupPolicySkip    = "skip"
	DedupPolicyReplace = "replace"
)

// Action-level `if_already_present` modes per ADR-0024
// §"Action-level match semantics".
const (
	IfAlreadyPresentSkip          = "skip"
	IfAlreadyPresentReplace       = "replace"
	IfAlreadyPresentAppendAnyway  = "append-anyway"
)

// Status constants — the v1 closed set.
const (
	StatusActive = "active"
	StatusPaused = "paused"
	StatusDraft  = "draft"
)

// NoteKind constants — the v1 closed set per #186 for the
// add_note action's `kind:` field. Mirror vault.NoteKind* by
// value; kept local to the parser package so the parser
// stays free of an internal/vault dependency.
const (
	NoteKindNote       = "note"
	NoteKindAnnotation = "annotation"
)
