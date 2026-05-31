package parser

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validWorkflowMarkdown is the ADR-0024 worked-example
// boardgame-news file verbatim. The parser must accept it +
// preserve every field semantically.
const validWorkflowMarkdown = "---\n" +
	"name: boardgame-news\n" +
	"version: 1\n" +
	"status: active\n" +
	"---\n" +
	"\n" +
	"# Boardgame news → review queue\n" +
	"\n" +
	"Surfaces news articles about boardgames I own or care about.\n" +
	"\n" +
	"```yaml\n" +
	"allowed_plugins:\n" +
	"  - yaad-gmail\n" +
	"\n" +
	"trigger:\n" +
	"  type: edge_created\n" +
	"  match:\n" +
	"    edge_type: is_about\n" +
	"    target_kind: boardgame\n" +
	"\n" +
	"subject: '{{ entity.slug }}'\n" +
	"\n" +
	"context:\n" +
	"  - name: prior\n" +
	"    via: 'has(entity.previous_edition_id) ? graph.get(entity.previous_edition_id) : null'\n" +
	"\n" +
	"condition: 'entity.rating > 7 || (prior != null && prior.rating > 7) || entity.owned == true'\n" +
	"\n" +
	"dedup:\n" +
	"  key: 'workflow + entity.id'\n" +
	"  policy: update\n" +
	"\n" +
	"actions:\n" +
	"  - task_append:\n" +
	"      section: candidates\n" +
	"      content: '{{ entity.name }} ({{ entity.year }}) — surfaced via {{ edge.from_title }}'\n" +
	"      if_already_present: skip\n" +
	"```\n"

// TestParse_HappyPath_ADRExample verifies that the ADR-0024
// worked-example parses cleanly and every field round-trips
// (frontmatter metadata + YAML-body engine rules + the single
// action primitive). This is the load-bearing test: if it
// breaks, the parser doesn't match the documented schema.
func TestParse_HappyPath_ADRExample(t *testing.T) {
	t.Parallel()
	wf, err := Parse([]byte(validWorkflowMarkdown))
	require.NoError(t, err, "parse should succeed on the ADR worked example")

	// Frontmatter
	assert.Equal(t, "boardgame-news", wf.Name)
	assert.Equal(t, 1, wf.Version)
	assert.Equal(t, "active", wf.Status)

	// Body — allowed_plugins
	assert.Equal(t, []string{"yaad-gmail"}, wf.AllowedPlugins)

	// Trigger
	assert.Equal(t, TriggerTypeEdgeCreated, wf.Trigger.Type)
	assert.Equal(t, "is_about", wf.Trigger.Match.EdgeType)
	assert.Equal(t, "boardgame", wf.Trigger.Match.TargetKind)
	assert.Empty(t, wf.Trigger.Match.Gap, "edge_created trigger has no gap filter")

	// Subject (CEL template) preserved verbatim
	assert.Equal(t, "{{ entity.slug }}", wf.Subject)

	// Context binding round-trip
	require.Len(t, wf.Context, 1)
	assert.Equal(t, "prior", wf.Context[0].Name)
	assert.Equal(t,
		"has(entity.previous_edition_id) ? graph.get(entity.previous_edition_id) : null",
		wf.Context[0].Via)

	// Condition preserved verbatim
	assert.Equal(t,
		"entity.rating > 7 || (prior != null && prior.rating > 7) || entity.owned == true",
		wf.Condition)

	// Dedup
	assert.Equal(t, "workflow + entity.id", wf.Dedup.Key)
	assert.Equal(t, DedupPolicyUpdate, wf.Dedup.Policy)

	// Action — task_append populated, others nil
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].TaskAppend)
	assert.Nil(t, wf.Actions[0].AddNote)
	assert.Nil(t, wf.Actions[0].PluginDispatch)
	assert.Nil(t, wf.Actions[0].AddGap)
	assert.Equal(t, "candidates", wf.Actions[0].TaskAppend.Section)
	assert.Equal(t,
		"{{ entity.name }} ({{ entity.year }}) — surfaced via {{ edge.from_title }}",
		wf.Actions[0].TaskAppend.Content)
	assert.Equal(t, IfAlreadyPresentSkip, wf.Actions[0].TaskAppend.IfAlreadyPresent)
}

// TestParse_DefaultsApplied: version + status default when the
// operator omits them; auto_archive_on_done stays nil so the
// engine can distinguish "operator wanted the default" from
// "operator set false explicitly". Subject defaults to
// `entity.id` per ADR-0024 §"Workflow" (symmetric with the
// other defaults; PR-77 fold-in).
func TestParse_DefaultsApplied(t *testing.T) {
	t.Parallel()
	md := "---\nname: minimal\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\ntrigger:\n  type: manual\nactions:\n  - add_note:\n      content: 'hi'\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	assert.Equal(t, "minimal", wf.Name)
	assert.Equal(t, 1, wf.Version, "version defaults to 1 when omitted")
	assert.Equal(t, StatusActive, wf.Status, "status defaults to active when omitted")
	assert.Equal(t, "entity.id", wf.Subject, "subject defaults to entity.id when omitted")
	assert.Nil(t, wf.AutoArchiveOnDone, "auto_archive_on_done stays nil (engine treats as default-true)")
}

// TestParse_TaskAppendIfAlreadyPresentDefault: task_append's
// if_already_present defaults to "skip" per ADR-0024 §"Action-
// level match semantics". Applied at parse time for symmetry
// with version=1 / status=active / subject=entity.id defaults.
// PR-77 fold-in.
func TestParse_TaskAppendIfAlreadyPresentDefault(t *testing.T) {
	t.Parallel()
	md := "---\nname: ta-default\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\ntrigger:\n  type: manual\nactions:\n  - task_append:\n      section: s\n      content: c\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].TaskAppend)
	assert.Equal(t, IfAlreadyPresentSkip, wf.Actions[0].TaskAppend.IfAlreadyPresent,
		"if_already_present defaults to skip when operator omits it")
}

// TestParse_TaskResolveHappyPath pins #266 parse shape: a
// task_resolve action with all required fields parses + lands
// on Action.TaskResolve.
func TestParse_TaskResolveHappyPath(t *testing.T) {
	t.Parallel()
	md := "---\nname: cross-resolve\n---\n\n```yaml\nallowed_plugins: []\ntrigger:\n  type: manual\nactions:\n  - task_resolve:\n      workflow: gmail-github-mentions\n      subject: to-refetch\n      section: pending-refetch\n      match_key: 'acme/repo#42'\n      mode: check\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].TaskResolve)
	tr := wf.Actions[0].TaskResolve
	assert.Equal(t, "gmail-github-mentions", tr.Workflow)
	assert.Equal(t, "to-refetch", tr.Subject)
	assert.Equal(t, "pending-refetch", tr.Section)
	assert.Equal(t, "acme/repo#42", tr.MatchKey)
	assert.Equal(t, TaskResolveModeCheck, tr.Mode)
}

// TestValidate_TaskResolveRejectsUnknownMode pins #266
// validation shape: a non-{check,remove} mode rejects at
// workflow load.
func TestValidate_TaskResolveRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	md := "---\nname: bad-mode\n---\n\n```yaml\nallowed_plugins: []\ntrigger:\n  type: manual\nactions:\n  - task_resolve:\n      workflow: x\n      subject: y\n      section: s\n      match_key: k\n      mode: archive\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mode")
}

// TestValidate_TaskResolveRequiresAllFields pins the
// required-field set: missing workflow / subject / section /
// match_key / mode each reject.
func TestValidate_TaskResolveRequiresAllFields(t *testing.T) {
	t.Parallel()
	missingWorkflow := "---\nname: x\n---\n\n```yaml\nallowed_plugins: []\ntrigger: {type: manual}\nactions:\n  - task_resolve:\n      subject: s\n      section: sec\n      match_key: k\n      mode: check\n```\n"
	_, err := Parse([]byte(missingWorkflow))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflow")

	missingMode := "---\nname: x\n---\n\n```yaml\nallowed_plugins: []\ntrigger: {type: manual}\nactions:\n  - task_resolve:\n      workflow: w\n      subject: s\n      section: sec\n      match_key: k\n```\n"
	_, err = Parse([]byte(missingMode))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mode")
}

// TestParse_TaskAppendIfAlreadyPresentExplicit: when the
// operator sets a non-empty value, the parser preserves it
// verbatim (no overwrite to default).
func TestParse_TaskAppendIfAlreadyPresentExplicit(t *testing.T) {
	t.Parallel()
	md := "---\nname: ta-explicit\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\ntrigger:\n  type: manual\nactions:\n  - task_append:\n      section: s\n      content: c\n      if_already_present: append-anyway\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	assert.Equal(t, IfAlreadyPresentAppendAnyway, wf.Actions[0].TaskAppend.IfAlreadyPresent,
		"explicit value preserved over default")
}

// TestParse_SubjectExplicit: explicit subject preserved over
// the entity.id default.
func TestParse_SubjectExplicit(t *testing.T) {
	t.Parallel()
	md := "---\nname: subj\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\ntrigger:\n  type: manual\nsubject: '{{ entity.slug }}'\nactions:\n  - add_note: {content: hi}\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	assert.Equal(t, "{{ entity.slug }}", wf.Subject, "explicit subject preserved over default")
}

// TestParse_NoFrontmatter rejects a workflow file that doesn't
// open with `---\n`.
func TestParse_NoFrontmatter(t *testing.T) {
	t.Parallel()
	md := "# no frontmatter\n```yaml\nname: x\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFrontmatterMissing)
}

// TestParse_NoYAMLBody rejects a workflow file with frontmatter
// but no engine-rules code-fence — a documentation stub, not a
// workflow.
func TestParse_NoYAMLBody(t *testing.T) {
	t.Parallel()
	md := "---\nname: docs-only\n---\n\n# just prose, no yaml fence\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrYAMLBodyMissing)
}

// TestParse_DuplicateYAMLBody rejects a file with more than one
// yaml code-fence in the body. Single canonical block is
// required so merge semantics don't have to exist.
func TestParse_DuplicateYAMLBody(t *testing.T) {
	t.Parallel()
	md := "---\nname: dup\n---\n\n```yaml\nallowed_plugins: [a]\n```\n\n```yaml\nallowed_plugins: [b]\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrYAMLBodyDuplicated)
}

// TestParse_UnknownFrontmatterField fails strict-mode decode so
// operator typos surface at parse time. (yaml.v3 with
// KnownFields(true).)
func TestParse_UnknownFrontmatterField(t *testing.T) {
	t.Parallel()
	md := "---\nname: x\nverison: 1\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - add_note: {content: hi}\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verison",
		"unknown field should appear in the error message")
}

// TestParse_UnknownBodyField: same strict-mode behavior on the
// YAML body half.
func TestParse_UnknownBodyField(t *testing.T) {
	t.Parallel()
	md := "---\nname: x\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - add_note: {content: hi}\nbogus_field: yes\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus_field")
}

// TestValidate_NameRequired pins the load-bearing required
// field. The loader uses Name for dedup; missing it breaks
// registry semantics.
func TestValidate_NameRequired(t *testing.T) {
	t.Parallel()
	wf := &Workflow{
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        Trigger{Type: TriggerTypeManual},
		Actions:        []Action{{AddNote: &AddNoteAction{Content: "x"}}},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

// TestValidate_AllowedPluginsRequired_WhenPluginDispatch: a
// workflow whose actions include plugin_dispatch MUST declare
// allowed_plugins. The validator enforces the gate so a
// misconfigured dispatch can't reach a plugin outside the
// workflow's declared scope.
func TestValidate_AllowedPluginsRequired_WhenPluginDispatch(t *testing.T) {
	t.Parallel()
	wf := &Workflow{
		Name:    "x",
		Trigger: Trigger{Type: TriggerTypeManual},
		Actions: []Action{{PluginDispatch: &PluginDispatchAction{Plugin: "yaad-gmail", Command: "fetch"}}},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_plugins")
	assert.Contains(t, err.Error(), "plugin_dispatch")
}

// TestValidate_AllowedPluginsOptional_WhenNoPluginDispatch: per
// ADR-0027 cut 4 fold, workflows without plugin_dispatch (manual
// digests, add_note / set_property / add_canonical_edge / task_append
// only) MAY omit allowed_plugins — they have no plugin surface to
// declare.
func TestValidate_AllowedPluginsOptional_WhenNoPluginDispatch(t *testing.T) {
	t.Parallel()
	wf := &Workflow{
		Name:    "x",
		Trigger: Trigger{Type: TriggerTypeManual},
		Actions: []Action{{AddNote: &AddNoteAction{Content: "x"}}},
	}
	err := Validate(wf)
	require.NoError(t, err, "workflow with no plugin_dispatch may omit allowed_plugins")
}

// TestValidate_AllowedPluginsDuplicated catches the same plugin
// listed twice (operator typo / merge artifact).
func TestValidate_AllowedPluginsDuplicated(t *testing.T) {
	t.Parallel()
	wf := &Workflow{
		Name:           "x",
		AllowedPlugins: []string{"yaad-gmail", "yaad-gmail"},
		Trigger:        Trigger{Type: TriggerTypeManual},
		Actions:        []Action{{AddNote: &AddNoteAction{Content: "x"}}},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicated")
}

// TestValidate_TriggerTypeRequired
func TestValidate_TriggerTypeRequired(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger.Type = ""
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trigger.type is required")
}

// TestValidate_TriggerTypeUnknown
func TestValidate_TriggerTypeUnknown(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger.Type = "scheduled" // post-v1
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not one of")
}

// TestValidate_EdgeCreatedRequiresEdgeType
func TestValidate_EdgeCreatedRequiresEdgeType(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger = Trigger{Type: TriggerTypeEdgeCreated}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "edge_type is required")
}

// TestValidate_EdgeCreatedRejectsKindFilter: edge_created uses
// target_kind, not kind (kind is the entity_created filter).
// Operators who put the wrong filter on the wrong trigger get
// a clear error.
func TestValidate_EdgeCreatedRejectsKindFilter(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger = Trigger{
		Type: TriggerTypeEdgeCreated,
		Match: TriggerMatch{
			EdgeType: "is_about",
			Kinds:    []string{"boardgame"},
		},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical_kind=")
	assert.Contains(t, err.Error(), "not valid for this trigger.type")
}

// TestValidate_FillCompletedAcceptsGap
func TestValidate_FillCompletedAcceptsGap(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger = Trigger{
		Type:  TriggerTypeFillCompleted,
		Match: TriggerMatch{Gap: "is_interesting_to_me", Source: "operator"},
	}
	require.NoError(t, Validate(wf))
}

// TestValidate_EntityUpdatedRequiresFieldChanged: the
// entity_updated trigger added in ADR-0024's 2026-05-21
// amendment requires match.field_changed naming the dotted
// data path the workflow filters on.
func TestValidate_EntityUpdatedRequiresFieldChanged(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger = Trigger{Type: TriggerTypeEntityUpdated}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field_changed is required")
}

// TestValidate_EntityUpdatedAcceptsKindFilter: kind narrows
// the trigger to a specific canonical entity kind (e.g.
// github-pr). field_changed alone is sufficient; kind is
// optional but must NOT be rejected when present.
func TestValidate_EntityUpdatedAcceptsKindFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kinds []string
	}{
		{"single-kind", []string{"github-pr"}},
		{"multi-kind", []string{"github-pr", "github-issue"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := minimalWorkflow()
			wf.Trigger = Trigger{
				Type: TriggerTypeEntityUpdated,
				Match: TriggerMatch{
					FieldChanged: "data.state",
					Kinds:        tc.kinds,
				},
			}
			require.NoError(t, Validate(wf))
		})
	}
}

// TestParse_CanonicalKind_RoundTripsBothShapes: the
// `canonical_kind:` YAML key accepts both a single scalar
// (auto-wrapped to a one-element list) and an explicit list
// per the ADR-0024 + ADR-0026 §6 worked examples.
func TestParse_CanonicalKind_RoundTripsBothShapes(t *testing.T) {
	t.Parallel()
	const single = "---\nname: gh-archive-single\n---\n\n" +
		"```yaml\n" +
		"allowed_plugins: [yaad-gmail]\n" +
		"trigger:\n" +
		"  type: entity_updated\n" +
		"  match:\n" +
		"    field_changed: data.state\n" +
		"    canonical_kind: github-pr\n" +
		"actions:\n" +
		"  - archive_entity: {}\n" +
		"```\n"
	const list = "---\nname: gh-archive-list\n---\n\n" +
		"```yaml\n" +
		"allowed_plugins: [yaad-gmail]\n" +
		"trigger:\n" +
		"  type: entity_updated\n" +
		"  match:\n" +
		"    field_changed: data.state\n" +
		"    canonical_kind: [github-pr, github-issue]\n" +
		"actions:\n" +
		"  - archive_entity: {}\n" +
		"```\n"

	wfSingle, err := Parse([]byte(single))
	require.NoError(t, err)
	assert.Equal(t, []string{"github-pr"}, wfSingle.Trigger.Match.Kinds,
		"scalar `canonical_kind: github-pr` round-trips to a single-element list")

	wfList, err := Parse([]byte(list))
	require.NoError(t, err)
	assert.Equal(t, []string{"github-pr", "github-issue"}, wfList.Trigger.Match.Kinds,
		"list `canonical_kind: [a, b]` round-trips verbatim")
}

// TestValidate_EntityUpdatedRejectsForeignFields: edge_type,
// target_kind, gap, source are all foreign to entity_updated.
func TestValidate_EntityUpdatedRejectsForeignFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*TriggerMatch)
		errFrag string
	}{
		{"edge_type", func(m *TriggerMatch) { m.EdgeType = "x" }, "edge_type="},
		{"target_kind", func(m *TriggerMatch) { m.TargetKind = "x" }, "target_kind="},
		{"gap", func(m *TriggerMatch) { m.Gap = "x" }, "gap="},
		{"source", func(m *TriggerMatch) { m.Source = "x" }, "source="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := minimalWorkflow()
			wf.Trigger = Trigger{
				Type:  TriggerTypeEntityUpdated,
				Match: TriggerMatch{FieldChanged: "data.state"},
			}
			tc.mutate(&wf.Trigger.Match)
			err := Validate(wf)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errFrag)
		})
	}
}

// TestValidate_FieldChangedRejectedOnOtherTriggers: the
// field_changed match field is exclusive to entity_updated.
func TestValidate_FieldChangedRejectedOnOtherTriggers(t *testing.T) {
	t.Parallel()
	cases := []string{
		TriggerTypeEdgeCreated,
		TriggerTypeEntityCreated,
		TriggerTypeFillCompleted,
		TriggerTypeManual,
	}
	for _, ttype := range cases {
		t.Run(ttype, func(t *testing.T) {
			wf := minimalWorkflow()
			wf.Trigger = Trigger{
				Type:  ttype,
				Match: TriggerMatch{FieldChanged: "data.state"},
			}
			// edge_created also needs edge_type, otherwise it
			// errors before field_changed is checked.
			if ttype == TriggerTypeEdgeCreated {
				wf.Trigger.Match.EdgeType = "is_about"
			}
			err := Validate(wf)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "field_changed=")
		})
	}
}

// TestValidate_RestoreEntity_Permissive: restore_entity is
// the mirror of archive_entity — both Entity and Reason are
// optional CEL strings, so `- restore_entity: {}` is a valid
// shape (defaults to acting on the triggering entity).
func TestValidate_RestoreEntity_Permissive(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{RestoreEntity: &RestoreEntityAction{}}}
	require.NoError(t, Validate(wf))
}

// TestValidate_ManualRejectsAnyMatch: manual trigger has no
// event match shape.
func TestValidate_ManualRejectsAnyMatch(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Trigger = Trigger{
		Type:  TriggerTypeManual,
		Match: TriggerMatch{EdgeType: "x"},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid for this trigger.type")
}

// TestValidate_ContextDuplicateName
func TestValidate_ContextDuplicateName(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Context = []ContextBinding{
		{Name: "prior", Via: "graph.get(entity.x)"},
		{Name: "prior", Via: "graph.get(entity.y)"},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicated")
}

// TestValidate_DedupPolicyUnknown
func TestValidate_DedupPolicyUnknown(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Dedup = Dedup{Policy: "merge"}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dedup.policy")
}

// TestValidate_ActionsRequired
func TestValidate_ActionsRequired(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = nil
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions is required")
}

// TestValidate_ActionMultiplePrimitives rejects a list entry
// that sets more than one action shape.
func TestValidate_ActionMultiplePrimitives(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{
		TaskAppend: &TaskAppendAction{Section: "x", Content: "y"},
		AddNote: &AddNoteAction{Content: "z"},
	}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sets 2 primitives")
}

// TestValidate_ActionNoPrimitive: empty action entry.
func TestValidate_ActionNoPrimitive(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sets no primitive")
}

// TestValidate_TaskAppendRequiresSection
func TestValidate_TaskAppendRequiresSection(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{TaskAppend: &TaskAppendAction{Content: "x"}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "section is required")
}

// TestValidate_TaskAppendIfAlreadyPresentUnknown
func TestValidate_TaskAppendIfAlreadyPresentUnknown(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{TaskAppend: &TaskAppendAction{
		Section: "s", Content: "c", IfAlreadyPresent: "merge",
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "if_already_present")
}

// TestValidate_PluginDispatchUnknownPlugin: plugin not in
// allowed_plugins is rejected.
func TestValidate_PluginDispatchUnknownPlugin(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AllowedPlugins = []string{"yaad-gmail"}
	wf.Actions = []Action{{PluginDispatch: &PluginDispatchAction{
		Plugin:  "yaad-bgg", Command: "fetch",
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the workflow's allowed_plugins")
}

// TestValidate_PluginDispatchNegativeTimeout
func TestValidate_PluginDispatchNegativeTimeout(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AllowedPlugins = []string{"yaad-bgg"}
	wf.Actions = []Action{{PluginDispatch: &PluginDispatchAction{
		Plugin:  "yaad-bgg", Command: "fetch", TimeoutSeconds: -1,
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")
}

// TestValidate_AddGapMustBeInAddableGaps pins the load-bearing
// add_gap constraint per ADR-0024 §"Constraints on add_gap":
// the gap name MUST appear in the workflow's addable_gaps
// vocabulary. Single source of truth.
func TestValidate_AddGapMustBeInAddableGaps(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"is_interesting_to_me"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap: "owned_status", // NOT in addable_gaps
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "addable_gaps vocabulary")

	// Positive path: gap that IS in the vocabulary validates.
	wf.Actions = []Action{{AddGap: &AddGapAction{Gap: "is_interesting_to_me"}}}
	require.NoError(t, Validate(wf))
}

// TestParser_AddGapWithDataSchema: the workflow YAML `add_gap`
// action carries optional `data_schema` (#117) which round-trips
// onto AddGapAction.DataSchema with keys trimmed.
func TestParser_AddGapWithDataSchema(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: data-schema-flow\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"addable_gaps:\n" +
		"  - hiring_alert_for\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - add_gap:\n" +
		"      entity: 'entity.id'\n" +
		"      gap: hiring_alert_for\n" +
		"      data_schema:\n" +
		"        role: \"the role title in the hiring alert\"\n" +
		"        salary: \"salary range if mentioned, else omit\"\n" +
		"        work_mode: \"remote / hybrid / onsite if mentioned, else omit\"\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].AddGap)
	got := wf.Actions[0].AddGap
	assert.Equal(t, "entity.id", got.Entity)
	assert.Equal(t, "hiring_alert_for", got.Gap)
	require.Len(t, got.DataSchema, 3)
	assert.Equal(t, "the role title in the hiring alert", got.DataSchema["role"])
	assert.Equal(t, "salary range if mentioned, else omit", got.DataSchema["salary"])
	assert.Equal(t, "remote / hybrid / onsite if mentioned, else omit", got.DataSchema["work_mode"])
}

// TestParser_AddGapWithoutDataSchema: the absence of
// `data_schema` is fine — AddGapAction.DataSchema is nil and
// the action validates and runs unchanged from pre-#117.
func TestParser_AddGapWithoutDataSchema(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: no-schema-flow\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"addable_gaps:\n" +
		"  - is_interesting_to_me\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - add_gap:\n" +
		"      gap: is_interesting_to_me\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, wf.Actions[0].AddGap)
	assert.Nil(t, wf.Actions[0].AddGap.DataSchema)
}

// TestValidate_AddGapDataSchemaEmptyValueRejected: a workflow
// that declares data_schema but supplies an empty extraction
// instruction is an author bug — the agent gets useless guidance.
func TestValidate_AddGapDataSchemaEmptyValueRejected(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:        "g",
		DataSchema: map[string]string{"role": ""},
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data_schema")
	assert.Contains(t, err.Error(), "role")
}

// TestValidate_AddGapDataSchemaEmptyKeyRejected: empty key in
// the schema map (trimmed) is also rejected.
func TestValidate_AddGapDataSchemaEmptyKeyRejected(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:        "g",
		DataSchema: map[string]string{"": "some instruction"},
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data_schema")
}

// TestParser_AddCanonicalEdgeRoundTrip pins the wire shape of the
// #132 add_canonical_edge action: literal edge_type + target.kind,
// CEL-expression target.name + data values.
func TestParser_AddCanonicalEdgeRoundTrip(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: github-notification-classify\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - add_canonical_edge:\n" +
		"      source: 'entity.id'\n" +
		"      edge_type: 'is_about'\n" +
		"      target:\n" +
		"        kind: 'github-repository'\n" +
		"        name: 'regex_capture(entity.data.subject, \"\\\\[([^/]+/[^\\\\]]+)\\\\]\", 1)'\n" +
		"      data:\n" +
		"        reference: 'regex_capture(entity.data.subject, \"#(\\\\d+)\", 1)'\n" +
		"        type: 'entity.data.subject.contains(\"review\") ? \"review\" : \"notification\"'\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].AddCanonicalEdge)
	a := wf.Actions[0].AddCanonicalEdge
	assert.Equal(t, "entity.id", a.Source)
	assert.Equal(t, "is_about", a.EdgeType)
	assert.Equal(t, "github-repository", a.TargetKind)
	assert.Equal(t, `regex_capture(entity.data.subject, "\\[([^/]+/[^\\]]+)\\]", 1)`, a.TargetName)
	require.Len(t, a.Data, 2)
	assert.Equal(t, `regex_capture(entity.data.subject, "#(\\d+)", 1)`, a.Data["reference"])
	assert.Equal(t, `entity.data.subject.contains("review") ? "review" : "notification"`, a.Data["type"])
}

// TestParser_AddCanonicalEdgeNoData: the data map is optional.
// Edge-only fires (no dataview-paragraph append) parse cleanly.
func TestParser_AddCanonicalEdgeNoData(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: edge-only\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - add_canonical_edge:\n" +
		"      edge_type: 'is_a'\n" +
		"      target:\n" +
		"        kind: 'source-type'\n" +
		"        name: 'github-notification'\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, wf.Actions[0].AddCanonicalEdge)
	a := wf.Actions[0].AddCanonicalEdge
	assert.Empty(t, a.Source, "source defaults at runtime to entity.id")
	assert.Equal(t, "is_a", a.EdgeType)
	assert.Equal(t, "source-type", a.TargetKind)
	assert.Equal(t, "github-notification", a.TargetName)
	assert.Nil(t, a.Data)
}

// TestValidate_AddCanonicalEdgeRequiredFields covers the
// non-empty edge_type / target.kind / target.name rules.
func TestValidate_AddCanonicalEdgeRequiredFields(t *testing.T) {
	t.Parallel()

	mk := func(action *AddCanonicalEdgeAction) *Workflow {
		wf := minimalWorkflow()
		wf.Actions = []Action{{AddCanonicalEdge: action}}
		return wf
	}

	// Empty edge_type rejected.
	err := Validate(mk(&AddCanonicalEdgeAction{
		TargetKind: "person",
		TargetName: "Uwe",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "edge_type is required")

	// Empty target.kind rejected.
	err = Validate(mk(&AddCanonicalEdgeAction{
		EdgeType:   "is_about",
		TargetName: "Uwe",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target.kind is required")

	// Empty target.name rejected.
	err = Validate(mk(&AddCanonicalEdgeAction{
		EdgeType:   "is_about",
		TargetKind: "person",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target.name is required")

	// Positive path validates.
	err = Validate(mk(&AddCanonicalEdgeAction{
		EdgeType:   "is_about",
		TargetKind: "person",
		TargetName: "Uwe",
	}))
	assert.NoError(t, err)
}

// TestValidate_AddCanonicalEdgeDataKeysAndValues mirrors the
// #117 add_gap.data_schema validation: non-empty keys + non-empty
// CEL expressions only.
func TestValidate_AddCanonicalEdgeDataKeysAndValues(t *testing.T) {
	t.Parallel()
	mk := func(data map[string]string) *Workflow {
		wf := minimalWorkflow()
		wf.Actions = []Action{{AddCanonicalEdge: &AddCanonicalEdgeAction{
			EdgeType: "is_about", TargetKind: "person", TargetName: "Uwe",
			Data: data,
		}}}
		return wf
	}

	err := Validate(mk(map[string]string{"role": ""}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data")

	err = Validate(mk(map[string]string{"": "expr"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data")

	err = Validate(mk(map[string]string{"role": "entity.data.role"}))
	assert.NoError(t, err)
}

// TestValidate_AddableGapsDuplicate
func TestValidate_AddableGapsDuplicate(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"is_interesting_to_me", "is_interesting_to_me"}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicated")
}

// TestValidate_StatusUnknown
func TestValidate_StatusUnknown(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Status = "archived"
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status")
}

// TestParseFile_RoundTrip writes the worked-example to disk
// and parses through the file entry-point.
func TestParseFile_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "boardgame-news.md")
	require.NoError(t, writeFile(path, validWorkflowMarkdown))
	wf, err := ParseFile(path)
	require.NoError(t, err)
	assert.Equal(t, "boardgame-news", wf.Name)
}

// TestParseFile_PathInError ensures the file path appears in
// the error so the loader can produce structured logs without
// re-wrapping.
func TestParseFile_PathInError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.md")
	require.NoError(t, writeFile(path, "no frontmatter here\n"))
	_, err := ParseFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
}

// TestParseFile_ReadError wraps a non-existent path's error so
// the loader can branch on the underlying type.
func TestParseFile_ReadError(t *testing.T) {
	t.Parallel()
	_, err := ParseFile("/does/not/exist/workflow.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does/not/exist")
}

// minimalWorkflow constructs a Workflow that passes Validate
// — used as the baseline for negative tests that mutate one
// field at a time.
func minimalWorkflow() *Workflow {
	return &Workflow{
		Name:           "test",
		Version:        1,
		Status:         StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger:        Trigger{Type: TriggerTypeManual},
		Actions:        []Action{{AddNote: &AddNoteAction{Content: "x"}}},
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestParse_CRLF_Frontmatter tolerates Windows-style CRLF line
// endings in the frontmatter envelope — common when operators
// edit on Windows / Notion / etc. and paste into the vault.
func TestParse_CRLF_Frontmatter(t *testing.T) {
	t.Parallel()
	md := strings.ReplaceAll(validWorkflowMarkdown, "\n", "\r\n")
	wf, err := Parse([]byte(md))
	require.NoError(t, err, "CRLF should parse same as LF")
	assert.Equal(t, "boardgame-news", wf.Name)
}

// TestParse_SetProperty_HappyPath: a workflow with a
// set_property action parses cleanly; fields map preserves
// keys + values verbatim.
func TestParse_SetProperty_HappyPath(t *testing.T) {
	t.Parallel()
	md := "---\nname: classify\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - set_property:\n      entity: 'entity.id'\n      fields:\n        repo: \"'example-org/my-project'\"\n        type: \"'note'\"\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	sp := wf.Actions[0].SetProperty
	require.NotNil(t, sp, "set_property action should be populated")
	assert.Equal(t, "entity.id", sp.Entity)
	assert.Equal(t, "'example-org/my-project'", sp.Fields["repo"])
	assert.Equal(t, "'note'", sp.Fields["type"])
}

// TestParse_SetProperty_DefaultEntity: omitting `entity:`
// leaves it empty — the runner defaults to dec.EntityID at
// execution time (mirrors add_note's default-target shape).
func TestParse_SetProperty_DefaultEntity(t *testing.T) {
	t.Parallel()
	md := "---\nname: sp-default\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - set_property:\n      fields:\n        x: \"'y'\"\n```\n"
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].SetProperty)
	assert.Empty(t, wf.Actions[0].SetProperty.Entity,
		"empty entity flows through; runner applies the default at exec time")
}

// TestValidate_SetProperty_EmptyFieldsRejected: parser
// rejects a set_property with no fields.
func TestValidate_SetProperty_EmptyFieldsRejected(t *testing.T) {
	t.Parallel()
	md := "---\nname: sp-empty\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - set_property:\n      entity: 'entity.id'\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fields is required")
}

// TestValidate_SetProperty_EmptyValueRejected: a field with
// an empty value (whitespace-only or "") is rejected — the
// runner has nothing to evaluate.
func TestValidate_SetProperty_EmptyValueRejected(t *testing.T) {
	t.Parallel()
	md := "---\nname: sp-empty-val\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - set_property:\n      fields:\n        x: ''\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestValidate_SetPropertyAndAddNote_MutuallyExclusive: a
// single Action with both set_property and another primitive
// trips the multi-primitive guard.
func TestValidate_SetPropertyAndAddNote_MutuallyExclusive(t *testing.T) {
	t.Parallel()
	md := "---\nname: x\n---\n\n```yaml\nallowed_plugins: [a]\ntrigger: {type: manual}\nactions:\n  - set_property:\n      fields: {x: \"'y'\"}\n    add_note: {content: hi}\n```\n"
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected exactly one")
}

// TestParser_AddGapInlineSpecRoundTrip pins the #142 wire shape:
// add_gap carries type / kinds / fill_strategy / description /
// range / max_length / values inline alongside the existing
// gap + data_schema fields.
func TestParser_AddGapInlineSpecRoundTrip(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: linkedin-hiring-classify\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"addable_gaps:\n" +
		"  - hiring_alert_for\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - add_gap:\n" +
		"      entity: 'edge.from'\n" +
		"      gap: hiring_alert_for\n" +
		"      type: canonical_type\n" +
		"      kinds: [company]\n" +
		"      fill_strategy: agent\n" +
		"      description: \"The company that's hiring.\"\n" +
		"      data_schema:\n" +
		"        role: \"the role title\"\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, wf.Actions[0].AddGap)
	a := wf.Actions[0].AddGap
	assert.Equal(t, "edge.from", a.Entity)
	assert.Equal(t, "hiring_alert_for", a.Gap)
	assert.Equal(t, "canonical_type", a.Type)
	assert.Equal(t, []string{"company"}, a.Kinds)
	assert.Equal(t, "agent", a.FillStrategy)
	assert.Equal(t, "The company that's hiring.", a.Description)
	assert.Equal(t, "the role title", a.DataSchema["role"])
}

// TestValidate_AddGapInlineCanonicalTypeRequiresKinds: a
// canonical_type inline declaration without `kinds` rejects
// per the cross-field rule.
func TestValidate_AddGapInlineCanonicalTypeRequiresKinds(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:  "g",
		Type: "canonical_type",
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical_type requires non-empty kinds")
}

// TestValidate_AddGapInlineKindsRejectsNonCanonicalType: kinds
// with a non-canonical_type type rejects.
func TestValidate_AddGapInlineKindsRejectsNonCanonicalType(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:   "g",
		Type:  "string",
		Kinds: []string{"person"},
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kinds is only valid for type=canonical_type")
}

// TestValidate_AddGapInlineEnumRequiresValues: enum without
// values rejects.
func TestValidate_AddGapInlineEnumRequiresValues(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:  "g",
		Type: "enum",
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enum requires non-empty values")
}

// TestValidate_AddGapInlineRangeRejectsNonInt: range with a
// non-int type rejects.
func TestValidate_AddGapInlineRangeRejectsNonInt(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:   "g",
		Type:  "string",
		Range: []int{1, 10},
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "range is only valid for type=int")
}

// TestValidate_AddGapInlineTypeOnlyOnePerExtras: kinds /
// range / max_length / values without a Type rejects.
func TestValidate_AddGapInlineTypeOnlyOnePerExtras(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:   "g",
		Kinds: []string{"company"},
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kinds requires type=canonical_type")
}

// TestValidate_AddGapInlineFillStrategy: invalid fill_strategy
// rejects.
func TestValidate_AddGapInlineFillStrategy(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.AddableGaps = []string{"g"}
	wf.Actions = []Action{{AddGap: &AddGapAction{
		Gap:          "g",
		FillStrategy: "everyone",
	}}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fill_strategy")
}

// TestParser_ArchiveEntityRoundTrip pins the #150 archive_entity
// wire shape: both entity (CEL) and reason (CEL) are optional;
// the action with no fields archives the triggering entity.
func TestParser_ArchiveEntityRoundTrip(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: linkedin-post-classify\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - archive_entity:\n" +
		"      entity: 'entity.id'\n" +
		"      reason: 'classified-into-canonical-edge'\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].ArchiveEntity)
	a := wf.Actions[0].ArchiveEntity
	assert.Equal(t, "entity.id", a.Entity)
	assert.Equal(t, "classified-into-canonical-edge", a.Reason)
}

// TestParser_ArchiveEntityDefaults: both entity + reason omitted
// parses cleanly. Runner-side, entity will default to
// dec.EntityID at fire time.
func TestParser_ArchiveEntityDefaults(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: archive-no-fields\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - archive_entity: {}\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, wf.Actions[0].ArchiveEntity)
	a := wf.Actions[0].ArchiveEntity
	assert.Empty(t, a.Entity, "entity defaults at runtime to entity.id")
	assert.Empty(t, a.Reason, "reason is purely optional audit metadata")
}

// TestParser_ArchiveEntityReasonOnly: reason without entity is
// a legitimate shape — the workflow archives the triggering
// entity with an explicit audit string.
func TestParser_ArchiveEntityReasonOnly(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: archive-reason-only\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - archive_entity:\n" +
		"      reason: '\"workflow-cleanup-\" + entity.id'\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, wf.Actions[0].ArchiveEntity)
	a := wf.Actions[0].ArchiveEntity
	assert.Empty(t, a.Entity)
	assert.Equal(t, `"workflow-cleanup-" + entity.id`, a.Reason)
}

// TestValidate_ArchiveEntityAcceptsEmpty pins that an
// `archive_entity: {}` shape passes Validate — both fields are
// optional, no required-field check applies.
func TestValidate_ArchiveEntityAcceptsEmpty(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{ArchiveEntity: &ArchiveEntityAction{}}}
	require.NoError(t, Validate(wf))
}

// TestValidate_ArchiveEntityRejectsMultiplePrimitives: setting
// archive_entity alongside another primitive on the same Action
// trips the exactly-one-primitive guard.
func TestValidate_ArchiveEntityRejectsMultiplePrimitives(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{
		ArchiveEntity: &ArchiveEntityAction{},
		AddNote:       &AddNoteAction{Content: "x"},
	}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sets 2 primitives")
}

// TestParser_ClaimEntityRoundTrip pins the #169 wire shape:
// the action is a bare `- claim_entity: {}` invocation; no
// fields v1.
func TestParser_ClaimEntityRoundTrip(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: linkedin-classify\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: manual\n" +
		"\n" +
		"actions:\n" +
		"  - claim_entity: {}\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Len(t, wf.Actions, 1)
	require.NotNil(t, wf.Actions[0].ClaimEntity)
}

// TestParser_CatchAllRoundTrip pins the catch_all frontmatter
// field per #169. Defaults to false when omitted; explicit
// true round-trips.
func TestParser_CatchAllRoundTrip(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"name: review-orphans\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: entity_created\n" +
		"\n" +
		"catch_all: true\n" +
		"\n" +
		"actions:\n" +
		"  - claim_entity: {}\n" +
		"```\n"
	wf, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.True(t, wf.CatchAll, "catch_all: true round-trips")
}

// TestParser_CatchAllDefaultFalse: workflows that omit
// catch_all parse as regular (CatchAll == false).
func TestParser_CatchAllDefaultFalse(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	assert.False(t, wf.CatchAll,
		"omitted catch_all defaults to false (regular workflow)")
}

// TestValidate_CatchAllForbidsCondition: a catch_all workflow
// with a `condition` is rejected — per #169 the only allowed
// scoping is trigger.kind; per-event filtering would collapse
// the catch-all into a regular workflow.
func TestValidate_CatchAllForbidsCondition(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.CatchAll = true
	wf.Condition = "entity.kind == \"gmail\""
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "catch_all=true forbids `condition`")
}

// TestValidate_CatchAllNoConditionPasses: a catch_all without
// a condition is the canonical shape.
func TestValidate_CatchAllNoConditionPasses(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.CatchAll = true
	wf.Condition = ""
	require.NoError(t, Validate(wf))
}

// TestValidate_ClaimEntityRejectsMultiplePrimitives: claim_entity
// alongside another primitive on the same Action trips the
// exactly-one-primitive guard.
func TestValidate_ClaimEntityRejectsMultiplePrimitives(t *testing.T) {
	t.Parallel()
	wf := minimalWorkflow()
	wf.Actions = []Action{{
		ClaimEntity: &ClaimEntityAction{},
		AddNote:     &AddNoteAction{Content: "x"},
	}}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sets 2 primitives")
}

// TestParse_ContentHashStampedDeterministic pins #280: Parse
// stamps a stable SHA-256 of the input bytes on every Workflow
// it produces. Identical input → identical hash; modified
// input → different hash. The engine's Reconcile no-op gate
// reads this field to skip re-registering unchanged workflows
// across the loader's 15s poll cycles.
func TestParse_ContentHashStampedDeterministic(t *testing.T) {
	t.Parallel()
	body := []byte(`---
name: alpha
version: 1
status: active
---

` + "```yaml\nallowed_plugins: [yaad-gmail]\ntrigger:\n  type: manual\nsubject: 'entity.id'\nactions:\n  - add_note:\n      content: \"'x'\"\n```")

	wf1, err := Parse(body)
	require.NoError(t, err)
	require.NotEmpty(t, wf1.ContentHash, "ContentHash must be stamped on Parse")
	assert.Len(t, wf1.ContentHash, 64, "SHA-256 hex digest is 64 chars")

	wf2, err := Parse(body)
	require.NoError(t, err)
	assert.Equal(t, wf1.ContentHash, wf2.ContentHash,
		"identical input bytes produce identical ContentHash (deterministic)")

	// Modify one byte — flip the version from 1 to 2.
	modified := bytes.Replace(body, []byte("version: 1"), []byte("version: 2"), 1)
	wf3, err := Parse(modified)
	require.NoError(t, err)
	assert.NotEqual(t, wf1.ContentHash, wf3.ContentHash,
		"different input bytes produce different ContentHash")
}

// archiveWhenWorkflowMarkdown threads archive_when through the full
// ADR-0024 worked-example shape so Parse exercises the complete
// frontmatter → body → validate pipeline including the new
// predicate fields.
func archiveWhenWorkflowMarkdown(predicate string) string {
	return "---\n" +
		"name: gmail-catch-all\n" +
		"version: 1\n" +
		"status: active\n" +
		"---\n" +
		"\n" +
		"# Gmail catch-all archive\n" +
		"\n" +
		"```yaml\n" +
		"allowed_plugins:\n" +
		"  - yaad-gmail\n" +
		"\n" +
		"trigger:\n" +
		"  type: entity_created\n" +
		"  match:\n" +
		"    canonical_kind: gmail\n" +
		"\n" +
		"actions:\n" +
		"  - add_note:\n" +
		"      content: 'observed'\n" +
		"\n" +
		predicate +
		"```\n"
}

// TestParse_ArchiveWhen_AllGapsResolved pins the most-common shape
// per ADR-0030 §2: `archive_when: { all_gaps_resolved: true }`.
// Validates that the parser carries the predicate onto the
// Workflow value with the correct primitive populated.
func TestParse_ArchiveWhen_AllGapsResolved(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown("archive_when:\n  all_gaps_resolved: true\n")
	wf, err := Parse([]byte(md))
	require.NoError(t, err, "parse should accept archive_when: all_gaps_resolved")
	require.NotNil(t, wf.ArchiveWhen, "archive_when must carry through to the Workflow value")
	assert.True(t, wf.ArchiveWhen.AllGapsResolved)
	assert.Empty(t, wf.ArchiveWhen.HasEdges)
	assert.Empty(t, wf.ArchiveWhen.FieldEquals)
	assert.Empty(t, wf.ArchiveWhen.AnyOf)
	assert.Empty(t, wf.ArchiveWhen.AllOf)
}

// TestParse_ArchiveWhen_AllPrimitives pins that sibling primitives
// AND together implicitly per ADR-0030 §2 — declaring multiple at
// the top level decodes onto the value verbatim.
func TestParse_ArchiveWhen_AllPrimitives(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown(
		"archive_when:\n" +
			"  all_gaps_resolved: true\n" +
			"  has_edges:\n" +
			"    - is_about\n" +
			"    - is_actionable_for\n" +
			"  field_equals:\n" +
			"    is_actionable: 'no'\n" +
			"    state: closed\n",
	)
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	require.NotNil(t, wf.ArchiveWhen)
	assert.True(t, wf.ArchiveWhen.AllGapsResolved)
	assert.Equal(t, []string{"is_about", "is_actionable_for"}, wf.ArchiveWhen.HasEdges)
	assert.Equal(t, map[string]any{"is_actionable": "no", "state": "closed"}, wf.ArchiveWhen.FieldEquals)
}

// TestParse_ArchiveWhen_NestedAnyOfAllOf pins that the recursive
// composition shape decodes verbatim, including the inner branches.
func TestParse_ArchiveWhen_NestedAnyOfAllOf(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown(
		"archive_when:\n" +
			"  any_of:\n" +
			"    - all_of:\n" +
			"        - all_gaps_resolved: true\n" +
			"        - field_equals:\n" +
			"            category: auto\n" +
			"    - field_equals:\n" +
			"        state: closed\n",
	)
	wf, err := Parse([]byte(md))
	require.NoError(t, err)
	require.NotNil(t, wf.ArchiveWhen)
	require.Len(t, wf.ArchiveWhen.AnyOf, 2)
	require.Len(t, wf.ArchiveWhen.AnyOf[0].AllOf, 2)
	assert.True(t, wf.ArchiveWhen.AnyOf[0].AllOf[0].AllGapsResolved)
	assert.Equal(t, map[string]any{"category": "auto"}, wf.ArchiveWhen.AnyOf[0].AllOf[1].FieldEquals)
	assert.Equal(t, map[string]any{"state": "closed"}, wf.ArchiveWhen.AnyOf[1].FieldEquals)
}

// TestParse_ArchiveWhen_Omitted pins that workflows without
// archive_when keep working — Validate accepts the workflow and
// ArchiveWhen on the result is nil (the opt-out signal the engine
// reads to skip evaluation).
func TestParse_ArchiveWhen_Omitted(t *testing.T) {
	t.Parallel()
	wf, err := Parse([]byte(validWorkflowMarkdown))
	require.NoError(t, err)
	assert.Nil(t, wf.ArchiveWhen, "archive_when omitted → nil (workflow opted out)")
}

// TestValidate_ArchiveWhen_EmptyPredicateRejected pins the
// no-primitive-populated guard per ADR-0030 §2. An archive_when
// block with no fields set has no semantic meaning; the parser
// must reject it so the operator gets a fail-fast file-shape error
// rather than a silently-no-op workflow.
func TestValidate_ArchiveWhen_EmptyPredicateRejected(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown("archive_when: {}\n")
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archive_when")
	assert.Contains(t, err.Error(), "empty predicate")
}

// TestValidate_ArchiveWhen_EmptyAnyOfRejected pins that a non-nil
// any_of must contain at least one branch.
func TestValidate_ArchiveWhen_EmptyAnyOfRejected(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown("archive_when:\n  any_of: []\n")
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archive_when")
	// any_of branch must populate; root-level empty triggers the
	// no-primitive guard, not the empty-any_of one — match on the
	// generic predicate error which covers both.
}

// TestValidate_ArchiveWhen_EmptyAllOfRejected pins that a non-nil
// all_of must contain at least one branch.
func TestValidate_ArchiveWhen_EmptyAllOfRejected(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown("archive_when:\n  all_of: []\n")
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archive_when")
}

// TestValidate_ArchiveWhen_NestedEmptyPredicateRejected pins that
// a deeply-nested empty predicate also rejects + the error path
// names the branch (so the operator can find it in their YAML).
func TestValidate_ArchiveWhen_NestedEmptyPredicateRejected(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown(
		"archive_when:\n" +
			"  any_of:\n" +
			"    - all_gaps_resolved: true\n" +
			"    - {}\n",
	)
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "any_of[1]",
		"error path must name the nested empty branch so operator can find it")
}

// TestValidate_ArchiveWhen_EmptyAnyOfWithSiblingRejected pins the
// #376 Cut 2 review finding: when archive_when has a populated sibling
// primitive AND a present-empty `any_of: []`, the present-empty
// branch must still reject. The wire-shape's nil-vs-empty slice
// distinction now threads faithfully into the public type so the
// validator's `aw.AnyOf != nil && len(aw.AnyOf) == 0` reject fires
// correctly.
func TestValidate_ArchiveWhen_EmptyAnyOfWithSiblingRejected(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown(
		"archive_when:\n" +
			"  all_gaps_resolved: true\n" +
			"  any_of: []\n",
	)
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archive_when.any_of",
		"present-empty any_of with populated sibling primitive must reject + name the bad branch")
}

// TestValidate_ArchiveWhen_EmptyAllOfWithSiblingRejected is the
// AllOf counterpart of the above #376 Cut 2 review finding.
func TestValidate_ArchiveWhen_EmptyAllOfWithSiblingRejected(t *testing.T) {
	t.Parallel()
	md := archiveWhenWorkflowMarkdown(
		"archive_when:\n" +
			"  all_gaps_resolved: true\n" +
			"  all_of: []\n",
	)
	_, err := Parse([]byte(md))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archive_when.all_of",
		"present-empty all_of with populated sibling primitive must reject + name the bad branch")
}
