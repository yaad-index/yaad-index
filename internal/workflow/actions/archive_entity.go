// archive_entity runner — exposes the operator-side archive
// surface (ADR-0018) inside the workflow action vocabulary per
// #150. The runner resolves the target entity id + audit
// reason via CEL, then hands the resolved pair to the
// ArchiveWriter; the writer owns the vault move + DB toggle +
// per-entity write-lock (AcquireWithTimeout — same async-side
// shape as the other workflow action writers post-PR-153).
//
// **Idempotence + soft-skip contracts.** The writer guarantees
// already-archived = no-op (no error, no double-stamp); the
// runner forwards that contract upstream. Entity-not-found is
// a soft skip — the runner logs and returns success so the
// workflow chain continues, because the entity may have been
// archived by another path (a sibling workflow, an
// operator-side action) between the trigger event and this
// action firing.

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// ArchiveWriter is the archive surface the archive_entity
// runner depends on. Production wires VaultArchiveWriter
// (vault.Writer.ArchiveWithCommit + store.Store.ArchiveEntity
// behind a per-entity write-lock via AcquireWithTimeout, per
// PR-153). Tests wire StubArchiveWriter that records calls.
//
// Implementations MUST be idempotent on already-archived
// entities (no error, no audit double-stamp) and MUST treat
// not-found as a soft skip (return nil so the workflow chain
// continues). The workflow caller cannot meaningfully retry on
// either shape — both are convergent states.
type ArchiveWriter interface {
	ArchiveEntity(
		ctx context.Context,
		workflow, entityID, reason string,
	) error
}

// runArchiveEntity executes one archive_entity action: resolves
// the entity id + reason via the engine's pre-rendered
// templates, falls back to the triggering entity id when the
// rendered entity is empty, then invokes ArchiveWriter.
func (d *dispatcher) runArchiveEntity(ctx context.Context, idx int, _ *parser.Workflow, a *parser.ArchiveEntityAction, dec Decision, act Activation) ActionResult {
	if d.archiveWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "archive_entity",
			Err:       fmt.Errorf("archive_entity: no ArchiveWriter wired (engine constructed without actions.Options.ArchiveWriter)"),
		}
	}

	entityID := strings.TrimSpace(d.rendered(act, idx, "entity", a.Entity))
	if entityID == "" {
		entityID = dec.EntityID
	}
	if entityID == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "archive_entity",
			Err:       fmt.Errorf("%w: archive_entity has no target (action.entity empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	reason := strings.TrimSpace(d.rendered(act, idx, "reason", a.Reason))

	if err := d.archiveWriter.ArchiveEntity(ctx, dec.Workflow, entityID, reason); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "archive_entity",
			Err:       fmt.Errorf("archive_entity: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "archive_entity"}
}
