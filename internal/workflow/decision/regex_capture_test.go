// Tests for the cel-go strings extension + the regex_capture custom
// function exposed via the workflow decision env (#123).

package decision

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestEvaluator_StringCast_Timestamp pins the #160 contract: the
// cel-go standard library's `string(timestamp)` overload formats
// a CEL Timestamp value as RFC3339 / ISO 8601, and the result
// concatenates with `+` against other strings. Operators authoring
// `task_append` / `add_note` content templates that want to embed
// `edge.timestamp` wrap it with `string(...)` — the implicit
// `string + time` overload doesn't exist in CEL by design, but
// the explicit cast is always available.
//
// No daemon code change is needed; this test exists so a future
// cel-go bump or env-construction change that breaks the
// contract fails loudly.
func TestEvaluator_StringCast_Timestamp(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)
	ts := time.Date(2026, 5, 17, 19, 0, 0, 0, time.UTC)

	t.Run("explicit cast renders RFC3339", func(t *testing.T) {
		prog, err := ev.Compile(`string(edge.timestamp)`, "string")
		require.NoError(t, err)
		got, _, err := prog.EvalString(context.Background(), Activation{
			Edge: map[string]any{"timestamp": ts},
		})
		require.NoError(t, err)
		assert.Equal(t, "2026-05-17T19:00:00Z", got,
			"string(timestamp) must format as RFC3339 (ISO 8601)")
	})

	t.Run("concat into a task_append-shaped template", func(t *testing.T) {
		prog, err := ev.Compile(
			`"- edge " + edge.type + " landed at " + string(edge.timestamp)`,
			"string")
		require.NoError(t, err)
		got, _, err := prog.EvalString(context.Background(), Activation{
			Edge: map[string]any{
				"type":      "hiring_alert_for",
				"timestamp": ts,
			},
		})
		require.NoError(t, err)
		assert.Equal(t,
			"- edge hiring_alert_for landed at 2026-05-17T19:00:00Z",
			got)
	})

	t.Run("date-only via substring composes with ext.Strings()", func(t *testing.T) {
		// docs/workflows.md examples lean on this composition for
		// the "I just want the date" case — proves the recipe
		// without adding a format_time function.
		prog, err := ev.Compile(`string(edge.timestamp).substring(0, 10)`, "string")
		require.NoError(t, err)
		got, _, err := prog.EvalString(context.Background(), Activation{
			Edge: map[string]any{"timestamp": ts},
		})
		require.NoError(t, err)
		assert.Equal(t, "2026-05-17", got)
	})
}
