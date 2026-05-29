// Tests for the #337 Cut 2 bounded task-body primitives.

package actions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freshBody builds a minimal valid task body for the primitive
// tests — populated prompt + empty other sections — so each
// test starts from a parser-clean baseline. Returns the body
// string with no leading frontmatter (primitive helpers handle
// both shapes via splitFrontmatter).
func freshBody(t *testing.T) string {
	t.Helper()
	body, err := RenderTaskSections(TaskSections{Prompt: "initial prompt"})
	require.NoError(t, err)
	return body
}

// TestSetPrompt_Replaces pins the replace-shape contract:
// the second SetPrompt call wins regardless of the first.
func TestSetPrompt_Replaces(t *testing.T) {
	t.Parallel()
	body := freshBody(t)

	first, err := SetPrompt(body, "first prompt")
	require.NoError(t, err)
	second, err := SetPrompt(first, "second prompt")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(second)
	require.NoError(t, err)
	assert.Equal(t, "second prompt", parsed.Prompt)
}

// TestSetPrompt_EmptyRejected pins the mandatory-prompt
// rule: clearing the prompt via SetPrompt is rejected so
// the body stays parseable on the next round-trip.
func TestSetPrompt_EmptyRejected(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	_, err := SetPrompt(body, "   ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mandatory")
}

// TestAddCheckbox_Appends pins the set-shape append: the
// item lands in the todo section as a `- [ ] ...` line.
func TestAddCheckbox_Appends(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	out, err := AddCheckbox(body, "review the diff")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(out)
	require.NoError(t, err)
	assert.Equal(t, "- [ ] review the diff", parsed.Todo)
}

// TestAddCheckbox_IdempotentOnDuplicate pins the set-shape
// commutativity carve-out: two AddCheckbox calls with the
// same item end up with one entry.
func TestAddCheckbox_IdempotentOnDuplicate(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	once, err := AddCheckbox(body, "review the diff")
	require.NoError(t, err)
	twice, err := AddCheckbox(once, "review the diff")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(twice)
	require.NoError(t, err)
	assert.Equal(t, "- [ ] review the diff", parsed.Todo,
		"duplicate AddCheckbox must collapse to one entry")
}

// TestAddCheckbox_MultipleDistinctEntries pins that distinct
// items accumulate in the todo section.
func TestAddCheckbox_MultipleDistinctEntries(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	body, err := AddCheckbox(body, "first")
	require.NoError(t, err)
	body, err = AddCheckbox(body, "second")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(body)
	require.NoError(t, err)
	assert.Equal(t, "- [ ] first\n- [ ] second", parsed.Todo)
}

// TestAddNote_Appends pins the set-shape append for the
// notes section.
func TestAddNote_Appends(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	out, err := AddNote(body, "- 2026-05-29T17:00:00Z: failed")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(out)
	require.NoError(t, err)
	assert.Equal(t, "- 2026-05-29T17:00:00Z: failed", parsed.Notes)
}

// TestAddNote_IdempotentOnDuplicate pins the set-shape
// commutativity carve-out — duplicate notes collapse.
// Real err-task entries differ by timestamp so the carve-
// out triggers in practice only on actual replays.
func TestAddNote_IdempotentOnDuplicate(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	once, err := AddNote(body, "the same line")
	require.NoError(t, err)
	twice, err := AddNote(once, "the same line")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(twice)
	require.NoError(t, err)
	assert.Equal(t, "the same line", parsed.Notes)
}

// TestAddNote_PreservesOrderAcrossDistinctEntries pins that
// distinct notes accumulate in insertion order. Order
// doesn't carry semantic weight under the set-shape carve-
// out — the parser produces the same set regardless — but
// the renderer happens to preserve it deterministically.
func TestAddNote_PreservesOrderAcrossDistinctEntries(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	body, err := AddNote(body, "first")
	require.NoError(t, err)
	body, err = AddNote(body, "second")
	require.NoError(t, err)
	body, err = AddNote(body, "third")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(body)
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond\nthird", parsed.Notes)
}

// TestAppendFreeform_OrderMatters pins the non-commutative
// ordered-append contract: two calls with the same text
// produce two paragraphs, and earlier calls land above
// later ones.
func TestAppendFreeform_OrderMatters(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	body, err := AppendFreeform(body, "first paragraph")
	require.NoError(t, err)
	body, err = AppendFreeform(body, "second paragraph")
	require.NoError(t, err)
	body, err = AppendFreeform(body, "first paragraph")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(body)
	require.NoError(t, err)
	assert.Equal(t,
		"first paragraph\n\nsecond paragraph\n\nfirst paragraph",
		parsed.Freeform,
		"ordered-append: duplicates are NOT collapsed; the third call lands at the end")
}

// TestAppendFreeform_MultilineInput pins that the helper
// preserves embedded newlines in the input — operator
// prose can include line breaks.
func TestAppendFreeform_MultilineInput(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	body, err := AppendFreeform(body, "line one\nline two")
	require.NoError(t, err)

	parsed, err := ParseTaskSections(body)
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two", parsed.Freeform)
}

// TestAddEdge_StubReturnsBodyUnchanged pins the Cut 2 stub
// contract: AddEdge accepts the call signature so callers
// can wire against the surface, but doesn't mutate the
// body. Cut 3 will land the real implementation once the
// graph-write vs section-regen atomicity contract settles.
func TestAddEdge_StubReturnsBodyUnchanged(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	out, err := AddEdge(body, "boardgame:brass", "is_about")
	require.NoError(t, err)
	assert.Equal(t, body, out, "AddEdge v1 stub returns body unchanged pending Cut 3 atomicity decision")
}

// TestAddEdge_RejectsEmptyEntityID pins the input
// validation: caller must supply a target id.
func TestAddEdge_RejectsEmptyEntityID(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	_, err := AddEdge(body, "", "is_about")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entityID")
}

// TestPrimitives_RoundTripStability pins that any
// combination of primitive calls round-trips through
// parse → render byte-stably. This is what err-task and
// resolution-task writers depend on: repeated section
// mutations don't drift the on-disk body.
func TestPrimitives_RoundTripStability(t *testing.T) {
	t.Parallel()
	body := freshBody(t)
	body, err := AddCheckbox(body, "check the diff")
	require.NoError(t, err)
	body, err = AddNote(body, "- 2026-05-29T17:00Z: noted")
	require.NoError(t, err)
	body, err = AppendFreeform(body, "operator commentary")
	require.NoError(t, err)
	body, err = SetPrompt(body, "updated instruction")
	require.NoError(t, err)

	// Round-trip through parse + render.
	parsed, err := ParseTaskSections(body)
	require.NoError(t, err)
	rendered, err := RenderTaskSections(parsed)
	require.NoError(t, err)
	assert.Equal(t, body, rendered, "round-trip byte stability")
}

// TestPrimitives_WorkAgainstFrontmatterBody pins that the
// helpers handle bodies with a yaml frontmatter block —
// the resolution-task and err-task writers emit one and
// the primitives must route around it.
func TestPrimitives_WorkAgainstFrontmatterBody(t *testing.T) {
	t.Parallel()
	frontmatter := "---\nkind: task\nfoo: bar\n---\n\n"
	body, err := RenderTaskSections(TaskSections{Prompt: "p"})
	require.NoError(t, err)
	full := frontmatter + body

	out, err := AddNote(full, "test note")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, frontmatter),
		"frontmatter preserved through primitive call")

	_, sectionsBody, err := splitFrontmatter(out)
	require.NoError(t, err)
	parsed, err := ParseTaskSections(sectionsBody)
	require.NoError(t, err)
	assert.Equal(t, "test note", parsed.Notes)
}
