// task_append runner — appends a content line to a named
// section of the workflow's canonical task file per
// ADR-0024 §"Output surface". The dispatcher above routes
// task_append actions here; the runner re-renders the
// content template against the activation + invokes the
// TaskWriter.
//
// **What the runner does NOT do.** The template re-render
// for the action's `content` field is happening here
// against the workflow's compiled programs cache (the
// engine pre-compiled the content template at registration
// time per Phase 3.B). Phase 4.A's MVP cut: the engine
// passes the ALREADY-EVALUATED content string to the
// runner via a wrapped interface, sidestepping the need
// for the runner to know about CEL. Future cleanup can
// expose the template surface directly here.
//
// For 4.A: the runner accepts the raw content (CEL
// template) from the parsed Action + delegates the
// re-render to a `Renderer` if provided; otherwise it
// uses the content verbatim (which works for static-text
// content lines, the simplest test case + a sizable
// subset of real workflows).

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// runTaskAppend executes one task_append action. Uses the
// dispatcher's TaskWriter dependency; if TaskWriter is nil
// the result names a configuration error so operators see
// "engine started without a task writer wired" rather than
// silent skip.
func (d *dispatcher) runTaskAppend(ctx context.Context, idx int, wf *parser.Workflow, a *parser.TaskAppendAction, dec Decision, _ Activation) ActionResult {
	if d.taskWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_append",
			Err:       fmt.Errorf("task_append: no TaskWriter wired (engine constructed without actions.Options.TaskWriter)"),
		}
	}
	if strings.TrimSpace(a.Section) == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_append",
			Err:       fmt.Errorf("%w: task_append.section is empty", ErrActionAuthorBug),
		}
	}

	// Content rendering: Phase 4.A passes the raw content
	// verbatim. For workflows whose `content` field is a
	// static line (no CEL placeholders) this is correct.
	// CEL-template content rendering is a follow-up that
	// couples to the decision package's Program cache —
	// the engine layer pre-compiles the content template
	// at registration time + passes the rendered string
	// through a future activation-aware wrapper. The
	// current cut intentionally keeps the surface narrow.
	content := a.Content

	ifAlreadyPresent := a.IfAlreadyPresent
	if ifAlreadyPresent == "" {
		ifAlreadyPresent = parser.IfAlreadyPresentSkip
	}

	if err := d.taskWriter.AppendTaskSection(
		ctx,
		dec.Workflow,
		dec.Subject,
		a.Section,
		content,
		ifAlreadyPresent,
	); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_append",
			Err:       fmt.Errorf("task_append: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "task_append"}
}
