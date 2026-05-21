// Package github carries the constants, capabilities, and helpers
// the yaad-github plugin binary uses to implement the subprocess
// plugin protocol (ADR-0005 + ADR-0006 + ADR-0021 + ADR-0022 +
// ADR-0023). The plugin itself lives at cmd/yaad-github/; this
// package keeps everything that's unit-testable without spawning
// a subprocess.
//
// Per ADR-0026: hybrid trigger (URL-shape + command-shape), split
// canonical kinds (github-pr + github-issue), multi-instance via
// configurable base URL.
package github

import (
	"net/url"
	"regexp"

	"github.com/yaad-index/yaad-index/internal/buildinfo"
)

// PluginName is the stable identifier surfaced as the capabilities
// document's `name` field. Operators may override per-instance via
// the config's `name:` entry (e.g. `github-personal`, `github-work`)
// per ADR-0026 §7 multi-instance pattern.
const PluginName = "github"

// PluginVersion is the version string the plugin's `--version`
// handler emits. Build-time-injected via ldflags; falls back to
// FallbackVersion when the linker didn't rewrite it (go test, IDE
// builds).
var PluginVersion = resolvePluginVersion(buildinfo.Version)

// FallbackVersion is the hardcoded version reported when no
// build-time injection happened.
const FallbackVersion = "0.1.0-dev"

func resolvePluginVersion(injected string) string {
	if injected == "" || injected == buildinfo.Unknown {
		return FallbackVersion
	}
	return injected
}

// SourceNamespace is the per-plugin vault path prefix and entity-
// ID namespace under the ADR-0021 universal `kind: source`
// contract. Per ADR-0026 §2 Option A this is a single `github`
// namespace; the PR-vs-issue discriminator lives inside the slug
// (`github:<owner>_<repo>_pr_<n>` vs `_issue_<n>`).
const SourceNamespace = "github"

// UniversalSourceKind is the wire-shape `kind` value the plugin
// emits per ADR-0021. Always `source`.
const UniversalSourceKind = "source"

// SourceTypeName is the descriptive name yaad-github emits on the
// `is_a` edge target. Daemon-derived slug produces
// `source-type:github-record`.
const SourceTypeName = "github-record"

// SourceTypeKind is the canonical kind the `is_a` edge resolves
// to. Matches every other source-shape plugin (universal per
// ADR-0021).
const SourceTypeKind = "source-type"

// CanonicalKind* constants name the four canonical kinds the
// plugin emits per ADR-0026 §2 + §Consequences.
const (
	CanonicalKindPR         = "github-pr"
	CanonicalKindIssue      = "github-issue"
	CanonicalKindRepository = "repository"
	CanonicalKindUser       = "github-user"
)

// KnownCanonicalKinds is the alphabetized set yaad-github
// declares in `canonical_kinds_emitted` at `--init`.
var KnownCanonicalKinds = []string{
	CanonicalKindPR,
	CanonicalKindIssue,
	CanonicalKindRepository,
	CanonicalKindUser,
}

// CanonicalEdgeType* constants name the six edge types yaad-
// github emits per ADR-0026 §1 + §Consequences. Operators
// enable each in `canonical_edge_types:` config per ADR-0008.
const (
	EdgeTypeIsA        = "is_a"
	EdgeTypeInRepo     = "in_repo"
	EdgeTypeAuthoredBy = "authored_by"
	EdgeTypeInvolves   = "involves"
	EdgeTypeAssignedTo = "assigned_to"
	EdgeTypeReviewedBy = "reviewed_by"
)

// KnownCanonicalEdgeTypes is the alphabetized set yaad-github
// declares in `canonical_edge_types_emitted` at `--init`.
var KnownCanonicalEdgeTypes = []string{
	EdgeTypeIsA,
	EdgeTypeInRepo,
	EdgeTypeAuthoredBy,
	EdgeTypeInvolves,
	EdgeTypeAssignedTo,
	EdgeTypeReviewedBy,
}

// DefaultCacheTTLSeconds is the plugin-level TTL declaration per
// ADR-0026 §1 + the three-level cache resolution chain (see
// docs/plugin-flow.md §4). 900s (15min) — short enough that an
// agent re-reading a PR sees fresh state, long enough that a
// quick second pass doesn't burn API budget.
const DefaultCacheTTLSeconds = 900

// CommandFetch is the named imperative invocation the plugin
// exposes per ADR-0022 + ADR-0026 §1. Bare name (no `!` sigil —
// the sigil lives only in the operator-side invocation surface
// `github: !fetch`).
const CommandFetch = "fetch"

// DeclaredCommands is the list yaad-github surfaces in its
// `commands` capability per ADR-0022.
var DeclaredCommands = []string{CommandFetch}

// DefaultBaseURL is the API endpoint yaad-github targets when
// `YAAD_GITHUB_BASE_URL` is unset. Operators pointing the plugin
// at GHES set the env var to the API root (e.g.
// `https://ghes.example.com/api/v3`) per ADR-0026 §7.
const DefaultBaseURL = "https://api.github.com"

// Env-var integration points operators set via the daemon's
// `plugins[].env:` config block per ADR-0006 + ADR-0026 §7-§8.
const (
	EnvToken   = "YAAD_GITHUB_TOKEN"
	EnvBaseURL = "YAAD_GITHUB_BASE_URL"
)

// ResolveBaseURL returns the effective API base URL. Operator
// override via EnvBaseURL takes precedence; falls back to
// DefaultBaseURL. The result is suitable as the prefix for
// REST calls like `<base>/user`.
func ResolveBaseURL(envValue string) string {
	if envValue == "" {
		return DefaultBaseURL
	}
	return envValue
}

// BuildURLPatterns assembles the three URL-pattern regexes the
// plugin advertises in `--init` per ADR-0026 §1 + §7. The
// hostname for the PR + issue URL patterns is interpolated from
// the base-URL so multi-instance setups (public github.com + a
// GHES install) get correct dispatch routing.
//
// The shorthand pattern uses `pluginName` (operator may override
// to e.g. `github-personal`, `github-work`) so two instances of
// the same binary disambiguate via the sigil prefix per
// ADR-0026 §7.
//
// A bare `pluginName` of "" or a malformed base URL falls back
// to the conservative `github.com` host + the canonical plugin
// name `github`; in practice the binary's main() always passes
// non-empty values, but the fallback keeps the function pure +
// safe to call from tests.
func BuildURLPatterns(pluginName, baseURL string) []string {
	host := hostFromBaseURL(baseURL)
	if host == "" {
		host = "github.com"
	}
	hostQuoted := regexp.QuoteMeta(host)

	name := pluginName
	if name == "" {
		name = PluginName
	}
	nameQuoted := regexp.QuoteMeta(name)

	return []string{
		`^https?://` + hostQuoted + `/[^/]+/[^/]+/pull/\d+`,
		`^https?://` + hostQuoted + `/[^/]+/[^/]+/issues/\d+`,
		`(?i)^` + nameQuoted + `:\s*[^/]+/[^/]+#\d+`,
	}
}

// hostFromBaseURL extracts the host portion from a base URL
// suitable for embedding into a regex. Returns "" on parse
// failure or a base URL with no host; the caller falls back to
// the public default.
//
// Note: GHES base URLs typically include a path suffix
// (`/api/v3`), but only the host is needed for URL-pattern
// matching — the dispatched URL form is the user-facing
// `https://ghes.example.com/<owner>/<repo>/pull/<n>`, not the
// API form.
func hostFromBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
