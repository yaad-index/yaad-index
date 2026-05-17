// plugin_dispatch runner — fires a plugin command from inside
// the workflow per ADR-0024 §"plugin_dispatch execution
// semantics". The call is synchronous from the workflow's
// point of view: the runner blocks on the plugin result up to
// the action's TimeoutSeconds (defaulting to 30s when unset)
// and surfaces a timeout / plugin-error result back to the
// engine for the Phase 5 err-task pattern.
//
// Phase 4.C ships the runner contract + a stub-reject
// production PluginDispatcher (see stub_writers.go). The real
// implementation wiring against the plugins registry +
// ADR-0023 unified plugin response protocol lands as a later
// phase; the stub keeps the operator-visible failure mode
// clear ("vault-backed impl pending" semantics, same shape as
// PR-83's add_note / add_gap stubs).

package actions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// DefaultPluginDispatchTimeout is the per-action timeout the
// runner applies when the workflow's PluginDispatchAction has
// TimeoutSeconds=0 (unset). Mirrors ADR-0024 §"plugin_dispatch
// execution semantics" — "v1 default: 30s".
const DefaultPluginDispatchTimeout = 30 * time.Second

// PluginDispatcher is the plugin-command surface the
// plugin_dispatch runner depends on. Production wires an
// implementation that resolves the named plugin against the
// daemon's plugins.Registry + drives the ADR-0022 command
// protocol over the ADR-0023 unified plugin response wire;
// tests substitute an in-memory fake.
//
// Dispatch is synchronous (the workflow blocks on the plugin
// result up to ctx's deadline). The plugin may do its work
// asynchronously internally, but the runner only sees inline
// result-or-error — the "async result later" framing in
// earlier ADR drafts was retired. On timeout, ctx is
// cancelled and the implementation returns a deadline-exceeded
// error which the runner surfaces verbatim.
type PluginDispatcher interface {
	// Dispatch fires the plugin command with the given args.
	// Returns nil on a successful run (the plugin's response
	// payload is consumed by the dispatcher — Phase 4.C does
	// not yet thread the response back into the activation
	// for downstream actions; that lands when the real
	// dispatcher impl wires the response → bindings path).
	Dispatch(ctx context.Context, plugin, command string, args map[string]any) error
}

// runPluginDispatch executes one plugin_dispatch action:
// enforces the workflow's AllowedPlugins (defense in depth
// alongside the parser-side static check), applies the
// action's timeout, and invokes the configured
// PluginDispatcher.
func (d *dispatcher) runPluginDispatch(ctx context.Context, idx int, wf *parser.Workflow, a *parser.PluginDispatchAction, _ Decision, _ Activation) ActionResult {
	if d.pluginDispatcher == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "plugin_dispatch",
			Err:       fmt.Errorf("plugin_dispatch: no PluginDispatcher wired (engine constructed without actions.Options.PluginDispatcher)"),
		}
	}
	plugin := strings.TrimSpace(a.Plugin)
	if plugin == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "plugin_dispatch",
			Err:       fmt.Errorf("%w: plugin_dispatch.plugin is empty", ErrActionAuthorBug),
		}
	}
	cmd := strings.TrimSpace(a.Command)
	if cmd == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "plugin_dispatch",
			Err:       fmt.Errorf("%w: plugin_dispatch.command is empty", ErrActionAuthorBug),
		}
	}

	// Runtime AllowedPlugins check. Mirrors validatePluginDispatch
	// in the parser package but kicks in regardless of how the
	// action arrived (hot-reload, future dynamically-constructed
	// action lists, etc.) — defense in depth, matching the
	// add_gap.addable_gaps runtime check pattern.
	allowed := make(map[string]struct{}, len(wf.AllowedPlugins))
	for _, p := range wf.AllowedPlugins {
		allowed[p] = struct{}{}
	}
	if _, ok := allowed[plugin]; !ok {
		return ActionResult{
			ActionIdx: idx,
			Type:      "plugin_dispatch",
			Err: fmt.Errorf("%w: plugin_dispatch.plugin %q is not in the workflow's allowed_plugins list",
				ErrActionAuthorBug, plugin),
		}
	}

	timeout := DefaultPluginDispatchTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}
	dispatchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := d.pluginDispatcher.Dispatch(dispatchCtx, plugin, cmd, a.Args); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "plugin_dispatch",
			Err:       fmt.Errorf("plugin_dispatch %s/%s: %w", plugin, cmd, err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "plugin_dispatch"}
}
