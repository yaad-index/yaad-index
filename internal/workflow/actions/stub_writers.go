// Phase 4.B / 4.C stub-reject NoteWriter, GapWriter, and
// PluginDispatcher implementations. The runner-side contracts
// (add_note, add_gap, plugin_dispatch) are real; these
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

// StubNoteWriter is the Phase 4.B production-default
// NoteWriter retained for tests + dev binaries running
// without a vault. Returns a wrapped ErrActionNotImplemented
// so operators see "vault-backed add_note pending" instead
// of a silent skip. Phase 4.B.2 replaces this with
// VaultNoteWriter at the production wiring layer; this
// stub stays on as the zero-config default.
type StubNoteWriter struct{}

func (StubNoteWriter) AppendNote(_ context.Context, workflow, entityID, body string) error {
	return fmt.Errorf("%w: vault-backed add_note not wired; attempted workflow=%s entity=%s body_len=%d",
		ErrActionNotImplemented, workflow, entityID, len(body))
}

// StubGapWriter is the Phase 4.B production-default GapWriter
// retained for tests + dev binaries running without a vault.
// Same stub-reject shape as StubNoteWriter.
type StubGapWriter struct{}

func (StubGapWriter) AddGap(_ context.Context, workflow, entityID, gap string, _ GapInjection) error {
	return fmt.Errorf("%w: vault-backed add_gap not wired; attempted workflow=%s entity=%s gap=%s",
		ErrActionNotImplemented, workflow, entityID, gap)
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

// StubPropertyWriter is the production-default PropertyWriter
// retained for tests + dev binaries running without a vault.
// Same stub-reject shape as StubNoteWriter / StubGapWriter.
type StubPropertyWriter struct{}

func (StubPropertyWriter) SetProperties(_ context.Context, workflow, entityID string, fields map[string]any) error {
	return fmt.Errorf("%w: vault-backed set_property not wired; attempted workflow=%s entity=%s fields=%d",
		ErrActionNotImplemented, workflow, entityID, len(fields))
}

// StubEdgeWriter is the production-default EdgeWriter retained
// for tests + dev binaries running without a vault. Same
// stub-reject shape as the other stub writers.
type StubEdgeWriter struct{}

func (StubEdgeWriter) AddCanonicalEdge(_ context.Context, workflow, sourceID, edgeType, targetKind, targetName string, _ map[string]string) error {
	return fmt.Errorf("%w: vault-backed add_canonical_edge not wired; attempted workflow=%s source=%s edge_type=%s target=%s:%s",
		ErrActionNotImplemented, workflow, sourceID, edgeType, targetKind, targetName)
}

// StubArchiveWriter is the production-default ArchiveWriter
// retained for tests + dev binaries running without a vault.
// Same stub-reject shape as the other stub writers per #150.
type StubArchiveWriter struct{}

func (StubArchiveWriter) ArchiveEntity(_ context.Context, workflow, entityID, reason string) error {
	return fmt.Errorf("%w: vault-backed archive_entity not wired; attempted workflow=%s entity=%s reason=%q",
		ErrActionNotImplemented, workflow, entityID, reason)
}
