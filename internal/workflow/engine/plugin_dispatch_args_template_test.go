// Regression for #456: plugin_dispatch.args support mustache-only /
// literal-default templating — a value carrying a `{{ … }}` segment is
// rendered against the activation (so a workflow can pass e.g.
// entity.id into a plugin command), while a bare value passes through
// to the dispatcher verbatim (plugin args are literal-heavy). Before
// this, compileActionTemplates deferred plugin_dispatch entirely.

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

// TestCompileActionTemplates_PluginDispatchArgsCompiled pins the
// mustache-only / literal-default rule: a plugin_dispatch.args entry
// carrying a `{{ … }}` segment compiles into the templates map under
// `arg:<name>` (mirroring set_property's `field:<name>`), while a bare
// (no-`{{`) value is NOT compiled — it passes through verbatim.
func TestCompileActionTemplates_PluginDispatchArgsCompiled(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{PluginDispatch: &parser.PluginDispatchAction{
		Plugin:  "yaad-fetch",
		Command: "refetch",
		Args: map[string]any{
			"id":   "{{ entity.id }}",
			"mode": "refetch-only",
		},
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)
	assert.Contains(t, tpls, "arg:id", "a `{{ … }}` arg compiles under arg:<name>")
	assert.NotContains(t, tpls, "arg:mode", "a bare literal arg is NOT compiled — passes through verbatim")
}

// TestCompileActionTemplates_PluginDispatchNonStringArgsSkipped
// pins that non-string arg values (numbers, bools) are not
// templatable and produce no compiled-template entry — they pass
// through to the dispatcher verbatim at runner time.
func TestCompileActionTemplates_PluginDispatchNonStringArgsSkipped(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{PluginDispatch: &parser.PluginDispatchAction{
		Plugin:  "yaad-fetch",
		Command: "refetch",
		Args: map[string]any{
			"count":   int64(3),
			"enabled": true,
			"name":    "{{ entity.id }}",
		},
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)
	assert.NotContains(t, tpls, "arg:count", "numeric arg is not templatable")
	assert.NotContains(t, tpls, "arg:enabled", "bool arg is not templatable")
	assert.Contains(t, tpls, "arg:name")
}

// TestRenderActionTemplates_PluginDispatchArgResolvesActivation
// pins the end-to-end render: a templated arg evaluates against
// the activation and lands as a resolved string in the
// runner-facing map under `arg:<name>`.
func TestRenderActionTemplates_PluginDispatchArgResolvesActivation(t *testing.T) {
	t.Parallel()

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)

	a := parser.Action{PluginDispatch: &parser.PluginDispatchAction{
		Plugin:  "yaad-fetch",
		Command: "refetch",
		Args: map[string]any{
			"id": "{{ entity.id }}",
		},
	}}
	tpls, err := compileActionTemplates(ev, a)
	require.NoError(t, err)

	e := &Engine{}
	reg := &registeredWorkflow{
		actionTemplates: []map[string]*template.Template{tpls},
	}
	act := decision.Activation{
		Entity: map[string]any{"id": "widget:alpha-prime"},
	}
	rendered, err := e.renderActionTemplates(context.Background(), reg, act)
	require.NoError(t, err)
	require.Contains(t, rendered, 0)
	assert.Equal(t, "widget:alpha-prime", rendered[0]["arg:id"],
		"plugin_dispatch arg templated with entity.id resolves to the entity id")
}
