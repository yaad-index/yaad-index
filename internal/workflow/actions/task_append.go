// task_append runner — appends a content line to a named
// section of the workflow's canonical task file per
// ADR-0024 §"Output surface". The dispatcher above routes
// task_append actions here; the runner picks up the engine's
// pre-rendered Content template (or falls back to the raw
// CEL source when no renderer is wired) + invokes the
// TaskWriter.

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
func (d *dispatcher) runTaskAppend(ctx context.Context, idx int, _ *parser.Workflow, a *parser.TaskAppendAction, dec Decision, act Activation) ActionResult {
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

	content := d.rendered(act, idx, "content", a.Content)

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
