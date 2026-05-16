// Exercises the mustache template parser + per-segment CEL
// compile + render. Combined parse / compile / render coverage
// because the three stages are tightly coupled; mustache parser
// errors surface from Compile, segment-eval errors surface from
// Render, and the literal-vs-CEL boundary needs to round-trip
// in both shapes.

package template

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/decision"
)

func newTestEvaluator(t *testing.T, bindings []string) *decision.Evaluator {
	t.Helper()
	ev, err := decision.NewEvaluator(decision.Options{Bindings: bindings})
	require.NoError(t, err)
	return ev
}

// TestCompileAndRender_BareCELExpression: a source with no
// `{{` is treated as a single CEL expression returning a
// string. Preserves the operator-authored shape
// `subject: entity.id` (no mustache braces).
func TestCompileAndRender_BareCELExpression(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("entity.id", ev)
	require.NoError(t, err)
	require.Len(t, tpl.segments, 1)
	got, res, err := tpl.Render(context.Background(), decision.Activation{
		Entity: map[string]any{"id": "boardgame:brass-birmingham"},
	})
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got)
	assert.Empty(t, res.MissingRefs)
}

// TestCompileAndRender_BareCELStringLiteral: a bare-CEL source
// that is itself a CEL string literal renders to the literal's
// contents. Use-case: `subject: "'daily'"` for a fixed-string
// subject.
func TestCompileAndRender_BareCELStringLiteral(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile(`"daily"`, ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{})
	require.NoError(t, err)
	assert.Equal(t, "daily", got)
}

// TestCompileAndRender_BareCELInvalid: a bare source that
// isn't valid CEL (e.g. `hello world` — two identifiers
// separated by whitespace) errors at Compile time. The
// operator gets the early signal rather than a runtime
// surprise.
func TestCompileAndRender_BareCELInvalid(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	_, err := Compile("hello world", ev)
	require.Error(t, err)
}

// TestCompileAndRender_SingleExpression: `{{ entity.slug }}` —
// one CEL segment, no surrounding literal.
func TestCompileAndRender_SingleExpression(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("{{ entity.slug }}", ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{
		Entity: map[string]any{"slug": "brass-birmingham"},
	})
	require.NoError(t, err)
	assert.Equal(t, "brass-birmingham", got)
}

// TestCompileAndRender_MixedLiteralAndExpressions: the ADR-0024
// worked example content template — literals + multiple
// expressions with surrounding text.
func TestCompileAndRender_MixedLiteralAndExpressions(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("{{ entity.name }} ({{ entity.year }}) — surfaced via {{ edge.from_title }}", ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{
		Entity: map[string]any{"name": "Brass: Birmingham", "year": int64(2018)},
		Edge:   map[string]any{"from_title": "Boardgame News Weekly"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Brass: Birmingham (2018) — surfaced via Boardgame News Weekly", got)
}

// TestCompileAndRender_AdjacentExpressions: two `{{...}}`
// segments with no separating literal text.
func TestCompileAndRender_AdjacentExpressions(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("{{ entity.kind }}{{ entity.slug }}", ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{
		Entity: map[string]any{"kind": "boardgame:", "slug": "brass-birmingham"},
	})
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got)
}

// TestCompileAndRender_BindingReference: a template that
// references a workflow-declared context binding (a named
// pre-evaluated value).
func TestCompileAndRender_BindingReference(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, []string{"prior"})
	tpl, err := Compile("prior: {{ prior.rating }}", ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{
		Entity:   map[string]any{},
		Bindings: map[string]any{"prior": map[string]any{"rating": int64(8)}},
	})
	require.NoError(t, err)
	assert.Equal(t, "prior: 8", got)
}

// TestCompile_UnmatchedOpenBrace: `{{ entity.slug` without
// closing braces is a parse-time error.
func TestCompile_UnmatchedOpenBrace(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	_, err := Compile("{{ entity.slug", ev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmatched")
}

// TestCompile_StrayCloseBrace: `entity.slug }}` (no opening)
// is a parse-time error.
func TestCompile_StrayCloseBrace(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	_, err := Compile("entity.slug }}", ev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmatched")
}

// TestCompile_EmptyExpression: `{{ }}` (no expression source)
// is rejected at parse time so the operator gets a clean
// "empty expression" error rather than a downstream CEL parse
// failure.
func TestCompile_EmptyExpression(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	_, err := Compile("prefix {{ }} suffix", ev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty expression")
}

// TestCompile_InvalidCELSegment: `{{ entity. }}` is valid
// mustache but invalid CEL — Compile must surface the CEL
// parse error (not silently accept it).
func TestCompile_InvalidCELSegment(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	_, err := Compile("{{ entity. }}", ev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entity.")
}

// TestRender_NonStringExpressionStringified: a CEL expression
// returning a non-string value (e.g. int) is stringified via
// the decision package's EvalString default behavior.
func TestRender_NonStringExpressionStringified(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("year: {{ entity.year }}", ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{
		Entity: map[string]any{"year": int64(2018)},
	})
	require.NoError(t, err)
	assert.Equal(t, "year: 2018", got)
}

// TestRender_MissingRefsDedup_HappyPath: a fake graph lookup
// resolves all ids — MissingRefs should be empty.
func TestRender_MissingRefsDedup_HappyPath(t *testing.T) {
	t.Parallel()
	ev, err := decision.NewEvaluator(decision.Options{
		Lookup: &fakeGraph{entities: map[string]map[string]any{
			"alpha": {"x": "A"}, "beta": {"x": "B"},
		}},
	})
	require.NoError(t, err)
	tpl, err := Compile("a={{ graph.get(\"alpha\").x }} b={{ graph.get(\"beta\").x }} c={{ graph.get(\"alpha\").x }}", ev)
	require.NoError(t, err)
	got, res, err := tpl.Render(context.Background(), decision.Activation{})
	require.NoError(t, err)
	assert.Equal(t, "a=A b=B c=A", got)
	assert.Empty(t, res.MissingRefs)
}

// TestRender_MissingRefsCollected: when graph.get(id) misses,
// the MissingRef is captured in the result, deduplicated by
// id, and id-sorted.
func TestRender_MissingRefsCollected(t *testing.T) {
	t.Parallel()
	// Resolver returns ErrEntityNotFound for some ids; CEL
	// renders null which the template package stringifies as
	// "null".
	ev, err := decision.NewEvaluator(decision.Options{
		Lookup: &fakeGraph{entities: map[string]map[string]any{
			"known": {"name": "Known Entity"},
		}},
	})
	require.NoError(t, err)
	tpl, err := Compile("k={{ graph.get(\"known\").name }} m={{ graph.get(\"missing-a\") }} n={{ graph.get(\"missing-b\") }} m2={{ graph.get(\"missing-a\") }}", ev)
	require.NoError(t, err)
	_, res, err := tpl.Render(context.Background(), decision.Activation{})
	require.NoError(t, err)
	assert.Equal(t, []decision.MissingRef{{ID: "missing-a"}, {ID: "missing-b"}}, res.MissingRefs)
}

// TestCompile_NilEvaluator: a Compile call with ev=nil is
// caller-error; Compile rejects it rather than panicking
// downstream.
func TestCompile_NilEvaluator(t *testing.T) {
	t.Parallel()
	_, err := Compile("{{ entity.slug }}", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil")
}

// TestCompile_EscapedOpenBrace: a CEL string literal segment
// can embed the literal `{{` characters in mustache mode.
// Documents the escape pattern for surface text that contains
// open-braces. (Closing `}}` in surface text is a documented
// v1 limit — operators use bare-CEL mode with a CEL string
// literal for those: see TestCompileAndRender_BareCELStringLiteral.)
func TestCompile_EscapedOpenBrace(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile(`open: {{ "{{" }} done`, ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{})
	require.NoError(t, err)
	assert.Equal(t, "open: {{ done", got)
}

// TestCompile_LeadingTrailingWhitespaceInExpr: `{{   x   }}`
// (with internal whitespace) renders identically to `{{ x }}`.
func TestCompile_LeadingTrailingWhitespaceInExpr(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("{{   entity.slug   }}", ev)
	require.NoError(t, err)
	got, _, err := tpl.Render(context.Background(), decision.Activation{
		Entity: map[string]any{"slug": "brass"},
	})
	require.NoError(t, err)
	assert.Equal(t, "brass", got)
}

// TestCompile_EmptyTemplate: empty source string renders as
// empty string with no segments compiled.
func TestCompile_EmptyTemplate(t *testing.T) {
	t.Parallel()
	ev := newTestEvaluator(t, nil)
	tpl, err := Compile("", ev)
	require.NoError(t, err)
	assert.Empty(t, tpl.segments)
	got, _, err := tpl.Render(context.Background(), decision.Activation{})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// fakeGraph is an in-package GraphLookup for the tests that
// need a populated resolver.
type fakeGraph struct {
	entities map[string]map[string]any
}

func (g *fakeGraph) Get(_ context.Context, id string) (map[string]any, error) {
	if e, ok := g.entities[id]; ok {
		return e, nil
	}
	return nil, decision.ErrEntityNotFound
}
