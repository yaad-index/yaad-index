package plugins

import "strings"

// InvocationShape discriminates the two routing paths a daemon dispatch
// can take per ADR-0022 §2. URL-shape covers the existing
// `<plugin>: <pattern>` form (full URLs and shorthand inputs); command-
// shape covers the new `<plugin>: !<command>` imperative form.
type InvocationShape int

const (
	// InvocationURL is the URL-shape input: `<plugin>: <pattern>`
	// (existing) — covers `wikipedia: Tehran`,
	// `https://boardgamegeek.com/...`, etc. Everything that isn't
	// the bang-discriminated command-shape parses as URL-shape and
	// continues through the existing url_patterns matcher.
	InvocationURL InvocationShape = iota

	// InvocationCommand is the command-shape input: `<plugin>: !<command>`
	// (ADR-0022) — `gmail: !fetch`. The `!` is the discriminator;
	// the parser strips it so the resulting Command is the bare
	// name (matching the plugin's advertised Capabilities.Commands
	// vocabulary).
	InvocationCommand
)

// Invocation is the parsed shape of a dispatch input. URL-shape
// populates only Plugin + URL (the full input is preserved verbatim
// so existing url_patterns regex matchers see what they always saw).
// Command-shape populates Plugin + Command (sigil-stripped, trimmed).
//
// Per ADR-0022 §2 the `!` sigil lives in the invocation surface only;
// the plugin's `commands` list contains bare names. ParseInvocation
// strips the sigil at parse time so downstream code never sees it.
//
// Instance is the ADR-0028 §4 (Cut 4) instance-scope qualifier. When
// the input shape is `<plugin>/<instance>: !<command>`, Instance
// carries the named instance; the daemon's dispatch layer routes the
// command to exactly that instance. When the input is bare
// `<plugin>: !<command>` Instance is empty, and the dispatch layer
// fans the command out serially across all enabled instances per
// ADR-0028 §4. URL-shape Invocations always leave Instance empty —
// URL-shape instance routing happens via Cut 3's instance_routing
// capability, not via grammar.
type Invocation struct {
	Shape InvocationShape
	Plugin string
	Instance string
	URL string
	Command string
}

// ParseInvocation decodes a dispatch input into one of the two shapes
// per ADR-0022 §2.
//
// Recognition rules:
//
// - Inputs of the exact shape `<plugin>: !<command>` (where `<plugin>`
// is a non-empty plugin-name token and `<command>` is non-empty
// after sigil-strip + trim) parse as InvocationCommand. The
// returned Command is the sigil-stripped bare name; whitespace
// between the colon and the `!` is tolerated (`gmail: !fetch` and
// `gmail:!fetch` both parse).
//
// - Everything else parses as InvocationURL with URL = input
// verbatim. URL-shape parsing intentionally does NOT split the
// plugin namespace prefix — the existing dispatch path matches
// against compiled url_patterns regexes that span the full input
// (including the `<plugin>:` prefix where present), and rewriting
// the URL would break that contract.
//
// The parser does NOT consult the plugin registry; routing-time
// validation against the named plugin's declared Capabilities.Commands
// is a separate concern ( / ADR-0022 §4). A command-shape input
// against an unknown plugin or an unknown command name still parses
// successfully here — the downstream validator owns the rejection.
//
// Empty / whitespace-only input parses as InvocationURL with the
// original (untrimmed) input as URL; the dispatch path's existing
// "url is required" guard handles that surface.
func ParseInvocation(input string) Invocation {
	plugin, rest, ok := splitPluginPrefix(input)
	if !ok {
		return Invocation{Shape: InvocationURL, URL: input}
	}

	// ADR-0028 §4 (Cut 4): the plugin prefix may carry an instance
	// qualifier as `<plugin>/<instance>`. Split on the first `/`
	// before the `:`-separator if present; the suffix is the
	// instance name. Empty halves (`/foo` or `foo/`) fall through
	// to URL-shape — the dispatch surface's existing "no plugin
	// handles URL" guard rejects them so the operator sees a
	// recognizable error.
	var instance string
	if slash := strings.IndexByte(plugin, '/'); slash >= 0 {
		pluginPart := plugin[:slash]
		instancePart := plugin[slash+1:]
		if pluginPart == "" || instancePart == "" {
			return Invocation{Shape: InvocationURL, URL: input}
		}
		plugin = pluginPart
		instance = instancePart
	}

	// Command-shape: the rest must begin with `!` after leading
	// whitespace. The colon-to-bang gap is part of the typed
	// invocation form (`gmail: !fetch`); we tolerate `gmail:!fetch`
	// for terseness — the discriminator is the `!`, not its column.
	rest = strings.TrimLeft(rest, " \t")
	if !strings.HasPrefix(rest, "!") {
		return Invocation{Shape: InvocationURL, Plugin: plugin, Instance: instance, URL: input}
	}
	cmd := strings.TrimSpace(rest[1:])
	if cmd == "" {
		// Bang with nothing after it isn't a valid command-shape;
		// fall through to URL-shape so the dispatch surface
		// rejects it via the existing "no plugin handles URL"
		// path (or the future routing-time validator catches it
		// per ADR-0022 §4).
		return Invocation{Shape: InvocationURL, Plugin: plugin, Instance: instance, URL: input}
	}
	return Invocation{
		Shape: InvocationCommand,
		Plugin: plugin,
		Instance: instance,
		Command: cmd,
	}
}

// splitPluginPrefix extracts a leading `<plugin>:` namespace token
// from input, returning (plugin, remainder, true) on success. The
// plugin token is the substring before the FIRST `:`; it must be
// non-empty and whitespace-free (the whitespace guard filters
// human-language inputs like `"buy milk: !now"`, not URLs — URL
// schemes contain no whitespace and pass through). Returns
// ("", "", false) when no colon is present or the prefix is empty.
//
// Note: a full URL like `https://example.org/path` DOES split here
// (plugin="https", rest="//example.org/path"). The caller's rest-
// shape check then rejects it as command-shape because rest doesn't
// begin with `!`; the URL-shape return path preserves the input
// verbatim so the url_patterns regex matcher sees the unmodified
// URL. The `Plugin` field on a URL-shape Invocation is therefore
// best-effort metadata, not authoritative — downstream callers must
// not treat `Plugin` as the routing key for URL-shape inputs (they
// continue to walk the registry's url_patterns).
func splitPluginPrefix(input string) (string, string, bool) {
	idx := strings.IndexByte(input, ':')
	if idx <= 0 {
		return "", "", false
	}
	plugin := input[:idx]
	if strings.ContainsAny(plugin, " \t\n") {
		return "", "", false
	}
	return plugin, input[idx+1:], true
}
