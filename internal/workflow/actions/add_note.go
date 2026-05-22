// add_note runner — attaches a note to an existing
// entity per ADR-0024 §"Output surface". Delegates to a
// NoteWriter interface so the production-side vault
// integration (read-merge-write into vault.Entity.Notes)
// stays out of this package + the test-side fake stays in.
//
// Phase 4.B ships the runner contract + a stub-reject
// production NoteWriter (see stub_writers.go). Phase
// 4.B.2 (planned follow-up) replaces the stub with the
// real vault-backed impl; tests in this package use
// in-memory fakes already.

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// NoteWriter is the entity-note surface the
// add_note runner depends on. Production wires a
// vault-backed implementation that does the
// read-Notes-append-WriteWithCommit dance (mirroring
// the existing handleNotes handler); tests wire an
// in-memory fake.
type NoteWriter interface {
	// AppendNote appends a note with the given body
	// to the entity's vault frontmatter Notes table.
	// EntityID is the canonical id (`<kind>:<slug>`).
	// workflow names the originating workflow so the
	// production vault impl can stamp the Note.Author as
	// `workflow:<name>` per the ADR-0024 Source vocabulary,
	// keeping operator-readable attribution consistent with
	// the bus-event source-tag (`workflow:<name>` is the
	// same vocabulary fill.completed events use).
	//
	// field + kind are the #186 agent-feedback annotations:
	// field scopes the note to a specific entity field
	// (e.g. `birth_date`); kind discriminates `note`
	// (default, operator-level commentary) from
	// `annotation` (agent observation surfaced for operator
	// attention via the read-side kind filter). Both empty
	// preserves the legacy add_note shape.
	AppendNote(ctx context.Context, workflow, entityID, body, field, kind string) error
}

// runAddNote executes one add_note action by
// resolving the target entity id and content from the engine's
// pre-rendered template values (or the raw action fields when
// no renderer is wired), then invoking the NoteWriter. The
// triggering entity id is the target fallback when the
// rendered/raw target is empty.
func (d *dispatcher) runAddNote(ctx context.Context, idx int, _ *parser.Workflow, a *parser.AddNoteAction, dec Decision, act Activation) ActionResult {
	if d.commentWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_note",
			Err:       fmt.Errorf("add_note: no NoteWriter wired (engine constructed without actions.Options.NoteWriter)"),
		}
	}
	content := d.rendered(act, idx, "content", a.Content)
	if strings.TrimSpace(content) == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_note",
			Err:       fmt.Errorf("%w: add_note.content is empty", ErrActionAuthorBug),
		}
	}

	// Target resolution: prefer the engine's rendered Target
	// (or the raw action.Target as a fallback when no renderer
	// is wired), then default to the triggering entity's id
	// when neither is set.
	target := strings.TrimSpace(d.rendered(act, idx, "target", a.Target))
	if target == "" {
		target = dec.EntityID
	}
	if target == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_note",
			Err:       fmt.Errorf("%w: add_note has no target (action.target empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	if err := d.commentWriter.AppendNote(ctx, dec.Workflow, target, content, a.Field, a.Kind); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_note",
			Err:       fmt.Errorf("add_note: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "add_note"}
}
