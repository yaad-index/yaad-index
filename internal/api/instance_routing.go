package api

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
)

// ErrUnroutedURL surfaces from pickInstance when no enabled
// instance's globs match the URL's extracted fields per ADR-0028
// §3 fail-fast. /v1/ingest converts this to a 400 response with
// `{instance: "unrouted", url, message}` so the operator (or
// agent) sees the exact URL that failed routing.
var ErrUnroutedURL = errors.New("no instance glob matched URL")

// ErrUnsupportedRoutingStrategy surfaces when a plugin declared
// an instance_routing.strategy the daemon doesn't recognize.
// v1 supports `glob_match` only.
var ErrUnsupportedRoutingStrategy = errors.New("unsupported instance_routing.strategy")

// ErrNoURLRouting surfaces when a multi-instance plugin omitted
// the instance_routing block entirely AND the operator declared
// multiple instances. URL ingest can't pick an instance under
// those conditions; the operator must either drop the extra
// instances or use a command-shape invocation.
var ErrNoURLRouting = errors.New("plugin advertises no URL routing")

// pickInstance resolves the active instance for a URL-shape
// invocation per ADR-0028 §3. Returns the picked instance name
// on a successful glob match, ErrUnroutedURL when no enabled
// instance's glob matches, ErrUnsupportedRoutingStrategy when
// the plugin declared an unknown strategy, and ErrNoURLRouting
// when the plugin is multi-instance with no routing declaration.
//
// Single-instance plugins (supports_instances=false in
// capabilities OR a single configured instance) short-circuit
// to the implicit / explicit single instance name without
// running the glob walk — the routing surface only matters when
// there's more than one candidate.
func pickInstance(plugin plugins.Plugin, instances []config.InstanceEntry, rawURL string) (string, error) {
	caps := plugin.Capabilities()

	// Short-circuit: 0 instances (test paths) → "default"; 1
	// instance → that instance's name. The glob walk only runs
	// for the 2+ case where there's an actual decision to make.
	if len(instances) == 0 {
		return "default", nil
	}
	if len(instances) == 1 {
		return instances[0].Name, nil
	}

	// Multi-instance: require a routing declaration. Per ADR-0028
	// §3, plugins with supports_instances=true whose primary path
	// is URL-shape MUST declare instance_routing. The Cut 1 gate
	// in cmd/yaad-index already rejects supports_instances=false
	// + 2+ instances, so reaching here with 2+ instances implies
	// supports_instances=true — the missing declaration is a
	// plugin-author bug to surface clearly.
	if caps.InstanceRouting == nil {
		return "", fmt.Errorf("%w: plugin %q has 2+ instances configured but declared no instance_routing in --init",
			ErrNoURLRouting, plugin.Name())
	}

	if caps.InstanceRouting.Strategy != "glob_match" {
		return "", fmt.Errorf("%w: plugin %q declared strategy %q (v1 supports `glob_match` only)",
			ErrUnsupportedRoutingStrategy, plugin.Name(), caps.InstanceRouting.Strategy)
	}

	// Extract named capture groups from the first matching
	// url_pattern. The Plugin interface's Match returns bool only;
	// recompile here so the routing layer doesn't depend on the
	// per-plugin pattern cache. Cost is acceptable on the per-URL
	// dispatch path — operator-facing URL ingest is not hot enough
	// to justify a Plugin interface change.
	captures, err := extractURLCaptures(caps.URLPatterns, rawURL)
	if err != nil {
		return "", err
	}

	// Format the match_template with the captured named groups.
	// Missing names render as the literal `{name}` placeholder so
	// the glob walk can't accidentally match — operator-visible
	// bug rather than silent mis-attribution.
	formatted := formatMatchTemplate(caps.InstanceRouting.MatchTemplate, captures)

	// Walk instances in declaration order. First glob match wins
	// per §3. Each instance's config[<config_field>] must be a
	// list of glob strings; non-list / wrong-shape values skip
	// that instance (operator-visible config error logged at
	// config-load time via warnInstanceRoutingOverlap).
	for _, inst := range instances {
		globs := extractGlobList(inst.Config, caps.InstanceRouting.ConfigField)
		for _, glob := range globs {
			matched, err := path.Match(glob, formatted)
			if err != nil {
				// path.Match returns ErrBadPattern for malformed
				// globs. Skip the bad entry but continue the walk
				// — operator gets a partial result rather than a
				// fail-everything.
				continue
			}
			if matched {
				return inst.Name, nil
			}
		}
	}

	// No glob matched. Per §3 ADR-0028 amendment: fail-fast,
	// no silent fallback. The error message names the
	// match-template'd value so the operator can correlate
	// directly with the config block they need to fix.
	return "", fmt.Errorf("%w: plugin=%q field=%q formatted=%q",
		ErrUnroutedURL,
		plugin.Name(),
		caps.InstanceRouting.ConfigField,
		formatted)
}

// extractURLCaptures runs the URL against each pattern in
// url_patterns (in declaration order) and returns the named
// capture groups from the first match. Returns an error when no
// pattern matches the URL — should be impossible in production
// (the routing layer is only reached after registry.Lookup
// already confirmed a match) but defensive against
// out-of-order changes elsewhere.
func extractURLCaptures(urlPatterns []string, rawURL string) (map[string]string, error) {
	for _, pat := range urlPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			// Malformed pattern — the plugin registration should
			// have caught this already (subprocess.New compiles
			// the patterns at registration). Defensive: skip.
			continue
		}
		match := re.FindStringSubmatch(rawURL)
		if match == nil {
			continue
		}
		captures := map[string]string{}
		for i, name := range re.SubexpNames() {
			if i == 0 || name == "" {
				continue
			}
			captures[name] = match[i]
		}
		return captures, nil
	}
	return nil, fmt.Errorf("no url_pattern matched %q (routing called before registry.Lookup?)", rawURL)
}

// formatMatchTemplate interpolates `{name}` placeholders in tpl
// with values from captures. Missing names render as the literal
// `{name}` so the glob walk can't accidentally match — surfaces
// the plugin-author / operator misconfig as a noticeable
// "unrouted" diagnostic rather than silent mis-attribution.
func formatMatchTemplate(tpl string, captures map[string]string) string {
	out := tpl
	for name, value := range captures {
		out = strings.ReplaceAll(out, "{"+name+"}", value)
	}
	return out
}

// extractGlobList pulls the per-instance config value at
// configField and returns it as a slice of glob strings. Non-
// list shapes (a bare string, a map) skip with an empty result
// so the routing walk continues to the next instance — the
// config-load-time validator surfaces the operator-visible
// error separately.
func extractGlobList(cfg map[string]any, configField string) []string {
	raw, ok := cfg[configField]
	if !ok {
		return nil
	}
	// YAML unmarshals lists as either []any or []string depending
	// on the YAML reader's normalization. Accept both.
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// WarnInstanceRoutingOverlap walks every multi-instance plugin's
// instance configs and emits a startup warning when two enabled
// instances declare overlapping globs against the routing
// `config_field`. Per ADR-0028 §3 the daemon still resolves
// first-match-wins; the overlap warning is a diagnostic so
// operators notice ambiguous routing rather than discover it
// through misattributed ingest.
//
// Strategy: for each plugin with InstanceRouting set, walk every
// pair of (instance, glob) entries and check whether they would
// match the same value. Overlap detection is approximate — we
// flag entries where one glob is a prefix or suffix of another,
// or where literal portions overlap. False negatives on subtle
// glob intersections are acceptable; this is a heuristic warning
// surface, not a correctness gate.
func WarnInstanceRoutingOverlap(
	logger interface {
		Warn(msg string, args ...any)
	},
	pluginInstanceConfigs map[string][]config.InstanceEntry,
	capabilities map[string]plugins.Capabilities,
) {
	for pluginName, instances := range pluginInstanceConfigs {
		caps, ok := capabilities[pluginName]
		if !ok || caps.InstanceRouting == nil {
			continue
		}
		if len(instances) < 2 {
			continue
		}
		field := caps.InstanceRouting.ConfigField
		type globOwner struct {
			instance string
			glob     string
		}
		var seen []globOwner
		for _, inst := range instances {
			for _, glob := range extractGlobList(inst.Config, field) {
				for _, prior := range seen {
					if globsOverlap(prior.glob, glob) {
						logger.Warn("instance_routing overlap detected",
							"plugin", pluginName,
							"field", field,
							"instance_a", prior.instance, "glob_a", prior.glob,
							"instance_b", inst.Name, "glob_b", glob,
							"note", "first-declared wins per ADR-0028 §3; operator should resolve ambiguity")
					}
				}
				seen = append(seen, globOwner{instance: inst.Name, glob: glob})
			}
		}
	}
}

// globsOverlap reports whether two glob patterns plausibly match
// the same value. Heuristic: identical globs always overlap;
// either pattern being `*` always overlaps; one being a
// prefix-glob (`foo/*`) that covers the other's prefix overlaps.
// False negatives are acceptable for the warning surface.
func globsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	if a == "*" || b == "*" {
		return true
	}
	// Prefix-glob overlap: `foo/*` overlaps `foo/bar` and `foo/*`.
	if strings.HasSuffix(a, "/*") {
		prefix := strings.TrimSuffix(a, "/*")
		if strings.HasPrefix(b, prefix+"/") || b == prefix {
			return true
		}
	}
	if strings.HasSuffix(b, "/*") {
		prefix := strings.TrimSuffix(b, "/*")
		if strings.HasPrefix(a, prefix+"/") || a == prefix {
			return true
		}
	}
	return false
}
