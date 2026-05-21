package github

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginVersion_FallsBackWhenInjectedIsUnknown(t *testing.T) {
	t.Parallel()
	// Test the helper directly — PluginVersion itself is set at
	// package-init time so we can't reliably override
	// buildinfo.Version mid-test.
	assert.Equal(t, FallbackVersion, resolvePluginVersion(""))
	assert.Equal(t, FallbackVersion, resolvePluginVersion("unknown"))
	assert.Equal(t, "v1.2.3", resolvePluginVersion("v1.2.3"))
}

func TestKnownCanonicalKinds_MatchesADR0026(t *testing.T) {
	t.Parallel()
	// ADR-0026 §1 + §Consequences declares four canonical kinds
	// emitted by yaad-github. Pin the set so a future addition
	// (or accidental removal) shows up here.
	assert.Equal(t, []string{"github-pr", "github-issue", "repository", "github-user"}, KnownCanonicalKinds)
}

func TestKnownCanonicalEdgeTypes_MatchesADR0026(t *testing.T) {
	t.Parallel()
	// ADR-0026 §1 + §Consequences declares six edge types. Same
	// pinning rationale as above.
	assert.Equal(t, []string{
		"is_a",
		"in_repo",
		"authored_by",
		"involves",
		"assigned_to",
		"reviewed_by",
	}, KnownCanonicalEdgeTypes)
}

func TestResolveBaseURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultBaseURL, ResolveBaseURL(""))
	assert.Equal(t, "https://ghes.example.com/api/v3", ResolveBaseURL("https://ghes.example.com/api/v3"))
}

func TestBuildURLPatterns_DefaultBaseURL_PublicGitHub(t *testing.T) {
	t.Parallel()
	patterns := BuildURLPatterns("", "")
	require.Len(t, patterns, 3)

	// PR + issue patterns should match the canonical public-
	// GitHub URLs the operator dispatches against.
	cases := []struct {
		patternIdx int
		input      string
		want       bool
	}{
		{0, "https://github.com/acme/proj/pull/42", true},
		{0, "http://github.com/acme/proj/pull/42", true},
		{0, "https://github.com/acme/proj/issues/42", false},
		{0, "https://ghes.example.com/acme/proj/pull/42", false},
		{1, "https://github.com/acme/proj/issues/42", true},
		{1, "https://github.com/acme/proj/pull/42", false},
		{2, "github: acme/proj#42", true},
		{2, "GITHUB: acme/proj#42", true},
		{2, "GiThUb:acme/proj#42", true},
		{2, "github-personal: acme/proj#42", false},
		{2, "https://github.com/acme/proj/pull/42", false},
	}
	for _, tc := range cases {
		re := regexp.MustCompile(patterns[tc.patternIdx])
		assert.Equalf(t, tc.want, re.MatchString(tc.input),
			"pattern[%d]=%q against %q", tc.patternIdx, patterns[tc.patternIdx], tc.input)
	}
}

func TestBuildURLPatterns_GHESHost_OnlyMatchesGHES(t *testing.T) {
	t.Parallel()
	// ADR-0026 §7: per-instance base URL means the URL patterns
	// match ONLY that host. A GHES instance shouldn't pick up
	// public github.com URLs.
	patterns := BuildURLPatterns("github-work", "https://ghes.example.com/api/v3")
	require.Len(t, patterns, 3)

	prRE := regexp.MustCompile(patterns[0])
	assert.True(t, prRE.MatchString("https://ghes.example.com/team/svc/pull/1"))
	assert.False(t, prRE.MatchString("https://github.com/acme/proj/pull/1"),
		"GHES instance must not match public github.com URLs")

	issueRE := regexp.MustCompile(patterns[1])
	assert.True(t, issueRE.MatchString("https://ghes.example.com/team/svc/issues/1"))
	assert.False(t, issueRE.MatchString("https://github.com/acme/proj/issues/1"))

	// Shorthand uses the instance name (not "github") per
	// ADR-0026 §7 so the two instances disambiguate via the
	// sigil prefix.
	shortRE := regexp.MustCompile(patterns[2])
	assert.True(t, shortRE.MatchString("github-work: team/svc#1"))
	assert.False(t, shortRE.MatchString("github: team/svc#1"),
		"GHES instance must not claim the bare 'github:' shorthand")
}

func TestBuildURLPatterns_MalformedBaseURL_FallsBackToPublic(t *testing.T) {
	t.Parallel()
	// Defensive: a malformed base URL shouldn't crash the plugin
	// — the URL patterns silently fall back to the public-GitHub
	// host. The operator-side validation belongs at the daemon's
	// `plugins[].env:` config-load step, not in the plugin's
	// regex generator.
	patterns := BuildURLPatterns("github", "::not a url::")
	require.Len(t, patterns, 3)
	re := regexp.MustCompile(patterns[0])
	assert.True(t, re.MatchString("https://github.com/acme/proj/pull/1"))
}

func TestBuildURLPatterns_EscapesSpecialRegexCharsInHost(t *testing.T) {
	t.Parallel()
	// A GHES host with a `.` or other regex-meta character must
	// be regex-quoted so the pattern matches the literal host —
	// not the wildcard interpretation of `.`.
	patterns := BuildURLPatterns("gh", "https://ghes.example.com/api/v3")
	require.Len(t, patterns, 3)
	re := regexp.MustCompile(patterns[0])
	// Literal `.` matches `.`; would-be-wildcard match against
	// any other char (e.g. `x`) must fail.
	assert.True(t, re.MatchString("https://ghes.example.com/team/svc/pull/1"))
	assert.False(t, re.MatchString("https://ghesxexample.com/team/svc/pull/1"))
}

func TestDefaultCacheTTLSeconds_Is15Minutes(t *testing.T) {
	t.Parallel()
	// ADR-0026 §1 pins this to 900s; the constant is checked so a
	// future drift surfaces here.
	assert.Equal(t, 900, DefaultCacheTTLSeconds)
}

func TestSourceTypeName_IsGithubRecord(t *testing.T) {
	t.Parallel()
	// `is_a` edge target — daemon-derived slug becomes
	// `source-type:github-record`. ADR-0026 doesn't pin the
	// literal name; this test pins the value so any change is
	// deliberate (would-be source-type slugs change with it).
	assert.Equal(t, "github-record", SourceTypeName)
}

func TestSourceNamespace_IsSingularGithub(t *testing.T) {
	t.Parallel()
	// ADR-0026 §2 Option A pins a single `github` namespace.
	// The PR vs issue discriminator lives in the slug, not the
	// namespace.
	assert.Equal(t, "github", SourceNamespace)
}

func TestDeclaredCommands_ContainsFetch(t *testing.T) {
	t.Parallel()
	// ADR-0022 + ADR-0026 §1: the only declared command in v1
	// is `fetch`. Pin so a future addition shows up here.
	assert.Equal(t, []string{"fetch"}, DeclaredCommands)
	// The convenience constant is the same value plugin code
	// uses to switch — pinning both avoids a string-literal
	// drift.
	assert.Equal(t, "fetch", CommandFetch)
}

func TestEnvVarNames_MatchADR0026(t *testing.T) {
	t.Parallel()
	// ADR-0026 §8 explicitly distinguishes from the `gh` CLI's
	// `GITHUB_TOKEN`; pin the names so a future rename surfaces.
	assert.Equal(t, "YAAD_GITHUB_TOKEN", EnvToken)
	assert.Equal(t, "YAAD_GITHUB_BASE_URL", EnvBaseURL)
	assert.NotEqual(t, "GITHUB_TOKEN", EnvToken,
		"YAAD_GITHUB_TOKEN must stay distinct from the gh CLI's GITHUB_TOKEN per ADR-0026 §8")
}

func TestBuildURLPatterns_ValidRegexes(t *testing.T) {
	t.Parallel()
	// Defensive: every returned pattern must compile.
	for i, p := range BuildURLPatterns("github", "https://api.github.com") {
		_, err := regexp.Compile(p)
		require.NoErrorf(t, err, "pattern[%d]=%q must compile", i, p)
		assert.NotEmptyf(t, strings.TrimSpace(p), "pattern[%d] must be non-empty", i)
	}
}
