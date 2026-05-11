// Section parser for UGC bodies per ADR-0012 / alice2-index.
//
// The containment model: every markdown ATX heading (`#` … `######`)
// is one addressable section in a flat list. A section's body
// extends from the line after its heading until the next heading of
// the same OR shallower depth — meaning DEEPER nested headings (and
// their content) are TEXTUALLY INCLUDED in the parent's body. The
// granularity choice IS the section choice: editing `# Top` rewrites
// the whole subtree below it; editing the leaf `### Foo` rewrites
// just its leaf content.
//
// Pre-heading body (text before any heading exists) is "section 0":
// Depth=0, Heading="", Body=that-prefix-text. A body with NO headings
// at all collapses to one Section{Depth=0, Heading="", Body=whole-body},
// which addresses cleanly as positional index 0.
//
// Heading detection follows CommonMark ATX rules: 1–6 `#` chars at
// start-of-line, followed by a space and the heading text. Headings
// inside fenced code blocks (```...```) are treated as content, not
// section boundaries — the parser tracks the fenced-code state.

package vault

import (
	"fmt"
	"strconv"
	"strings"
)

// Section is one addressable unit of a UGC entity body.
type Section struct {
	// Index is the 0-based positional address in the flat section list.
	// Positional addressing is the canonical fallback when a heading
	// slug collides with another section (see ResolveSectionAddr).
	Index int

	// Depth is 0 for the pre-heading body and 1..6 for `#`..`######`.
	Depth int

	// Heading is the heading text minus the leading `#+ ` and any
	// trailing whitespace. Empty for the pre-heading body. Markdown
	// formatting inside the heading is preserved verbatim.
	Heading string

	// Body is the section content as a string, INCLUDING any deeper
	// headings textually contained within it (containment model).
	// Excludes the section's OWN heading line. Trailing newlines are
	// preserved as they appeared in the source.
	Body string

	// ByteOffset is the byte offset in the original body where this
	// section's address begins (the heading line for headed sections,
	// 0 for the pre-heading body section). Used for cursor pagination.
	ByteOffset int
}

// HeadingSlug returns the canonical slug form of this section's
// heading for URL addressing. Pre-heading sections (Heading=="")
// have no slug.
func (s Section) HeadingSlug() string {
	if s.Heading == "" {
		return ""
	}
	return slugifyHeading(s.Heading)
}

// ParseSections splits a markdown body into a flat list of sections
// per the containment model described in this file's docstring.
//
// The function never returns an error: any input is parseable as
// "sections" — at minimum a single Section{Depth=0} carrying the
// whole body. Empty input returns one empty section.
func ParseSections(body string) []Section {
	type heading struct {
		lineStart int // byte offset of the heading line
		lineEnd int // byte offset of the newline AFTER the heading line (or len(body) if last)
		depth int
		text string
	}

	var headings []heading
	inFence := false
	pos := 0
	for pos < len(body) {
		// Find the end of this line.
		nl := strings.IndexByte(body[pos:], '\n')
		var lineEnd int
		if nl < 0 {
			lineEnd = len(body)
		} else {
			lineEnd = pos + nl
		}
		line := body[pos:lineEnd]
		trimmed := strings.TrimSpace(line)

		// Track fenced-code state. A line whose trimmed form starts
		// with three or more backticks toggles the fence.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			pos = lineEnd
			if nl >= 0 {
				pos++ // consume the newline
			}
			continue
		}

		if !inFence {
			depth, text, ok := parseATXHeading(line)
			if ok {
				// lineEnd here is start-of-newline; include the newline
				// so the body of the *previous* section ends at our
				// line's start (lineStart), and we record where the
				// next-section body begins (just after our newline).
				headings = append(headings, heading{
					lineStart: pos,
					lineEnd: lineEnd,
					depth: depth,
					text: text,
				})
			}
		}

		pos = lineEnd
		if nl >= 0 {
			pos++ // consume the newline so we continue past it
		}
	}

	// Build the flat section list per the containment model.
	var out []Section

	// Pre-heading body: present iff the first heading is not at byte 0.
	preEnd := len(body)
	if len(headings) > 0 {
		preEnd = headings[0].lineStart
	}
	if preEnd > 0 {
		out = append(out, Section{
			Index: 0,
			Depth: 0,
			Heading: "",
			Body: body[:preEnd],
			ByteOffset: 0,
		})
	} else if len(headings) == 0 {
		// Empty body, no headings — emit one empty section so callers
		// that paginate "by section" see one addressable unit.
		out = append(out, Section{Index: 0, Depth: 0})
		return out
	}

	for i, h := range headings {
		// Body for headings[i] runs from the byte after its newline
		// until the NEXT heading j > i with depth_j <= depth_i, or
		// end-of-body if no such j.
		bodyStart := h.lineEnd
		// Skip the newline character itself if there was one — bodies
		// should not start with the terminating newline of the heading
		// line.
		if bodyStart < len(body) && body[bodyStart] == '\n' {
			bodyStart++
		}
		bodyEnd := len(body)
		for j := i + 1; j < len(headings); j++ {
			if headings[j].depth <= h.depth {
				bodyEnd = headings[j].lineStart
				break
			}
		}
		out = append(out, Section{
			Index: len(out),
			Depth: h.depth,
			Heading: h.text,
			Body: body[bodyStart:bodyEnd],
			ByteOffset: h.lineStart,
		})
	}

	return out
}

// ResolveSectionAddr resolves a URL `{sec}` parameter against a parsed
// section list. The address is either:
//
// - a non-negative integer literal ("0", "1", …) → positional index
// - a heading slug (matched against Section.HeadingSlug() == addr)
//
// Numeric addresses always take the positional branch — even when a
// heading happens to slugify to digits — because positional is the
// canonical disambiguating fallback Per the prior design,'s clarifications.
//
// Returns the matched section's index and ok=true on success. ok=false
// when the address doesn't resolve (out-of-range index, unknown slug,
// or duplicate slug — duplicates fail because the agent must address
// by positional index in that case).
func ResolveSectionAddr(sections []Section, addr string) (int, bool) {
	if addr == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(addr); err == nil && n >= 0 && allDigits(addr) {
		if n < len(sections) {
			return n, true
		}
		return 0, false
	}
	matchIdx := -1
	for i, s := range sections {
		if s.HeadingSlug() == addr {
			if matchIdx >= 0 {
				// Duplicate: caller must use positional index.
				return 0, false
			}
			matchIdx = i
		}
	}
	if matchIdx < 0 {
		return 0, false
	}
	return matchIdx, true
}

// ReplaceSectionBody returns a new whole-document body with the body
// of the section at index `idx` (in the supplied parsed sections list)
// replaced by `newSectionBody`. The section's heading line is
// preserved verbatim — this swaps body content only, NOT the heading.
//
// `sections` must be the result of ParseSections(originalBody) — the
// caller is responsible for keeping them in sync; passing a stale
// section list against a mutated body produces nonsense output.
//
// `newSectionBody` is taken verbatim. Callers responsible for
// terminating it with the trailing newline they want — the function
// does no normalization, so an unintended missing newline can fold
// the next section's heading onto the same line as the new content.
//
// Returns an error when idx is out of range. Used by the PUT
// /v1/user-content/{id}/sections/{sec} handler per alice2-index.
func ReplaceSectionBody(body string, sections []Section, idx int, newSectionBody string) (string, error) {
	if idx < 0 || idx >= len(sections) {
		return "", fmt.Errorf("section index %d out of range [0, %d)", idx, len(sections))
	}
	target := sections[idx]
	var bodyStart int
	if target.Depth == 0 {
		// Pre-heading body section: starts at byte 0 of the document.
		bodyStart = 0
	} else {
		// Headed section: body starts right after the heading line's
		// trailing newline. ByteOffset points at the heading line; find
		// the next `\n` and start the body just past it.
		nl := strings.IndexByte(body[target.ByteOffset:], '\n')
		if nl < 0 {
			// Heading on the final line, no trailing newline → no body
			// content possible. Replacing yields the heading line +
			// newSectionBody concatenated.
			return body[:] + "\n" + newSectionBody, nil
		}
		bodyStart = target.ByteOffset + nl + 1
	}
	bodyEnd := bodyStart + len(target.Body)
	if bodyEnd > len(body) {
		bodyEnd = len(body)
	}
	return body[:bodyStart] + newSectionBody + body[bodyEnd:], nil
}

// parseATXHeading recognizes a CommonMark ATX heading line: 1–6 `#`
// characters at the line's first byte (no leading indentation), then
// a space and the heading text. The closing `#`s permitted by
// CommonMark §4.2 are stripped off the heading text per the spec —
// but ONLY when preceded by a space (or when they form the entire
// heading content). A trailing `#` not preceded by a space is part
// of the heading text proper, e.g. `## C# Language` → `C# Language`.
func parseATXHeading(line string) (depth int, text string, ok bool) {
	depth = 0
	for depth < len(line) && depth < 7 && line[depth] == '#' {
		depth++
	}
	if depth == 0 || depth > 6 {
		return 0, "", false
	}
	// Must be followed by a space (or end-of-line for an empty heading,
	// which CommonMark allows: `#` alone — but rare and not useful for
	// addressing, so we require the space).
	if depth >= len(line) || line[depth] != ' ' {
		return 0, "", false
	}
	raw := strings.TrimSpace(line[depth+1:])
	// CommonMark §4.2 closing-`#` strip: only when the trailing `#`
	// run is preceded by whitespace (or makes up the entire content).
	// `C#` keeps the `#`; `Title ##` becomes `Title`.
	if stripped := strings.TrimRight(raw, "#"); stripped != raw {
		if stripped == "" || strings.HasSuffix(stripped, " ") {
			raw = strings.TrimSpace(stripped)
		}
	}
	if raw == "" {
		return 0, "", false
	}
	return depth, raw, true
}

// slugifyHeading produces the URL-addressable form of a heading text:
// lowercase, ASCII-folded, with non-alphanumeric runs collapsed to
// single hyphens, trimmed of leading/trailing hyphens. Markdown
// formatting characters (`*`, `_`, “ ` “) are dropped before the
// alphanumeric pass so `## My **Bold** Heading` and `## My Bold
// Heading` slugify identically.
func slugifyHeading(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r == '*' || r == '_' || r == '`':
			// drop markdown formatting characters
			continue
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := b.String()
	out = strings.TrimRight(out, "-")
	return out
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
