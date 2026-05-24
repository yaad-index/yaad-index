package vault

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_NoteCountFrontmatterAndBodyTable pins the
// contract: with 2 notes, frontmatter has `note_count: 2`
// (and NO `notes:`), body has the `## Notes` table with 4
// content rows (2 entries × 2 rows each: heading then body).
func TestMarshal_NoteCountFrontmatterAndBodyTable(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Notes: []Note{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "First note text.",
				Author: "alice",
			},
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "Second note.",
				Author: "operator",
			},
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	// Frontmatter: note_count present, notes: absent.
	assert.Contains(t, out, "note_count: 2", "note_count must reflect note slice length")
	assert.NotContains(t, out, "notes:",
		"frontmatter must NOT carry a notes: list anymore")

	// Body table: header + separator + 4 content rows in order.
	bodyExpected := []string{
		"## Notes",
		"",
		"| Notes |",
		"|----------|",
		"| 2026-05-03 — alice |",
		"| First note text. |",
		"| 2026-05-03 — operator |",
		"| Second note. |",
	}
	for _, line := range bodyExpected {
		assert.Contains(t, out, line+"\n", "body must include line %q", line)
	}
	// Strict ordering: the heading row for entry 1 comes before the
	// body row for entry 1, which comes before entry 2's heading.
	idx1Head := strings.Index(out, "| 2026-05-03 — alice |")
	idx1Body := strings.Index(out, "| First note text. |")
	idx2Head := strings.Index(out, "| 2026-05-03 — operator |")
	idx2Body := strings.Index(out, "| Second note. |")
	require.True(t, idx1Head >= 0 && idx1Body >= 0 && idx2Head >= 0 && idx2Body >= 0,
		"all four rows present")
	assert.Less(t, idx1Head, idx1Body, "heading-1 before body-1")
	assert.Less(t, idx1Body, idx2Head, "body-1 before heading-2")
	assert.Less(t, idx2Head, idx2Body, "heading-2 before body-2")
}

// TestMarshal_NoCommentsOmitsCountAndSection pins the no-noise
// contract: zero notes → no `note_count` key, no `## Notes`
// section.
func TestMarshal_NoCommentsOmitsCountAndSection(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	assert.NotContains(t, out, "note_count",
		"note_count omitempty must drop on zero notes")
	assert.NotContains(t, out, "## Notes",
		"## Notes section must be skipped when there are no notes")
}

// TestMarshal_CommentsRoundTrip — Marshal → Unmarshal preserves the
// Notes slice exactly (date-only precision, text, author).
func TestMarshal_CommentsRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Notes: []Note{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "First note.",
				Author: "alice",
			},
			{
				Date: mustParseTime(t, "2026-05-04T00:00:00Z"),
				Text: "Second note with no author.",
				Author: "",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Notes, 2)
	for i := range original.Notes {
		assert.True(t, original.Notes[i].Date.Equal(got.Notes[i].Date),
			"notes[%d].date: want %s, got %s",
			i, original.Notes[i].Date, got.Notes[i].Date)
		assert.Equal(t, original.Notes[i].Text, got.Notes[i].Text, "notes[%d].text", i)
		assert.Equal(t, original.Notes[i].Author, got.Notes[i].Author, "notes[%d].author", i)
	}
}

// TestMarshal_CommentsMultiParagraphRoundTrip pins the multi-
// paragraph encoding: paragraph breaks inside a note body cell
// render as `<br><br>` (so the cell stays on one table row) and
// parse back to `\n\n` cleanly.
func TestMarshal_CommentsMultiParagraphRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Notes: []Note{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "First paragraph.\n\nSecond paragraph after a blank line.\n\nThird paragraph.",
				Author: "alice",
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
	require.Len(t, got.Notes, 1)
	assert.Equal(t, original.Notes[0].Text, got.Notes[0].Text,
		"multi-paragraph round-trip must preserve paragraph breaks")
}

// TestMarshal_CommentsPipeEscape pins the pipe-escaping path:
// literal `|` chars in a note must be escaped to `\|` on render
// (otherwise they'd terminate the table cell early) and decoded
// back on parse.
func TestMarshal_CommentsPipeEscape(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Notes: []Note{
			{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "Has a | pipe inside the cell text.",
				Author: "alice",
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)
	out := string(b)
	assert.Contains(t, out, `\|`, "pipe in cell text must be escaped on render")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Notes, 1)
	assert.Equal(t, original.Notes[0].Text, got.Notes[0].Text,
		"pipe round-trip must preserve the literal | char")
}

// TestUnmarshal_CommentsIgnoresLegacyFrontmatterField pins the
// no-backward-compat decision: a vault file whose
// frontmatter still carries a `notes:` list is silently ignored
// — only the body table populates Entity.Notes.
func TestUnmarshal_CommentsIgnoresLegacyFrontmatterField(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		// Legacy shape — must NOT surface on the parsed entity.
		"notes:",
		"  - date: 2026-04-15T08:30:00Z",
		"    text: Legacy frontmatter note.",
		"    author: alice",
		"---",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	assert.Empty(t, got.Notes,
		"legacy frontmatter notes: list must be ignored post-")
}

// TestMarshal_CommentsHeadingRowFormat pins the heading-row shape:
// `<date> — <author>` when author is present, just `<date>` when not.
func TestMarshal_CommentsHeadingRowFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		note Note
		wantRow string
	}{
		{
			name: "with author",
			note: Note{
				Date: mustParseTime(t, "2026-05-03T00:00:00Z"),
				Text: "x",
				Author: "alice",
			},
			wantRow: "| 2026-05-03 — alice |",
		},
		{
			name: "no author",
			note: Note{
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
				Source: []string{"wikipedia/default"},
				Notes: []Note{c.note},
			}
			b, err := Marshal(e, nil)
			require.NoError(t, err)
			assert.Contains(t, string(b), c.wantRow+"\n",
				"heading row format must match for %s", c.name)
		})
	}
}

// TestUnmarshal_CommentsOrphanedHeadingRowFlushesAsEmptyBody pins
// the cold-reviewer's catch: a notes section that ends after a heading
// row without a paired body row preserves the heading rather than
// silently dropping it. The orphaned note lands with the
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
				"note_count: 1",
				"---",
				"",
				NotesStartMarker,
				"## Notes",
				"",
				"| Notes |",
				"|----------|",
				"| 2026-05-03 — alice |",
				// no body row; the end-marker terminates the
				// orphan without ever seeing the body row.
				NotesEndMarker,
			}, "\n"),
		},
		{
			name: "orphan before end marker, more content after",
			raw: strings.Join([]string{
				"---",
				"id: wikipedia:foo",
				"kind: wikipedia-article",
				"plugin: wikipedia",
				"note_count: 1",
				"---",
				"",
				NotesStartMarker,
				"## Notes",
				"",
				"| Notes |",
				"|----------|",
				"| 2026-05-03 — alice |",
				NotesEndMarker,
				"",
				"## After-notes user heading",
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
			require.Len(t, got.Notes, 1,
				"orphaned heading must land as a note with empty Text, not be dropped")
			assert.Equal(t, "alice", got.Notes[0].Author)
			assert.Empty(t, got.Notes[0].Text,
				"empty Text is the signal that the heading was authored without a paired body")
			assert.Equal(t, "2026-05-03", got.Notes[0].Date.UTC().Format("2006-01-02"))
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
		"note_count: 1",
		"---",
		"",
		NotesStartMarker,
		"## Notes",
		"",
		"| Notes |",
		"|----------|",
		"| 2026-05-03 |",
		"| Note with no author. |",
		NotesEndMarker,
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	require.Len(t, got.Notes, 1)
	assert.Empty(t, got.Notes[0].Author)
	assert.Equal(t, "Note with no author.", got.Notes[0].Text)
	assert.Equal(t, time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC),
		got.Notes[0].Date.UTC())
}

// TestMarshal_NotesFieldAndKindRoundTrip pins the #186
// agent-feedback fields: Field + Kind round-trip through the
// heading-row `[kind=X field=Y]` metadata suffix.
func TestMarshal_NotesFieldAndKindRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID:     "wikipedia:foo",
		Kind:   "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Notes: []Note{
			{
				Date:   mustParseTime(t, "2026-05-22T00:00:00Z"),
				Text:   "Legacy-shape entity-level note.",
				Author: "alice",
			},
			{
				Date:   mustParseTime(t, "2026-05-22T00:00:00Z"),
				Text:   "Entity-level annotation flag.",
				Author: "agent-bob",
				Kind:   NoteKindAnnotation,
			},
			{
				Date:   mustParseTime(t, "2026-05-22T00:00:00Z"),
				Text:   "Per-field note (kind=note default).",
				Author: "alice",
				Field:  "birth_date",
			},
			{
				Date:   mustParseTime(t, "2026-05-22T00:00:00Z"),
				Text:   "Per-field annotation.",
				Author: "agent-bob",
				Field:  "birth_date",
				Kind:   NoteKindAnnotation,
			},
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Notes, 4)

	// Index 0: legacy shape — no Field, no Kind, no metadata bracket.
	assert.Empty(t, got.Notes[0].Field)
	assert.Empty(t, got.Notes[0].Kind, "kind empty round-trips as empty (the default `note` is implicit)")

	// Index 1: kind=annotation only.
	assert.Empty(t, got.Notes[1].Field)
	assert.Equal(t, NoteKindAnnotation, got.Notes[1].Kind)

	// Index 2: field set, kind default (the implicit "note").
	assert.Equal(t, "birth_date", got.Notes[2].Field)
	assert.Empty(t, got.Notes[2].Kind, "default kind=note doesn't surface in the metadata")

	// Index 3: both set.
	assert.Equal(t, "birth_date", got.Notes[3].Field)
	assert.Equal(t, NoteKindAnnotation, got.Notes[3].Kind)
}

// TestUnmarshal_NotesLegacyShapeNoMetadata pins backwards-
// compatibility: vault files written before #186 (no
// `[kind=X field=Y]` suffix) parse cleanly with empty
// Field + Kind.
func TestUnmarshal_NotesLegacyShapeNoMetadata(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"note_count: 1",
		"---",
		"",
		NotesStartMarker,
		"## Notes",
		"",
		"| Notes |",
		"|----------|",
		"| 2026-05-03 — alice @ alice |",
		"| Legacy note with no field/kind metadata. |",
		NotesEndMarker,
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	require.Len(t, got.Notes, 1)
	assert.Equal(t, "alice", got.Notes[0].Author)
	assert.Equal(t, "alice", got.Notes[0].Operator)
	assert.Empty(t, got.Notes[0].Field)
	assert.Empty(t, got.Notes[0].Kind)
	assert.Equal(t, "Legacy note with no field/kind metadata.", got.Notes[0].Text)
}

// TestUnmarshal_NotesUnknownMetadataKeyIgnored pins forward-
// compatibility: an unknown `key=value` token inside the
// metadata bracket is silently ignored. Future #186 follow-ups
// may add new tag keys; older parsers must not reject them.
func TestUnmarshal_NotesUnknownMetadataKeyIgnored(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"note_count: 1",
		"---",
		"",
		NotesStartMarker,
		"## Notes",
		"",
		"| Notes |",
		"|----------|",
		"| 2026-05-22 — alice [field=birth_date kind=annotation future_tag=v2] |",
		"| Forward-compat note. |",
		NotesEndMarker,
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	require.Len(t, got.Notes, 1)
	assert.Equal(t, "alice", got.Notes[0].Author)
	assert.Equal(t, "birth_date", got.Notes[0].Field)
	assert.Equal(t, NoteKindAnnotation, got.Notes[0].Kind)
}
