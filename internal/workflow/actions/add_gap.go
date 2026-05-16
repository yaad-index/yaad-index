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
	// twice should not duplicate. workflow names the
	// originating workflow so the production vault impl can
	// stamp the commit author as `workflow:<name>` per the
	// ADR-0024 Source vocabulary.
	AddGap(ctx context.Context, workflow, entityID, gap string) error
}

// runAddGap executes one add_gap action: enforces the
// workflow's addable_gaps vocabulary + invokes the
// GapWriter against the resolved entity id.
func (d *dispatcher) runAddGap(ctx context.Context, idx int, wf *parser.Workflow, a *parser.AddGapAction, dec Decision, act Activation) ActionResult {
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

	// Target resolution: prefer the engine's rendered Entity
	// template (or the raw action.Entity as a fallback when no
	// renderer is wired), then default to the triggering
	// entity's id when neither is set.
	target := strings.TrimSpace(d.rendered(act, idx, "entity", a.Entity))
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

	if err := d.gapWriter.AddGap(ctx, dec.Workflow, target, gap); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_gap",
			Err:       fmt.Errorf("add_gap: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "add_gap"}
}
