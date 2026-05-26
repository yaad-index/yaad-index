// Section parser for UGC bodies per ADR-0012 / yaad-index.
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
// /v1/user-content/{id}/sections/{sec} handler per yaad-index.
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

// SlugifyHeading is the exported alias of slugifyHeading. Used by
// the daemon handlers (per #299) for pre-write slug-collision
// checks so the agent gets a 409 before the entity is mutated.
func SlugifyHeading(s string) string { return slugifyHeading(s) }

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

// InsertSection returns a new whole-document body with a new section
// (heading + body) inserted AFTER the section at afterIdx, along
// with the byte offset of the inserted heading line in the new
// body so callers can unambiguously locate the new section in a
// post-write re-parse — the heading slug alone isn't unique when
// a same-named section already lives under a different parent.
// Used by the POST /v1/user-content/{id}/sections handler per #299.
//
// afterIdx semantics:
//
//   - 0..len(sections)-1 → insert immediately after that section's
//     last byte (i.e. the new section appears as the next sibling
//     at the same depth — or deeper, depending on the supplied
//     depth — with no sections of shallower-or-equal depth between).
//   - -1 → prepend at the document start. When the document already
//     has a pre-heading section (Depth=0, Heading=""), the new
//     section lands AFTER it; the pre-heading body is not eligible
//     to receive a prepended heading in front of it (a heading-
//     before-text-section would invert the containment model).
//   - len(sections) → append at end (caller can also pass -2 or
//     anything ≥ len; the function clamps both ways).
//
// depth is the new heading's `#`-count (1..6). If depth ≤ 0, defaults
// to the depth of the after-section (or 1 if afterIdx == -1 / the
// document is empty).
//
// heading is the new heading text; empty rejects (a section without
// a heading isn't independently addressable — section 0 is the only
// headless section).
//
// body is taken verbatim. The function appends a trailing newline
// when body is non-empty + doesn't end in one, so the next section's
// heading won't fold onto the same line.
//
// Returns the new whole-document body and the byte offset of the
// inserted heading line within that new body. Callers locate the
// new section in a post-write re-parse by matching ByteOffset
// against the returned value — the heading slug alone isn't
// unique because the containment model allows same-named
// siblings under different parents.
func InsertSection(body string, sections []Section, afterIdx int, depth int, heading, sectionBody string) (string, int, error) {
	if heading == "" {
		return "", 0, fmt.Errorf("InsertSection: heading is required (use the section-0 pre-heading body shape for headless content)")
	}
	if depth <= 0 {
		depth = defaultInsertDepth(sections, afterIdx)
	}
	if depth < 1 || depth > 6 {
		return "", 0, fmt.Errorf("InsertSection: depth %d out of range [1, 6]", depth)
	}

	insertAt, _ := computeInsertOffset(body, sections, afterIdx)
	pad := ""
	// Ensure the slice we splice in starts on a new line.
	if insertAt > 0 && (insertAt > len(body) || body[insertAt-1] != '\n') {
		pad = "\n"
	}
	headingLine := strings.Repeat("#", depth) + " " + heading + "\n"
	bodyChunk := sectionBody
	if bodyChunk != "" && !strings.HasSuffix(bodyChunk, "\n") {
		bodyChunk += "\n"
	}
	insertion := pad + headingLine + bodyChunk
	newBody := body[:insertAt] + insertion + body[insertAt:]
	// Byte offset of the new heading line in the new body: insertAt
	// plus the optional leading newline pad.
	newOffset := insertAt + len(pad)
	return newBody, newOffset, nil
}

// computeInsertOffset returns the byte offset to splice a new
// section into the body so it lands AFTER the section at afterIdx,
// along with the index the new section will occupy post-parse.
//
// Boundaries beyond [-1, len(sections)] are clamped: anything ≥
// len(sections) appends at end of body; anything ≤ -1 prepends after
// the pre-heading body (if any) or at byte 0.
func computeInsertOffset(body string, sections []Section, afterIdx int) (offset int, newIdx int) {
	if len(sections) == 0 {
		return len(body), 0
	}
	if afterIdx >= len(sections) {
		afterIdx = len(sections) - 1
	}
	if afterIdx <= -1 {
		// Prepend at document start, but never INSIDE the pre-heading
		// section — slot in after it (if present).
		if sections[0].Depth == 0 && sections[0].Heading == "" {
			afterIdx = 0
		} else {
			return 0, 0
		}
	}
	target := sections[afterIdx]
	// End-of-target's-textual-range. For Depth==0 (pre-heading) the
	// range is [0, len(body[..first-heading])]. For headed sections,
	// it's [heading.lineStart, next-shallower-or-equal-heading-start
	// OR end-of-body].
	endByte := len(body)
	for j := afterIdx + 1; j < len(sections); j++ {
		s := sections[j]
		if s.Depth == 0 {
			continue
		}
		if target.Depth == 0 {
			endByte = s.ByteOffset
			break
		}
		if s.Depth <= target.Depth {
			endByte = s.ByteOffset
			break
		}
	}
	return endByte, afterIdx + 1
}

// RenameSectionHeading returns a new whole-document body with the
// heading line of the section at idx rewritten to use newHeading.
// The section's body (including any nested headings textually
// contained within it) is preserved verbatim. Used by the PATCH
// /v1/user-content/{id}/sections/{sec}/heading handler per #299.
//
// The depth (`#`-count) is preserved. Pre-heading sections (Depth=0,
// Heading="") reject — they have no heading line to rewrite; create
// one with InsertSection first.
//
// newHeading is the new heading text without leading `#`s; empty
// rejects.
func RenameSectionHeading(body string, sections []Section, idx int, newHeading string) (string, error) {
	if idx < 0 || idx >= len(sections) {
		return "", fmt.Errorf("RenameSectionHeading: section index %d out of range [0, %d)", idx, len(sections))
	}
	if newHeading == "" {
		return "", fmt.Errorf("RenameSectionHeading: new heading is required")
	}
	target := sections[idx]
	if target.Depth == 0 {
		return "", fmt.Errorf("RenameSectionHeading: pre-heading section (index 0) has no heading line to rename")
	}
	// Find the end of the heading line so we know where to splice.
	lineEnd := target.ByteOffset
	for lineEnd < len(body) && body[lineEnd] != '\n' {
		lineEnd++
	}
	newHeadingLine := strings.Repeat("#", target.Depth) + " " + newHeading
	return body[:target.ByteOffset] + newHeadingLine + body[lineEnd:], nil
}

// DeleteSection returns a new whole-document body with the section
// at idx removed — heading + body + all textually contained nested
// headings — per the containment model. Used by the DELETE
// /v1/user-content/{id}/sections/{sec} handler per #299.
//
// Pre-heading section (Depth=0, Heading="") rejects: removing it
// would leave an entity with no body prefix, which the parser
// re-synthesizes as an empty section 0 anyway. Callers wanting to
// clear the pre-heading body should use ReplaceSectionBody with
// the empty string instead.
func DeleteSection(body string, sections []Section, idx int) (string, error) {
	if idx < 0 || idx >= len(sections) {
		return "", fmt.Errorf("DeleteSection: section index %d out of range [0, %d)", idx, len(sections))
	}
	target := sections[idx]
	if target.Depth == 0 {
		return "", fmt.Errorf("DeleteSection: pre-heading section (index 0) cannot be deleted; use ReplaceSectionBody with empty body to clear it")
	}
	start := target.ByteOffset
	// Range end is the next sibling-or-shallower heading, or
	// end-of-body — matches the parser's containment model.
	end := len(body)
	for j := idx + 1; j < len(sections); j++ {
		s := sections[j]
		if s.Depth == 0 {
			continue
		}
		if s.Depth <= target.Depth {
			end = s.ByteOffset
			break
		}
	}
	return body[:start] + body[end:], nil
}

// DefaultInsertDepth is the exported alias of defaultInsertDepth.
// Used by the daemon handlers (per #299) so the pre-write collision
// check picks the same depth vault.InsertSection will pick.
func DefaultInsertDepth(sections []Section, afterIdx int) int {
	return defaultInsertDepth(sections, afterIdx)
}

// defaultInsertDepth picks the heading depth to use when the caller
// passed depth ≤ 0. Rules:
//
//   - If afterIdx points at a headed section (Depth > 0), use that
//     depth — the new section lands as a sibling at the same level.
//   - If afterIdx is out-of-range (append-at-end), fall back to the
//     LAST headed section's depth, so appending continues at the
//     same level the document already established.
//   - If no headed section exists, default to 1.
func defaultInsertDepth(sections []Section, afterIdx int) int {
	if afterIdx >= 0 && afterIdx < len(sections) && sections[afterIdx].Depth > 0 {
		return sections[afterIdx].Depth
	}
	for i := len(sections) - 1; i >= 0; i-- {
		if sections[i].Depth > 0 {
			return sections[i].Depth
		}
	}
	return 1
}

// SectionSlugConflicts reports whether `wantSlug` would collide
// with the slug of any same-parent same-depth sibling of the
// section at `idx`. Used by the rename handler per #299.
//
// "Sibling" is strict containment-aware: a section is a sibling
// of another only when they share the same nearest enclosing
// shallower-depth section as parent — `## A / ### Notes` and
// `## B / ### Notes` are NOT siblings of each other and may
// coexist. The function excludes `idx` itself so a no-op rename
// doesn't self-collide.
func SectionSlugConflicts(sections []Section, idx int, wantSlug string) bool {
	if wantSlug == "" {
		return false
	}
	if idx < 0 || idx >= len(sections) {
		return false
	}
	target := sections[idx]
	if target.Depth == 0 {
		return false
	}
	start, end := sameParentRange(sections, idx, target.Depth)
	for i := start; i < end; i++ {
		if i == idx {
			continue
		}
		if sections[i].Depth == target.Depth && sections[i].HeadingSlug() == wantSlug {
			return true
		}
	}
	return false
}

// SectionSlugConflictsAtInsertion reports whether inserting a new
// section with depth `newDepth` AFTER the section at `afterIdx`
// would slug-collide with an existing same-parent same-depth
// sibling. Used by the add handler per #299 so the containment-
// aware sibling check is applied to the insertion slot, not the
// global document.
//
// `afterIdx` follows InsertSection's conventions: -1 prepends
// (at byte 0 or right after the pre-heading body); values ≥
// len(sections) append at end. Same parent-resolution rule as
// SectionSlugConflicts: parent = nearest preceding section with
// depth < newDepth at-or-before the insertion slot.
func SectionSlugConflictsAtInsertion(sections []Section, afterIdx, newDepth int, wantSlug string) bool {
	if wantSlug == "" || newDepth < 1 {
		return false
	}
	if len(sections) == 0 {
		return false
	}
	// Treat "after the last section" as scanning from index len-1
	// backward; "prepend" (-1) as scanning from before everything.
	slot := afterIdx
	if slot >= len(sections) {
		slot = len(sections) - 1
	}
	parentDepth := 0
	start := 0
	for i := slot; i >= 0; i-- {
		if sections[i].Depth > 0 && sections[i].Depth < newDepth {
			parentDepth = sections[i].Depth
			start = i + 1
			break
		}
	}
	// Range end: first section AFTER the insertion slot whose depth
	// closes the parent containment (≤ parentDepth, > 0). When no
	// parent (parentDepth == 0), the range runs to end-of-list.
	end := len(sections)
	scanFrom := slot + 1
	if scanFrom < 0 {
		scanFrom = 0
	}
	for i := scanFrom; i < len(sections); i++ {
		if sections[i].Depth > 0 && parentDepth > 0 && sections[i].Depth <= parentDepth {
			end = i
			break
		}
	}
	for i := start; i < end; i++ {
		if sections[i].Depth == newDepth && sections[i].HeadingSlug() == wantSlug {
			return true
		}
	}
	return false
}

// sameParentRange returns the half-open [start, end) range of
// section indices that share the containment-parent of the
// section at `idx` (which has depth `depth`). Used by the slug-
// collision helpers to scope sibling checks to one parent range
// instead of the whole document.
func sameParentRange(sections []Section, idx, depth int) (start, end int) {
	parentDepth := 0
	for i := idx - 1; i >= 0; i-- {
		if sections[i].Depth > 0 && sections[i].Depth < depth {
			parentDepth = sections[i].Depth
			start = i + 1
			break
		}
	}
	end = len(sections)
	for i := idx + 1; i < len(sections); i++ {
		if sections[i].Depth > 0 && parentDepth > 0 && sections[i].Depth <= parentDepth {
			end = i
			break
		}
	}
	return
}
