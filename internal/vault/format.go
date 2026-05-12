package vault

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrMalformedFrontmatter is returned by the reader when the file does
// not begin with a `---`-delimited YAML frontmatter block. The reader
// requires frontmatter; a vault file without it is not a yaad-index
// entity and is rejected at the boundary.
var ErrMalformedFrontmatter = errors.New("malformed frontmatter")

// ErrMissingRequiredField is returned by the reader/validator when a
// frontmatter field required by the v1 schema (id, kind, plugin) is
// absent or empty. The writer validates the same set before serializing.
var ErrMissingRequiredField = errors.New("missing required field")

// frontmatter is the on-disk YAML shape, separate from Entity so the
// yaml struct tags don't bleed into the public Entity API. Field order
// here drives the marshaled key order (yaml.v3 honors struct order).
type frontmatter struct {
	ID string `yaml:"id"`
	Kind string `yaml:"kind"`
	Plugin string `yaml:"plugin"`
	Aliases []string `yaml:"aliases,omitempty"`
	Notations []string `yaml:"notations,omitempty"`
	Data map[string]any `yaml:"data,omitempty"`
	Provenance []ProvenanceEntry `yaml:"provenance,omitempty"`
	Summary string `yaml:"summary,omitempty"`
	Tags []string `yaml:"tags,omitempty"`
	Edges []Edge `yaml:"edges,omitempty"`
	CommentCount int `yaml:"comment_count,omitempty"`
	Gaps []string `yaml:"gaps,omitempty"`
	CacheExpires *CacheExpires `yaml:"cache_expires,omitempty"`
	Attachments []Attachment `yaml:"attachments,omitempty"`
	GapState map[string]GapStateEntry `yaml:"gap_state,omitempty"`
}

// Marshal serializes an Entity to its on-disk markdown representation:
// `---`-delimited YAML frontmatter, then a blank line, then the body
// (clean_content + regenerated `## Edges` + `## Comments` sections).
//
// Required fields (id, kind, plugin) are validated; missing → error.
// The body sections are deterministic in their order (Edges first, then
// Comments) and stable across repeated writes of the same entity.
//
// canonicalKinds names the operator-enabled canonical kinds (per
// ADR-0008's CanonicalGuard). When the entity's Kind is in this set,
// the synthesized alias (per ADR-0011) is sourced from `data.name`;
// otherwise from `data.title`. Empty/nil set falls back to data.title
// only — useful for tests and source-shape-only deployments.
//
// Per yaad-index the source issue a prior PR, plugin-emitted aliases on
// `e.Aliases` merge with the title-synthesized one — synthesized
// first (deterministic), plugin entries appended in input order,
// duplicates dropped. Empty plugin set leaves only the synthesized
// alias on the frontmatter (legacy behavior preserved).
func Marshal(e *Entity, canonicalKinds []string) ([]byte, error) {
	if err := validateRequired(e); err != nil {
		return nil, err
	}

	aliases := mergeAliases(synthesizeAliases(e, canonicalKinds), e.Aliases)

	fm := frontmatter{
		ID: e.ID,
		Kind: e.Kind,
		Plugin: e.Plugin,
		Aliases: aliases,
		Notations: e.Notations,
		Data: e.Data,
		Provenance: e.Provenance,
		Summary: e.Summary,
		Tags: e.Tags,
		Edges: e.Edges,
		CommentCount: len(e.Comments),
		Gaps: e.Gaps,
		CacheExpires: e.CacheExpires,
		Attachments: e.Attachments,
		GapState: e.GapState,
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&fm); err != nil {
		return nil, fmt.Errorf("encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	buf.WriteString("---\n\n")

	if e.CleanContent != "" {
		buf.WriteString(e.CleanContent)
		if !strings.HasSuffix(e.CleanContent, "\n") {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}

	writeEdgesSection(&buf, e.Edges)
	writeCommentsSection(&buf, e.Comments)

	return buf.Bytes(), nil
}

// Unmarshal parses a vault file's bytes back into an Entity. Frontmatter
// is the authoritative source for every field; body sections (`## Edges`
// wikilinks, `## Comments` dated blocks) are parsed and merged on top —
// any wikilink or dated block found in the body that isn't already
// represented in frontmatter is added to the returned Entity. This is
// the read-side compensation for hand-edits per the package contract.
//
// Required fields (id, kind, plugin) are validated; missing → error.
func Unmarshal(b []byte) (*Entity, error) {
	fmBlock, body, err := splitFrontmatter(b)
	if err != nil {
		return nil, err
	}

	var fm frontmatter
	if err := yaml.Unmarshal(fmBlock, &fm); err != nil {
		return nil, fmt.Errorf("decode frontmatter: %w", err)
	}

	e := &Entity{
		ID: fm.ID,
		Kind: fm.Kind,
		Plugin: fm.Plugin,
		Aliases: fm.Aliases,
		Notations: fm.Notations,
		Data: fm.Data,
		Provenance: fm.Provenance,
		Summary: fm.Summary,
		Tags: fm.Tags,
		Edges: fm.Edges,
		Gaps: fm.Gaps,
		CacheExpires: fm.CacheExpires,
		Attachments: fm.Attachments,
		GapState: fm.GapState,
	}
	if err := validateRequired(e); err != nil {
		return nil, err
	}

	cleanContent, bodyEdges, bodyComments := splitBody(body)
	e.CleanContent = cleanContent
	e.Edges = mergeEdges(e.Edges, bodyEdges)
	// Comments live in the body `## Comments` section only — the
	// frontmatter `comment_count` is informational + queryable, the
	// body is the source of truth .
	e.Comments = bodyComments

	return e, nil
}

// synthesizeAliases derives the `aliases:` frontmatter list from the
// entity's title (per ADR-0011). Source-shape entities (Kind not in
// the operator's canonical_kinds set) use `data.title`; canonical-
// shape entities use `data.name`. Returns nil when:
// - The candidate title field is missing or empty.
// - The trimmed title equals the slug portion of the entity ID
// (no signal in writing aliases that match the filename anyway).
//
// Single-element list in v1; multi-alias support deferred per the
// ADR's out-of-scope.
func synthesizeAliases(e *Entity, canonicalKinds []string) []string {
	field := "title"
	for _, k := range canonicalKinds {
		if k == e.Kind {
			field = "name"
			break
		}
	}
	raw, ok := e.Data[field]
	if !ok {
		return nil
	}
	candidate, ok := raw.(string)
	if !ok {
		return nil
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return nil
	}
	slug, err := slugFromID(e.ID)
	if err == nil && candidate == slug {
		return nil
	}
	return []string{candidate}
}

// mergeAliases interleaves the ADR-0011 title-synthesized alias
// list (`synth`, typically zero or one element in v1) with the
// plugin-emitted aliases from `e.Aliases` (per yaad-index issue
// a prior PR). Synthesized entries land first for deterministic
// ordering; duplicate strings (case-sensitive exact match) are
// dropped. nil/empty inputs yield nil — the frontmatter then
// drops the field via omitempty.
func mergeAliases(synth, fromPlugin []string) []string {
	if len(synth) == 0 && len(fromPlugin) == 0 {
		return nil
	}
	out := make([]string, 0, len(synth)+len(fromPlugin))
	seen := make(map[string]struct{}, len(synth)+len(fromPlugin))
	for _, a := range synth {
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	for _, a := range fromPlugin {
		if a == "" {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validateRequired(e *Entity) error {
	switch {
	case e.ID == "":
		return fmt.Errorf("%w: id", ErrMissingRequiredField)
	case e.Kind == "":
		return fmt.Errorf("%w: kind", ErrMissingRequiredField)
	case e.Plugin == "":
		return fmt.Errorf("%w: plugin", ErrMissingRequiredField)
	}
	return nil
}

// splitFrontmatter peels the leading `---`-delimited YAML block off the
// input. The format requires the file to begin with `---\n`, contain
// exactly one closing `---` line, and have body content (possibly empty)
// after.
func splitFrontmatter(b []byte) (frontmatterBytes, body []byte, err error) {
	const delim = "---"
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		return nil, nil, fmt.Errorf("%w: empty file", ErrMalformedFrontmatter)
	}
	if strings.TrimRight(scanner.Text(), " \t") != delim {
		return nil, nil, fmt.Errorf("%w: file must begin with `---`", ErrMalformedFrontmatter)
	}

	var fmBuf bytes.Buffer
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimRight(line, " \t") == delim {
			closed = true
			break
		}
		fmBuf.WriteString(line)
		fmBuf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan frontmatter: %w", err)
	}
	if !closed {
		return nil, nil, fmt.Errorf("%w: unterminated frontmatter (no closing `---`)", ErrMalformedFrontmatter)
	}

	var bodyBuf bytes.Buffer
	for scanner.Scan() {
		bodyBuf.WriteString(scanner.Text())
		bodyBuf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan body: %w", err)
	}

	return fmBuf.Bytes(), bodyBuf.Bytes(), nil
}

// edgeBodyLine matches a single body-section edge line, e.g.
// `- [[entity:id]] (type)` or `- [[entity:id]]`. The wikilink target is
// captured in group 1; the optional type annotation in group 2.
var edgeBodyLine = regexp.MustCompile(`^-\s*\[\[([^\]]+)\]\](?:\s*\(([^)]+)\))?\s*$`)

// commentTableRow matches a single-column markdown-table row: a line
// shaped like `| <content> |` (leading/trailing pipes, anything
// between). The captured content is the raw cell text — pipe-escapes
// (`\|`) and `<br>` paragraph markers are decoded by parseCommentRow.
var commentTableRow = regexp.MustCompile(`^\|\s*(.*?)\s*\|\s*$`)

// commentTableSeparator matches the markdown-table separator row
// (`|---|`, with any number of dashes, and optional spaces). One
// occurrence sits between the header and the first comment row.
var commentTableSeparator = regexp.MustCompile(`^\|\s*-+\s*\|\s*$`)

// commentHeadingRow extracts `<date>`, `<date> — <author>`, or
// `<date> — <author> @ <operator>` from a comment-table heading row's
// cell content. Date is a single non-whitespace token (RFC3339 or
// YYYY-MM-DD; both are dash-only safe). Operator is optional and
// trailing — pre-yaad-index vault files end at the author and
// the parser must round-trip those without inventing an operator.
//
// Group 1: date. Group 2: author (may be empty when only date present;
// `[^@]+?` so the `@ <operator>` separator does not leak into the
// author capture). Group 3: operator (empty when not present).
var commentHeadingRow = regexp.MustCompile(`^(\S+)(?:\s+(?:—|-)\s+([^@]+?)(?:\s+@\s+(.+))?)?$`)

// splitBody walks the body bytes and extracts (clean_content, edges,
// comments). Lines before the first `## Edges` or `## Comments` heading
// are clean_content; subsequent sections are parsed for their content.
// Unknown `## ` headings are tolerated and folded into clean_content
// preceding the known headings.
//
// Per the prior design, the `## Comments` section is rendered as a single-column
// markdown table with alternating heading/body rows:
//
//	| Comments |
//	|----------|
//	| 2026-05-03 — operator |
//	| First comment text. |
//	| 2026-05-03 — yaad |
//	| Second comment, may use <br><br> for paragraph breaks. |
//
// The first non-separator row is the table's column header (`Comments`)
// and is skipped. Subsequent rows alternate between heading
// (`<date> — <author>`) and body (raw text, with `<br><br>` decoded
// back to `\n\n` and `\|` decoded back to `|`).
func splitBody(b []byte) (cleanContent string, edges []Edge, comments []Comment) {
	var (
		section = "clean"
		clean bytes.Buffer
		edgesB []Edge
		commsB []Comment

		// comment-table accumulator
		commentTableHeaderSeen bool // ate the `| Comments |` row
		commentTableSepSeen bool // ate the `|---|` row
		commentExpectHeading bool // next row is a heading row
		curDate time.Time
		curAuthor string
		curOperator string
	)

	// flushOrphanedHeading recovers from the edge case where the
	// parser read a heading row but the comments section ended
	// before the body row landed (truncated hand-edit, ad-hoc paste,
	// concurrent-write surface). Without the flush, the captured
	// date + author would be silently dropped (the cold-reviewer's catch).
	// Empty Text on the appended Comment is a clear "this comment
	// was authored mid-edit" signal that survives the parse — better
	// than silent loss; consistent with the stated contract that
	// hand-edits don't break round-trip.
	flushOrphanedHeading := func() {
		// commentExpectHeading == true means we're either at section
		// start (no heading consumed) or the last row read was a body
		// row (heading + body already paired and appended). false =
		// "heading read, body still pending."
		if commentExpectHeading {
			return
		}
		commsB = append(commsB, Comment{
			Date: curDate,
			Author: curAuthor,
			Operator: curOperator,
			Text: "",
		})
		curDate = time.Time{}
		curAuthor = ""
		curOperator = ""
		commentExpectHeading = true
	}

	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimRight(line, " \t")
		switch {
		case trimmed == "## Edges":
			if section == "comments" {
				flushOrphanedHeading()
			}
			section = "edges"
			continue
		case trimmed == "## Comments":
			if section == "comments" {
				flushOrphanedHeading()
			}
			section = "comments"
			commentTableHeaderSeen = false
			commentTableSepSeen = false
			commentExpectHeading = true
			continue
		case strings.HasPrefix(line, "## "):
			// Unknown section heading. Treat it as part of clean_content
			// — preserves user-authored body shape that doesn't match
			// the canonical sections. (Once we leave a known section
			// for an unknown one, any further content past this point
			// is body again until another known heading appears.)
			if section == "comments" {
				flushOrphanedHeading()
			}
			if section != "clean" {
				section = "clean"
			}
		}

		switch section {
		case "clean":
			clean.WriteString(line)
			clean.WriteByte('\n')
		case "edges":
			if m := edgeBodyLine.FindStringSubmatch(line); m != nil {
				edgesB = append(edgesB, Edge{
					Type: strings.TrimSpace(m[2]),
					To: strings.TrimSpace(m[1]),
				})
			}
		case "comments":
			if commentTableSeparator.MatchString(strings.TrimSpace(line)) {
				commentTableSepSeen = true
				continue
			}
			m := commentTableRow.FindStringSubmatch(line)
			if m == nil {
				// Blank line / decorative content — ignore inside the
				// comments section. Mid-table blank lines aren't
				// produced by Marshal but tolerate them on parse so
				// hand-edits don't break round-trip.
				continue
			}
			cell := decodeCommentCell(m[1])
			if !commentTableHeaderSeen {
				// First row is the column header (`| Comments |`).
				// Don't validate the literal `Comments` text — design-
				// in-flux means the column name may move; what matters
				// is "first row is header, rest is content."
				commentTableHeaderSeen = true
				continue
			}
			// Defensive: if the separator never showed up but rows are
			// flowing, treat it as seen. Markdown renderers tolerate a
			// missing separator inconsistently.
			if !commentTableSepSeen {
				commentTableSepSeen = true
			}
			if commentExpectHeading {
				if hm := commentHeadingRow.FindStringSubmatch(cell); hm != nil {
					curDate = parseCommentDate(hm[1])
					curAuthor = strings.TrimSpace(hm[2])
					curOperator = strings.TrimSpace(hm[3])
				} else {
					// Malformed heading row — keep the cell as the
					// date string so a hand-edit doesn't silently swallow
					// it; author + operator stay empty.
					curDate = parseCommentDate(cell)
					curAuthor = ""
					curOperator = ""
				}
				commentExpectHeading = false
				continue
			}
			// Body row — pair with the buffered heading.
			commsB = append(commsB, Comment{
				Date: curDate,
				Author: curAuthor,
				Operator: curOperator,
				Text: cell,
			})
			curDate = time.Time{}
			curAuthor = ""
			curOperator = ""
			commentExpectHeading = true
		}
	}

	// End-of-input: flush a trailing orphaned heading row when the
	// section ended without a paired body row.
	if section == "comments" {
		flushOrphanedHeading()
	}

	// Trim leading blank lines (the gap between frontmatter and body)
	// and trailing blank lines (gap before the first known section),
	// then re-add a single trailing newline so the output is canonical
	// regardless of how the caller spaced their input.
	trimmed := strings.Trim(clean.String(), "\n")
	if trimmed == "" {
		return "", edgesB, commsB
	}
	return trimmed + "\n", edgesB, commsB
}

// parseCommentDate accepts both RFC3339 timestamps (legacy from the
// legacy dated-block format) and bare YYYY-MM-DD dates (the new
// table-row shape). Returns the zero time on any parse failure —
// Marshal will then emit `0001-01-01` on the next round, which is a
// loud signal to the operator that the date didn't survive.
func parseCommentDate(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	return time.Time{}
}

// decodeCommentCell reverses encodeCommentCell — turns `<br><br>` back
// into paragraph breaks (`\n\n`), single `<br>` into `\n`, and `\|`
// back into a literal pipe. The order matters: `<br><br>` before `<br>`
// so paragraph breaks aren't double-decoded.
func decodeCommentCell(s string) string {
	s = strings.ReplaceAll(s, "<br><br>", "\n\n")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, `\|`, "|")
	return s
}

// encodeCommentCell escapes pipe characters and substitutes line
// breaks with `<br>` so the cell stays on one markdown-table row.
// Paragraph boundaries (`\n\n`) become `<br><br>`; intra-paragraph
// newlines become a single `<br>`.
func encodeCommentCell(s string) string {
	s = strings.ReplaceAll(s, `|`, `\|`)
	// Normalize paragraph breaks first so the single-newline pass
	// doesn't see the doubled newlines as two `<br>`s.
	s = strings.ReplaceAll(s, "\n\n", "<br><br>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}

func writeEdgesSection(w *bytes.Buffer, edges []Edge) {
	if len(edges) == 0 {
		return
	}
	w.WriteString("## Edges\n\n")
	for _, e := range edges {
		w.WriteString("- [[")
		w.WriteString(e.To)
		w.WriteString("]]")
		if e.Type != "" {
			w.WriteString(" (")
			w.WriteString(e.Type)
			w.WriteString(")")
		}
		w.WriteByte('\n')
	}
	w.WriteByte('\n')
}

// writeCommentsSection renders the entity's comments as a single-
// column markdown table Per the prior design,. Each comment becomes two rows: a
// heading (`<date> — <author>`) and a body (the text, with paragraph
// breaks encoded as `<br><br>` and pipe chars escaped as `\|`).
//
// The frontmatter `comment_count` field carries the count; the body
// table is the source of truth.
func writeCommentsSection(w *bytes.Buffer, comments []Comment) {
	if len(comments) == 0 {
		return
	}
	w.WriteString("## Comments\n\n")
	w.WriteString("| Comments |\n")
	w.WriteString("|----------|\n")
	for _, c := range comments {
		// Heading row: `| <date> — <author> @ <operator> |`. Date format
		// is YYYY-MM-DD (not RFC3339) — the table is operator-readable
		// shorthand; the underlying time.Time keeps the precision the
		// API layer needs. Operator suffix is omitted when empty so
		// pre-yaad-index comments render unchanged (backward-
		// compat); without an author, the operator suffix is also
		// omitted (operator-without-agent is a parse anomaly).
		w.WriteString("| ")
		w.WriteString(c.Date.UTC().Format("2006-01-02"))
		if c.Author != "" {
			w.WriteString(" — ")
			w.WriteString(c.Author)
			if c.Operator != "" {
				w.WriteString(" @ ")
				w.WriteString(c.Operator)
			}
		}
		w.WriteString(" |\n")
		// Body row: encoded text in a single cell.
		w.WriteString("| ")
		w.WriteString(encodeCommentCell(strings.TrimRight(c.Text, "\n")))
		w.WriteString(" |\n")
	}
	w.WriteByte('\n')
}

// DefaultBodyEdgeType is assigned to a body wikilink that does not
// carry an explicit type annotation (`- [[entity:id]]` with no
// `(type)` suffix). The untyped body form means "this entity is
// referenced from here" without a stronger semantic; `mentions` is
// the canonical name for that shape — chosen during a prior PR over
// "drop untyped" because dropping would lose information that
// survives the frontmatter→body→frontmatter round trip in a prior PR's
// design.
const DefaultBodyEdgeType = "mentions"

// mergeEdges returns the union of frontmatter edges and body-parsed
// edges. Dedup is by `to` alone — if a body wikilink targets an
// entity that frontmatter already names, the frontmatter edge wins
// and the body wikilink is dropped (regardless of which typed-edge
// relation the frontmatter used). Body wikilinks that are NOT
// covered by a frontmatter edge land as new entries; an empty
// Edge.Type from the parser is rewritten to DefaultBodyEdgeType so
// downstream consumers (reindex → DB rows) never see a typeless
// edge.
//
// This dedup-by-`to` + default-type rule resolves the cold-reviewer's a prior PR
// review note: the previous (type, to) dedup let a typed frontmatter
// edge `(designed, brass)` plus an untyped body wikilink `[[brass]]`
// produce two distinct entries (`(designed, brass)` and
// `("", brass)`), which propagated into the DB as bogus duplicate
// edges.
func mergeEdges(fm, body []Edge) []Edge {
	if len(body) == 0 {
		return fm
	}
	covered := make(map[string]struct{}, len(fm))
	for _, e := range fm {
		covered[e.To] = struct{}{}
	}
	out := append([]Edge(nil), fm...)
	for _, e := range body {
		if _, dup := covered[e.To]; dup {
			continue
		}
		covered[e.To] = struct{}{}
		if e.Type == "" {
			e.Type = DefaultBodyEdgeType
		}
		out = append(out, e)
	}
	return out
}
