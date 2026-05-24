// Schema validation. The decode step in parser.go converts the
// YAML into a Workflow shape; Validate runs the post-decode
// rules — required fields, allowed values, cross-field
// constraints (e.g. add_gap's gap must be in AddableGaps). CEL
// expression syntax validation is deferred to Phase 3 (when the
// cel-go integration lands).

package parser

import (
	"fmt"
	"strings"
)

// Validate runs the post-decode schema rules on wf. Returns nil
// when the workflow is well-formed; otherwise an error naming
// the offending field + the specific rule violated. The loader
// surfaces these as structured "file rejected" log lines.
//
// Rules enforced here:
//
//   - Name required, non-empty.
//   - AllowedPlugins required, non-empty, each entry non-empty.
//   - Status, if set, must be one of {active, paused, draft}.
//   - Trigger.Type required, one of the closed set.
//   - Per Trigger.Type: required + forbidden Match fields.
//   - Context entries: Name + Via both required; Name unique
//     within the list.
//   - Dedup.Policy, when set, must be one of the closed set.
//   - Actions required, non-empty; each action sets exactly one
//     primitive.
//   - TaskAppend: Section + Content required; IfAlreadyPresent
//     in the closed set when set.
//   - AddNote: Content required.
//   - PluginDispatch: Plugin + Command required; Plugin must
//     appear in AllowedPlugins; TimeoutSeconds non-negative.
//   - AddGap: Gap required + must be a member of
//     AddableGaps.
//   - AddableGaps entries non-empty + unique.
func Validate(wf *Workflow) error {
	if wf == nil {
		return fmt.Errorf("workflow: nil")
	}
	if wf.Name == "" {
		return fmt.Errorf("workflow: name is required")
	}
	if err := validateStatus(wf.Status); err != nil {
		return err
	}
	if err := validateAllowedPlugins(wf.AllowedPlugins, hasPluginDispatch(wf.Actions)); err != nil {
		return err
	}
	if err := validateAddableGaps(wf.AddableGaps); err != nil {
		return err
	}
	if err := validateTrigger(wf.Trigger); err != nil {
		return err
	}
	if err := validateContext(wf.Context); err != nil {
		return err
	}
	if err := validateDedup(wf.Dedup); err != nil {
		return err
	}
	if err := validateActions(wf); err != nil {
		return err
	}
	if err := validateCatchAll(wf); err != nil {
		return err
	}
	return nil
}

// validateCatchAll enforces the #169 catch-all constraints on a
// single workflow: catch-alls cannot carry a `condition` field
// (the only allowed scoping is trigger.kind; any further
// condition collapses the catch-all into a low-priority regular
// workflow which is the wrong shape). Uniqueness-per-kind is
// enforced at load time (loader sees the full registered set);
// this per-workflow check is the lighter "is this workflow's
// own shape valid" gate.
func validateCatchAll(wf *Workflow) error {
	if !wf.CatchAll {
		return nil
	}
	if strings.TrimSpace(wf.Condition) != "" {
		return fmt.Errorf("workflow: catch_all=true forbids `condition` (the only allowed scoping is trigger.kind; per-event filtering collapses the catch-all into a regular workflow)")
	}
	return nil
}

func validateStatus(s string) error {
	switch s {
	case "", StatusActive, StatusPaused, StatusDraft:
		return nil
	}
	return fmt.Errorf("workflow: status %q is not one of {active, paused, draft}", s)
}

// hasPluginDispatch reports whether the workflow's action list
// contains at least one plugin_dispatch primitive. Used to gate
// the allowed_plugins required-non-empty check — workflows that
// don't dispatch through plugins (manual-trigger digests,
// add_canonical_edge / set_property / add_note / task_append only)
// have no plugin surface to declare and can legitimately omit
// the field.
func hasPluginDispatch(actions []Action) bool {
	for _, a := range actions {
		if a.PluginDispatch != nil {
			return true
		}
	}
	return false
}

func validateAllowedPlugins(plugins []string, required bool) error {
	if len(plugins) == 0 {
		if required {
			return fmt.Errorf("workflow: allowed_plugins is required and must be non-empty when actions include plugin_dispatch")
		}
		return nil
	}
	seen := make(map[string]struct{}, len(plugins))
	for i, p := range plugins {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("workflow: allowed_plugins[%d] is empty", i)
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("workflow: allowed_plugins[%d]=%q is duplicated", i, p)
		}
		seen[p] = struct{}{}
	}
	return nil
}

func validateAddableGaps(gaps []string) error {
	seen := make(map[string]struct{}, len(gaps))
	for i, g := range gaps {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("workflow: addable_gaps[%d] is empty", i)
		}
		if _, dup := seen[g]; dup {
			return fmt.Errorf("workflow: addable_gaps[%d]=%q is duplicated", i, g)
		}
		seen[g] = struct{}{}
	}
	return nil
}

func validateTrigger(t Trigger) error {
	switch t.Type {
	case TriggerTypeEdgeCreated:
		if t.Match.EdgeType == "" {
			return fmt.Errorf("workflow: trigger.match.edge_type is required for trigger.type=%s", t.Type)
		}
		if err := rejectTriggerFields(t.Match, "Kinds", "Gap", "Source", "FieldChanged"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeEntityCreated:
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Gap", "Source", "FieldChanged"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeFillCompleted:
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Kinds", "FieldChanged"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeManual:
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Kinds", "Gap", "Source", "FieldChanged"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeEntityUpdated:
		if t.Match.FieldChanged == "" {
			return fmt.Errorf("workflow: trigger.match.field_changed is required for trigger.type=%s", t.Type)
		}
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Gap", "Source"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case "":
		return fmt.Errorf("workflow: trigger.type is required")
	default:
		return fmt.Errorf("workflow: trigger.type %q is not one of {edge_created, entity_created, fill_completed, manual, entity_updated}", t.Type)
	}
	return nil
}

// rejectTriggerFields returns an error if any of the named
// Match fields is non-empty. Used to enforce the per-Type
// discriminated-union shape — e.g. an edge_created trigger
// MUST NOT carry a `gap` filter.
func rejectTriggerFields(m TriggerMatch, fields ...string) error {
	for _, f := range fields {
		val := ""
		switch f {
		case "EdgeType":
			val = m.EdgeType
		case "TargetKind":
			val = m.TargetKind
		case "Kinds":
			if len(m.Kinds) > 0 {
				return fmt.Errorf("trigger.match.canonical_kind=%v is not valid for this trigger.type",
					m.Kinds)
			}
			continue
		case "Gap":
			val = m.Gap
		case "Source":
			val = m.Source
		case "FieldChanged":
			val = m.FieldChanged
		}
		if val != "" {
			return fmt.Errorf("trigger.match.%s=%q is not valid for this trigger.type",
				toYAMLKey(f), val)
		}
	}
	return nil
}

func toYAMLKey(goField string) string {
	switch goField {
	case "EdgeType":
		return "edge_type"
	case "TargetKind":
		return "target_kind"
	case "Kinds":
		return "canonical_kind"
	case "Gap":
		return "gap"
	case "Source":
		return "source"
	case "FieldChanged":
		return "field_changed"
	}
	return goField
}

func validateContext(entries []ContextBinding) error {
	seen := make(map[string]struct{}, len(entries))
	for i, e := range entries {
		if e.Name == "" {
			return fmt.Errorf("workflow: context[%d].name is required", i)
		}
		if e.Via == "" {
			return fmt.Errorf("workflow: context[%d].via is required (context.name=%q)", i, e.Name)
		}
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("workflow: context[%d].name=%q is duplicated", i, e.Name)
		}
		seen[e.Name] = struct{}{}
	}
	return nil
}

func validateDedup(d Dedup) error {
	switch d.Policy {
	case "", DedupPolicyUpdate, DedupPolicySkip, DedupPolicyReplace:
		return nil
	}
	return fmt.Errorf("workflow: dedup.policy %q is not one of {update, skip, replace}", d.Policy)
}

func validateActions(wf *Workflow) error {
	if len(wf.Actions) == 0 {
		return fmt.Errorf("workflow: actions is required and must be non-empty")
	}
	gapSet := make(map[string]struct{}, len(wf.AddableGaps))
	for _, g := range wf.AddableGaps {
		gapSet[g] = struct{}{}
	}
	pluginSet := make(map[string]struct{}, len(wf.AllowedPlugins))
	for _, p := range wf.AllowedPlugins {
		pluginSet[p] = struct{}{}
	}
	for i, a := range wf.Actions {
		set := 0
		if a.TaskAppend != nil {
			set++
		}
		if a.AddNote != nil {
			set++
		}
		if a.PluginDispatch != nil {
			set++
		}
		if a.AddGap != nil {
			set++
		}
		if a.SetProperty != nil {
			set++
		}
		if a.AddCanonicalEdge != nil {
			set++
		}
		if a.ArchiveEntity != nil {
			set++
		}
		if a.RestoreEntity != nil {
			set++
		}
		if a.ClaimEntity != nil {
			set++
		}
		if set == 0 {
			return fmt.Errorf("workflow: actions[%d] sets no primitive (expected exactly one of task_append / add_note / plugin_dispatch / add_gap / set_property / add_canonical_edge / archive_entity / restore_entity / claim_entity)", i)
		}
		if set > 1 {
			return fmt.Errorf("workflow: actions[%d] sets %d primitives (expected exactly one)", i, set)
		}
		switch {
		case a.TaskAppend != nil:
			if err := validateTaskAppend(a.TaskAppend); err != nil {
				return fmt.Errorf("workflow: actions[%d].task_append: %w", i, err)
			}
		case a.AddNote != nil:
			if err := validateAddNote(a.AddNote); err != nil {
				return fmt.Errorf("workflow: actions[%d].add_note: %w", i, err)
			}
		case a.PluginDispatch != nil:
			if err := validatePluginDispatch(a.PluginDispatch, pluginSet); err != nil {
				return fmt.Errorf("workflow: actions[%d].plugin_dispatch: %w", i, err)
			}
		case a.AddGap != nil:
			if err := validateAddGap(a.AddGap, gapSet); err != nil {
				return fmt.Errorf("workflow: actions[%d].add_gap: %w", i, err)
			}
		case a.SetProperty != nil:
			if err := validateSetProperty(a.SetProperty); err != nil {
				return fmt.Errorf("workflow: actions[%d].set_property: %w", i, err)
			}
		case a.AddCanonicalEdge != nil:
			if err := validateAddCanonicalEdge(a.AddCanonicalEdge); err != nil {
				return fmt.Errorf("workflow: actions[%d].add_canonical_edge: %w", i, err)
			}
		case a.ArchiveEntity != nil:
			if err := validateArchiveEntity(a.ArchiveEntity); err != nil {
				return fmt.Errorf("workflow: actions[%d].archive_entity: %w", i, err)
			}
		case a.RestoreEntity != nil:
			if err := validateRestoreEntity(a.RestoreEntity); err != nil {
				return fmt.Errorf("workflow: actions[%d].restore_entity: %w", i, err)
			}
		case a.ClaimEntity != nil:
			// claim_entity is a bare flag — no fields to validate
			// per #169 v1. The engine reads the action presence
			// and halts the per-event chain when fired.
		}
	}
	return nil
}

// validateArchiveEntity is the load-time check for #150's
// `archive_entity` action. Both `entity` and `reason` are
// optional CEL strings (Entity defaults to `entity.id` at runner
// time; Reason is purely audit metadata). The validation surface
// is intentionally permissive — there's no required field —
// because workflow authors writing `- archive_entity: {}` to
// archive the triggering entity is a legitimate shape.
func validateArchiveEntity(_ *ArchiveEntityAction) error {
	return nil
}

// validateRestoreEntity is the load-time check for the
// `restore_entity` action — the mirror of archive_entity per
// ADR-0024's 2026-05-21 amendment. Same permissive surface:
// both Entity and Reason are optional (Entity defaults to
// `entity.id` at runner time; Reason is audit metadata).
func validateRestoreEntity(_ *RestoreEntityAction) error {
	return nil
}

func validateTaskAppend(a *TaskAppendAction) error {
	if a.Section == "" {
		return fmt.Errorf("section is required")
	}
	if strings.TrimSpace(a.Content) == "" {
		return fmt.Errorf("content is required")
	}
	switch a.IfAlreadyPresent {
	case "", IfAlreadyPresentSkip, IfAlreadyPresentReplace, IfAlreadyPresentAppendAnyway:
		return nil
	}
	return fmt.Errorf("if_already_present %q is not one of {skip, replace, append-anyway}", a.IfAlreadyPresent)
}

func validateAddNote(a *AddNoteAction) error {
	if strings.TrimSpace(a.Content) == "" {
		return fmt.Errorf("content is required")
	}
	switch a.Kind {
	case "", NoteKindNote, NoteKindAnnotation:
		// accepted
	default:
		return fmt.Errorf("kind %q is not recognised (want %q or %q)",
			a.Kind, NoteKindNote, NoteKindAnnotation)
	}
	return nil
}

func validatePluginDispatch(a *PluginDispatchAction, allowed map[string]struct{}) error {
	if a.Plugin == "" {
		return fmt.Errorf("plugin is required")
	}
	if a.Command == "" {
		return fmt.Errorf("command is required")
	}
	if _, ok := allowed[a.Plugin]; !ok {
		return fmt.Errorf("plugin %q is not in the workflow's allowed_plugins list", a.Plugin)
	}
	if a.TimeoutSeconds < 0 {
		return fmt.Errorf("timeout_seconds=%d is negative", a.TimeoutSeconds)
	}
	return nil
}

func validateAddGap(a *AddGapAction, addable map[string]struct{}) error {
	if a.Gap == "" {
		return fmt.Errorf("gap is required")
	}
	if _, ok := addable[a.Gap]; !ok {
		return fmt.Errorf("gap %q is not in the workflow's addable_gaps vocabulary", a.Gap)
	}
	for k, v := range a.DataSchema {
		if k == "" {
			return fmt.Errorf("data_schema key is empty (after trim) — schema field names must be non-empty")
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("data_schema[%q] value is empty — extraction instruction must be non-empty", k)
		}
	}
	// #142 inline gap spec validation. Mirrors the
	// internal/config GapSpec.Validate rules for the type +
	// per-type extras + fill_strategy + kinds invariants.
	if err := validateAddGapInlineSpec(a); err != nil {
		return err
	}
	return nil
}

// addGapValidTypes enumerates the gap types add_gap can declare
// inline per #142. Mirrors the config.GapSpec recognised set
// (string/int/enum/canonical_type); bool/text/date are reserved
// for operator-config-only declarations until workflow-side use
// cases surface.
var addGapValidTypes = map[string]struct{}{
	"string":         {},
	"int":            {},
	"enum":           {},
	"canonical_type": {},
}

// addGapValidFillStrategies mirrors config.validFillStrategies
// for the inline spec path. Empty falls through to the engine /
// loader / merge defaults; the explicit values gate against
// operator typos.
var addGapValidFillStrategies = map[string]struct{}{
	"":         {},
	"agent":    {},
	"operator": {},
	"both":     {},
}

func validateAddGapInlineSpec(a *AddGapAction) error {
	// Type — when set, must be one of the recognised four.
	if a.Type != "" {
		if _, ok := addGapValidTypes[a.Type]; !ok {
			return fmt.Errorf("type %q not in {string, int, enum, canonical_type}", a.Type)
		}
	}
	// FillStrategy — when set, must be one of the recognised
	// three. Empty falls through to the engine default of "both".
	if _, ok := addGapValidFillStrategies[a.FillStrategy]; !ok {
		return fmt.Errorf("fill_strategy %q not in {agent, operator, both}", a.FillStrategy)
	}
	// Type-specific cross-field invariants. Mirror
	// config.GapSpec.Validate so the workflow-injected spec
	// can't ship a shape the daemon-side fill pipeline rejects.
	switch a.Type {
	case "canonical_type":
		if len(a.Kinds) == 0 {
			return fmt.Errorf("type=canonical_type requires non-empty kinds list")
		}
	case "int":
		if len(a.Kinds) > 0 {
			return fmt.Errorf("kinds is only valid for type=canonical_type, got type=%q", a.Type)
		}
		if len(a.Range) != 0 && len(a.Range) != 2 {
			return fmt.Errorf("range must be a [min, max] integer pair when set, got %d entries", len(a.Range))
		}
		if len(a.Range) == 2 && a.Range[0] > a.Range[1] {
			return fmt.Errorf("range[min=%d] > range[max=%d]", a.Range[0], a.Range[1])
		}
		if a.MaxLength != 0 {
			return fmt.Errorf("max_length is only valid for type=string, got type=int")
		}
		if len(a.Values) != 0 {
			return fmt.Errorf("values is only valid for type=enum, got type=int")
		}
	case "string":
		if len(a.Kinds) > 0 {
			return fmt.Errorf("kinds is only valid for type=canonical_type, got type=%q", a.Type)
		}
		if a.MaxLength < 0 {
			return fmt.Errorf("max_length must be non-negative, got %d", a.MaxLength)
		}
		if len(a.Range) > 0 {
			return fmt.Errorf("range is only valid for type=int, got type=string")
		}
		if len(a.Values) != 0 {
			return fmt.Errorf("values is only valid for type=enum, got type=string")
		}
	case "enum":
		if len(a.Values) == 0 {
			return fmt.Errorf("type=enum requires non-empty values list")
		}
		if len(a.Kinds) > 0 {
			return fmt.Errorf("kinds is only valid for type=canonical_type, got type=enum")
		}
		if len(a.Range) > 0 {
			return fmt.Errorf("range is only valid for type=int, got type=enum")
		}
		if a.MaxLength != 0 {
			return fmt.Errorf("max_length is only valid for type=string, got type=enum")
		}
	case "":
		// No inline type — kinds / range / max_length / values
		// remain plausible operator-config-driven shapes; the
		// loader's canonical_kinds cross-check enforces
		// agreement at workflow-load time. Per-type extras
		// without a Type are still rejected because they have
		// no spec to bind against.
		if len(a.Kinds) > 0 {
			return fmt.Errorf("kinds requires type=canonical_type; declare type alongside")
		}
		if len(a.Range) > 0 {
			return fmt.Errorf("range requires type=int; declare type alongside")
		}
		if a.MaxLength != 0 {
			return fmt.Errorf("max_length requires type=string; declare type alongside")
		}
		if len(a.Values) != 0 {
			return fmt.Errorf("values requires type=enum; declare type alongside")
		}
	}
	return nil
}

func validateAddCanonicalEdge(a *AddCanonicalEdgeAction) error {
	if a.EdgeType == "" {
		return fmt.Errorf("edge_type is required")
	}
	if a.TargetKind == "" {
		return fmt.Errorf("target.kind is required")
	}
	if a.TargetName == "" {
		return fmt.Errorf("target.name is required")
	}
	for k, v := range a.Data {
		if k == "" {
			return fmt.Errorf("data key is empty (after trim) — field names must be non-empty")
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("data[%q] value is empty — CEL expression must be non-empty", k)
		}
	}
	return nil
}

func validateSetProperty(a *SetPropertyAction) error {
	if len(a.Fields) == 0 {
		return fmt.Errorf("fields is required (at least one field)")
	}
	for name, expr := range a.Fields {
		if name == "" {
			return fmt.Errorf("fields key is empty (after trim) — field names must be non-empty")
		}
		if strings.TrimSpace(expr) == "" {
			return fmt.Errorf("fields[%q] value is empty", name)
		}
	}
	return nil
}
