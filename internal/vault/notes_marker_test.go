package vault

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_CommentsWrappedInMarkers pins the post-#8 write
// contract: writing an entity with notes wraps the
// `## Notes` section in NotesStartMarker / NotesEndMarker.
// New entities + re-writes of marker-wrapped entities both produce
// the wrapped shape.
func TestMarshal_CommentsWrappedInMarkers(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID:     "wikipedia:foo",
		Kind:   "wikipedia-article",
		Plugin: "wikipedia",
		Notes: []Note{
			{
				Date:   mustParseTime(t, "2026-05-13T00:00:00Z"),
				Text:   "First note.",
				Author: "alice",
			},
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	assert.Contains(t, out, NotesStartMarker,
		"marshalled body must include the notes start marker")
	assert.Contains(t, out, NotesEndMarker,
		"marshalled body must include the notes end marker")

	startIdx := strings.Index(out, NotesStartMarker)
	endIdx := strings.Index(out, NotesEndMarker)
	headIdx := strings.Index(out, "## Notes")
	require.Greater(t, headIdx, startIdx,
		"`## Notes` heading must appear AFTER the start marker")
	require.Greater(t, endIdx, headIdx,
		"end marker must appear AFTER the `## Notes` content")
}

// TestUnmarshal_MarkerWrappedRoundTrip pins the read path: a
// marker-wrapped body round-trips through Unmarshal → Marshal with
// notes preserved + still marker-wrapped on output.
func TestUnmarshal_MarkerWrappedRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID:     "wikipedia:foo",
		Kind:   "wikipedia-article",
		Plugin: "wikipedia",
		Notes: []Note{
			{
				Date:   mustParseTime(t, "2026-05-13T00:00:00Z"),
				Text:   "Round-trip note.",
				Author: "alice",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)

	parsed, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, parsed.Notes, 1)
	assert.Equal(t, "Round-trip note.", parsed.Notes[0].Text)
	assert.Equal(t, "alice", parsed.Notes[0].Author)

	// Re-marshal — must still be marker-wrapped (not double-wrapped,
	// not unwrapped).
	again, err := Marshal(parsed, nil)
	require.NoError(t, err)
	out := string(again)
	startCount := strings.Count(out, NotesStartMarker)
	endCount := strings.Count(out, NotesEndMarker)
	assert.Equal(t, 1, startCount,
		"re-marshal must produce exactly one start marker (no double-wrap)")
	assert.Equal(t, 1, endCount,
		"re-marshal must produce exactly one end marker")
}

// TestUnmarshal_BareHeadingTreatedAsCleanContent pins the marker-
// only contract: a `## Notes` heading WITHOUT the surrounding marker
// pair is normal user prose and lands in CleanContent unchanged.
// Notes-mode activates strictly on the marker — this prevents a
// user-content entity's `## Notes` heading from being silently
// consumed by the notes-table parser.
func TestUnmarshal_BareHeadingTreatedAsCleanContent(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(`
---
id: wikipedia:foo
kind: wikipedia-article
plugin: wikipedia
---

Some clean content.

## Notes

random thoughts a user typed; not the system notes table.
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	assert.Empty(t, parsed.Notes,
		"bare ## Notes heading must NOT activate notes-mode without the marker pair")
	assert.Contains(t, parsed.CleanContent, "## Notes",
		"bare ## Notes heading must round-trip in CleanContent")
	assert.Contains(t, parsed.CleanContent, "random thoughts",
		"prose under bare ## Notes heading must round-trip in CleanContent")
}

// TestUnmarshal_MarkerWrappedAfterPluginBody pins the layout case
// where the plugin body region precedes the notes region. The
// scanner must transition from plugin-body / clean to notes mode
// on the notes start marker, regardless of what came before.
func TestUnmarshal_MarkerWrappedAfterPluginBody(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(`
---
id: wikipedia:foo
kind: wikipedia-article
plugin: wikipedia
note_count: 1
---

<!-- yaad:plugin start -->
# Foo

Plugin-emitted prose with [[brass]] wikilink.
<!-- yaad:plugin end -->

<!-- yaad:notes start -->
## Notes

| Notes |
|----------|
| 2026-05-13 — alice |
| Marker-wrapped note. |
<!-- yaad:notes end -->
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	require.Len(t, parsed.Notes, 1)
	assert.Equal(t, "Marker-wrapped note.", parsed.Notes[0].Text)
	assert.Contains(t, parsed.CleanContent, "yaad:plugin start",
		"plugin-body region (inside its own markers) must survive in clean_content")
	assert.Contains(t, parsed.CleanContent, "Plugin-emitted prose with",
		"plugin body inner content must survive in clean_content")
}

// TestUnmarshal_MarkerWrappedTwoComments pins the multi-row table
// parse inside marker pair: heading row + body row pairs alternate,
// boundaries clean.
func TestUnmarshal_MarkerWrappedTwoComments(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(`
---
id: wikipedia:foo
kind: wikipedia-article
plugin: wikipedia
note_count: 2
---

<!-- yaad:notes start -->
## Notes

| Notes |
|----------|
| 2026-05-13 — alice |
| First. |
| 2026-05-14 — operator |
| Second. |
<!-- yaad:notes end -->
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	require.Len(t, parsed.Notes, 2)
	assert.Equal(t, "First.", parsed.Notes[0].Text)
	assert.Equal(t, "alice", parsed.Notes[0].Author)
	assert.Equal(t, "Second.", parsed.Notes[1].Text)
	assert.Equal(t, "operator", parsed.Notes[1].Author)
}

// TestMergePluginBody_DoesNotTouchCommentsRegion pins the
// independence of the two marker regions per the #8 spec: a plugin
// body merge splices only between yaad:plugin markers; the
// yaad:notes region outside survives verbatim.
func TestMergePluginBody_DoesNotTouchCommentsRegion(t *testing.T) {
	t.Parallel()

	existing := strings.TrimSpace(`<!-- yaad:plugin start -->
# Old plugin body
<!-- yaad:plugin end -->

<!-- yaad:notes start -->
## Notes

| Notes |
|----------|
| 2026-05-13 — alice |
| Pre-existing note. |
<!-- yaad:notes end -->`)

	pluginContent := "# New plugin body\n\nUpdated."
	merged, err := MergePluginBody(existing, pluginContent)
	require.NoError(t, err)
	assert.Equal(t, "clean", merged.PriorMarkers,
		"clean re-ingest path")
	assert.Contains(t, merged.Body, "# New plugin body",
		"plugin body region must update")
	assert.NotContains(t, merged.Body, "# Old plugin body",
		"old plugin body must be replaced")
	assert.Contains(t, merged.Body, NotesStartMarker,
		"notes region must survive plugin re-ingest")
	assert.Contains(t, merged.Body, "Pre-existing note.",
		"notes table content must survive plugin re-ingest")
	assert.Contains(t, merged.Body, NotesEndMarker,
		"notes end marker must survive plugin re-ingest")
}

// TestUnmarshal_OperatorHandEditInsideMarkerRegion documents the
// chosen merge semantic per the #8 sub-question: operator hand-
// edits inside the marker region get the current behavior —
// wholesale replace by the next agent-add. Markdown between the
// table rows is discarded on re-write; the in-memory []Note is
// the source of truth and re-renders the table.
//
// Pinning this so future contributors can't quietly change the
// semantic without flipping the test + adjusting the docstring.
func TestUnmarshal_OperatorHandEditInsideMarkerRegion(t *testing.T) {
	t.Parallel()

	bodyWithHandEdit := strings.TrimSpace(`
---
id: wikipedia:foo
kind: wikipedia-article
plugin: wikipedia
note_count: 1
---

<!-- yaad:notes start -->
## Notes

Operator added a paragraph here mid-edit.

| Notes |
|----------|
| 2026-05-13 — alice |
| Agent note. |
<!-- yaad:notes end -->
`) + "\n"

	parsed, err := Unmarshal([]byte(bodyWithHandEdit))
	require.NoError(t, err)
	require.Len(t, parsed.Notes, 1,
		"the structured note survives despite operator hand-edit")
	assert.Equal(t, "Agent note.", parsed.Notes[0].Text)

	// Re-marshal — operator's hand-edited paragraph is LOST per the
	// chosen v1 semantic (wholesale replace by structured re-render).
	out, err := Marshal(parsed, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "Operator added a paragraph",
		"v1 semantic: operator hand-edits inside the markers are discarded on re-write")
}
