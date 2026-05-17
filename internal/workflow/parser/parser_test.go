package parser

import (
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

// TestValidate_AllowedPluginsRequired: per ADR, every workflow
// declares its plugin scope. Empty list is a workflow-shape
// error.
func TestValidate_AllowedPluginsRequired(t *testing.T) {
	t.Parallel()
	wf := &Workflow{
		Name:    "x",
		Trigger: Trigger{Type: TriggerTypeManual},
		Actions: []Action{{AddNote: &AddNoteAction{Content: "x"}}},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_plugins")
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
			Kind:     "boardgame",
		},
	}
	err := Validate(wf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind=")
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
		Plugin: "yaad-bgg", Command: "fetch",
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
		Plugin: "yaad-bgg", Command: "fetch", TimeoutSeconds: -1,
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
