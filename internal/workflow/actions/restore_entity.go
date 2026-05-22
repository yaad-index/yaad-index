// restore_entity runner — exposes the operator-side restore
// surface (ADR-0018 inverse) inside the workflow action
// vocabulary per ADR-0024's 2026-05-21 amendment. Mirror of
// archive_entity: resolves the target entity id + audit
// reason via CEL, then hands the resolved pair to the
// RestoreWriter; the writer owns the vault move + DB toggle +
// per-entity write-lock (same AcquireWithTimeout shape as
// VaultArchiveWriter per PR-153).
//
// **Idempotence + soft-skip contracts.** The writer guarantees
// already-active = no-op (no error, no double-toggle); the
// runner forwards that contract upstream. Entity-not-found is
// a soft skip — the runner logs and returns success so the
// workflow chain continues, because the entity may have been
// restored by another path (a sibling workflow, an
// operator-side action) between the trigger event and this
// action firing.

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// RestoreWriter is the restore surface the restore_entity
// runner depends on. Production wires VaultRestoreWriter
// (vault.Writer.RestoreWithCommit + store.Store.RestoreEntity
// behind a per-entity write-lock via AcquireWithTimeout).
// Tests wire stubs that record calls.
//
// Implementations MUST be idempotent on already-active
// entities (no error, no double-toggle) and MUST treat
// not-found as a soft skip (return nil so the workflow chain
// continues). The workflow caller cannot meaningfully retry on
// either shape — both are convergent states.
type RestoreWriter interface {
	RestoreEntity(
		ctx context.Context,
		workflow, entityID, reason string,
	) error
}

// runRestoreEntity executes one restore_entity action: resolves
// the entity id + reason via the engine's pre-rendered
// templates, falls back to the triggering entity id when the
// rendered entity is empty, then invokes RestoreWriter.
func (d *dispatcher) runRestoreEntity(ctx context.Context, idx int, _ *parser.Workflow, a *parser.RestoreEntityAction, dec Decision, act Activation) ActionResult {
	if d.restoreWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "restore_entity",
			Err:       fmt.Errorf("restore_entity: no RestoreWriter wired (engine constructed without actions.Options.RestoreWriter)"),
		}
	}

	entityID := strings.TrimSpace(d.rendered(act, idx, "entity", a.Entity))
	if entityID == "" {
		entityID = dec.EntityID
	}
	if entityID == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "restore_entity",
			Err:       fmt.Errorf("%w: restore_entity has no target (action.entity empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	reason := strings.TrimSpace(d.rendered(act, idx, "reason", a.Reason))

	if err := d.restoreWriter.RestoreEntity(ctx, dec.Workflow, entityID, reason); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "restore_entity",
			Err:       fmt.Errorf("restore_entity: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "restore_entity"}
}
