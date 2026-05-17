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
	if err := validateAllowedPlugins(wf.AllowedPlugins); err != nil {
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
	return nil
}

func validateStatus(s string) error {
	switch s {
	case "", StatusActive, StatusPaused, StatusDraft:
		return nil
	}
	return fmt.Errorf("workflow: status %q is not one of {active, paused, draft}", s)
}

func validateAllowedPlugins(plugins []string) error {
	if len(plugins) == 0 {
		return fmt.Errorf("workflow: allowed_plugins is required and must be non-empty")
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
		if err := rejectTriggerFields(t.Match, "Kind", "Gap", "Source"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeEntityCreated:
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Gap", "Source"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeFillCompleted:
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Kind"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case TriggerTypeManual:
		if err := rejectTriggerFields(t.Match, "EdgeType", "TargetKind", "Kind", "Gap", "Source"); err != nil {
			return fmt.Errorf("workflow: %s: %w", t.Type, err)
		}
	case "":
		return fmt.Errorf("workflow: trigger.type is required")
	default:
		return fmt.Errorf("workflow: trigger.type %q is not one of {edge_created, entity_created, fill_completed, manual}", t.Type)
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
		case "Kind":
			val = m.Kind
		case "Gap":
			val = m.Gap
		case "Source":
			val = m.Source
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
	case "Kind":
		return "kind"
	case "Gap":
		return "gap"
	case "Source":
		return "source"
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
		if set == 0 {
			return fmt.Errorf("workflow: actions[%d] sets no primitive (expected exactly one of task_append / add_note / plugin_dispatch / add_gap)", i)
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
		}
	}
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
	return nil
}
