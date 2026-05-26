package vault_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/vault"
)

// A body with no headings collapses to one Section{Depth=0,
// Heading="", Body=whole-body}, addressable as positional index 0.
func TestParseSections_NoHeadings(t *testing.T) {
	body := "Some prose.\nAnother line.\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 1)
	require.Equal(t, 0, got[0].Index)
	require.Equal(t, 0, got[0].Depth)
	require.Equal(t, "", got[0].Heading)
	require.Equal(t, body, got[0].Body)
	require.Equal(t, 0, got[0].ByteOffset)
}

// Empty body returns a single empty section so paginators always see
// at least one addressable unit.
func TestParseSections_EmptyBody(t *testing.T) {
	got := vault.ParseSections("")
	require.Len(t, got, 1)
	require.Equal(t, 0, got[0].Depth)
	require.Equal(t, "", got[0].Body)
}

// Pre-heading body + headed sections: section 0 is the prefix, then
// each heading is its own section.
func TestParseSections_PreHeadingBodyPlusSections(t *testing.T) {
	body := "intro line\n\n## First\nfirst body\n## Second\nsecond body\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 3)

	require.Equal(t, 0, got[0].Depth)
	require.Equal(t, "", got[0].Heading)
	require.Equal(t, "intro line\n\n", got[0].Body)
	require.Equal(t, 0, got[0].ByteOffset)

	require.Equal(t, 2, got[1].Depth)
	require.Equal(t, "First", got[1].Heading)
	require.Equal(t, "first body\n", got[1].Body)
	require.Equal(t, strings.Index(body, "## First"), got[1].ByteOffset)

	require.Equal(t, 2, got[2].Depth)
	require.Equal(t, "Second", got[2].Heading)
	require.Equal(t, "second body\n", got[2].Body)
	require.Equal(t, strings.Index(body, "## Second"), got[2].ByteOffset)
}

// Heading at byte 0 → no pre-heading section is emitted.
func TestParseSections_NoPreHeadingBody(t *testing.T) {
	body := "## Only\nbody\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].Depth)
	require.Equal(t, "Only", got[0].Heading)
	require.Equal(t, "body\n", got[0].Body)
	require.Equal(t, 0, got[0].ByteOffset)
}

// Containment model — `# Top` body INCLUDES nested `##` and `###`
// content textually, until the next heading of same-or-shallower
// depth (`# Top2` here).
func TestParseSections_ContainmentModel(t *testing.T) {
	body := "# Top\ntop body\n## Mid\nmid body\n### Leaf\nleaf body\n## Mid2\nmid2 body\n# Top2\ntop2 body\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 5)

	// # Top: body includes ## Mid + ### Leaf + ## Mid2 (everything
	// until # Top2 because `Top2` is the next same-or-shallower depth).
	require.Equal(t, "Top", got[0].Heading)
	require.Equal(t, 1, got[0].Depth)
	require.Equal(t, "top body\n## Mid\nmid body\n### Leaf\nleaf body\n## Mid2\nmid2 body\n", got[0].Body)

	// ## Mid: body extends until ## Mid2 (same depth) — INCLUDES the
	// ### Leaf inside.
	require.Equal(t, "Mid", got[1].Heading)
	require.Equal(t, 2, got[1].Depth)
	require.Equal(t, "mid body\n### Leaf\nleaf body\n", got[1].Body)

	// ### Leaf: body extends until next heading of depth <= 3, which
	// is ## Mid2 (depth 2). Just the leaf content.
	require.Equal(t, "Leaf", got[2].Heading)
	require.Equal(t, 3, got[2].Depth)
	require.Equal(t, "leaf body\n", got[2].Body)

	// ## Mid2: extends until # Top2.
	require.Equal(t, "Mid2", got[3].Heading)
	require.Equal(t, 2, got[3].Depth)
	require.Equal(t, "mid2 body\n", got[3].Body)

	// # Top2: extends to end-of-body.
	require.Equal(t, "Top2", got[4].Heading)
	require.Equal(t, 1, got[4].Depth)
	require.Equal(t, "top2 body\n", got[4].Body)
}

// All six ATX heading depths are addressable.
func TestParseSections_AllSixDepths(t *testing.T) {
	body := "# d1\nx\n## d2\nx\n### d3\nx\n#### d4\nx\n##### d5\nx\n###### d6\nx\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 6)
	for i, want := range []int{1, 2, 3, 4, 5, 6} {
		require.Equal(t, want, got[i].Depth, "section %d depth", i)
	}
}

// Headings inside fenced code blocks are NOT section boundaries.
func TestParseSections_FencedCodeNotASection(t *testing.T) {
	body := "## Real\nbefore\n```\n## Fake heading\nstill code\n```\nafter real\n## Real Two\nbody2\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 2)
	require.Equal(t, "Real", got[0].Heading)
	require.Contains(t, got[0].Body, "## Fake heading", "fenced code with heading-shaped line should land in body, not as a section")
	require.Equal(t, "Real Two", got[1].Heading)
}

// Indented `#` lines are NOT ATX headings (CommonMark requires no
// leading indentation for ATX recognition; we inherit that rule).
func TestParseSections_IndentedHashIsNotHeading(t *testing.T) {
	body := "intro\n ## not a heading\nstill prose\n## Real\nbody\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 2)
	require.Equal(t, "", got[0].Heading)
	require.Contains(t, got[0].Body, " ## not a heading")
	require.Equal(t, "Real", got[1].Heading)
}

// CommonMark §4.2 closing `#`s are stripped from the heading text —
// but ONLY when preceded by whitespace. A trailing `#` adjacent to
// content is part of the heading text (`## C# Language` → `C# Language`,
// not `C`). Catch from the cold-reviewer on a prior PR / PR-A.
func TestParseSections_StripsClosingHashes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"trailing space-and-hashes stripped", "## Title ##\nbody\n", "Title"},
		{"single trailing space-and-hash stripped", "## Notes #\nbody\n", "Notes"},
		{"hash without preceding space is kept", "## C# Language\nbody\n", "C# Language"},
		{"trailing hash without space is kept", "## C#\nbody\n", "C#"},
		{"midword hash is kept", "## a#b\nbody\n", "a#b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vault.ParseSections(tc.body)
			require.Len(t, got, 1)
			require.Equal(t, tc.want, got[0].Heading)
		})
	}
}

// Heading with markdown formatting in the text — the text preserves
// the formatting verbatim, but the slug strips `**`, `_`, “ ` “.
func TestParseSections_HeadingFormattingInText(t *testing.T) {
	body := "## My **Bold** Heading\nbody\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 1)
	require.Equal(t, "My **Bold** Heading", got[0].Heading, "heading text preserves verbatim")
	require.Equal(t, "my-bold-heading", got[0].HeadingSlug(), "slug strips markdown formatting")
}

// Pre-heading body section has no slug.
func TestParseSections_PreHeadingHasNoSlug(t *testing.T) {
	body := "intro\n## First\nbody\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 2)
	require.Equal(t, "", got[0].HeadingSlug())
}

// Slug normalization: lowercase, alphanumeric runs separated by single
// hyphens, hyphen-trimmed.
func TestParseSections_SlugifyEdgeCases(t *testing.T) {
	cases := map[string]string{
		"## Books I Loved\n": "books-i-loved",
		"## Notes -- Q1 / 2026\n": "notes-q1-2026",
		"## My Heading\n": "my-heading",
		"## Section 0\n": "section-0",
		"## ALL CAPS\n": "all-caps",
	}
	for body, wantSlug := range cases {
		t.Run(wantSlug, func(t *testing.T) {
			got := vault.ParseSections(body + "x\n")
			require.Len(t, got, 1)
			require.Equal(t, wantSlug, got[0].HeadingSlug())
		})
	}
}

// Headings with empty bodies are valid addressable sections (the
// whole-subtree shape from's containment-model example: a parent
// heading with sub-headings can have its OWN body be just the text
// before the first sub-heading, which may be empty).
func TestParseSections_EmptyBodySection(t *testing.T) {
	body := "## Books I Loved This Year\n### Fiction\ncontent A\n### Non-fiction\ncontent B\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 3)
	require.Equal(t, "Books I Loved This Year", got[0].Heading)
	require.Equal(t, 2, got[0].Depth)
	// Body of the parent INCLUDES both sub-sections per containment.
	require.Equal(t, "### Fiction\ncontent A\n### Non-fiction\ncontent B\n", got[0].Body)
	require.Equal(t, "Fiction", got[1].Heading)
	require.Equal(t, "content A\n", got[1].Body)
	require.Equal(t, "Non-fiction", got[2].Heading)
	require.Equal(t, "content B\n", got[2].Body)
}

// ResolveSectionAddr — numeric address takes the positional branch.
func TestResolveSectionAddr_Positional(t *testing.T) {
	body := "## A\nbody A\n## B\nbody B\n## C\nbody C\n"
	sections := vault.ParseSections(body)
	require.Len(t, sections, 3)

	for in, want := range map[string]int{"0": 0, "1": 1, "2": 2} {
		got, ok := vault.ResolveSectionAddr(sections, in)
		require.True(t, ok, "addr %q must resolve", in)
		require.Equal(t, want, got)
	}

	// Out of range.
	_, ok := vault.ResolveSectionAddr(sections, "3")
	require.False(t, ok, "out-of-range positional must return false")

	// Empty addr.
	_, ok = vault.ResolveSectionAddr(sections, "")
	require.False(t, ok)
}

// ResolveSectionAddr — non-numeric address resolves by heading slug.
func TestResolveSectionAddr_Slug(t *testing.T) {
	body := "## Top\nA\n## Books I Loved\nB\n## Other\nC\n"
	sections := vault.ParseSections(body)
	require.Len(t, sections, 3)

	got, ok := vault.ResolveSectionAddr(sections, "books-i-loved")
	require.True(t, ok)
	require.Equal(t, 1, got)

	got, ok = vault.ResolveSectionAddr(sections, "top")
	require.True(t, ok)
	require.Equal(t, 0, got)

	_, ok = vault.ResolveSectionAddr(sections, "no-such-section")
	require.False(t, ok)
}

// Duplicate slug → ResolveSectionAddr returns false; agent must fall
// back to positional index. (Per the prior design, clarification: positional is the
// canonical disambiguating fallback when heading text collides.)
func TestResolveSectionAddr_DuplicateSlugFallsBack(t *testing.T) {
	body := "## Notes\nfirst body\n## Notes\nsecond body\n"
	sections := vault.ParseSections(body)
	require.Len(t, sections, 2)
	require.Equal(t, sections[0].HeadingSlug(), sections[1].HeadingSlug(), "test setup: both slugs match")

	_, ok := vault.ResolveSectionAddr(sections, "notes")
	require.False(t, ok, "duplicate slug must NOT auto-resolve to one of them")

	// Positional index works as fallback.
	got, ok := vault.ResolveSectionAddr(sections, "1")
	require.True(t, ok)
	require.Equal(t, 1, got)
}

// Pre-heading body resolves by positional index 0; it has no slug.
func TestResolveSectionAddr_PreHeadingByPositionalOnly(t *testing.T) {
	body := "intro\n## First\nbody\n"
	sections := vault.ParseSections(body)
	require.Len(t, sections, 2)

	got, ok := vault.ResolveSectionAddr(sections, "0")
	require.True(t, ok)
	require.Equal(t, 0, got)

	// No slug to address it by.
	require.Equal(t, "", sections[0].HeadingSlug())
}

// ReplaceSectionBody — replacing a leaf section body swaps just that
// section's content, leaving the heading and surrounding sections
// intact.
func TestReplaceSectionBody_LeafSection(t *testing.T) {
	body := "## A\nold A\n## B\nold B\n## C\nold C\n"
	sections := vault.ParseSections(body)
	got, err := vault.ReplaceSectionBody(body, sections, 1, "new B body\n")
	require.NoError(t, err)
	require.Equal(t, "## A\nold A\n## B\nnew B body\n## C\nold C\n", got)
}

// ReplaceSectionBody — replacing a parent section (under containment
// model) rewrites the whole sub-tree. The agent is responsible for
// including any sub-headings they want to keep.
func TestReplaceSectionBody_ParentRewritesSubtree(t *testing.T) {
	body := "# Top\ntop body\n## Mid\nmid body\n# Top2\ntop2 body\n"
	sections := vault.ParseSections(body)
	require.Equal(t, "Top", sections[0].Heading)
	require.Equal(t, 1, sections[0].Depth)
	got, err := vault.ReplaceSectionBody(body, sections, 0, "all-new top with no sub-headings\n")
	require.NoError(t, err)
	require.Equal(t, "# Top\nall-new top with no sub-headings\n# Top2\ntop2 body\n", got)
}

// ReplaceSectionBody — replacing the pre-heading body section
// rewrites the prefix without touching subsequent headings.
func TestReplaceSectionBody_PreHeadingBody(t *testing.T) {
	body := "old intro\n\n## First\nfirst body\n"
	sections := vault.ParseSections(body)
	require.Equal(t, 0, sections[0].Depth)
	got, err := vault.ReplaceSectionBody(body, sections, 0, "new intro\n\n")
	require.NoError(t, err)
	require.Equal(t, "new intro\n\n## First\nfirst body\n", got)
}

// ReplaceSectionBody — out-of-range index errors.
func TestReplaceSectionBody_OutOfRange(t *testing.T) {
	body := "## a\nbody\n"
	sections := vault.ParseSections(body)
	_, err := vault.ReplaceSectionBody(body, sections, 5, "x")
	require.Error(t, err)
	_, err = vault.ReplaceSectionBody(body, sections, -1, "x")
	require.Error(t, err)
}

// SlugFromTitle — happy path + edge cases.
func TestSlugFromTitle(t *testing.T) {
	cases := map[string]string{
		"Books I Loved This Year": "books-i-loved-this-year",
		"Notes -- Q1 / 2026": "notes-q1-2026",
		"My **Bold** Note": "my-bold-note",
		" Leading and trailing ": "leading-and-trailing",
		"---multiple--hyphens--": "multiple-hyphens",
		"ALL CAPS": "all-caps",
		"Section 0": "section-0",
	}
	for in, want := range cases {
		t.Run(want, func(t *testing.T) {
			got, err := vault.SlugFromTitle(in)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// SlugFromTitle — empty / unparseable titles error.
func TestSlugFromTitle_EmptyErrors(t *testing.T) {
	for _, bad := range []string{"", " ", "***", "---", "_ _ _"} {
		_, err := vault.SlugFromTitle(bad)
		require.Error(t, err, "title %q must error", bad)
	}
}

// ByteOffset addresses cursor pagination — each section's offset
// points at the start of its address (heading line for headed sections,
// 0 for pre-heading body).
func TestParseSections_ByteOffsetsAreAddressable(t *testing.T) {
	body := "intro\n## First\nfirst body\n## Second\nsecond body\n"
	got := vault.ParseSections(body)
	require.Len(t, got, 3)
	require.Equal(t, 0, got[0].ByteOffset)
	require.Equal(t, body[got[1].ByteOffset:got[1].ByteOffset+len("## First")], "## First")
	require.Equal(t, body[got[2].ByteOffset:got[2].ByteOffset+len("## Second")], "## Second")
}

// --- InsertSection (#299) -------------------------------------------------

func TestInsertSection_AppendsAtEnd(t *testing.T) {
	body := "intro\n## First\nfirst body\n"
	sections := vault.ParseSections(body)
	out, idx, err := vault.InsertSection(body, sections, len(sections), 2, "Second", "second body\n")
	require.NoError(t, err)
	require.Equal(t, len(sections), idx, "new section appended at end")
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 3, "intro + two ##-sections")
	require.Equal(t, "Second", parsed[2].Heading)
	require.Equal(t, "second body\n", parsed[2].Body)
}

func TestInsertSection_AfterSpecificSection(t *testing.T) {
	body := "intro\n## First\nfirst body\n## Third\nthird body\n"
	sections := vault.ParseSections(body)
	// after idx=1 (the First section) → new section lands between First and Third.
	out, idx, err := vault.InsertSection(body, sections, 1, 2, "Second", "second body\n")
	require.NoError(t, err)
	require.Equal(t, 2, idx)
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 4)
	require.Equal(t, []string{"", "First", "Second", "Third"}, []string{
		parsed[0].Heading, parsed[1].Heading, parsed[2].Heading, parsed[3].Heading,
	})
	require.Equal(t, "second body\n", parsed[2].Body)
}

func TestInsertSection_DepthDefaultsToAfterSection(t *testing.T) {
	body := "## First\nfirst\n"
	sections := vault.ParseSections(body)
	out, _, err := vault.InsertSection(body, sections, 0, 0 /* default depth */, "Second", "x\n")
	require.NoError(t, err)
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 2)
	require.Equal(t, 2, parsed[1].Depth, "default depth follows after-section's depth")
}

func TestInsertSection_PrependAfterPreHeadingBody(t *testing.T) {
	body := "intro\n## Existing\nbody\n"
	sections := vault.ParseSections(body)
	out, idx, err := vault.InsertSection(body, sections, -1, 2, "New", "n\n")
	require.NoError(t, err)
	require.Equal(t, 1, idx, "lands at index 1, after pre-heading section 0")
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 3)
	require.Equal(t, "intro\n", parsed[0].Body)
	require.Equal(t, "New", parsed[1].Heading)
	require.Equal(t, "Existing", parsed[2].Heading)
}

func TestInsertSection_PrependToHeadlessNoPreText(t *testing.T) {
	body := "## Existing\nbody\n"
	sections := vault.ParseSections(body)
	out, idx, err := vault.InsertSection(body, sections, -1, 2, "First", "x\n")
	require.NoError(t, err)
	require.Equal(t, 0, idx, "prepended at document start")
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 2)
	require.Equal(t, "First", parsed[0].Heading)
	require.Equal(t, "Existing", parsed[1].Heading)
}

func TestInsertSection_ContainmentMovesNestedHeadings(t *testing.T) {
	// Inserting a same-depth sibling AFTER A (which contains a nested
	// ### A.sub) must land BETWEEN A's subtree and B's heading, not
	// inside A's containment range.
	body := "## A\nA body\n### A.sub\nsub body\n## B\nB body\n"
	sections := vault.ParseSections(body)
	// afterIdx=0 is A; A's containment runs through A.sub up to B's
	// heading start, so the insert offset lands just before B.
	out, idx, err := vault.InsertSection(body, sections, 0, 2, "Between", "between body\n")
	require.NoError(t, err)
	require.Contains(t, out, "## Between\nbetween body\n")
	parsed := vault.ParseSections(out)
	// 4 sections: ## A (containing ### A.sub), ### A.sub, ## Between, ## B.
	require.Len(t, parsed, 4)
	// new section ordered between A's containment and B.
	require.Equal(t, []string{"A", "A.sub", "Between", "B"},
		[]string{parsed[0].Heading, parsed[1].Heading, parsed[2].Heading, parsed[3].Heading})
	// A still owns A.sub textually (containment preserved).
	require.Contains(t, parsed[0].Body, "### A.sub")
	// returned idx (1 = "afterIdx + 1") doesn't have to match
	// the post-parse position when nested children of `afterIdx`
	// shift the slot; callers re-parse for authoritative shape.
	require.GreaterOrEqual(t, idx, 1)
}

func TestInsertSection_AppendsNewlineWhenBodyLacksOne(t *testing.T) {
	body := "## First\nx\n"
	sections := vault.ParseSections(body)
	out, _, err := vault.InsertSection(body, sections, 0, 2, "Second", "body-no-trailing-newline")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(out, "body-no-trailing-newline\n"),
		"function normalizes trailing newline so next heading parses on its own line")
}

func TestInsertSection_RejectsEmptyHeading(t *testing.T) {
	body := "## First\nx\n"
	sections := vault.ParseSections(body)
	_, _, err := vault.InsertSection(body, sections, 0, 2, "", "x\n")
	require.Error(t, err)
}

func TestInsertSection_RejectsOutOfRangeDepth(t *testing.T) {
	body := "## First\nx\n"
	sections := vault.ParseSections(body)
	_, _, err := vault.InsertSection(body, sections, 0, 7, "X", "x\n")
	require.Error(t, err)
}

// --- RenameSectionHeading (#299) -----------------------------------------

func TestRenameSectionHeading_PreservesBodyAndNested(t *testing.T) {
	body := "## Old\nold body\n### nested\nnested body\n## Sibling\nsib body\n"
	sections := vault.ParseSections(body)
	// idx 0 is the parent `## Old`; idx 1 is `### nested`; idx 2 is sibling.
	out, err := vault.RenameSectionHeading(body, sections, 0, "New")
	require.NoError(t, err)
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 3)
	require.Equal(t, "New", parsed[0].Heading)
	require.Equal(t, 2, parsed[0].Depth, "depth preserved")
	// Parent body preserved verbatim, including the nested ### heading.
	require.Contains(t, parsed[0].Body, "old body\n")
	require.Contains(t, parsed[0].Body, "### nested\nnested body\n")
	// Sibling untouched.
	require.Equal(t, "Sibling", parsed[2].Heading)
}

func TestRenameSectionHeading_PreservesDepth(t *testing.T) {
	body := "### Deep\nx\n"
	sections := vault.ParseSections(body)
	out, err := vault.RenameSectionHeading(body, sections, 0, "Renamed")
	require.NoError(t, err)
	parsed := vault.ParseSections(out)
	require.Equal(t, 3, parsed[0].Depth, "depth (`###`) preserved")
	require.Equal(t, "Renamed", parsed[0].Heading)
}

func TestRenameSectionHeading_RejectsPreHeadingSection(t *testing.T) {
	body := "intro\n## Headed\nx\n"
	sections := vault.ParseSections(body)
	_, err := vault.RenameSectionHeading(body, sections, 0, "Anything")
	require.Error(t, err, "pre-heading section has no heading line to rename")
}

func TestRenameSectionHeading_RejectsEmptyHeading(t *testing.T) {
	body := "## First\nx\n"
	sections := vault.ParseSections(body)
	_, err := vault.RenameSectionHeading(body, sections, 0, "")
	require.Error(t, err)
}

func TestRenameSectionHeading_OutOfRange(t *testing.T) {
	body := "## First\nx\n"
	sections := vault.ParseSections(body)
	_, err := vault.RenameSectionHeading(body, sections, 99, "X")
	require.Error(t, err)
}

// --- DeleteSection (#299) ------------------------------------------------

func TestDeleteSection_RemovesHeadingAndBody(t *testing.T) {
	body := "## A\nA body\n## B\nB body\n## C\nC body\n"
	sections := vault.ParseSections(body)
	out, err := vault.DeleteSection(body, sections, 1) // delete B
	require.NoError(t, err)
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 2)
	require.Equal(t, []string{"A", "C"}, []string{parsed[0].Heading, parsed[1].Heading})
}

func TestDeleteSection_RemovesNestedSubtree(t *testing.T) {
	// Deleting `## Parent` must remove its `### Child` per the
	// containment model.
	body := "## Parent\np body\n### Child\nc body\n## Sibling\ns body\n"
	sections := vault.ParseSections(body)
	out, err := vault.DeleteSection(body, sections, 0)
	require.NoError(t, err)
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 1)
	require.Equal(t, "Sibling", parsed[0].Heading)
	require.NotContains(t, out, "### Child", "nested heading deleted with parent")
}

func TestDeleteSection_LeafOnlyLeavesParent(t *testing.T) {
	body := "## Parent\np body\n### Child\nc body\n"
	sections := vault.ParseSections(body)
	out, err := vault.DeleteSection(body, sections, 1) // delete just Child
	require.NoError(t, err)
	parsed := vault.ParseSections(out)
	require.Len(t, parsed, 1)
	require.Equal(t, "Parent", parsed[0].Heading)
	require.Equal(t, "p body\n", parsed[0].Body)
}

func TestDeleteSection_RejectsPreHeadingSection(t *testing.T) {
	body := "intro\n## A\nx\n"
	sections := vault.ParseSections(body)
	_, err := vault.DeleteSection(body, sections, 0)
	require.Error(t, err)
}

func TestDeleteSection_OutOfRange(t *testing.T) {
	body := "## A\nx\n"
	sections := vault.ParseSections(body)
	_, err := vault.DeleteSection(body, sections, 99)
	require.Error(t, err)
}

// --- SectionSlugConflicts (#299) -----------------------------------------

func TestSectionSlugConflicts_DetectsSiblingCollision(t *testing.T) {
	body := "## A\nx\n## B\ny\n"
	sections := vault.ParseSections(body)
	// Renaming B to "A" would slug-collide with the existing A.
	require.True(t, vault.SectionSlugConflicts(sections, 1, 1, "a"))
}

func TestSectionSlugConflicts_AllowsSameSectionRename(t *testing.T) {
	body := "## A\nx\n## B\ny\n"
	sections := vault.ParseSections(body)
	// excludeIdx == at should always return false (renaming a
	// section to its own current slug is a no-op).
	require.False(t, vault.SectionSlugConflicts(sections, 0, 0, "a"))
}

func TestSectionSlugConflicts_AllowsDifferentDepth(t *testing.T) {
	body := "## A\nx\n### Subsection\ny\n"
	sections := vault.ParseSections(body)
	// New `## Subsection` doesn't collide with existing `### Subsection`.
	require.False(t, vault.SectionSlugConflicts(sections, 0, -1, "subsection"))
}
