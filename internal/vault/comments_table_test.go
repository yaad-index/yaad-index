package vault

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_CommentCountFrontmatterAndBodyTable pins the
// contract: with 2 comments, frontmatter has `comment_count: 2`
// (and NO `comments:`), body has the `## Comments` table with 4
// content rows (2 entries × 2 rows each: heading then body).
func TestMarshal_CommentCountFrontmatterAndBodyTable(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "First comment text.",
				Author: "alice2",
			},
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "Second comment.",
				Author: "operator",
			},
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	// Frontmatter: comment_count present, comments: absent.
	assert.Contains(t, out, "comment_count: 2", "comment_count must reflect comment slice length")
	assert.NotContains(t, out, "comments:",
		"frontmatter must NOT carry a comments: list anymore")

	// Body table: header + separator + 4 content rows in order.
	bodyExpected := []string{
		"## Comments",
		"",
		"| Comments |",
		"|----------|",
		"| 2026-05-03 — alice2 |",
		"| First comment text. |",
		"| 2026-05-03 — operator |",
		"| Second comment. |",
	}
	for _, line := range bodyExpected {
		assert.Contains(t, out, line+"\n", "body must include line %q", line)
	}
	// Strict ordering: the heading row for entry 1 comes before the
	// body row for entry 1, which comes before entry 2's heading.
	idx1Head := strings.Index(out, "| 2026-05-03 — alice2 |")
	idx1Body := strings.Index(out, "| First comment text. |")
	idx2Head := strings.Index(out, "| 2026-05-03 — operator |")
	idx2Body := strings.Index(out, "| Second comment. |")
	require.True(t, idx1Head >= 0 && idx1Body >= 0 && idx2Head >= 0 && idx2Body >= 0,
		"all four rows present")
	assert.Less(t, idx1Head, idx1Body, "heading-1 before body-1")
	assert.Less(t, idx1Body, idx2Head, "body-1 before heading-2")
	assert.Less(t, idx2Head, idx2Body, "heading-2 before body-2")
}

// TestMarshal_NoCommentsOmitsCountAndSection pins the no-noise
// contract: zero comments → no `comment_count` key, no `## Comments`
// section.
func TestMarshal_NoCommentsOmitsCountAndSection(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	assert.NotContains(t, out, "comment_count",
		"comment_count omitempty must drop on zero comments")
	assert.NotContains(t, out, "## Comments",
		"## Comments section must be skipped when there are no comments")
}

// TestMarshal_CommentsRoundTrip — Marshal → Unmarshal preserves the
// Comments slice exactly (date-only precision, text, author).
func TestMarshal_CommentsRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "First comment.",
				Author: "alice2",
			},
			{
				Date: mustParseTime(t, "2026-05-04T00:00:00Z"),
				Text: "Second comment with no author.",
				Author: "",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Comments, 2)
	for i := range original.Comments {
		assert.True(t, original.Comments[i].Date.Equal(got.Comments[i].Date),
			"comments[%d].date: want %s, got %s",
			i, original.Comments[i].Date, got.Comments[i].Date)
		assert.Equal(t, original.Comments[i].Text, got.Comments[i].Text, "comments[%d].text", i)
		assert.Equal(t, original.Comments[i].Author, got.Comments[i].Author, "comments[%d].author", i)
	}
}

// TestMarshal_CommentsMultiParagraphRoundTrip pins the multi-
// paragraph encoding: paragraph breaks inside a comment body cell
// render as `<br><br>` (so the cell stays on one table row) and
// parse back to `\n\n` cleanly.
func TestMarshal_CommentsMultiParagraphRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "First paragraph.\n\nSecond paragraph after a blank line.\n\nThird paragraph.",
				Author: "alice2",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)
	out := string(b)

	// Wire form: paragraphs joined with <br><br> on a single row.
	assert.Contains(t, out,
		"| First paragraph.<br><br>Second paragraph after a blank line.<br><br>Third paragraph. |\n",
		"multi-paragraph body row must encode \\n\\n as <br><br>")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, original.Comments[0].Text, got.Comments[0].Text,
		"multi-paragraph round-trip must preserve paragraph breaks")
}

// TestMarshal_CommentsPipeEscape pins the pipe-escaping path:
// literal `|` chars in a comment must be escaped to `\|` on render
// (otherwise they'd terminate the table cell early) and decoded
// back on parse.
func TestMarshal_CommentsPipeEscape(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "Has a | pipe inside the cell text.",
				Author: "alice2",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)
	out := string(b)
	assert.Contains(t, out, `\|`, "pipe in cell text must be escaped on render")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, original.Comments[0].Text, got.Comments[0].Text,
		"pipe round-trip must preserve the literal | char")
}

// TestUnmarshal_CommentsIgnoresLegacyFrontmatterField pins the
// no-backward-compat decision: a vault file whose
// frontmatter still carries a `comments:` list is silently ignored
// — only the body table populates Entity.Comments.
func TestUnmarshal_CommentsIgnoresLegacyFrontmatterField(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		// Legacy shape — must NOT surface on the parsed entity.
		"comments:",
		"  - date: 2026-04-15T08:30:00Z",
		"    text: Legacy frontmatter comment.",
		"    author: alice",
		"---",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	assert.Empty(t, got.Comments,
		"legacy frontmatter comments: list must be ignored post-")
}

// TestMarshal_CommentsHeadingRowFormat pins the heading-row shape:
// `<date> — <author>` when author is present, just `<date>` when not.
func TestMarshal_CommentsHeadingRowFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		comment Comment
		wantRow string
	}{
		{
			name: "with author",
			comment: Comment{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "x",
				Author: "alice2",
			},
			wantRow: "| 2026-05-03 — alice2 |",
		},
		{
			name: "no author",
			comment: Comment{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "x",
			},
			wantRow: "| 2026-05-03 |",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := &Entity{
				ID: "wikipedia:foo",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Comments: []Comment{c.comment},
			}
			b, err := Marshal(e, nil)
			require.NoError(t, err)
			assert.Contains(t, string(b), c.wantRow+"\n",
				"heading row format must match for %s", c.name)
		})
	}
}

// TestUnmarshal_CommentsOrphanedHeadingRowFlushesAsEmptyBody pins
// the cold-reviewer's catch: a comments section that ends after a heading
// row without a paired body row preserves the heading rather than
// silently dropping it. The orphaned comment lands with the
// captured date + author and an empty Text — a clear "authored
// mid-edit" signal.
func TestUnmarshal_CommentsOrphanedHeadingRowFlushesAsEmptyBody(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw string
	}{
		{
			name: "orphan at end of file",
			raw: strings.Join([]string{
				"---",
				"id: wikipedia:foo",
				"kind: wikipedia-article",
				"plugin: wikipedia",
				"comment_count: 1",
				"---",
				"",
				"## Comments",
				"",
				"| Comments |",
				"|----------|",
				"| 2026-05-03 — alice2 |",
				// no body row; section ends here.
			}, "\n"),
		},
		{
			name: "orphan before another section heading",
			raw: strings.Join([]string{
				"---",
				"id: wikipedia:foo",
				"kind: wikipedia-article",
				"plugin: wikipedia",
				"comment_count: 1",
				"---",
				"",
				"## Comments",
				"",
				"| Comments |",
				"|----------|",
				"| 2026-05-03 — alice2 |",
				"",
				"## Notes", // unknown heading flips back to clean
				"",
				"some hand-authored body content",
			}, "\n"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Unmarshal([]byte(c.raw))
			require.NoError(t, err)
			require.Len(t, got.Comments, 1,
				"orphaned heading must land as a comment with empty Text, not be dropped")
			assert.Equal(t, "alice2", got.Comments[0].Author)
			assert.Empty(t, got.Comments[0].Text,
				"empty Text is the signal that the heading was authored without a paired body")
			assert.Equal(t, "2026-05-03", got.Comments[0].Date.UTC().Format("2006-01-02"))
		})
	}
}

// TestUnmarshal_CommentsHeadingRowAuthorOptional pins the parse
// side of the no-author heading row.
func TestUnmarshal_CommentsHeadingRowAuthorOptional(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"comment_count: 1",
		"---",
		"",
		"## Comments",
		"",
		"| Comments |",
		"|----------|",
		"| 2026-05-03 |",
		"| Comment with no author. |",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	assert.Empty(t, got.Comments[0].Author)
	assert.Equal(t, "Comment with no author.", got.Comments[0].Text)
	assert.Equal(t, time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC),
		got.Comments[0].Date.UTC())
}
