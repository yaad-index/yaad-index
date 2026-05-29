// Generalized 5-section task body schema per #337. Every task
// file body — resolution-task (#304 Cut C3), err-task (PR-327 /
// PR-328), workflow-internal queue items — uses the same
// section layout so the future bounded-primitive surface
// (set_prompt, add_checkbox, add_edge, append_freeform,
// add_note in Cut 2) can target sections without parsing
// free-form markdown.
//
// Sections in fixed order, each delimited by an HTML-comment
// marker pair so the renderer + parser converge on byte-stable
// output:
//
//  1. prompt — mandatory; the instruction for an agent
//     processing this task. Single block (may span multiple
//     lines / sentences). Replaceable by operator;
//     not appendable. Cut 2 exposes set_prompt as the
//     bounded operation.
//  2. edges — optional; a daemon-managed read-only view of
//     entities the task references. The view is regenerated
//     from the actual edge graph rather than mutated
//     independently; agents read it as a one-call context-
//     load convenience and treat it as eventually-consistent.
//     Cut 1 emits the marker pair empty; the graph-write /
//     section-regen atomicity contract is settled in Cut 2.
//  3. todo — optional; checkbox list. Set-shape per #337
//     commutativity carve-out: two add_checkbox calls produce
//     the same end-state regardless of order. Cut 2 exposes
//     add_checkbox.
//  4. freeform — optional; free-form prose appended by
//     agent/operator. Ordered-append per #337 carve-out:
//     order-of-calls determines order-of-text. Cut 2 exposes
//     append_freeform.
//  5. notes — optional; `add_comment(task_id, ...)` appends
//     here in Cut 2. System-stamped with author + timestamp.
//
// Cut 1 (this file + the resolution-task / err-task writer
// updates) lands the schema constants, renderer, and parser
// so the section markers are always emitted on initial task
// creation. Cut 2 builds the bounded primitives on top.

package actions

import (
	"fmt"
	"strings"
)

// Task section names per #337. Exported so downstream packages
// (Cut 2's bounded primitives, future MCP tools) reference the
// same vocabulary without re-declaring the strings.
const (
	TaskSectionPrompt   = "prompt"
	TaskSectionEdges    = "edges"
	TaskSectionTodo     = "todo"
	TaskSectionFreeform = "freeform"
	TaskSectionNotes    = "notes"
)

// taskSectionOrder defines the fixed render order. Always all
// five, always in this sequence — readers depend on the fixed
// ordering to locate section start positions without scanning
// the whole body for every marker.
var taskSectionOrder = []string{
	TaskSectionPrompt,
	TaskSectionEdges,
	TaskSectionTodo,
	TaskSectionFreeform,
	TaskSectionNotes,
}

// taskMarkerOpen renders the opening HTML-comment marker for a
// section, e.g. `<!-- yaad-index prompt -->`. The yaad-index
// prefix scopes the marker so a task body that happens to
// embed unrelated HTML comments doesn't trip the parser.
func taskMarkerOpen(section string) string {
	return "<!-- yaad-index " + section + " -->"
}

// taskMarkerClose renders the closing HTML-comment marker for
// a section, e.g. `<!-- /yaad-index prompt -->`. The slash
// follows the opening `<!--` per common HTML close-tag
// convention so a reader scanning for the close finds it
// adjacent-but-distinct from the open shape.
func taskMarkerClose(section string) string {
	return "<!-- /yaad-index " + section + " -->"
}

// TaskSections carries the per-section content for the
// 5-section schema. Empty strings are valid for every section
// except prompt (renderer enforces prompt non-empty); empty
// content still emits the marker pair so the parser sees a
// well-formed body on round-trips.
//
// Content fields are stored without the surrounding markers;
// the renderer adds them. Callers populating the struct write
// just the inner text.
type TaskSections struct {
	Prompt   string
	Edges    string
	Todo     string
	Freeform string
	Notes    string
}

// content returns the per-section text by section name. Cut 2's
// bounded-primitive callers route through this so they don't
// reimplement the name→field mapping.
func (s TaskSections) content(section string) string {
	switch section {
	case TaskSectionPrompt:
		return s.Prompt
	case TaskSectionEdges:
		return s.Edges
	case TaskSectionTodo:
		return s.Todo
	case TaskSectionFreeform:
		return s.Freeform
	case TaskSectionNotes:
		return s.Notes
	default:
		return ""
	}
}

// RenderTaskSections produces the 5-section body per #337.
// Output shape: each section's marker pair appears in order
// with the section content sandwiched between (and a trailing
// newline so the next marker starts on a fresh line). Empty
// sections render as marker-open + blank line + marker-close
// so the structure is preserved on parse + re-render.
//
// Returns an error when the mandatory prompt section is empty
// — Cut 2's bounded primitives can call set_prompt to populate
// before the first render; Cut 1 callers (resolution-task,
// err-task) supply the prompt at construction time.
func RenderTaskSections(sections TaskSections) (string, error) {
	if strings.TrimSpace(sections.Prompt) == "" {
		return "", fmt.Errorf("task schema: prompt section is mandatory and must be non-empty")
	}
	var b strings.Builder
	for i, section := range taskSectionOrder {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(taskMarkerOpen(section))
		b.WriteByte('\n')
		content := strings.TrimRight(sections.content(section), "\n")
		if content != "" {
			b.WriteString(content)
			b.WriteByte('\n')
		}
		b.WriteString(taskMarkerClose(section))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// ParseTaskSections extracts the per-section content from a
// rendered body. Tolerates leading frontmatter (yaml block
// delimited by `---` lines) and any prose preceding the first
// section marker — the resolution-task / err-task writers emit
// frontmatter ahead of the sections, so the parser must skip
// past it before locating the first marker.
//
// Returns an error when any required marker pair is missing or
// when markers appear out of the fixed order — the schema is
// a hard contract for Cut 2's bounded primitives, which assume
// a well-formed structure to mutate against.
func ParseTaskSections(body string) (TaskSections, error) {
	var out TaskSections
	cursor := 0
	for _, section := range taskSectionOrder {
		open := taskMarkerOpen(section)
		close := taskMarkerClose(section)
		openIdx := strings.Index(body[cursor:], open)
		if openIdx < 0 {
			return TaskSections{}, fmt.Errorf("task schema: missing opening marker for section %q", section)
		}
		openIdx += cursor
		closeIdx := strings.Index(body[openIdx+len(open):], close)
		if closeIdx < 0 {
			return TaskSections{}, fmt.Errorf("task schema: missing closing marker for section %q", section)
		}
		closeIdx += openIdx + len(open)
		content := body[openIdx+len(open) : closeIdx]
		// Trim leading newline that the renderer always emits
		// after the open marker + trailing newline before the
		// close so round-trips stay byte-stable through Cut 2's
		// section-rewrite operations.
		content = strings.TrimPrefix(content, "\n")
		content = strings.TrimSuffix(content, "\n")
		switch section {
		case TaskSectionPrompt:
			out.Prompt = content
		case TaskSectionEdges:
			out.Edges = content
		case TaskSectionTodo:
			out.Todo = content
		case TaskSectionFreeform:
			out.Freeform = content
		case TaskSectionNotes:
			out.Notes = content
		}
		cursor = closeIdx + len(close)
	}
	return out, nil
}
