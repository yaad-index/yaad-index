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

	// Kind is the entity-kind filter for
	// TriggerTypeEntityCreated. Empty = no kind filter (all new
	// entities match).
	Kind string

	// Gap is the gap-name filter for TriggerTypeFillCompleted.
	// Empty = no gap filter (any fill on any gap matches).
	Gap string

	// Source is the optional Source-tag filter for
	// TriggerTypeFillCompleted — useful for the "act only on
	// operator-authored fills" shape. Recognised values mirror
	// internal/eventbus.Source: "agent", "operator",
	// "workflow:<name>". Empty = no source filter.
	Source string
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
	TaskAppend     *TaskAppendAction
	AddNote        *AddNoteAction
	PluginDispatch *PluginDispatchAction
	AddGap         *AddGapAction
	SetProperty    *SetPropertyAction
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

// Trigger type constants — the v1 closed set per ADR-0024
// §"Trigger types (v1)". Internal time-based is deferred
// post-v1; external host cron + manual covers the immediate
// need.
const (
	TriggerTypeEdgeCreated    = "edge_created"
	TriggerTypeEntityCreated  = "entity_created"
	TriggerTypeFillCompleted  = "fill_completed"
	TriggerTypeManual         = "manual"
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
