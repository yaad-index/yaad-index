// Bounded task-body primitives per #337 Cut 2. The five
// section-aware operations callers reach for instead of
// rewriting the markdown body free-form. Each takes the
// current body, parses it via task_schema's
// ParseTaskSections, mutates the targeted section's content,
// and re-renders. The parse→mutate→render round-trip is
// byte-stable by construction (TestParseRenderRoundTrip in
// task_schema_test.go pins this), so repeated primitive
// invocations against the same body don't drift output even
// when the touched section's content stays identical.
//
// Commutativity carve-out from the #337 spec:
//
//   - Set-shape (AddCheckbox, AddNote, AddEdge): two calls
//     against the same body produce the same end-state
//     regardless of insertion order modulo the chosen
//     in-section ordering. AddCheckbox + AddNote are
//     idempotent on duplicate items (skip-if-present);
//     AddEdge is a stub deferring the atomicity contract
//     until Cut 3.
//   - Ordered-append (AppendFreeform): order-of-calls
//     determines order-of-text. AppendFreeform with the
//     same text twice produces two paragraphs (history-as-
//     event-log).
//   - Replace-shape (SetPrompt): last write wins. Two
//     SetPrompt calls with different prompts end up with
//     the second one's content. SetPrompt is the only
//     replace-shape primitive in v1.
//
// The primitives operate on the section-bearing portion of
// a task body. When the caller's body has a leading yaml
// frontmatter block (resolution-task + err-task both emit
// one), the helpers split it off via splitFrontmatter,
// route the mutation through the section parser/renderer,
// then re-concatenate. Bodies without frontmatter route
// straight through. This lets workflow-internal queue
// items (no frontmatter) and writer-emitted tasks (with
// frontmatter) share the same primitive surface.

package actions

import (
	"fmt"
	"strings"
)

// SetPrompt replaces the prompt section's content with the
// supplied text. Returns an error when prompt is empty —
// the schema treats prompt as mandatory and a clearing
// operation would render the body unparseable on the next
// round-trip.
//
// Caller-side commutativity: SetPrompt is replace-shape;
// the final SetPrompt call wins regardless of how many
// earlier calls landed.
func SetPrompt(body, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("SetPrompt: prompt is mandatory and must be non-empty")
	}
	return mutateTaskBody(body, func(s *TaskSections) {
		s.Prompt = strings.TrimRight(prompt, "\n")
	})
}

// AddCheckbox appends a `- [ ] <item>` line to the todo
// section. Idempotent: when an exact match (post-TrimSpace
// per-line) already exists in the section, the body
// returns unchanged. Per the #337 commutativity carve-out
// the todo section is set-shape, so two AddCheckbox calls
// with the same item end up with one entry regardless of
// order.
//
// The line shape is the standard markdown unchecked
// checkbox; callers wanting a pre-checked item can compose
// the raw line with `- [x] ...` and route through
// addRawLineToTodo (unexported — operators check items via
// HTTP / MCP, not via this primitive in v1).
func AddCheckbox(body, item string) (string, error) {
	item = strings.TrimSpace(item)
	if item == "" {
		return "", fmt.Errorf("AddCheckbox: item is required")
	}
	line := "- [ ] " + item
	return mutateTaskBody(body, func(s *TaskSections) {
		if containsLine(s.Todo, line) {
			return
		}
		s.Todo = appendSectionLine(s.Todo, line)
	})
}

// AddNote appends a single note line to the notes section.
// Idempotent on exact-duplicate line: same as AddCheckbox,
// skips when the line already exists. The notes section
// commutativity carve-out applies — order doesn't matter
// for set membership.
//
// Note content should be a single line (the err-task and
// add_comment surface both compose single-line entries);
// embedded newlines aren't stripped here, but the parser
// would still round-trip them through if the caller
// supplies multi-line content. Cut 1's
// appendErrFailureLine already collapses newlines on the
// upstream side; this helper trusts the caller.
func AddNote(body, note string) (string, error) {
	note = strings.TrimRight(note, "\n")
	if strings.TrimSpace(note) == "" {
		return "", fmt.Errorf("AddNote: note is required")
	}
	return mutateTaskBody(body, func(s *TaskSections) {
		if containsLine(s.Notes, note) {
			return
		}
		s.Notes = appendSectionLine(s.Notes, note)
	})
}

// AppendFreeform appends free-form prose to the freeform
// section. Unlike the set-shape primitives, this is
// ordered-append — the same text twice produces two
// paragraphs separated by a blank line. Order-of-calls
// determines order-of-text per the #337 carve-out.
//
// Multi-line input is accepted as-is; the helper inserts
// a blank line between the existing freeform content and
// the new paragraph when both are non-empty so the rendered
// markdown produces visually-distinct blocks. Trailing
// whitespace on the input is stripped to keep round-trips
// byte-stable.
func AppendFreeform(body, text string) (string, error) {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("AppendFreeform: text is required")
	}
	return mutateTaskBody(body, func(s *TaskSections) {
		if s.Freeform == "" {
			s.Freeform = text
			return
		}
		s.Freeform = s.Freeform + "\n\n" + text
	})
}

// AddEdge is a STUB per #337 Cut 2. The edges section's
// atomicity contract (graph-edge-write vs section-regen,
// see Cut 1's open question) is unresolved; v1 of this
// primitive returns the body unchanged + a non-error so
// callers can wire the surface without depending on the
// final semantics. Cut 3 lands the real implementation
// once the eventually-consistent-vs-strict-atomic decision
// settles.
//
// The signature accepts the same shape the eventual
// implementation will (canonical entity id + optional
// edge type) so callers can write against it now and the
// behavior change in Cut 3 stays source-compatible.
func AddEdge(body, entityID, edgeType string) (string, error) {
	if strings.TrimSpace(entityID) == "" {
		return "", fmt.Errorf("AddEdge: entityID is required")
	}
	// Section-regen-from-graph is the Cut 3 implementation per
	// #337's eventually-consistent-view note. v1 stub returns
	// the body unchanged — caller's edge write should go
	// through the existing edgewrite.Service.CreateEdge path
	// and the section view will catch up when Cut 3 wires the
	// regen hook.
	_ = entityID
	_ = edgeType
	return body, nil
}

// mutateTaskBody is the shared parse → mutate → render
// helper every primitive routes through. Splits leading
// frontmatter (if present), parses the section block,
// applies the caller's mutation, re-renders, and
// re-concatenates.
func mutateTaskBody(body string, mutate func(*TaskSections)) (string, error) {
	frontmatter, sectionsBody, err := splitFrontmatter(body)
	if err != nil {
		return "", fmt.Errorf("task primitive: %w", err)
	}
	sections, err := ParseTaskSections(sectionsBody)
	if err != nil {
		return "", fmt.Errorf("task primitive: %w", err)
	}
	mutate(&sections)
	rendered, err := RenderTaskSections(sections)
	if err != nil {
		return "", fmt.Errorf("task primitive: %w", err)
	}
	return frontmatter + rendered, nil
}

// containsLine reports whether the section's content
// already includes the given line (post-TrimSpace per-
// line comparison). Used by the idempotent set-shape
// primitives (AddCheckbox, AddNote) to skip exact
// duplicates.
func containsLine(content, line string) bool {
	target := strings.TrimSpace(line)
	if target == "" {
		return false
	}
	for _, existing := range strings.Split(content, "\n") {
		if strings.TrimSpace(existing) == target {
			return true
		}
	}
	return false
}

// appendSectionLine adds a new line to a section's content
// with the canonical separator: blank existing → just the
// line; non-blank → existing + newline + line. Trailing
// newlines on the existing content are normalized so
// repeated appends don't accumulate gaps.
func appendSectionLine(existing, line string) string {
	existing = strings.TrimRight(existing, "\n")
	if existing == "" {
		return line
	}
	return existing + "\n" + line
}
