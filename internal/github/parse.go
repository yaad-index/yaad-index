package github

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ItemKind is the GitHub-side distinction between PRs and
// issues. Surfaces as the canonical-kind discriminator the
// daemon resolves (`github-pr` vs `github-issue` per ADR-
// 0026 §2) and as the slug suffix that disambiguates IDs
// (`<owner>_<repo>_pr_<n>` vs `_issue_<n>`).
type ItemKind string

const (
	// ItemKindPR is the discriminator for pull requests.
	ItemKindPR ItemKind = "pr"
	// ItemKindIssue is the discriminator for issues.
	ItemKindIssue ItemKind = "issue"
)

// CanonicalKind returns the canonical-kind name the daemon
// materializes for items of this kind, matching the
// `canonical_kinds_emitted` declaration on the plugin's
// `--init` (ADR-0026 §2).
func (k ItemKind) CanonicalKind() string {
	switch k {
	case ItemKindPR:
		return CanonicalKindPR
	case ItemKindIssue:
		return CanonicalKindIssue
	default:
		return ""
	}
}

// Target identifies a single PR or issue parsed from one of
// the input shapes the plugin's URL patterns match per
// ADR-0026 §1:
//
//   - `https://<host>/<owner>/<repo>/pull/<num>`
//   - `https://<host>/<owner>/<repo>/issues/<num>`
//   - `<plugin-name>:<owner>/<repo>#<num>` shorthand
//
// The plugin builds the GitHub REST URL from the Target via
// the routes `/repos/<Owner>/<Repo>/pulls/<Number>` (PR) or
// `/repos/<Owner>/<Repo>/issues/<Number>` (issue), and the
// vault-side entity ID via the slug-shape
// `<Owner>_<Repo>_<Kind>_<Number>`.
type Target struct {
	Owner  string
	Repo   string
	Kind   ItemKind
	Number int
}

// EntityName returns the descriptive name the plugin emits in
// `structured.name` so the daemon's `slug.Slug(name)` (per
// ADR-0021) lands on the ADR-0026 §2 entity-ID shape:
// `github:<owner>_<repo>_pr_<n>` / `_issue_<n>`.
//
// Construction: literal `<owner>_<repo>_<kind>_<num>` with
// owner/repo lowercased up-front. gosimple/slug preserves
// underscores + lowercases its input, so this round-trips
// through `slug.Slug` as a no-op (verified by the
// internal/slug test fixture's underscore-preservation case).
func (t Target) EntityName() string {
	return fmt.Sprintf("%s_%s_%s_%d",
		strings.ToLower(t.Owner),
		strings.ToLower(t.Repo),
		t.Kind,
		t.Number,
	)
}

// ErrUnsupportedTarget surfaces when ParseTarget sees an
// input that doesn't match any of the three documented
// shapes. The plugin's URL-pattern dispatch should keep this
// from firing in production (the daemon only routes matching
// inputs to the plugin) but it's a defensive guard for
// direct-stdin invocations + tests.
var ErrUnsupportedTarget = errors.New("github: unsupported target shape (want pull/issue URL or `<name>:owner/repo#num` shorthand)")

// shorthandRE matches the ADR-0026 §1 shorthand:
// `<plugin-name>:<owner>/<repo>#<num>`. The plugin-name
// prefix may be any identifier per ADR-0026 §7 multi-
// instance (operator picks `github-personal`, `github-work`,
// etc.). The case-insensitive flag mirrors the URL pattern.
var shorthandRE = regexp.MustCompile(`(?i)^[a-z][a-z0-9_-]*:\s*([^/\s]+)/([^/#\s]+)#(\d+)\s*$`)

// urlPathRE pulls the (owner, repo, kind, num) tuple out of
// either of the URL shapes. `kind` is captured as `pull` or
// `issues` and normalized below.
var urlPathRE = regexp.MustCompile(`^/([^/]+)/([^/]+)/(pull|issues)/(\d+)(?:/.*)?$`)

// ParseTarget normalizes one of the three input shapes into
// a Target. Returns ErrUnsupportedTarget on a malformed or
// unrecognized input.
//
// The plugin invokes this on the `url` field of an ingest
// request body the daemon writes to stdin (per ADR-0005).
func ParseTarget(raw string) (*Target, error) {
	input := strings.TrimSpace(raw)
	if input == "" {
		return nil, fmt.Errorf("github: empty target: %w", ErrUnsupportedTarget)
	}

	// Shorthand shape: try first so a URL-looking string with
	// a `:` doesn't accidentally match. The regex's anchoring
	// rejects URLs (the `://` would force a non-match at the
	// `/` after the colon).
	if m := shorthandRE.FindStringSubmatch(input); m != nil {
		return targetFromShorthand(m[1], m[2], m[3])
	}

	// URL shape: parse + walk the path.
	u, err := url.Parse(input)
	if err != nil {
		return nil, fmt.Errorf("github: parse target URL %q: %w", input, ErrUnsupportedTarget)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("github: target URL %q must be http(s): %w", input, ErrUnsupportedTarget)
	}
	pm := urlPathRE.FindStringSubmatch(u.Path)
	if pm == nil {
		return nil, fmt.Errorf("github: target URL path %q doesn't match /<owner>/<repo>/{pull,issues}/<num>: %w", u.Path, ErrUnsupportedTarget)
	}

	kind := ItemKindPR
	if pm[3] == "issues" {
		kind = ItemKindIssue
	}
	return targetFromParts(pm[1], pm[2], kind, pm[4])
}

// targetFromShorthand builds a Target from a shorthand match.
// The shorthand always names a PR vs issue ambiguously — `#N`
// could refer to either since GitHub's numbering is unified
// per-repo. We default to ItemKindPR because the plugin's
// fetch path can detect issue-vs-PR from the REST response
// and re-target as needed; if the operator wants to
// disambiguate the input itself, the URL shapes are the
// explicit form.
//
// Note: a future PR could change this to call the REST API's
// disambiguating shape (e.g. probe `/pulls/N` first, fall
// through to `/issues/N` on 404). v1 takes the simpler
// default-to-PR path; the fetch path falls back to issue
// when the PR-shape `/pulls/N` returns 404.
func targetFromShorthand(owner, repo, numStr string) (*Target, error) {
	return targetFromParts(owner, repo, ItemKindPR, numStr)
}

func targetFromParts(owner, repo string, kind ItemKind, numStr string) (*Target, error) {
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("github: target number %q must be a positive integer: %w", numStr, ErrUnsupportedTarget)
	}
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: target missing owner or repo: %w", ErrUnsupportedTarget)
	}
	return &Target{
		Owner:  owner,
		Repo:   repo,
		Kind:   kind,
		Number: n,
	}, nil
}
