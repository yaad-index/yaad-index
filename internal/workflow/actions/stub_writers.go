// Phase 4.B stub-reject CommentWriter + GapWriter
// implementations. The runner-side contracts (add_comment,
// add_gap) are real; these production stubs return
// ErrActionNotImplemented so operators see a clear "vault-
// backed impl pending" failure between Phase 4.B and the
// follow-up Phase 4.B.2 that wires the vault.Writer
// integration.
//
// This mirrors PR-82's stub-reject pattern for the action
// dispatcher itself — the same operator-visible failure
// mode (visible error, not silent no-op) across the action
// surface. Tests substitute fake writers that perform real
// in-memory work, so the runner contract is exercised.

package actions

import (
	"context"
	"fmt"
)

// StubCommentWriter is the production-default CommentWriter
// for Phase 4.B. Returns a wrapped ErrActionNotImplemented
// so operators see "vault-backed add_comment pending
// (Phase 4.B.2)" instead of a silent skip.
type StubCommentWriter struct{}

func (StubCommentWriter) AppendComment(_ context.Context, entityID, body string) error {
	return fmt.Errorf("%w: vault-backed add_comment pending (Phase 4.B.2); attempted entity=%s body_len=%d",
		ErrActionNotImplemented, entityID, len(body))
}

// StubGapWriter is the production-default GapWriter for
// Phase 4.B. Same stub-reject shape as StubCommentWriter.
type StubGapWriter struct{}

func (StubGapWriter) AddGap(_ context.Context, entityID, gap string) error {
	return fmt.Errorf("%w: vault-backed add_gap pending (Phase 4.B.2); attempted entity=%s gap=%s",
		ErrActionNotImplemented, entityID, gap)
}
