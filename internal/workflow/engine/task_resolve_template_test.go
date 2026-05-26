// Regression test for the PR-293 review catch: task_resolve.subject
// + task_resolve.match_key are documented as CEL templates but were
// missing from compileActionTemplates → d.rendered() fell back to
// raw `{{ ... }}` source. Both fields are now compiled + rendered
// through the engine's templating pipeline; this test pins that
// contract at the engine layer.

package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
	"github.com/yaad-index/yaad-index/internal/workflow/template"
)

// TestCompileActionTemplates_TaskResolveCompilesTemplatedFields
// pins that compileActionTemplates emits compiled templates for
// both subject + match_key when the action's CEL source is non-
// empty. Without these compiles, the runner's d.rendered() helper
// would fall back to the raw template source — every templated
// subject would target the wrong file + every templated match_key
// would never match.
func TestCompileActionTemplates_TaskResolveCompilesTemplatedFields(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{TaskResolve: &parser.TaskResolveAction{
		Workflow: "owning-wf",
		Subject:  "{{ entity.id }}",
		Section:  "pending",
		MatchKey: "{{ entity.data.key }}",
		Mode:     parser.TaskResolveModeCheck,
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)
	assert.Contains(t, tpls, "subject", "task_resolve.subject MUST be in the compiled-templates map; otherwise the runner falls back to raw source per PR-293 review catch")
	assert.Contains(t, tpls, "match_key", "task_resolve.match_key MUST be in the compiled-templates map; otherwise the runner falls back to raw source")
}

// TestCompileActionTemplates_TaskResolveStaticFieldsSkipped pins
// the symmetric: the static workflow/section/mode fields are NOT
// in the compiled-templates map — they pass through verbatim from
// the parser. Workflow/section/mode entries in the map would
// suggest the runner expects them rendered, which it doesn't.
func TestCompileActionTemplates_TaskResolveStaticFieldsSkipped(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{TaskResolve: &parser.TaskResolveAction{
		Workflow: "static-name",
		Subject:  `"still-rendered"`,
		Section:  "pending",
		MatchKey: `"still-rendered-too"`,
		Mode:     parser.TaskResolveModeRemove,
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)
	assert.NotContains(t, tpls, "workflow")
	assert.NotContains(t, tpls, "section")
	assert.NotContains(t, tpls, "mode")
}

// TestCompileActionTemplates_TaskResolveEmptySubjectSkipsCompile
// pins the empty-field shape: when subject or match_key is empty
// (validator should have rejected, but defensively), the entry is
// omitted from the templates map rather than crashing the compile.
func TestCompileActionTemplates_TaskResolveEmptySubjectSkipsCompile(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{TaskResolve: &parser.TaskResolveAction{
		Workflow: "owning",
		Subject:  "",
		Section:  "p",
		MatchKey: `"k"`,
		Mode:     parser.TaskResolveModeCheck,
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)
	assert.NotContains(t, tpls, "subject", "empty subject leaves the entry unset")
	assert.Contains(t, tpls, "match_key")
}

// TestRenderActionTemplates_TaskResolveSubjectAndMatchKey pins
// the end-to-end render: a templated subject + match_key both
// evaluate against an Activation and land as rendered strings in
// the runner-facing map. Without the compileActionTemplates fix,
// the rendered map would be empty + the runner would re-emit the
// raw template source as the literal subject / match_key.
func TestRenderActionTemplates_TaskResolveSubjectAndMatchKey(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{TaskResolve: &parser.TaskResolveAction{
		Workflow: "owning-wf",
		Subject:  "{{ entity.data.subject_key }}",
		Section:  "pending",
		MatchKey: "{{ entity.data.match_key }}",
		Mode:     parser.TaskResolveModeCheck,
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)

	e := &Engine{}
	reg := &registeredWorkflow{
		actionTemplates: []map[string]*template.Template{tpls},
	}
	act := decision.Activation{
		Entity: map[string]any{
			"id": "github:example",
			"data": map[string]any{
				"subject_key": "to-refetch",
				"match_key":   "acme/repo#42",
			},
		},
	}
	rendered, err := e.renderActionTemplates(context.Background(), reg, act)
	require.NoError(t, err)
	require.Contains(t, rendered, 0)
	assert.Equal(t, "to-refetch", rendered[0]["subject"])
	assert.Equal(t, "acme/repo#42", rendered[0]["match_key"])
}
