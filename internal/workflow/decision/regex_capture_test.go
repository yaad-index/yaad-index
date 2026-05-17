// Tests for the cel-go strings extension + the regex_capture custom
// function exposed via the workflow decision env (#123).

package decision

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluator_StringsExt_Split(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)
	prog, err := ev.Compile(`entity.subject.split(" ")[0]`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "hello world from cel"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
}

func TestEvaluator_StringsExt_Replace(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`entity.subject.replace(" ", "-")`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "a b c"},
	})
	require.NoError(t, err)
	assert.Equal(t, "a-b-c", got)
}

func TestEvaluator_StringsExt_LowerAscii(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`entity.name.lowerAscii()`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"name": "MIXED-Case"},
	})
	require.NoError(t, err)
	assert.Equal(t, "mixed-case", got)
}

func TestEvaluator_StringsExt_Substring(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`entity.name.substring(0, 4)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"name": "abcdefgh"},
	})
	require.NoError(t, err)
	assert.Equal(t, "abcd", got)
}

func TestEvaluator_RegexCapture_Group0WholeMatch(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, "[A-Z]+", 0)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "lower MIXED rest"},
	})
	require.NoError(t, err)
	assert.Equal(t, "MIXED", got)
}

func TestEvaluator_RegexCapture_Group1(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, "\\[([^/]+/[^\\]]+)\\]", 1)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "[acme/widget] PR #42 opened"},
	})
	require.NoError(t, err)
	assert.Equal(t, "acme/widget", got)
}

func TestEvaluator_RegexCapture_Group2(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, "(\\w+)-(\\d+)", 2)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "issue-1234 raised"},
	})
	require.NoError(t, err)
	assert.Equal(t, "1234", got)
}

func TestEvaluator_RegexCapture_NoMatchReturnsEmpty(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, "#(\\d+)", 1)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "no numbers here"},
	})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestEvaluator_RegexCapture_OutOfRangeGroupReturnsEmpty(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, "(\\w+)", 5)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "single"},
	})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestEvaluator_RegexCapture_NegativeGroupReturnsEmpty(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, "(\\w+)", -1)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "single"},
	})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestEvaluator_RegexCapture_LiteralBadPatternFailsAtCompile(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	_, err := ev.Compile(`regex_capture(entity.subject, "[unclosed", 0)`, "string")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "regex_capture pattern")
	assert.Contains(t, err.Error(), "[unclosed")
}

func TestEvaluator_RegexCapture_NonLiteralBadPatternReturnsEmptyAtEval(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`regex_capture(entity.subject, entity.pattern, 0)`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{
			"subject": "anything",
			"pattern": "[unclosed",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestEvaluator_RegexCapture_GitHubEmailSubjectExtraction(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	repoProg, err := ev.Compile(
		`regex_capture(entity.subject, "\\[([^/]+/[^\\]]+)\\]", 1)`, "string")
	require.NoError(t, err)
	refProg, err := ev.Compile(
		`regex_capture(entity.subject, "#(\\d+)", 1)`, "string")
	require.NoError(t, err)

	act := Activation{
		Entity: map[string]any{"subject": "[acme/widget] Re: PR #42 review requested"},
	}
	repo, _, err := repoProg.EvalString(context.Background(), act)
	require.NoError(t, err)
	assert.Equal(t, "acme/widget", repo)
	ref, _, err := refProg.EvalString(context.Background(), act)
	require.NoError(t, err)
	assert.Equal(t, "42", ref)
}

func TestEvaluator_RegexCapture_CachesCompiledPattern(t *testing.T) {
	t.Parallel()
	pattern := "(unique-cache-test-\\d+)"
	re1, err := compiledRegex(pattern)
	require.NoError(t, err)
	re2, err := compiledRegex(pattern)
	require.NoError(t, err)
	require.Same(t, re1, re2, "second compile hits the cache")
}

func TestEvaluator_RegexCapture_CombinedWithStringsExt(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(
		`regex_capture(entity.subject.lowerAscii(), "pr #(\\d+)", 1)`,
		"string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"subject": "Notification: PR #99 opened"},
	})
	require.NoError(t, err)
	assert.Equal(t, "99", got)
}

func TestEvaluator_RegexCapture_LiteralBadPatternErrorMentionsExpr(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	_, err := ev.Compile(`regex_capture("text", "(unbalanced", 0)`, "string")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "regex_capture pattern"))
}
