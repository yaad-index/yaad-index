// Registry-backed PluginDispatcher (Phase 4.C.2). Replaces
// StubPluginDispatcher at the production wiring layer once the
// daemon's plugins.Registry is available — the runner contract
// from PR-85 stays unchanged.
//
// **Dispatch shape.** Workflows fire commands via the parsed
// PluginDispatchAction (plugin / command / args / timeout).
// The real dispatcher resolves the plugin by name, validates
// the command against the plugin's advertised
// Capabilities.Commands (defense in depth — the workflow
// loader does this at parse time, but a hot-reload that
// changes the plugin's commands list mid-flight is the case
// the runtime check covers), synthesizes the
// `<plugin>: !<command>` invocation per ADR-0022, and drives
// plugin.Fetch with the action's context (which already
// carries the per-action timeout per the Phase 4.C runner).
//
// **Args + result handling — deliberately narrow in v1.** The
// PluginDispatchAction.Args map exists in the parser but
// ADR-0022 doesn't yet specify a structured-args wire format
// — plugin authors carry per-plugin args via stdin / env per
// the subprocess plugin protocol. This impl DROPS the args
// map (no logging, no invocation-URL concatenation); a future
// ADR-0022 extension will define the wire format + the
// dispatcher gains the threading then. The TestArgsAreOpaque
// test pins the current behavior so a future change has to
// touch the contract explicitly. The plugin.Fetch result is
// consumed by the dispatcher — Phase 5+ threads the response
// back into Activation.Bindings for downstream actions
// referencing the dispatched result; v1 fire-and-success
// semantics keep the runner contract narrow.

package actions

import (
	"context"
	"errors"
	"fmt"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// ErrUnknownPlugin is returned when the action's Plugin name
// doesn't resolve in the registry. The parser load-time check
// usually catches this (Plugin must be in AllowedPlugins +
// each AllowedPlugins entry must resolve in the registry at
// load), but a hot-reload that removes a plugin between load
// and execute lands here.
var ErrUnknownPlugin = errors.New("actions: plugin not registered")

// ErrUnknownCommand is returned when the action's Command
// isn't in the plugin's advertised Capabilities().Commands
// list. The parser doesn't (and can't, without consulting the
// runtime registry) validate the command at load time, so the
// runtime check is the only enforcement point.
var ErrUnknownCommand = errors.New("actions: plugin does not advertise this command")

// PluginRegistry is the narrow Registry surface
// RegistryPluginDispatcher depends on. Production wires
// *plugins.Registry directly; tests substitute fakes with
// just LookupByName.
type PluginRegistry interface {
	LookupByName(name string) (plugins.Plugin, bool)
}

// RegistryPluginDispatcher implements PluginDispatcher against
// the daemon's plugins.Registry per ADR-0022 §"Routing-time
// validation" + ADR-0023's single-envelope Fetch contract.
type RegistryPluginDispatcher struct {
	registry PluginRegistry
}

// NewRegistryPluginDispatcher constructs a real PluginDispatcher
// backed by registry. Returns an error when registry is nil so
// the daemon's main wiring surfaces the mis-config at boot
// rather than at first dispatch.
func NewRegistryPluginDispatcher(registry PluginRegistry) (*RegistryPluginDispatcher, error) {
	if registry == nil {
		return nil, errors.New("RegistryPluginDispatcher: registry is required")
	}
	return &RegistryPluginDispatcher{registry: registry}, nil
}

// Dispatch implements PluginDispatcher. Resolves the plugin,
// validates the command, builds the invocation, and drives
// plugin.Fetch.
func (d *RegistryPluginDispatcher) Dispatch(ctx context.Context, plugin, command string, _ map[string]any) error {
	p, ok := d.registry.LookupByName(plugin)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPlugin, plugin)
	}
	caps := p.Capabilities()
	if !containsCommand(caps.Commands, command) {
		return fmt.Errorf("%w: plugin=%s command=%s (advertised: %v)",
			ErrUnknownCommand, plugin, command, caps.Commands)
	}
	invocation := fmt.Sprintf("%s: !%s", plugin, command)
	if _, err := p.Fetch(ctx, invocation); err != nil {
		return fmt.Errorf("plugin.Fetch %s: %w", invocation, err)
	}
	return nil
}

// containsCommand checks whether command is in cmds. Exact-
// match per ADR-0022 §4 "Command-shape requests" — no
// substring / prefix matches.
func containsCommand(cmds []string, command string) bool {
	for _, c := range cmds {
		if c == command {
			return true
		}
	}
	return false
}
