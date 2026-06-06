package vault

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
// frontmatter field required by the v1 schema (id, kind, source) is
// absent or empty. The writer validates the same set before serializing.
var ErrMissingRequiredField = errors.New("missing required field")

// sourceField is the on-disk shape of the `source:` frontmatter field
// per ADR-0028 §5. Marshals as a scalar string when the slice carries
// exactly one entry, and as a YAML sequence when it carries multiple.
// Unmarshal accepts both shapes so operator-written single-source
// entities stay terse on disk while multi-source overlap renders as
// a sequence the operator can hand-edit naturally.
type sourceField []string

// MarshalYAML emits the slash-form source field as a scalar for
// the single-source case (the operator-common shape) and as a
// sequence for the multi-source case. Empty slice marshals as
// the YAML null literal — Marshal's validateRequired catches the
// missing-field case before we reach here.
func (s sourceField) MarshalYAML() (any, error) {
	switch len(s) {
	case 0:
		return nil, nil
	case 1:
		return s[0], nil
	default:
		return []string(s), nil
	}
}

// UnmarshalYAML accepts either a scalar string or a sequence of
// strings for the `source:` field per ADR-0028 §5. Mapping nodes,
// null nodes, and other shapes return an error so misshapen
// frontmatter surfaces at read time rather than silently
// decoding to an empty slice.
func (s *sourceField) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// Null scalar (`~` / `null` / empty) decodes as empty
		// slice; Unmarshal lifts LegacyPlugin into Source when
		// this happens AND a legacy `plugin:` key is present.
		if node.Tag == "!!null" || node.Value == "" {
			*s = nil
			return nil
		}
		*s = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			if child.Kind != yaml.ScalarNode {
				return fmt.Errorf("source: sequence entry must be a string, got %v at line %d", child.Kind, child.Line)
			}
			out = append(out, child.Value)
		}
		*s = out
		return nil
	default:
		return fmt.Errorf("source: expected scalar or sequence, got %v at line %d", node.Kind, node.Line)
	}
}

// frontmatter is the on-disk YAML shape, separate from Entity so the
// yaml struct tags don't bleed into the public Entity API. Field order
// here drives the marshaled key order (yaml.v3 honors struct order).
//
// Per ADR-0028 §5, the `source:` field carries `<plugin>/<instance>`
// slash-form attribution. The custom sourceField type marshals as a
// scalar string when the slice has exactly one entry and as a YAML
// sequence when it carries multiple (multi-source overlap). Unmarshal
// accepts both shapes plus the pre-ADR-0028 legacy `plugin: <name>`
// key for back-compat reads — reindex re-emits in the new shape.
type frontmatter struct {
	ID string `yaml:"id"`
	Kind string `yaml:"kind"`
	Source sourceField `yaml:"source"`
	// LegacyPlugin holds the pre-ADR-0028 `plugin: <name>` scalar
	// when present in the input. Read-only / decode-only — never
	// emitted on write. Unmarshal lifts the value into Source as a
	// single `<name>/default` entry when Source itself is empty.
	LegacyPlugin string `yaml:"plugin,omitempty"`
	Aliases []string `yaml:"aliases,omitempty"`
	Notations []string `yaml:"notations,omitempty"`
	// UGC per ADR-0031 — true when the entity carries an operator-
	// authored section-editable body. omitempty so non-UGC files
	// never gain a `ugc: false` artifact.
	UGC bool `yaml:"ugc,omitempty"`
	Data map[string]any `yaml:"data,omitempty"`
	Provenance []ProvenanceEntry `yaml:"provenance,omitempty"`
	Summary string `yaml:"summary,omitempty"`
	Tags []string `yaml:"tags,omitempty"`
	Edges []Edge `yaml:"edges,omitempty"`
	NoteCount int `yaml:"note_count,omitempty"`
	Gaps []string `yaml:"gaps,omitempty"`
	CacheExpires *CacheExpires `yaml:"cache_expires,omitempty"`
	Attachments []Attachment `yaml:"attachments,omitempty"`
	GapState map[string]GapStateEntry `yaml:"gap_state,omitempty"`
}

// Marshal serializes an Entity to its on-disk markdown representation:
// `---`-delimited YAML frontmatter, then a blank line, then the body
// (clean_content + regenerated `## Edges` + `## Notes` sections).
//
// Required fields (id, kind, plugin) are validated; missing → error.
// The body sections are deterministic in their order (Edges first, then
// Notes) and stable across repeated writes of the same entity.
//
// canonicalKinds names the operator-enabled canonical kinds (per
// ADR-0008's CanonicalGuard). When the entity's Kind is in this set,
// the synthesized alias (per ADR-0011) is sourced from `data.name`;
// otherwise from `data.title`. Empty/nil set falls back to data.title
// only — useful for tests and source-shape-only deployments.
//
// Plugin-emitted aliases on
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
		Source: sourceField(e.Source),
		Aliases: aliases,
		Notations: e.Notations,
		UGC: e.UGC,
		Data: e.Data,
		Provenance: e.Provenance,
		Summary: e.Summary,
		Tags: e.Tags,
		Edges: e.Edges,
		NoteCount: len(e.Notes),
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
	writeNotesSection(&buf, e.Notes)
	writeDataviewSection(&buf, e.Dataview)

	return buf.Bytes(), nil
}

// Unmarshal parses a vault file's bytes back into an Entity. Frontmatter
// is the authoritative source for every field; body sections (`## Edges`
// wikilinks, `## Notes` dated blocks) are parsed and merged on top —
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

	// Source population per ADR-0028 §5 with back-compat for the
	// pre-ADR `plugin: <name>` scalar: prefer the new `source:`
	// field when present; fall back to lifting LegacyPlugin into
	// a single `<name>/default` entry so pre-Cut-2 vault files
	// still read cleanly through reindex.
	source := []string(fm.Source)
	if len(source) == 0 && fm.LegacyPlugin != "" {
		source = []string{fm.LegacyPlugin + "/default"}
	}

	e := &Entity{
		ID: fm.ID,
		Kind: fm.Kind,
		Source: source,
		Aliases: fm.Aliases,
		Notations: fm.Notations,
		UGC: fm.UGC,
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

	cleanContent, bodyEdges, bodyNotes, bodyDataview := splitBody(body)
	e.CleanContent = cleanContent
	e.Edges = mergeEdges(e.Edges, bodyEdges)
	// Notes live in the body `## Notes` section only — the
	// frontmatter `note_count` is informational + queryable, the
	// body is the source of truth .
	e.Notes = bodyNotes
	// Dataview paragraphs live in the body's yaad:dataview marker
	// region only — no frontmatter mirror (frontmatter `data` stays
	// global per #119).
	e.Dataview = bodyDataview

	return e, nil
}

// MergedAliasesFor returns the alias list `Marshal` would write for
// an entity with the given identity and plugin-emitted aliases.
// Mirror of the merge `Marshal` performs internally
// (title-synthesized + plugin entries, synth first, dedup) without
// round-tripping through a full marshal cycle.
//
// Callers outside the vault package (notably the daemon's ingest
// tracker per #3) use this to mirror the vault frontmatter into the
// DB `entity_aliases` index — keeping search-index reads and vault
// frontmatter in lockstep without re-encoding the YAML.
func MergedAliasesFor(id, kind string, data map[string]any, pluginAliases []string, canonicalKinds []string) []string {
	e := &Entity{ID: id, Kind: kind, Data: data, Aliases: pluginAliases}
	return mergeAliases(synthesizeAliases(e, canonicalKinds), e.Aliases)
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
// plugin-emitted aliases from `e.Aliases`. Synthesized entries
// land first for deterministic
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
	case len(e.Source) == 0:
		return fmt.Errorf("%w: source", ErrMissingRequiredField)
	}
	// Per ADR-0028 §5, every Source entry must be the slash-form
	// `<plugin>/<instance>` — exactly two non-empty `/`-separated
	// segments. Empty entries, bare-plugin shapes (`github`),
	// half-shapes (`/default`, `github/`), and over-segmented
	// shapes (`github/personal/extra`) all indicate a producer
	// that hasn't migrated to the new attribution contract.
	// Reject at write time so the bug lands at the offending
	// site, not on a downstream reader (especially PluginName(),
	// which would return the empty string for a `/default`-style
	// entry and silently mis-attribute the entity in cache
	// filters and UI rendering).
	for i, s := range e.Source {
		if s == "" {
			return fmt.Errorf("%w: source[%d] is empty", ErrMissingRequiredField, i)
		}
		parts := strings.Split(s, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("%w: source[%d] %q must be the slash-form `<plugin>/<instance>` (exactly two non-empty segments)",
				ErrMissingRequiredField, i, s)
		}
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

// noteTableRow matches a single-column markdown-table row: a line
// shaped like `| <content> |` (leading/trailing pipes, anything
// between). The captured content is the raw cell text — pipe-escapes
// (`\|`) and `<br>` paragraph markers are decoded by parseNoteRow.
var noteTableRow = regexp.MustCompile(`^\|\s*(.*?)\s*\|\s*$`)

// noteTableSeparator matches the markdown-table separator row
// (`|---|`, with any number of dashes, and optional spaces). One
// occurrence sits between the header and the first note row.
var noteTableSeparator = regexp.MustCompile(`^\|\s*-+\s*\|\s*$`)

// noteHeadingRow extracts `<date>`, `<date> — <author>`, or
// `<date> — <author> @ <operator>` from a note-table heading row's
// cell content, optionally followed by a `[kind=X field=Y]`
// metadata suffix per #186. Date is a single non-whitespace token
// (RFC3339 or YYYY-MM-DD; both are dash-only safe). Operator is
// optional and trailing — pre-yaad-index vault files end at the
// author and the parser must round-trip those without inventing
// an operator. Metadata bracket is optional; pre-#186 vault files
// without it round-trip with empty Field + Kind.
//
// Group 1: date. Group 2: author (may be empty when only date present;
// `[^@\[]+?` so the `@ <operator>` and ` [meta]` separators do not
// leak into the author capture). Group 3: operator (empty when not
// present; `[^[]+?` so the metadata suffix doesn't leak in). Group 4:
// the metadata-bracket contents (without the brackets).
var noteHeadingRow = regexp.MustCompile(`^(\S+)(?:\s+(?:—|-)\s+([^@\[]+?)(?:\s+@\s+([^\[]+?))?)?(?:\s+\[([^\]]*)\])?$`)

// noteMetadataPair parses one `key=value` token inside the
// `[kind=X field=Y]` metadata suffix. Both key and value are
// non-whitespace runs; spaces separate pairs.
var noteMetadataPair = regexp.MustCompile(`^(\S+?)=(\S*)$`)

// renderNoteMetadata renders the `[kind=X field=Y]` heading-row
// suffix per #186. Empty result (no brackets emitted) when both
// fields are at their default values (kind="" or "note" + field
// empty), preserving the legacy heading-row shape so pre-#186
// vault files round-trip unchanged.
//
// Field is emitted before kind when both are set so the read-back
// pair-order is stable. Kind="note" is treated as the implicit
// default — emitting it explicitly would surface noise in every
// existing-shape note.
func renderNoteMetadata(n Note) string {
	var parts []string
	if n.ID != "" {
		parts = append(parts, "id="+n.ID)
	}
	if n.Field != "" {
		parts = append(parts, "field="+n.Field)
	}
	if n.Kind != "" && n.Kind != NoteKindNote {
		parts = append(parts, "kind="+n.Kind)
	}
	if !n.LastEditedAt.IsZero() {
		parts = append(parts, "edited="+n.LastEditedAt.UTC().Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// parseNoteMetadata splits a `key=value key=value` metadata-suffix
// string into a `(field, kind, id)` tuple. Unknown keys are silently
// ignored — the format is forward-compatible with future tag
// additions; the parser doesn't reject what it doesn't recognise.
func parseNoteMetadata(raw string) (field, kind, id string, lastEdited time.Time) {
	for _, token := range strings.Fields(raw) {
		m := noteMetadataPair.FindStringSubmatch(token)
		if m == nil {
			continue
		}
		switch m[1] {
		case "field":
			field = m[2]
		case "kind":
			kind = m[2]
		case "id":
			id = m[2]
		case "edited":
			// Best-effort: a malformed timestamp leaves lastEdited
			// zero (treated as never-edited) rather than failing the
			// whole parse — consistent with the metadata bracket's
			// forward-compatible, lenient contract.
			if t, err := time.Parse(time.RFC3339, m[2]); err == nil {
				lastEdited = t
			}
		}
	}
	return field, kind, id, lastEdited
}

// splitBody walks the body bytes and extracts (clean_content, edges,
// notes). Lines before the first `## Edges` or `## Notes` heading
// are clean_content; subsequent sections are parsed for their content.
// Unknown `## ` headings are tolerated and folded into clean_content
// preceding the known headings.
//
// Per the prior design, the `## Notes` section is rendered as a single-column
// markdown table with alternating heading/body rows:
//
//	| Notes |
//	|----------|
//	| 2026-05-03 — operator |
//	| First note text. |
//	| 2026-05-03 — yaad |
//	| Second note, may use <br><br> for paragraph breaks. |
//
// The first non-separator row is the table's column header (`Notes`)
// and is skipped. Subsequent rows alternate between heading
// (`<date> — <author>`) and body (raw text, with `<br><br>` decoded
// back to `\n\n` and `\|` decoded back to `|`).
//
// Per #8: the `## Notes` section is wrapped in the
// NotesStartMarker / NotesEndMarker pair on write. On read,
// the parser enters "notes" mode on encountering the start
// marker (regardless of whether a `## Notes` heading follows
// immediately) and exits on the end marker. Legacy un-marked
// entities continue to enter notes mode on the `## Notes`
// heading — the fallback path lets first-read recover notes
// from pre-marker vault files; the next write produces marker-
// wrapped output.
func splitBody(b []byte) (cleanContent string, edges []Edge, notes []Note, dataview []DataviewParagraph) {
	var (
		section = "clean"
		clean bytes.Buffer
		edgesB []Edge
		notesB []Note
		dvB []DataviewParagraph

		// note-table accumulator
		noteTableHeaderSeen bool // ate the `| Notes |` row
		noteTableSepSeen bool // ate the `|---|` row
		noteExpectHeading bool // next row is a heading row
		curDate time.Time
		curAuthor string
		curOperator string
		curField string
		curKind string
		curID string
		curLastEdited time.Time
	)

	// flushOrphanedHeading recovers from the edge case where the
	// parser read a heading row but the notes section ended
	// before the body row landed (truncated hand-edit, ad-hoc paste,
	// concurrent-write surface). Without the flush, the captured
	// date + author would be silently dropped.
	// Empty Text on the appended Note is a clear "this note
	// was authored mid-edit" signal that survives the parse — better
	// than silent loss; consistent with the stated contract that
	// hand-edits don't break round-trip.
	flushOrphanedHeading := func() {
		// noteExpectHeading == true means we're either at section
		// start (no heading consumed) or the last row read was a body
		// row (heading + body already paired and appended). false =
		// "heading read, body still pending."
		if noteExpectHeading {
			return
		}
		notesB = append(notesB, Note{
			ID:           curID,
			Date:         curDate,
			LastEditedAt: curLastEdited,
			Author:       curAuthor,
			Operator:     curOperator,
			Field:        curField,
			Kind:         curKind,
			Text:         "",
		})
		curDate = time.Time{}
		curAuthor = ""
		curOperator = ""
		curField = ""
		curKind = ""
		curID = ""
		curLastEdited = time.Time{}
		noteExpectHeading = true
	}

	// Whether we're currently inside a marker-wrapped notes
	// region. Distinct from `section == "notes"` because the
	// start-marker line may precede the `## Notes` heading; we
	// enter the notes section on the marker and ignore the
	// heading inside it.
	inNotesMarker := false

	resetNotesState := func() {
		noteTableHeaderSeen = false
		noteTableSepSeen = false
		noteExpectHeading = true
	}

	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimRight(line, " \t")
		switch {
		case trimmed == NotesStartMarker:
			// Enter notes-marker region. Notes parsing
			// activates regardless of whether a `## Notes`
			// heading follows.
			if section == "notes" {
				flushOrphanedHeading()
			}
			section = "notes"
			inNotesMarker = true
			resetNotesState()
			continue
		case trimmed == NotesEndMarker:
			// Exit notes-marker region. Flush any orphaned
			// heading row + return to clean section so subsequent
			// content (rare; usually nothing follows the end
			// marker) is captured as clean_content.
			if section == "notes" {
				flushOrphanedHeading()
			}
			inNotesMarker = false
			section = "clean"
			continue
		case trimmed == DataviewStartMarker:
			if section == "notes" {
				flushOrphanedHeading()
			}
			section = "dataview"
			continue
		case trimmed == DataviewEndMarker:
			section = "clean"
			continue
		case trimmed == "## Edges":
			if inNotesMarker {
				// Inside marker region — ignore section-like
				// headings. Notes end at the end marker.
				continue
			}
			if section == "notes" {
				flushOrphanedHeading()
			}
			section = "edges"
			continue
		case trimmed == "## Notes" && inNotesMarker:
			// Inside marker region — the `## Notes` heading is
			// decorative for human reading; the table parser below
			// skips it via the column-header rule. Outside the
			// marker region, `## Notes` is normal user prose (no
			// special handling) — see the `## ` clean-section
			// branch below.
			continue
		case strings.HasPrefix(line, "## "):
			if inNotesMarker {
				// Inside marker region — unknown headings are
				// folded as decorative table content (table parser
				// below will skip non-table-row lines).
				continue
			}
			// Unknown section heading. Treat it as part of clean_content
			// — preserves user-authored body shape that doesn't match
			// the canonical sections. (Once we leave a known section
			// for an unknown one, any further content past this point
			// is body again until another known heading appears.)
			if section == "notes" {
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
		case "notes":
			if noteTableSeparator.MatchString(strings.TrimSpace(line)) {
				noteTableSepSeen = true
				continue
			}
			m := noteTableRow.FindStringSubmatch(line)
			if m == nil {
				// Blank line / decorative content — ignore inside the
				// notes section. Mid-table blank lines aren't
				// produced by Marshal but tolerate them on parse so
				// hand-edits don't break round-trip.
				continue
			}
			cell := decodeNoteCell(m[1])
			if !noteTableHeaderSeen {
				// First row is the column header (`| Notes |`).
				// Don't validate the literal `Notes` text — design-
				// in-flux means the column name may move; what matters
				// is "first row is header, rest is content."
				noteTableHeaderSeen = true
				continue
			}
			// Defensive: if the separator never showed up but rows are
			// flowing, treat it as seen. Markdown renderers tolerate a
			// missing separator inconsistently.
			if !noteTableSepSeen {
				noteTableSepSeen = true
			}
			if noteExpectHeading {
				if hm := noteHeadingRow.FindStringSubmatch(cell); hm != nil {
					curDate = parseCommentDate(hm[1])
					curAuthor = strings.TrimSpace(hm[2])
					curOperator = strings.TrimSpace(hm[3])
					curField, curKind, curID, curLastEdited = parseNoteMetadata(hm[4])
				} else {
					// Malformed heading row — keep the cell as the
					// date string so a hand-edit doesn't silently swallow
					// it; author + operator stay empty.
					curDate = parseCommentDate(cell)
					curAuthor = ""
					curOperator = ""
					curField = ""
					curKind = ""
					curID = ""
					curLastEdited = time.Time{}
				}
				noteExpectHeading = false
				continue
			}
			// Body row — pair with the buffered heading.
			notesB = append(notesB, Note{
				ID:           curID,
				Date:         curDate,
				LastEditedAt: curLastEdited,
				Author:       curAuthor,
				Operator:     curOperator,
				Field:        curField,
				Kind:         curKind,
				Text:         cell,
			})
			curDate = time.Time{}
			curAuthor = ""
			curOperator = ""
			curField = ""
			curKind = ""
			curID = ""
			curLastEdited = time.Time{}
			noteExpectHeading = true
		case "dataview":
			if fields := parseDataviewLine(line); len(fields) > 0 {
				dvB = append(dvB, DataviewParagraph{Fields: fields})
			}
		}
	}

	// End-of-input: flush a trailing orphaned heading row when the
	// section ended without a paired body row.
	if section == "notes" {
		flushOrphanedHeading()
	}

	// Trim leading blank lines (the gap between frontmatter and body)
	// and trailing blank lines (gap before the first known section),
	// then re-add a single trailing newline so the output is canonical
	// regardless of how the caller spaced their input.
	trimmed := strings.Trim(clean.String(), "\n")
	if trimmed == "" {
		return "", edgesB, notesB, dvB
	}
	return trimmed + "\n", edgesB, notesB, dvB
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

// decodeNoteCell reverses encodeNoteCell — turns `<br><br>` back
// into paragraph breaks (`\n\n`), single `<br>` into `\n`, and `\|`
// back into a literal pipe. The order matters: `<br><br>` before `<br>`
// so paragraph breaks aren't double-decoded.
func decodeNoteCell(s string) string {
	s = strings.ReplaceAll(s, "<br><br>", "\n\n")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, `\|`, "|")
	return s
}

// encodeNoteCell escapes pipe characters and substitutes line
// breaks with `<br>` so the cell stays on one markdown-table row.
// Paragraph boundaries (`\n\n`) become `<br><br>`; intra-paragraph
// newlines become a single `<br>`.
func encodeNoteCell(s string) string {
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

// writeNotesSection renders the entity's notes as a single-
// column markdown table Per the prior design,. Each note becomes two rows: a
// heading (`<date> — <author>`) and a body (the text, with paragraph
// breaks encoded as `<br><br>` and pipe chars escaped as `\|`).
//
// The frontmatter `note_count` field carries the count; the body
// table is the source of truth.
//
// Per #8: wraps the section in NotesStartMarker /
// NotesEndMarker so the read path can splice deterministically
// + so a plugin body re-ingest doesn't touch this region.
func writeNotesSection(w *bytes.Buffer, notes []Note) {
	if len(notes) == 0 {
		return
	}
	w.WriteString(NotesStartMarker)
	w.WriteByte('\n')
	w.WriteString("## Notes\n\n")
	w.WriteString("| Notes |\n")
	w.WriteString("|----------|\n")
	for _, c := range notes {
		// Heading row: `| <date> — <author> @ <operator> |`. Date format
		// is YYYY-MM-DD (not RFC3339) — the table is operator-readable
		// shorthand; the underlying time.Time keeps the precision the
		// API layer needs. Operator suffix is omitted when empty so
		// pre-yaad-index notes render unchanged (backward-
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
		// Optional trailing metadata per #186: `[kind=X field=Y]`.
		// Both fields omitted (default kind=note + empty field) →
		// no brackets, preserving the legacy heading-row shape so
		// pre-#186 vault files round-trip unchanged.
		if meta := renderNoteMetadata(c); meta != "" {
			w.WriteString(" ")
			w.WriteString(meta)
		}
		w.WriteString(" |\n")
		// Body row: encoded text in a single cell.
		w.WriteString("| ")
		w.WriteString(encodeNoteCell(strings.TrimRight(c.Text, "\n")))
		w.WriteString(" |\n")
	}
	w.WriteString(NotesEndMarker)
	w.WriteByte('\n')
}

// writeDataviewSection renders the entity's dataview paragraphs
// per #119. Each paragraph becomes one line in the
// marker-wrapped region:
//
//	<!-- yaad:dataview start -->
//	role:: Staff Platform Engineer  salary:: 150k+  work_mode:: hybrid
//	role:: Senior Engineer  salary:: 130k+  work_mode:: remote
//	<!-- yaad:dataview end -->
//
// Keys are sorted within each paragraph for deterministic
// rendering (the dedup contract assumes sorted-key equality).
// Empty Dataview slice skips the section entirely so entities
// that never receive a canonical-type fill with `data` stay
// noise-free.
func writeDataviewSection(w *bytes.Buffer, paragraphs []DataviewParagraph) {
	if len(paragraphs) == 0 {
		return
	}
	w.WriteString(DataviewStartMarker)
	w.WriteByte('\n')
	for _, p := range paragraphs {
		if len(p.Fields) == 0 {
			continue
		}
		w.WriteString(RenderDataviewParagraph(p))
		w.WriteByte('\n')
	}
	w.WriteString(DataviewEndMarker)
	w.WriteByte('\n')
}

// RenderDataviewParagraph turns a DataviewParagraph into its
// `key:: value  key:: value` wire form with sorted-key order.
// Exposed at the package level so the handler dedup path can
// content-hash a candidate paragraph the same way the writer
// will render it.
func RenderDataviewParagraph(p DataviewParagraph) string {
	if len(p.Fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(k)
		b.WriteString(":: ")
		b.WriteString(p.Fields[k])
	}
	return b.String()
}

// parseDataviewLine inverts RenderDataviewParagraph for one
// line in a yaad:dataview block. Two-space separator between
// key-value pairs is the cell delimiter; `::` separates a key
// from its value. Lines without `::` (blank lines, decorative
// content) return nil so the parser skips them silently.
// Permissive enough to round-trip operator hand-edits that
// shuffle field order (sorted-key render canonicalizes on
// next write).
func parseDataviewLine(line string) map[string]string {
	if !strings.Contains(line, "::") {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(line, "  ") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.Index(pair, "::")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(pair[:idx])
		val := strings.TrimSpace(pair[idx+2:])
		if key == "" {
			continue
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DefaultBodyEdgeType is assigned to a body wikilink that does not
// carry an explicit type annotation (`- [[entity:id]]` with no
// `(type)` suffix). The untyped body form means "this entity is
// referenced from here" without a stronger semantic; `mentions` is
// the canonical name for that shape — chosen over
// "drop untyped" because dropping would lose information that
// survives the frontmatter→body→frontmatter round trip.
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
// This dedup-by-`to` + default-type rule resolves an earlier
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
