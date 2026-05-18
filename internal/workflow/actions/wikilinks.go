// Wikilink helpers per #163 — Obsidian-readable references to
// vault entities. Two helpers, two responsibilities:
//
//   - wrapWorkflow: always wraps the given workflow name in
//     `[[ ]]`. Caller knows by construction the value names a
//     workflow file (workflows live under vault/workflows/).
//     Empty input passes through unchanged.
//
//   - maybeWrapEntity: format-based detection. Wraps the input
//     in `[[ ]]` only when it matches the `<kind>:<id>` entity-
//     slug shape AND `<kind>` appears in the canonical-kinds
//     registry. Used at the writer layer for CEL-rendered
//     strings where the caller doesn't know upfront whether
//     the value is an entity reference.
//
// Splitting into two helpers (not one with detection-of-
// everything) keeps the caller intent explicit: code that
// emits a workflow name spells it via `wrapWorkflow`; code
// that emits a possibly-entity-shaped string spells it via
// `maybeWrapEntity`. The detection rule for the second helper
// stays narrow on purpose — the registry-membership check
// prevents false positives like timestamps `2026-05-18:T12`
// or arbitrary `<scheme>:<value>` strings.

package actions

import (
	"strings"

	"github.com/yaad-index/yaad-index/internal/config"
)

// wrapWorkflow returns `[[<name>]]` when name is non-empty.
// Empty input passes through unchanged so the caller can use
// the helper unconditionally without a pre-check (`[[]]` is
// not a useful Obsidian link).
func wrapWorkflow(name string) string {
	if name == "" {
		return ""
	}
	return "[[" + name + "]]"
}

// wrapEntityValue applies maybeWrapEntity per element type
// to a value emitted into vault frontmatter `data` per #166.
// String values pass through maybeWrapEntity; array values
// recurse element-wise (the spec's mixed-array case — some
// elements wrap, others don't, per individual match against
// the registry); other types (bool, int, nested map) pass
// through unchanged because they can't be entity refs.
//
// Used by VaultPropertyWriter.SetProperties to wrap workflow-
// emitted property values before they land in the vault file.
// Add_note bodies go through maybeWrapEntity directly (no
// per-element dispatch — body is always a string).
func wrapEntityValue(v any, kinds map[string]config.CanonicalKindConfig) any {
	switch x := v.(type) {
	case string:
		return maybeWrapEntity(x, kinds)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = wrapEntityValue(e, kinds)
		}
		return out
	case []string:
		// CEL templates rendering a homogeneous string slice
		// surface as []string before vault Marshal. Wrap each
		// element + return the same shape so the YAML output
		// stays a list of strings rather than `[]any` round-
		// trip (cosmetic — yaml.v3 handles both, but []string
		// is what the operator-config side emits).
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = maybeWrapEntity(e, kinds)
		}
		return out
	default:
		return v
	}
}

// maybeWrapEntity inspects s for the `<kind>:<id>` entity-slug
// shape and wraps it in `[[ ]]` only when:
//
//  1. The string contains exactly one `:` separator.
//  2. The leading segment (`<kind>`) is non-empty AND appears
//     in the canonical-kinds registry.
//  3. The trailing segment (`<id>`) is non-empty.
//
// Returns the original string unwrapped on any miss — strings
// with no colon, multiple colons, an unknown kind, or an
// empty segment.
//
// The registry-membership check is the load-bearing rule. It
// prevents accidental wrapping of timestamp-shaped strings
// (`2026-05-18T19:00:00Z`), package paths (`pkg:something`),
// and other `<scheme>:<value>` literals that happen to share
// the format. An entity ref has both a colon-separator AND a
// kind the operator's config knows about.
func maybeWrapEntity(s string, kinds map[string]config.CanonicalKindConfig) string {
	if s == "" {
		return ""
	}
	// Strings with multiple colons aren't single-`<kind>:<id>`
	// shaped — pass through.
	idx := strings.IndexByte(s, ':')
	if idx < 0 || idx != strings.LastIndexByte(s, ':') {
		return s
	}
	kind, id := s[:idx], s[idx+1:]
	if kind == "" || id == "" {
		return s
	}
	if _, known := kinds[kind]; !known {
		return s
	}
	return "[[" + s + "]]"
}
