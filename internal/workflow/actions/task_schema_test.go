// Tests for the #337 Cut 1 5-section task body schema.

package actions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderTaskSections_AllMarkersAlwaysEmitted pins the
// core schema contract: even when only the mandatory prompt
// section is populated, all five marker pairs render in the
// fixed order. This is what Cut 2's bounded primitives count
// on — they parse the body to find a section, mutate it, and
// re-render; missing markers would break that round-trip.
func TestRenderTaskSections_AllMarkersAlwaysEmitted(t *testing.T) {
	t.Parallel()
	body, err := RenderTaskSections(TaskSections{
		Prompt: "find me a job",
	})
	require.NoError(t, err)

	for _, section := range []string{
		TaskSectionPrompt, TaskSectionEdges, TaskSectionTodo,
		TaskSectionFreeform, TaskSectionNotes,
	} {
		assert.Contains(t, body, taskMarkerOpen(section), "open marker for %q", section)
		assert.Contains(t, body, taskMarkerClose(section), "close marker for %q", section)
	}
}

// TestRenderTaskSections_FixedOrder pins that the renderer
// always emits sections in the same sequence regardless of
// content presence. Set-shape sections (edges, todo, notes)
// rely on positional anchoring at parse time.
func TestRenderTaskSections_FixedOrder(t *testing.T) {
	t.Parallel()
	body, err := RenderTaskSections(TaskSections{
		Prompt:   "p",
		Edges:    "e",
		Todo:     "t",
		Freeform: "f",
		Notes:    "n",
	})
	require.NoError(t, err)

	promptIdx := strings.Index(body, taskMarkerOpen(TaskSectionPrompt))
	edgesIdx := strings.Index(body, taskMarkerOpen(TaskSectionEdges))
	todoIdx := strings.Index(body, taskMarkerOpen(TaskSectionTodo))
	freeformIdx := strings.Index(body, taskMarkerOpen(TaskSectionFreeform))
	notesIdx := strings.Index(body, taskMarkerOpen(TaskSectionNotes))

	assert.GreaterOrEqual(t, promptIdx, 0)
	assert.Less(t, promptIdx, edgesIdx)
	assert.Less(t, edgesIdx, todoIdx)
	assert.Less(t, todoIdx, freeformIdx)
	assert.Less(t, freeformIdx, notesIdx)
}

// TestRenderTaskSections_EmptyPromptRejected pins the
// mandatory-prompt rule: render must error when the prompt
// section is empty since the agent has nothing to act on
// per #337.
func TestRenderTaskSections_EmptyPromptRejected(t *testing.T) {
	t.Parallel()
	_, err := RenderTaskSections(TaskSections{Prompt: "   "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt")
	assert.Contains(t, err.Error(), "mandatory")
}

// TestParseRenderRoundTrip pins byte-stability under the
// parse → render cycle Cut 2's bounded primitives will run on
// every section-mutating call. A non-stable round-trip would
// degrade the on-disk body across writes even when no content
// changed.
func TestParseRenderRoundTrip(t *testing.T) {
	t.Parallel()
	original := TaskSections{
		Prompt:   "find me a job: salary > X",
		Edges:    "- email:m1\n- person:alice",
		Todo:     "- [ ] check job ad\n- [x] update resume",
		Freeform: "Notes from the operator on the search.",
		Notes:    "- 2026-05-29T15:00:00Z (alice): initial filing",
	}
	body, err := RenderTaskSections(original)
	require.NoError(t, err)

	parsed, err := ParseTaskSections(body)
	require.NoError(t, err)
	assert.Equal(t, original, parsed)

	// Second round-trip stays identical.
	body2, err := RenderTaskSections(parsed)
	require.NoError(t, err)
	assert.Equal(t, body, body2)
}

// TestParseTaskSections_SkipsFrontmatter pins the frontmatter-
// tolerant parsing the resolution-task / err-task writers
// depend on: they emit a yaml frontmatter block ahead of the
// section markers; the parser must skip past it.
func TestParseTaskSections_SkipsFrontmatter(t *testing.T) {
	t.Parallel()
	sections, err := RenderTaskSections(TaskSections{Prompt: "p"})
	require.NoError(t, err)
	withFrontmatter := "---\nkind: task\nfoo: bar\n---\n\n" + sections

	parsed, err := ParseTaskSections(withFrontmatter)
	require.NoError(t, err)
	assert.Equal(t, "p", parsed.Prompt)
}

// TestParseTaskSections_MissingMarkerErrors pins the schema as
// a hard contract: a body missing any marker pair errors out
// so Cut 2's bounded primitives don't silently overwrite a
// malformed file.
func TestParseTaskSections_MissingMarkerErrors(t *testing.T) {
	t.Parallel()
	// Body with prompt + edges + todo + freeform but no notes
	// close marker — simulates a corrupt write.
	mangled := "<!-- yaad-index prompt -->\np\n<!-- /yaad-index prompt -->\n" +
		"<!-- yaad-index edges -->\n<!-- /yaad-index edges -->\n" +
		"<!-- yaad-index todo -->\n<!-- /yaad-index todo -->\n" +
		"<!-- yaad-index freeform -->\n<!-- /yaad-index freeform -->\n" +
		"<!-- yaad-index notes -->\nstuff\n" // no close
	_, err := ParseTaskSections(mangled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notes")
}

// TestSplitFrontmatter_PreservesBody pins the err_task's
// append helper depends on splitting frontmatter from sections
// cleanly so re-render concatenates back to a byte-stable
// body.
func TestSplitFrontmatter_PreservesBody(t *testing.T) {
	t.Parallel()
	frontmatter := "---\nkind: task\n---\n\n"
	sections, err := RenderTaskSections(TaskSections{Prompt: "p"})
	require.NoError(t, err)
	full := frontmatter + sections

	gotFM, gotBody, err := splitFrontmatter(full)
	require.NoError(t, err)
	assert.Equal(t, frontmatter, gotFM)
	assert.Equal(t, sections, gotBody)
}

// TestSplitFrontmatter_NoFrontmatter pins the no-frontmatter
// case — the helper returns ("", body) so callers can always
// concat fm + body.
func TestSplitFrontmatter_NoFrontmatter(t *testing.T) {
	t.Parallel()
	body := "<!-- yaad-index prompt -->\np\n<!-- /yaad-index prompt -->\n"
	gotFM, gotBody, err := splitFrontmatter(body)
	require.NoError(t, err)
	assert.Empty(t, gotFM)
	assert.Equal(t, body, gotBody)
}
