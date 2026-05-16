// Phase 4.B / 4.C stub-reject CommentWriter, GapWriter, and
// PluginDispatcher implementations. The runner-side contracts
// (add_comment, add_gap, plugin_dispatch) are real; these
// production stubs return ErrActionNotImplemented so operators
// see a clear "real impl pending" failure between the runner
// PRs and the follow-up phases that wire the
// vault.Writer / plugins.Registry integrations (Phase 4.B.2 /
// 4.C.2).
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

// StubPluginDispatcher is the production-default
// PluginDispatcher for Phase 4.C. Returns a wrapped
// ErrActionNotImplemented so operators see "registry-backed
// plugin_dispatch pending (Phase 4.C.2)" rather than a silent
// skip. Real wiring against the plugins.Registry +
// ADR-0022 / ADR-0023 command-and-response protocols lands
// in a follow-up phase.
type StubPluginDispatcher struct{}

func (StubPluginDispatcher) Dispatch(_ context.Context, plugin, command string, args map[string]any) error {
	return fmt.Errorf("%w: registry-backed plugin_dispatch pending (Phase 4.C.2); attempted plugin=%s command=%s args_len=%d",
		ErrActionNotImplemented, plugin, command, len(args))
}
