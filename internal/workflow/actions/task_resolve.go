// task_resolve runner — flips `- [ ]` → `- [x]` or removes
// the first content-prefix-matched line in ANOTHER workflow's
// task file per #266. The dispatcher routes task_resolve
// actions here; the runner picks up the engine's pre-rendered
// Subject + MatchKey CEL templates + invokes TaskWriter's
// ResolveTaskLine.

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// runTaskResolve executes one task_resolve action. Reuses the
// dispatcher's TaskWriter dependency; the same writer that
// owns task_append owns the resolve path so file-locking +
// atomic-write semantics stay uniform.
func (d *dispatcher) runTaskResolve(ctx context.Context, idx int, _ *parser.Workflow, a *parser.TaskResolveAction, _ Decision, act Activation) ActionResult {
	if d.taskWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_resolve",
			Err:       fmt.Errorf("task_resolve: no TaskWriter wired (engine constructed without actions.Options.TaskWriter)"),
		}
	}
	if a.Workflow == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_resolve",
			Err:       fmt.Errorf("%w: task_resolve.workflow is empty", ErrActionAuthorBug),
		}
	}
	if strings.TrimSpace(a.Section) == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_resolve",
			Err:       fmt.Errorf("%w: task_resolve.section is empty", ErrActionAuthorBug),
		}
	}

	subject := d.rendered(act, idx, "subject", a.Subject)
	matchKey := d.rendered(act, idx, "match_key", a.MatchKey)

	if strings.TrimSpace(subject) == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_resolve",
			Err:       fmt.Errorf("%w: task_resolve.subject rendered to empty", ErrActionAuthorBug),
		}
	}
	if matchKey == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_resolve",
			Err:       fmt.Errorf("%w: task_resolve.match_key rendered to empty", ErrActionAuthorBug),
		}
	}

	if err := d.taskWriter.ResolveTaskLine(
		ctx,
		a.Workflow,
		subject,
		a.Section,
		matchKey,
		a.Mode,
	); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "task_resolve",
			Err:       fmt.Errorf("task_resolve: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "task_resolve"}
}
