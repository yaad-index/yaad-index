package api

import (
	"fmt"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// routingValidationError is the typed error returned by
// validateRouting when an input carries a recognizable plugin
// namespace whose declared shape doesn't accept the input. Not all
// inputs are validated — see validateRouting's godoc for the
// fall-through cases.
type routingValidationError struct {
	// Code is the machine-readable error code; one of
	// "invalid_input" (URL-shape regex miss / command not in list)
	// or "plugin_not_found" (command-shape against an unregistered
	// plugin name).
	Code string
	// Status is the HTTP status the handler should write — 400 for
	// invalid_input, 404 for plugin_not_found.
	Status int
	// Message is the human-readable reason naming the plugin and
	// what was wrong.
	Message string
}

func (e *routingValidationError) Error() string { return e.Message }

// validateRouting runs ADR-0022 §4 routing-time validation against
// an /v1/ingest input. The cheap shape-check rejects inputs that
// can't possibly succeed before the daemon's job-system / spawn
// path runs — saving subprocess wall-clock on the unhappy path.
//
// Behavior by input shape:
//
// - Command-shape (`<plugin>: !<command>` or
// `<plugin>/<instance>: !<command>`): the namespace MUST be a
// registered plugin. Lookup misses → 404 plugin_not_found.
// Lookup hits + command in plugin.Capabilities().Commands → pass.
// Lookup hits + command NOT in list → 400 invalid_input.
// Instance qualifier (when present) is NOT validated here — the
// dispatch layer in handleIngest resolves the instance against
// the operator's configured list and surfaces a separate error
// for unknown instances per ADR-0028 §4.
//
// - URL-shape (`<plugin>: <pattern>` or full URL): if the
// namespace prefix matches a registered plugin's Name(),
// validate the full input against that plugin's url_patterns
// via plugin.Match. Match miss → 400 invalid_input. If the
// namespace doesn't match any registered plugin name (e.g. the
// namespace is a URL scheme like "https"), validateRouting
// returns nil — the existing first-match-wins registry.Lookup
// walk in handleIngest decides dispatch.
//
// Returning nil means "passed validation" (or "not applicable").
// Returning *routingValidationError means "reject with the named
// status / code / message."
func validateRouting(registry *plugins.Registry, input string) *routingValidationError {
	inv := plugins.ParseInvocation(input)
	if inv.Plugin == "" {
		// No `<prefix>:` separator; nothing to validate against a
		// named plugin. Fall through to the registry.Lookup walk.
		return nil
	}

	plugin, found := registry.LookupByName(inv.Plugin)

	switch inv.Shape {
	case plugins.InvocationCommand:
		// Command-shape requires a registered plugin name. A
		// command-shape input pointing at an unknown namespace is a
		// hard error — there's no fall-through "maybe another
		// plugin's url_patterns will accept it" since the `!` sigil
		// is unambiguous.
		if !found {
			return &routingValidationError{
				Code: "plugin_not_found",
				Status: 404,
				Message: fmt.Sprintf("no plugin registered with name %q", inv.Plugin),
			}
		}
		caps := plugin.Capabilities()
		for _, cmd := range caps.Commands {
			if cmd.Name == inv.Command {
				return nil
			}
		}
		return &routingValidationError{
			Code: "invalid_input",
			Status: 400,
			Message: fmt.Sprintf("plugin %q has no command %q (declared commands: %v)", inv.Plugin, inv.Command, commandNames(caps.Commands)),
		}

	case plugins.InvocationURL:
		// URL-shape with a recognized plugin namespace: the plugin's
		// url_patterns MUST accept the full input. The namespace
		// could also be a URL scheme like "https" — those don't
		// match a registered plugin and fall through to existing
		// dispatch logic.
		if !found {
			return nil
		}
		if !plugin.Match(input) {
			return &routingValidationError{
				Code: "invalid_input",
				Status: 400,
				Message: fmt.Sprintf("plugin %q does not accept input shape (url_patterns mismatch)", inv.Plugin),
			}
		}
		return nil

	default:
		// Unreachable — InvocationShape is closed at two values
		// today. Defensive: don't reject unknown shapes; the parser
		// would have returned URL.
		return nil
	}
}

// commandNames projects a CommandSpec slice to its bare names for
// human-readable error messages. The full long-form (including the
// operator_only flag) is internal-to-the-daemon detail; error
// surfacing keeps the same shape it had when Commands was a string
// slice.
func commandNames(specs []plugins.CommandSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}
