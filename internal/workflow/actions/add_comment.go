// add_comment runner — attaches a comment to an existing
// entity per ADR-0024 §"Output surface". Delegates to a
// CommentWriter interface so the production-side vault
// integration (read-merge-write into vault.Entity.Comments)
// stays out of this package + the test-side fake stays in.
//
// Phase 4.B ships the runner contract + a stub-reject
// production CommentWriter (see stub_writers.go). Phase
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

// CommentWriter is the entity-comment surface the
// add_comment runner depends on. Production wires a
// vault-backed implementation that does the
// read-Comments-append-WriteWithCommit dance (mirroring
// the existing handleComments handler); tests wire an
// in-memory fake.
type CommentWriter interface {
	// AppendComment appends a comment with the given body
	// to the entity's vault frontmatter Comments table.
	// EntityID is the canonical id (`<kind>:<slug>`).
	AppendComment(ctx context.Context, entityID, body string) error
}

// runAddComment executes one add_comment action by
// resolving the target entity id from the action
// (defaulting to the triggering entity's id when omitted)
// + invoking the CommentWriter.
func (d *dispatcher) runAddComment(ctx context.Context, idx int, _ *parser.Workflow, a *parser.AddCommentAction, dec Decision, _ Activation) ActionResult {
	if d.commentWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_comment",
			Err:       fmt.Errorf("add_comment: no CommentWriter wired (engine constructed without actions.Options.CommentWriter)"),
		}
	}
	if strings.TrimSpace(a.Content) == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_comment",
			Err:       fmt.Errorf("%w: add_comment.content is empty", ErrActionAuthorBug),
		}
	}

	// Target resolution: Phase 4.B passes the action's
	// Target verbatim when set, otherwise falls back to the
	// triggering entity's id. CEL-template rendering for
	// the Target + Content fields is a carry-over from
	// PR-82 review — lands alongside Phase 4.C / 4.B.2
	// once the engine's program cache is exposed to action
	// runners.
	target := strings.TrimSpace(a.Target)
	if target == "" {
		target = dec.EntityID
	}
	if target == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_comment",
			Err:       fmt.Errorf("%w: add_comment has no target (action.target empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	if err := d.commentWriter.AppendComment(ctx, target, a.Content); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_comment",
			Err:       fmt.Errorf("add_comment: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "add_comment"}
}
