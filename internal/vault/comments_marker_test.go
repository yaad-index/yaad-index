package vault

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_CommentsWrappedInMarkers pins the post-#8 write
// contract: writing an entity with comments wraps the
// `## Comments` section in CommentsStartMarker / CommentsEndMarker.
// New entities + re-writes of marker-wrapped entities both produce
// the wrapped shape.
func TestMarshal_CommentsWrappedInMarkers(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID:     "wikipedia:foo",
		Kind:   "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date:   mustParseTime(t, "2026-05-13T00:00:00Z"),
				Text:   "First comment.",
				Author: "yaad",
			},
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	assert.Contains(t, out, CommentsStartMarker,
		"marshalled body must include the comments start marker")
	assert.Contains(t, out, CommentsEndMarker,
		"marshalled body must include the comments end marker")

	startIdx := strings.Index(out, CommentsStartMarker)
	endIdx := strings.Index(out, CommentsEndMarker)
	headIdx := strings.Index(out, "## Comments")
	require.Greater(t, headIdx, startIdx,
		"`## Comments` heading must appear AFTER the start marker")
	require.Greater(t, endIdx, headIdx,
		"end marker must appear AFTER the `## Comments` content")
}

// TestUnmarshal_MarkerWrappedRoundTrip pins the read path: a
// marker-wrapped body round-trips through Unmarshal → Marshal with
// comments preserved + still marker-wrapped on output.
func TestUnmarshal_MarkerWrappedRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID:     "wikipedia:foo",
		Kind:   "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date:   mustParseTime(t, "2026-05-13T00:00:00Z"),
				Text:   "Round-trip comment.",
				Author: "yaad",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)

	parsed, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, parsed.Comments, 1)
	assert.Equal(t, "Round-trip comment.", parsed.Comments[0].Text)
	assert.Equal(t, "yaad", parsed.Comments[0].Author)

	// Re-marshal — must still be marker-wrapped (not double-wrapped,
	// not unwrapped).
	again, err := Marshal(parsed, nil)
	require.NoError(t, err)
	out := string(again)
	startCount := strings.Count(out, CommentsStartMarker)
	endCount := strings.Count(out, CommentsEndMarker)
	assert.Equal(t, 1, startCount,
		"re-marshal must produce exactly one start marker (no double-wrap)")
	assert.Equal(t, 1, endCount,
		"re-marshal must produce exactly one end marker")
}

// TestUnmarshal_LegacyUnmarkedFallback pins the migration path:
// a vault file written before #8 (no markers, plain `## Comments`
// heading + table) parses correctly. The fallback section-aware
// parser recovers the comments; the next Marshal writes them
// under the new marker-pair.
func TestUnmarshal_LegacyUnmarkedFallback(t *testing.T) {
	t.Parallel()

	legacy := strings.TrimSpace(`
---
id: wikipedia:foo
kind: wikipedia-article
plugin: wikipedia
comment_count: 1
---

Some clean content.

## Comments

| Comments |
|----------|
| 2026-05-13 — yaad |
| Legacy comment. |
`) + "\n"

	parsed, err := Unmarshal([]byte(legacy))
	require.NoError(t, err)
	require.Len(t, parsed.Comments, 1,
		"legacy un-marked Comments section must parse via section-aware fallback")
	assert.Equal(t, "Legacy comment.", parsed.Comments[0].Text)
	assert.Equal(t, "yaad", parsed.Comments[0].Author)

	// Re-marshal — must produce marker-wrapped output (migration
	// complete on first write).
	out, err := Marshal(parsed, nil)
	require.NoError(t, err)
	body := string(out)
	assert.Contains(t, body, CommentsStartMarker,
		"first write under new code must wrap legacy comments in start marker")
	assert.Contains(t, body, CommentsEndMarker,
		"first write under new code must wrap legacy comments in end marker")
}

// TestUnmarshal_MarkerWrappedAfterPluginBody pins the layout case
// where the plugin body region precedes the comments region. The
// scanner must transition from plugin-body / clean to comments mode
// on the comments start marker, regardless of what came before.
func TestUnmarshal_MarkerWrappedAfterPluginBody(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(`
---
id: wikipedia:foo
kind: wikipedia-article
plugin: wikipedia
comment_count: 1
---

<!-- yaad:plugin start -->
# Foo

Plugin-emitted prose with [[brass]] wikilink.
<!-- yaad:plugin end -->

<!-- yaad:comments start -->
## Comments

| Comments |
|----------|
| 2026-05-13 — yaad |
| Marker-wrapped comment. |
<!-- yaad:comments end -->
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	require.Len(t, parsed.Comments, 1)
	assert.Equal(t, "Marker-wrapped comment.", parsed.Comments[0].Text)
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
comment_count: 2
---

<!-- yaad:comments start -->
## Comments

| Comments |
|----------|
| 2026-05-13 — yaad |
| First. |
| 2026-05-14 — operator |
| Second. |
<!-- yaad:comments end -->
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	require.Len(t, parsed.Comments, 2)
	assert.Equal(t, "First.", parsed.Comments[0].Text)
	assert.Equal(t, "yaad", parsed.Comments[0].Author)
	assert.Equal(t, "Second.", parsed.Comments[1].Text)
	assert.Equal(t, "operator", parsed.Comments[1].Author)
}

// TestMergePluginBody_DoesNotTouchCommentsRegion pins the
// independence of the two marker regions per the #8 spec: a plugin
// body merge splices only between yaad:plugin markers; the
// yaad:comments region outside survives verbatim.
func TestMergePluginBody_DoesNotTouchCommentsRegion(t *testing.T) {
	t.Parallel()

	existing := strings.TrimSpace(`<!-- yaad:plugin start -->
# Old plugin body
<!-- yaad:plugin end -->

<!-- yaad:comments start -->
## Comments

| Comments |
|----------|
| 2026-05-13 — yaad |
| Pre-existing comment. |
<!-- yaad:comments end -->`)

	pluginContent := "# New plugin body\n\nUpdated."
	merged, err := MergePluginBody(existing, pluginContent)
	require.NoError(t, err)
	assert.Equal(t, "clean", merged.PriorMarkers,
		"clean re-ingest path")
	assert.Contains(t, merged.Body, "# New plugin body",
		"plugin body region must update")
	assert.NotContains(t, merged.Body, "# Old plugin body",
		"old plugin body must be replaced")
	assert.Contains(t, merged.Body, CommentsStartMarker,
		"comments region must survive plugin re-ingest")
	assert.Contains(t, merged.Body, "Pre-existing comment.",
		"comments table content must survive plugin re-ingest")
	assert.Contains(t, merged.Body, CommentsEndMarker,
		"comments end marker must survive plugin re-ingest")
}

// TestUnmarshal_OperatorHandEditInsideMarkerRegion documents the
// chosen merge semantic per the #8 sub-question: operator hand-
// edits inside the marker region get the current behavior —
// wholesale replace by the next agent-add. Markdown between the
// table rows is discarded on re-write; the in-memory []Comment is
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
comment_count: 1
---

<!-- yaad:comments start -->
## Comments

Operator added a paragraph here mid-edit.

| Comments |
|----------|
| 2026-05-13 — yaad |
| Agent comment. |
<!-- yaad:comments end -->
`) + "\n"

	parsed, err := Unmarshal([]byte(bodyWithHandEdit))
	require.NoError(t, err)
	require.Len(t, parsed.Comments, 1,
		"the structured comment survives despite operator hand-edit")
	assert.Equal(t, "Agent comment.", parsed.Comments[0].Text)

	// Re-marshal — operator's hand-edited paragraph is LOST per the
	// chosen v1 semantic (wholesale replace by structured re-render).
	out, err := Marshal(parsed, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "Operator added a paragraph",
		"v1 semantic: operator hand-edits inside the markers are discarded on re-write")
}
