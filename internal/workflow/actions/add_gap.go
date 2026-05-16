// add_gap runner — injects a gap onto an entity from the
// workflow's action stage per ADR-0024 §"Output surface".
// The gap is permanent on the entity (vault-as-truth per
// ADR-0008); subsequent ingests / fills reuse the stored
// gap rather than re-deriving.
//
// **Constraint enforcement.** Per ADR-0024 §"Constraints
// on add_gap", the gap value MUST appear in the workflow's
// declared addable_gaps vocabulary. The parser enforces
// this statically at workflow-load time (validateAddGap),
// AND the runner enforces it again at execute time —
// defense in depth, and the runtime check catches the
// edge case where the parser's static check has drifted
// from runtime semantics (e.g. a workflow's addable_gaps
// shrunk in a hot-reload but the action references the
// dropped gap).

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// GapWriter is the gap-injection surface the add_gap
// runner depends on. Production wires a vault-backed
// implementation that appends to vault.Entity.Gaps +
// initializes the GapState entry + WriteWithCommit's
// the change (mirroring handleEntityOperatorFill's
// gap-state writes); tests wire an in-memory fake.
type GapWriter interface {
	// AddGap appends the named gap to the entity's vault
	// frontmatter Gaps list. EntityID is the canonical id
	// (`<kind>:<slug>`). Idempotent — adding the same gap
	// twice should not duplicate.
	AddGap(ctx context.Context, entityID, gap string) error
}

// runAddGap executes one add_gap action: enforces the
// workflow's addable_gaps vocabulary + invokes the
// GapWriter against the resolved entity id.
func (d *dispatcher) runAddGap(ctx context.Context, idx int, wf *parser.Workflow, a *parser.AddGapAction, dec Decision, _ Activation) ActionResult {
	if d.gapWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_gap",
			Err:       fmt.Errorf("add_gap: no GapWriter wired (engine constructed without actions.Options.GapWriter)"),
		}
	}
	gap := strings.TrimSpace(a.Gap)
	if gap == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_gap",
			Err:       fmt.Errorf("%w: add_gap.gap is empty", ErrActionAuthorBug),
		}
	}

	// Runtime vocabulary check. Mirrors validateAddGap in
	// the parser package but kicks in regardless of how
	// the action arrived (hot-reload, future
	// dynamically-constructed action lists, etc.). Defense
	// in depth — gives operators a clean error class for
	// the rare drift case rather than letting an
	// invariant violation reach the vault writer.
	addable := make(map[string]struct{}, len(wf.AddableGaps))
	for _, g := range wf.AddableGaps {
		addable[g] = struct{}{}
	}
	if _, ok := addable[gap]; !ok {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_gap",
			Err: fmt.Errorf("%w: add_gap.gap %q is not in the workflow's addable_gaps vocabulary",
				ErrActionAuthorBug, gap),
		}
	}

	// Target resolution: a.Entity is a CEL expression
	// (Phase 4.B+ — the engine layer renders it before
	// invoking the runner); the current cut treats the
	// raw string verbatim when set, otherwise falls back
	// to the triggering entity. CEL rendering is a
	// carry-over from PR-82 review.
	target := strings.TrimSpace(a.Entity)
	if target == "" {
		target = dec.EntityID
	}
	if target == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_gap",
			Err:       fmt.Errorf("%w: add_gap has no target (action.entity empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	if err := d.gapWriter.AddGap(ctx, target, gap); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_gap",
			Err:       fmt.Errorf("add_gap: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "add_gap"}
}
