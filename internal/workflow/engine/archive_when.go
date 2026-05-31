// archive_when post-action hook per ADR-0030. Wires the parser's
// ArchiveWhen predicate + the decision package's EvaluateArchiveWhen
// into the engine's evaluateAndRecord flow. On true the hook
// invokes the same ArchiveWriter the existing archive_entity action
// already uses (vault.Writer.ArchiveWithCommit + store.ArchiveEntity
// behind a per-entity write-lock) so workflow-driven archives land
// in the same archived state as operator-driven + agent-driven
// /v1/entities/{id}/archive calls.
//
// Failure mode is log-and-continue per ADR-0030 §5: the archive
// is advisory housekeeping, and a vault-side miss must not
// invalidate the workflow's actual side-effects (gaps filled,
// edges written, tasks spawned). The audit log carries the WARN
// for operator inspection; reindex reconciles any vault-DB drift.

package engine

import (
	"context"

	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// ArchiveStateProbe is the post-action entity-state surface the
// archive_when hook inspects per ADR-0030. The engine doesn't
// hold a direct store handle (EntityResolver returns a flat CEL
// map without the unfilled-gaps + edge-type fields the predicate
// vocabulary needs), so the probe carries the typed view the
// evaluator consumes. Production wires a thin store.GetEntity
// adapter (see cmd/yaad-index/main.go); tests substitute fakes.
//
// Implementations return an error only on infrastructure failure
// (DB unreachable, etc.). Entity-not-found returns a non-nil view
// with zero state — the predicate evaluates against missing
// fields the same way it does against absent ones, so a deleted-
// between-fill-and-archive race degrades to "predicate false →
// no archive," matching the no-op-on-drift design.
type ArchiveStateProbe interface {
	EntityArchiveState(ctx context.Context, entityID string) (decision.EntityView, error)
}

// maybeArchiveAfterActions evaluates the workflow's ArchiveWhen
// predicate against the post-action entity state and, on true,
// invokes the engine's wired ArchiveWriter to archive the source
// entity per ADR-0030. Bails silently on the common no-op cases:
//
//   - The workflow didn't declare archive_when (most workflows).
//   - The engine wasn't constructed with an archiveStateProbe or
//     archiveWriter (test paths + dev binaries that don't wire
//     either; log a single WARN to surface the misconfiguration
//     so an opted-in workflow doesn't silently fail to archive).
//   - The action set had at least one per-action failure per
//     ADR-0030 §3 ("failed action sets do not trigger archive
//     evaluation — a workflow that errored mid-action chain
//     should leave the source row in place for the operator to
//     inspect"). The caller threads anyActionErrored from the
//     runActions result.
//
// Failure on the probe or writer side logs at WARN per ADR-0030
// §5 and returns; the workflow's overall run is untouched.
func (e *Engine) maybeArchiveAfterActions(ctx context.Context, workflowName, entityID string, predicate *parser.ArchiveWhen, anyActionErrored bool) {
	if predicate == nil {
		return
	}
	if anyActionErrored {
		// ADR-0030 §3: a workflow that errored mid-action chain
		// leaves the source row in place.
		e.logger.Info("workflow archive_when skipped: action set had failures",
			"workflow", workflowName, "entity", entityID)
		return
	}
	if e.archiveStateProbe == nil || e.archiveWriter == nil {
		e.logger.Warn("workflow archive_when skipped: engine missing ArchiveStateProbe or ArchiveWriter wire (production misconfiguration)",
			"workflow", workflowName, "entity", entityID,
			"has_probe", e.archiveStateProbe != nil,
			"has_writer", e.archiveWriter != nil)
		return
	}
	view, err := e.archiveStateProbe.EntityArchiveState(ctx, entityID)
	if err != nil {
		e.logger.Warn("workflow archive_when probe failed; skipping archive",
			"workflow", workflowName, "entity", entityID, "err", err.Error())
		return
	}
	if !decision.EvaluateArchiveWhen(predicate, view) {
		return
	}
	// Author convention: the ArchiveWriter's third arg is a
	// human-readable reason that lands on the audit log; the
	// VaultArchiveWriter wires "workflow:<name>" through to
	// ArchiveWithCommit's author argument via the
	// `<source>:<name>` provenance shape per
	// internal/api/autocommit_messages.go's agentAuthorRef
	// convention. The reason here doubles as the operator-
	// facing trail of "why this archive happened" without
	// needing a separate field.
	if err := e.archiveWriter.ArchiveEntity(ctx, workflowName, entityID, "archive_when predicate"); err != nil {
		// ADR-0030 §5 log-and-continue.
		e.logger.Warn("workflow archive_when archive failed; workflow run unaffected",
			"workflow", workflowName, "entity", entityID, "err", err.Error())
		return
	}
	e.logger.Info("workflow archive_when archived source entity",
		"workflow", workflowName, "entity", entityID)
}
