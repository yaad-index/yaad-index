package vault

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err, "parse time %q", s)
	return tt
}

// fixtureEntity returns a fully-populated v1-schema entity with every
// frontmatter field set. The round-trip tests pin against this so any
// future schema field that lands in entity.go also lands here.
func fixtureEntity(t *testing.T) *Entity {
	t.Helper()
	fetched := mustParseTime(t, "2026-04-12T15:03:11Z")
	filled := mustParseTime(t, "2026-04-13T10:00:00Z")
	return &Entity{
		ID: "wikipedia:martin-wallace",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{
			"title": "Martin Wallace (game designer)",
			"lang": "en",
			"url": "https://en.wikipedia.org/wiki/Martin_Wallace_(game_designer)",
		},
		Provenance: []ProvenanceEntry{
			{Source: "wikipedia:Martin_Wallace_(game_designer)", FetchedAt: &fetched, OK: true},
			{Source: "agent:fill", FilledAt: &filled, OK: true},
		},
		Summary: "British boardgame designer best known for Brass: Birmingham.",
		Tags: []string{"boardgame-designer", "british"},
		Edges: []Edge{
			{Type: "designed", To: "boardgame:brass-birmingham"},
			{Type: "is_about", To: "person:martin-wallace", Metadata: map[string]any{"confidence": "high"}},
		},
		Comments: []Comment{
			// Per the prior design, the on-disk comment table renders date-only
			// (`YYYY-MM-DD`); time-of-day is dropped on round-trip.
			// Fixtures use midnight-UTC so the round-trip equality
			// assertions hold.
			{
				Date: mustParseTime(t, "2026-04-15T00:00:00Z"),
				Text: "Met him at Essen 2024 — friendly, signed my Brass copy.",
				Author: "alice",
			},
			{
				Date: mustParseTime(t, "2026-04-20T00:00:00Z"),
				Text: "Reading the Wikipedia article got me interested in his older A-Z Capitalism design.",
			},
		},
		Gaps: []string{}, // all gaps filled in this fixture
		CleanContent: "Martin Wallace is a British boardgame designer.\nHis works include Brass: Birmingham, Liberté, and A Few Acres of Snow.",
	}
}

func TestMarshal_RoundTrip(t *testing.T) {
	t.Parallel()

	want := fixtureEntity(t)
	b, err := Marshal(want, nil)
	require.NoError(t, err, "Marshal")

	got, err := Unmarshal(b)
	require.NoError(t, err, "Unmarshal")

	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Kind, got.Kind)
	assert.Equal(t, want.Plugin, got.Plugin)
	assert.Equal(t, want.Data, got.Data)
	assert.Equal(t, want.Summary, got.Summary)
	assert.Equal(t, want.Tags, got.Tags)
	assert.Equal(t, want.Edges, got.Edges)

	require.Len(t, got.Provenance, len(want.Provenance))
	for i := range want.Provenance {
		assertProvenanceEqual(t, want.Provenance[i], got.Provenance[i])
	}

	require.Len(t, got.Comments, len(want.Comments))
	for i := range want.Comments {
		assert.True(t, want.Comments[i].Date.Equal(got.Comments[i].Date),
			"comments[%d].date: want %s, got %s", i, want.Comments[i].Date, got.Comments[i].Date)
		assert.Equal(t, want.Comments[i].Text, got.Comments[i].Text, "comments[%d].text", i)
		assert.Equal(t, want.Comments[i].Author, got.Comments[i].Author, "comments[%d].author", i)
	}

	// CleanContent normalizes to a trailing newline on round-trip.
	expectedClean := want.CleanContent
	if !strings.HasSuffix(expectedClean, "\n") {
		expectedClean += "\n"
	}
	assert.Equal(t, expectedClean, got.CleanContent)
}

// TestMarshal_CacheExpiresRoundTrip pins the new yaad-index
// `cache_expires:` frontmatter field: nil / Never / dated values
// all round-trip through Marshal → Unmarshal cleanly.
func TestMarshal_CacheExpiresRoundTrip(t *testing.T) {
	t.Parallel()

	when := time.Date(2027, 5, 3, 8, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		ce *CacheExpires
	}{
		{"absent (nil)", nil},
		{"never sentinel", CacheExpiresNever()},
		{"finite date", CacheExpiresAt(when)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := fixtureEntity(t)
			want.CacheExpires = tc.ce
			b, err := Marshal(want, nil)
			require.NoError(t, err)

			got, err := Unmarshal(b)
			require.NoError(t, err)

			if tc.ce == nil {
				assert.Nil(t, got.CacheExpires, "absent must round-trip as nil")
				return
			}
			require.NotNil(t, got.CacheExpires)
			assert.Equal(t, tc.ce.Never, got.CacheExpires.Never)
			if !tc.ce.Never {
				assert.True(t, tc.ce.Time.Equal(got.CacheExpires.Time),
					"want %s, got %s", tc.ce.Time, got.CacheExpires.Time)
			}
		})
	}
}

// TestCacheExpires_UnmarshalRejectsBadValue rejects malformed
// frontmatter values rather than silently degrading to nil.
func TestCacheExpires_UnmarshalRejectsBadValue(t *testing.T) {
	t.Parallel()
	body := `---
id: test:foo
kind: test
plugin: test
cache_expires: not-a-date-or-never
---
body
`
	_, err := Unmarshal([]byte(body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache_expires")
}

// TestCacheExpires_Expired honors the Never sentinel + nil-receiver
// + before/after-instant boundary.
func TestCacheExpires_Expired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	var nilCE *CacheExpires
	assert.False(t, nilCE.Expired(now), "nil receiver = no opinion = not expired")
	assert.False(t, CacheExpiresNever().Expired(now), "Never sentinel = never expired")
	assert.True(t, CacheExpiresAt(now.Add(-time.Hour)).Expired(now),
		"past expiry must report expired")
	assert.False(t, CacheExpiresAt(now.Add(time.Hour)).Expired(now),
		"future expiry must report not expired")
	// Boundary: equal instant does NOT count as expired (After is strict).
	assert.False(t, CacheExpiresAt(now).Expired(now),
		"equal instant is not 'after now' per Go's time.After")
}

// assertProvenanceEqual compares two provenance entries with time.Equal
// (which honors monotonic-clock-stripped equality from yaml round-trip)
// instead of struct ==. Mirrors the store package's provenance compare.
func assertProvenanceEqual(t *testing.T, want, got ProvenanceEntry) {
	t.Helper()
	assert.Equal(t, want.Source, got.Source)
	assert.Equal(t, want.OK, got.OK)
	assert.Equal(t, want.Error, got.Error)
	assert.Equal(t, want.ErrorMessage, got.ErrorMessage)
	assert.Equal(t, want.FetchedAt == nil, got.FetchedAt == nil, "FetchedAt nilness")
	if want.FetchedAt != nil && got.FetchedAt != nil {
		assert.True(t, want.FetchedAt.Equal(*got.FetchedAt),
			"FetchedAt: want %s, got %s", want.FetchedAt, got.FetchedAt)
	}
	assert.Equal(t, want.FilledAt == nil, got.FilledAt == nil, "FilledAt nilness")
	if want.FilledAt != nil && got.FilledAt != nil {
		assert.True(t, want.FilledAt.Equal(*got.FilledAt),
			"FilledAt: want %s, got %s", want.FilledAt, got.FilledAt)
	}
	// FetchAttachments (per ADR-0014) must round-trip exactly.
	// Order matters — the dispatcher walks input attachments in
	// order so the YAML list ordering is meaningful for the next
	// fetch's (role, uri) string-compare.
	assert.Equal(t, want.FetchAttachments, got.FetchAttachments, "FetchAttachments")
}

// TestMarshal_FetchAttachmentsRoundTrip exercises the ADR-0014
// fetch_attachments YAML round-trip on the provenance row. The
// dispatcher stamps these on the new provenance entry; the next
// ingest reads them back to drive the (role, uri) re-fetch
// comparison — a YAML drift here would silently break re-fetch
// hygiene.
func TestMarshal_FetchAttachmentsRoundTrip(t *testing.T) {
	t.Parallel()

	fetched := mustParseTime(t, "2026-05-06T10:00:00Z")
	want := &Entity{
		ID: "boardgame:130680",
		Kind: "boardgame",
		Plugin: "bgg",
		Data: map[string]any{"name": "Brass: Birmingham"},
		Provenance: []ProvenanceEntry{
			{
				Source: "bgg:130680",
				FetchedAt: &fetched,
				OK: true,
				FetchAttachments: []FetchAttachmentRef{
					{Role: "thumb", URI: "https://cf.geekdo-images.com/.../thumb.jpg"},
					{Role: "cover", URI: "file:///tmp/staging/cover-130680.png"},
				},
			},
		},
	}
	b, err := Marshal(want, nil)
	require.NoError(t, err, "Marshal")
	got, err := Unmarshal(b)
	require.NoError(t, err, "Unmarshal")

	require.Len(t, got.Provenance, 1)
	assertProvenanceEqual(t, want.Provenance[0], got.Provenance[0])

	// And surface check: the YAML must contain the literal
	// `fetch_attachments:` key — defense against a silent
	// `omitempty` regression that drops the field on marshal.
	if !strings.Contains(string(b), "fetch_attachments:") {
		t.Errorf("marshaled YAML missing `fetch_attachments:` key:\n%s", b)
	}
}

func TestMarshal_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut func(*Entity)
		field string
	}{
		{"missing id", func(e *Entity) { e.ID = "" }, "id"},
		{"missing kind", func(e *Entity) { e.Kind = "" }, "kind"},
		{"missing plugin", func(e *Entity) { e.Plugin = "" }, "plugin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := fixtureEntity(t)
			tc.mut(e)
			_, err := Marshal(e, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMissingRequiredField)
			assert.Contains(t, err.Error(), tc.field)
		})
	}
}

func TestUnmarshal_RejectsMalformedFrontmatter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw string
	}{
		{"empty", ""},
		{"no leading delim", "id: foo\n---\nbody"},
		{"unterminated", "---\nid: foo\nkind: bar\nplugin: baz\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Unmarshal([]byte(tc.raw))
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrMalformedFrontmatter),
				"want ErrMalformedFrontmatter, got %v", err)
		})
	}
}

func TestUnmarshal_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()

	const raw = "---\nid: wikipedia:foo\nkind: wikipedia-article\n---\n\n"
	_, err := Unmarshal([]byte(raw))
	require.Error(t, err, "missing plugin field should reject")
	assert.ErrorIs(t, err, ErrMissingRequiredField)
}

// TestUnmarshal_AbsentEdgesBlockDecodesNil pins the back-compat
// promise from: a vault file with no `edges:` block in
// frontmatter (legacy legacy shape) decodes cleanly with
// Edges == nil — NOT a parse error. Reindex on those entities
// derives no edges and they pass through clean.
func TestUnmarshal_AbsentEdgesBlockDecodesNil(t *testing.T) {
	t.Parallel()

	const raw = "---\nid: bgg:legacy\nkind: bgg\nplugin: bgg\n---\n\n"
	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err, "absent edges block must not fail to decode")
	assert.Nil(t, got.Edges, "Edges field is nil when frontmatter omits the block")
}

// TestUnmarshal_BodyEdgesMergedIntoFrontmatter exercises the read-side
// merge contract: a wikilink in the `## Edges` body section that isn't
// in the frontmatter shows up on the parsed Entity. Documents the
// hand-edit recovery path: a user writes a wikilink in Obsidian, and
// the next vault-aware reader sees it.
func TestUnmarshal_BodyEdgesMergedIntoFrontmatter(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"edges:",
		"  - type: designed",
		"    to: boardgame:brass",
		"---",
		"",
		"Body content.",
		"",
		"## Edges",
		"",
		"- [[boardgame:brass]] (designed)",
		"- [[person:martin-wallace]] (is_about)",
		"- [[city:tehran]]",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)

	require.Len(t, got.Edges, 3, "frontmatter+body merge")
	assert.Equal(t, Edge{Type: "designed", To: "boardgame:brass"}, got.Edges[0],
		"edges[0]: from frontmatter — body `[[boardgame:brass]] (designed)` deduped by `to`")
	assert.Equal(t, Edge{Type: "is_about", To: "person:martin-wallace"}, got.Edges[1],
		"edges[1]: body-only with explicit type")
	assert.Equal(t, Edge{Type: DefaultBodyEdgeType, To: "city:tehran"}, got.Edges[2],
		"edges[2]: body-only untyped → default `mentions` type per a prior PR dedup rule")
}

// TestUnmarshal_FrontmatterEdgeWinsOverBodyDifferentType pins the cold-reviewer's
// a prior PR review case: a typed frontmatter edge plus a body wikilink
// to the same target collapse to one row keyed on `to`. The
// frontmatter type wins; the body wikilink is dropped, even when its
// type annotation differs (e.g. body `(mentions)` while frontmatter
// has `(designed)`).
func TestUnmarshal_FrontmatterEdgeWinsOverBodyDifferentType(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"edges:",
		"  - type: designed",
		"    to: boardgame:brass",
		"---",
		"",
		"## Edges",
		"",
		"- [[boardgame:brass]] (mentions)",
		"- [[boardgame:brass]]",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)

	require.Len(t, got.Edges, 1, "dedup by `to`: frontmatter wins on collision")
	assert.Equal(t, Edge{Type: "designed", To: "boardgame:brass"}, got.Edges[0],
		"frontmatter edge survives; body annotations discarded")
}

// TestUnmarshal_BodyOnlyUntypedWikilinkAssignsDefaultType locks the
// post-merge invariant: no edge lands with an empty Type. Body
// wikilinks without a `(type)` annotation get DefaultBodyEdgeType.
func TestUnmarshal_BodyOnlyUntypedWikilinkAssignsDefaultType(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"---",
		"",
		"## Edges",
		"",
		"- [[city:tehran]]",
		"- [[person:somebody]]",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)

	require.Len(t, got.Edges, 2)
	for i, e := range got.Edges {
		assert.NotEmpty(t, e.Type, "edges[%d].type: must not be empty post-merge", i)
		assert.Equal(t, DefaultBodyEdgeType, e.Type, "edges[%d].type: default for body-only", i)
	}
}

// TestUnmarshal_CommentsFromBodyTable pins the post- contract:
// comments live in the body `## Comments` table only — frontmatter
// no longer carries them. The table parser reads alternating
// heading/body rows.
func TestUnmarshal_CommentsFromBodyTable(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"comment_count: 2",
		"---",
		"",
		"## Comments",
		"",
		"| Comments |",
		"|----------|",
		"| 2026-04-15 — alice |",
		"| First comment from the body table. |",
		"| 2026-04-16 — operator |",
		"| Second comment, also a body row. |",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)

	require.Len(t, got.Comments, 2)
	assert.Equal(t, "alice", got.Comments[0].Author)
	assert.Equal(t, "First comment from the body table.", got.Comments[0].Text)
	assert.Equal(t, "2026-04-15", got.Comments[0].Date.UTC().Format("2006-01-02"))
	assert.Equal(t, "operator", got.Comments[1].Author)
	assert.Equal(t, "Second comment, also a body row.", got.Comments[1].Text)
	assert.Equal(t, "2026-04-16", got.Comments[1].Date.UTC().Format("2006-01-02"))
}

// TestUnmarshal_CommentsWithOperator pins the yaad-index a prior PR
// extension: heading rows of the form `<date> — <author> @ <operator>`
// parse the operator into Comment.Operator. Backward compat: the
// legacy form (`<date> — <author>`) leaves Operator empty, so legacy
// vault files round-trip unchanged.
func TestUnmarshal_CommentsWithOperator(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"---",
		"id: wikipedia:foo",
		"kind: wikipedia-article",
		"plugin: wikipedia",
		"comment_count: 3",
		"---",
		"",
		"## Comments",
		"",
		"| Comments |",
		"|----------|",
		"| 2026-04-15 — bob @ alice |",
		"| Comment with full pair-claim attribution. |",
		"| 2026-04-16 — alice2 |",
		"| Legacy comment — author only, no operator. |",
		"| 2026-04-17 |",
		"| Anonymous comment — date only. |",
		"",
	}, "\n")

	got, err := Unmarshal([]byte(raw))
	require.NoError(t, err)
	require.Len(t, got.Comments, 3)

	// Pair-claim shape: agent + operator both populated.
	assert.Equal(t, "bob", got.Comments[0].Author)
	assert.Equal(t, "alice", got.Comments[0].Operator)
	assert.Equal(t, "Comment with full pair-claim attribution.", got.Comments[0].Text)

	// Legacy: author-only. Operator stays empty (no invention).
	assert.Equal(t, "alice2", got.Comments[1].Author)
	assert.Empty(t, got.Comments[1].Operator,
		"legacy comment must round-trip with empty Operator")

	// Date-only: both author + operator stay empty.
	assert.Empty(t, got.Comments[2].Author)
	assert.Empty(t, got.Comments[2].Operator)
}

// TestMarshal_CommentRenderingShape pins the wire shape of the
// rendered comments table for a prior PR:
//
// - Operator non-empty → heading reads `<date> — <author> @ <operator>`.
// - Operator empty (legacy) → heading reads `<date> — <author>` (unchanged).
// - Author empty → heading reads `<date>` (no operator suffix without an
// author — that combination is a parse anomaly, never produced here).
func TestMarshal_CommentRenderingShape(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:render-comments",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date: time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC),
				Text: "first — pair claim",
				Author: "bob",
				Operator: "alice",
			},
			{
				Date: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
				Text: "second — author only (legacy)",
				Author: "alice2",
			},
			{
				Date: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
				Text: "third — date only",
			},
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	assert.Contains(t, out, "| 2026-05-05 — bob @ alice |",
		"pair-claim heading row must include `@ <operator>` suffix")
	assert.Contains(t, out, "| 2026-04-01 — alice2 |",
		"legacy author-only heading row must omit `@` (backward compat)")
	assert.Contains(t, out, "| 2026-03-01 |",
		"date-only heading row stays unchanged")
}

// TestRoundTrip_CommentsWithOperator: Marshal → Unmarshal preserves
// Author + Operator on every comment. The full content-hash invariant
// (text, date, author, operator) survives the body table format.
func TestRoundTrip_CommentsWithOperator(t *testing.T) {
	t.Parallel()

	in := &Entity{
		ID: "wikipedia:roundtrip",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Comments: []Comment{
			{
				Date: time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC),
				Text: "with operator",
				Author: "bob",
				Operator: "alice",
			},
			{
				Date: time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC),
				Text: "without operator",
				Author: "alice2",
			},
		},
	}
	b, err := Marshal(in, nil)
	require.NoError(t, err)
	out, err := Unmarshal(b)
	require.NoError(t, err)

	require.Len(t, out.Comments, 2)
	assert.Equal(t, "bob", out.Comments[0].Author)
	assert.Equal(t, "alice", out.Comments[0].Operator)
	assert.Equal(t, "with operator", out.Comments[0].Text)
	assert.Equal(t, "alice2", out.Comments[1].Author)
	assert.Empty(t, out.Comments[1].Operator)
	assert.Equal(t, "without operator", out.Comments[1].Text)
}

// TestMarshal_BodySectionsRegeneratedFromFrontmatter is the inverse
// contract: hand-edits to `## Edges` / `## Comments` that haven't been
// merged back into the entity's frontmatter via a prior Unmarshal are
// LOST on the next Marshal. Locks the documented "frontmatter is
// canonical on write" rule from the package comment, so a future
// refactor doesn't accidentally start preserving body shape.
func TestMarshal_BodySectionsRegeneratedFromFrontmatter(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:x",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		// No Edges, no Comments in frontmatter — even though a real
		// vault file might have body-section content, Marshal never
		// sees it.
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)

	out := string(b)
	assert.NotContains(t, out, "## Edges",
		"empty Edges should not produce a `## Edges` section")
	assert.NotContains(t, out, "## Comments",
		"empty Comments should not produce a `## Comments` section")
}

// TestMarshal_HandEditWithoutReindexLoses pins alice2's recommended
// behavior assertion: a writer firing on an entity that hasn't first
// been re-read (so body hand-edits aren't in the in-memory entity)
// overwrites those body hand-edits. This is the v1 staleness window
// ADR-0008 explicitly accepted (deferred file watcher).
func TestMarshal_HandEditWithoutReindexLoses(t *testing.T) {
	t.Parallel()

	// Step 1: serialize an entity with a body section.
	original := &Entity{
		ID: "wikipedia:x",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Edges: []Edge{
			{Type: "designed", To: "boardgame:brass"},
		},
	}
	first, err := Marshal(original, nil)
	require.NoError(t, err)
	assert.Contains(t, string(first), "[[boardgame:brass]]",
		"first marshal: body should contain the edge wikilink")

	// Step 2: simulate a hand-edit by appending a raw body wikilink to
	// the file content (without re-reading via Unmarshal).
	withHandEdit := append([]byte{}, first...)
	withHandEdit = append(withHandEdit, []byte("- [[city:tehran]]\n")...)

	// Step 3: a writer that re-marshals the in-memory `original` (which
	// never saw the hand-edit) produces output that has only the
	// frontmatter-known edge. The hand-edit is lost.
	second, err := Marshal(original, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(second), "city:tehran",
		"second marshal: hand-edit not in in-memory entity → lost on rewrite")
	assert.Contains(t, string(second), "[[boardgame:brass]]",
		"second marshal: frontmatter edge survives")

	// Step 4: the recovery path is via Unmarshal — re-reading the
	// hand-edited file picks up the wikilink, so a subsequent Marshal
	// on the read-back entity DOES preserve it.
	recovered, err := Unmarshal(withHandEdit)
	require.NoError(t, err)
	third, err := Marshal(recovered, nil)
	require.NoError(t, err)
	assert.Contains(t, string(third), "[[city:tehran]]",
		"third marshal: hand-edit recovered via Unmarshal+Marshal")
}

// TestMarshal_AttachmentsRoundTrip exercises the ADR-0018 step 6
// attachment-manifest YAML round-trip. The manifest lives on the
// entity frontmatter, separate from per-provenance fetch_attachments
// (which records what was fetched, not what's currently attached).
func TestMarshal_AttachmentsRoundTrip(t *testing.T) {
	t.Parallel()

	want := &Entity{
		ID: "boardgame:brass-birmingham-2018",
		Kind: "boardgame",
		Plugin: "bgg",
		Data: map[string]any{"name": "Brass: Birmingham"},
		Attachments: []Attachment{
			{
				Name: "thumbnail.jpg",
				Kind: "image/jpeg",
				Path: "attachments/thumbnail.jpg",
				Bytes: 12453,
			},
			{
				Name: "rules.pdf",
				Path: "attachments/rules.pdf",
			},
		},
	}
	b, err := Marshal(want, nil)
	require.NoError(t, err, "Marshal")
	got, err := Unmarshal(b)
	require.NoError(t, err, "Unmarshal")

	require.Len(t, got.Attachments, 2)
	assert.Equal(t, "thumbnail.jpg", got.Attachments[0].Name)
	assert.Equal(t, "image/jpeg", got.Attachments[0].Kind)
	assert.Equal(t, "attachments/thumbnail.jpg", got.Attachments[0].Path)
	assert.Equal(t, int64(12453), got.Attachments[0].Bytes)
	// Optional fields (Kind, Bytes) on the second entry round-trip
	// as zero values (omitempty on write, absent on parse).
	assert.Equal(t, "rules.pdf", got.Attachments[1].Name)
	assert.Equal(t, "", got.Attachments[1].Kind)
	assert.Equal(t, "attachments/rules.pdf", got.Attachments[1].Path)
	assert.Equal(t, int64(0), got.Attachments[1].Bytes)

	// Surface check: the YAML must contain the literal `attachments:`
	// key — defense against a silent omitempty regression on a
	// non-empty manifest.
	if !strings.Contains(string(b), "attachments:") {
		t.Errorf("marshaled YAML missing `attachments:` key:\n%s", b)
	}
}

// TestMarshal_AttachmentsEmpty_OmitsKey: an entity with no
// attachments must not surface an empty `attachments: []` artifact
// in the frontmatter — agents grepping the vault for attachment
// presence should get a clean negative.
func TestMarshal_AttachmentsEmpty_OmitsKey(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "boardgame:no-attachments-2024",
		Kind: "boardgame",
		Plugin: "bgg",
		Data: map[string]any{"name": "Plain Game"},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	if strings.Contains(string(b), "attachments:") {
		t.Errorf("empty manifest must omit `attachments:` key; got:\n%s", b)
	}

	// And the inverse: nil-vs-empty-slice both omit cleanly.
	e.Attachments = []Attachment{}
	b, err = Marshal(e, nil)
	require.NoError(t, err)
	if strings.Contains(string(b), "attachments:") {
		t.Errorf("empty-slice manifest must also omit; got:\n%s", b)
	}
}
