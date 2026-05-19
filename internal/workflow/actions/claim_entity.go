// claim_entity runner — the #169 explicit-claim signal. A
// workflow's `- claim_entity: {}` action sets the per-event
// claim flag in the returned ActionResult; the engine reads
// the flag after the per-workflow action chain completes and
// halts further workflow dispatch for the current event (no
// remaining pass-1 workflows fire, no pass-2 catch_all
// fires).
//
// **No external surface.** Claim is pure engine queue state —
// there's no vault write, no DB toggle, no bus emission tied
// to the action itself. The engine's per-event bookkeeping
// (which workflow claimed, recorded in the queue's claimed_by
// slot) is the only observable side effect. That's why the
// runner doesn't need a writer interface like the other
// vault-touching primitives.
//
// **Position in the action list.** Workflow authors can place
// claim_entity anywhere in their actions block; the engine
// runs every action declared before it, then halts after the
// claim_entity result lands. A workflow that wants to do
// task_append + claim semantically reads as
//
//   actions:
//     - task_append: {...}
//     - claim_entity: {}
//
// — the task lands AND the chain stops. (The engine's
// stop-after-claim logic gates further WORKFLOWS, not further
// actions within the same workflow; intra-workflow ordering
// stays deterministic per the existing dispatcher
// best-effort-across-actions contract.)

package actions

// runClaimEntity is the per-action runner. Stateless +
// dependency-free — sets ActionResult.Claim true so the engine
// can branch on the post-chain claim status. No engine state
// is mutated here; the engine owns the "stop further
// workflows" decision based on the flag.
func (d *dispatcher) runClaimEntity(idx int) ActionResult {
	return ActionResult{
		ActionIdx: idx,
		Type:      "claim_entity",
		Claim:     true,
	}
}
