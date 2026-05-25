package api

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
)

// fixturePlugin builds a fixture.Plugin with the named capabilities.
func fixturePlugin(t *testing.T, caps plugins.Capabilities) plugins.Plugin {
	t.Helper()
	return &fixture.Plugin{
		NameValue:         caps.Name,
		MatchFunc:         func(string) bool { return true },
		CapabilitiesValue: caps,
	}
}

// TestPickInstance_NoInstances_ReturnsDefault pins the test-path
// shortcut: when the operator config has no instances declared for
// this plugin (or pickInstance receives nil), the routing layer
// returns "default" so the slash-form source: source/default still
// composes cleanly. Mirrors the ingest tracker's resolveInstanceName
// fallback.
func TestPickInstance_NoInstances_ReturnsDefault(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{Name: "test"})
	name, err := pickInstance(p, nil, "https://example.test/x")
	require.NoError(t, err)
	assert.Equal(t, "default", name)
}

// TestPickInstance_SingleInstance_ReturnsOperatorName: when only
// one instance is declared, pickInstance returns its name without
// running the routing scan (no glob walk needed).
func TestPickInstance_SingleInstance_ReturnsOperatorName(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{Name: "test"})
	instances := []config.InstanceEntry{{Name: "personal"}}
	name, err := pickInstance(p, instances, "https://example.test/x")
	require.NoError(t, err)
	assert.Equal(t, "personal", name)
}

// TestPickInstance_MultiInstance_GlobMatch_FirstWins exercises the
// canonical ADR-0028 §3 glob_match routing surface: two instances
// configured for distinct repo globs; URL extracts owner/repo
// captures; first matching glob's instance wins.
func TestPickInstance_MultiInstance_GlobMatch_FirstWins(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "github",
		URLPatterns:       []string{`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\d+`},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "glob_match",
			ConfigField:   "repos",
			MatchTemplate: "{owner}/{repo}",
		},
	})
	instances := []config.InstanceEntry{
		{Name: "personal", Config: map[string]any{"repos": []any{"alice/personal-*"}}},
		{Name: "acme-org", Config: map[string]any{"repos": []any{"acme-org/*"}}},
	}
	name, err := pickInstance(p, instances, "https://github.com/acme-org/project/pull/42")
	require.NoError(t, err)
	assert.Equal(t, "acme-org", name)
}

// TestPickInstance_MultiInstance_DeclarationOrderWinsOnOverlap
// pins the first-match-wins rule per ADR-0028 §3: when two
// instances declare overlapping globs, the first-declared wins.
func TestPickInstance_MultiInstance_DeclarationOrderWinsOnOverlap(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "github",
		URLPatterns:       []string{`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\d+`},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "glob_match",
			ConfigField:   "repos",
			MatchTemplate: "{owner}/{repo}",
		},
	})
	// Both instances would match owner-a/repo-1; first-declared
	// wins per §3 + the WarnInstanceRoutingOverlap warning surface.
	instances := []config.InstanceEntry{
		{Name: "first", Config: map[string]any{"repos": []any{"owner-a/*"}}},
		{Name: "second", Config: map[string]any{"repos": []any{"owner-a/repo-1"}}},
	}
	name, err := pickInstance(p, instances, "https://github.com/owner-a/repo-1/pull/1")
	require.NoError(t, err)
	assert.Equal(t, "first", name)
}

// TestPickInstance_MultiInstance_UnmatchedURL_FailFast pins the
// ADR-0028 §3 amendment locked in PR-242: no silent fallback to
// the first-declared instance when no glob matches. Surface
// ErrUnroutedURL with the formatted match-template value so the
// operator can correlate to the missing config entry.
func TestPickInstance_MultiInstance_UnmatchedURL_FailFast(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "github",
		URLPatterns:       []string{`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\d+`},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "glob_match",
			ConfigField:   "repos",
			MatchTemplate: "{owner}/{repo}",
		},
	})
	instances := []config.InstanceEntry{
		{Name: "personal", Config: map[string]any{"repos": []any{"alice/*"}}},
		{Name: "acme-org", Config: map[string]any{"repos": []any{"acme-org/*"}}},
	}
	_, err := pickInstance(p, instances, "https://github.com/external-user/repo/pull/1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnroutedURL)
	// Error message must carry the formatted match-template so the
	// operator sees the exact value that didn't match.
	assert.Contains(t, err.Error(), "external-user/repo")
}

// TestPickInstance_MultiInstance_NoRouting_Error: a multi-instance
// plugin whose --init omitted instance_routing can't pick — surface
// ErrNoURLRouting so the operator gets a clear "plugin advertises
// no URL routing" error (vs silently mis-attributing).
func TestPickInstance_MultiInstance_NoRouting_Error(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "broken",
		URLPatterns:       []string{`^https?://example\.test/.*`},
		SupportsInstances: true,
		// InstanceRouting deliberately omitted.
	})
	instances := []config.InstanceEntry{
		{Name: "a"},
		{Name: "b"},
	}
	_, err := pickInstance(p, instances, "https://example.test/x")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoURLRouting)
}

// TestPickInstance_UnsupportedStrategy_Error pins v1's strategy
// gate: any value other than "glob_match" is rejected.
func TestPickInstance_UnsupportedStrategy_Error(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "future",
		URLPatterns:       []string{`^https?://example\.test/.*`},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "hash_of_field",
			ConfigField:   "shard",
			MatchTemplate: "{shard}",
		},
	})
	instances := []config.InstanceEntry{
		{Name: "a"},
		{Name: "b"},
	}
	_, err := pickInstance(p, instances, "https://example.test/x")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedRoutingStrategy)
}

// TestExtractURLCaptures_NamedGroups verifies the helper extracts
// named capture groups from a matching URL pattern.
func TestExtractURLCaptures_NamedGroups(t *testing.T) {
	t.Parallel()
	patterns := []string{
		`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\d+`,
	}
	captures, err := extractURLCaptures(patterns, "https://github.com/acme-org/project/pull/42")
	require.NoError(t, err)
	assert.Equal(t, "acme-org", captures["owner"])
	assert.Equal(t, "project", captures["repo"])
}

// TestFormatMatchTemplate_MissingNameLiteralPasses pins the
// missing-name behavior: a capture name not in the map renders
// as the literal `{name}` placeholder so the glob walk can't
// accidentally match — operator-visible misconfig rather than
// silent mis-attribution.
func TestFormatMatchTemplate_MissingNameLiteralPasses(t *testing.T) {
	t.Parallel()
	captures := map[string]string{"owner": "acme"}
	out := formatMatchTemplate("{owner}/{repo}", captures)
	assert.Equal(t, "acme/{repo}", out)
}

// TestExtractGlobList_Shapes accepts []string and []any (YAML
// reader shape variance) and rejects scalar or map shapes by
// returning nil (caller skips that instance gracefully).
func TestExtractGlobList_Shapes(t *testing.T) {
	t.Parallel()
	cfg := map[string]any{
		"strings": []string{"a", "b"},
		"any":     []any{"c", "d", 42}, // 42 dropped — not a string
		"scalar":  "not-a-list",
		"map":     map[string]any{"x": 1},
	}
	assert.Equal(t, []string{"a", "b"}, extractGlobList(cfg, "strings"))
	assert.Equal(t, []string{"c", "d"}, extractGlobList(cfg, "any"))
	assert.Nil(t, extractGlobList(cfg, "scalar"))
	assert.Nil(t, extractGlobList(cfg, "map"))
	assert.Nil(t, extractGlobList(cfg, "missing"))
}

// TestWarnInstanceRoutingOverlap emits a warning when two
// instances declare overlapping globs against the routing
// config_field.
func TestWarnInstanceRoutingOverlap(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	capsByName := map[string]plugins.Capabilities{
		"github": {
			Name:              "github",
			SupportsInstances: true,
			InstanceRouting: &plugins.InstanceRoutingSpec{
				Strategy:      "glob_match",
				ConfigField:   "repos",
				MatchTemplate: "{owner}/{repo}",
			},
		},
	}
	configs := map[string][]config.InstanceEntry{
		"github": {
			{Name: "first", Config: map[string]any{"repos": []any{"acme/*"}}},
			{Name: "second", Config: map[string]any{"repos": []any{"acme/repo-1"}}},
		},
	}
	WarnInstanceRoutingOverlap(logger, configs, capsByName)
	assert.Contains(t, buf.String(), "instance_routing overlap detected")
	assert.Contains(t, buf.String(), "first")
	assert.Contains(t, buf.String(), "second")
}

// TestWarnInstanceRoutingOverlap_NoOverlap_NoWarn pins the no-op
// case so the warning surface stays signal-only.
func TestWarnInstanceRoutingOverlap_NoOverlap_NoWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	capsByName := map[string]plugins.Capabilities{
		"github": {
			Name:              "github",
			SupportsInstances: true,
			InstanceRouting: &plugins.InstanceRoutingSpec{
				Strategy:      "glob_match",
				ConfigField:   "repos",
				MatchTemplate: "{owner}/{repo}",
			},
		},
	}
	configs := map[string][]config.InstanceEntry{
		"github": {
			{Name: "alice", Config: map[string]any{"repos": []any{"alice/*"}}},
			{Name: "bob", Config: map[string]any{"repos": []any{"bob/*"}}},
		},
	}
	WarnInstanceRoutingOverlap(logger, configs, capsByName)
	assert.NotContains(t, buf.String(), "instance_routing overlap detected",
		"disjoint globs must not trigger an overlap warning")
}

// Sanity: ErrUnroutedURL stays distinct from a generic error so
// the /v1/ingest handler can dispatch the correct 400 unrouted-URL
// envelope (errors.Is check).
func TestErrUnroutedURL_IsDistinct(t *testing.T) {
	t.Parallel()
	assert.False(t, errors.Is(ErrNoURLRouting, ErrUnroutedURL))
	assert.False(t, errors.Is(ErrUnsupportedRoutingStrategy, ErrUnroutedURL))
	assert.True(t, errors.Is(ErrUnroutedURL, ErrUnroutedURL))
	// Strings package check: error message wrapping preserves
	// errors.Is dispatch.
	wrapped := errors.New("plugin x: " + ErrUnroutedURL.Error())
	assert.False(t, errors.Is(wrapped, ErrUnroutedURL),
		"plain string-wrap does NOT preserve errors.Is; production uses fmt.Errorf with %w")
	_ = strings.Contains // keep import for the literal check above
}

// TestPickInstance_UnresolvedPlaceholder_FailFast pins the
// plugin-author bug surface from the PR-251 cold-review: when
// the url_pattern captures don't cover every name the
// match_template references, formatMatchTemplate leaves the
// missing name as a literal `{name}` substring. Without the
// fail-fast gate, a permissive instance glob (`acme/*`) could
// match that literal via path.Match — silent mis-routing. Pick
// must reject before the glob walk.
func TestPickInstance_UnresolvedPlaceholder_FailFast(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name: "broken-template",
		URLPatterns: []string{
			// Captures only `owner`; template references both
			// `owner` AND `repo`.
			`^https?://example\.test/(?P<owner>[^/]+)/.*`,
		},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "glob_match",
			ConfigField:   "repos",
			MatchTemplate: "{owner}/{repo}",
		},
	})
	// Operator glob `acme/*` would silently match the literal
	// `acme/{repo}` formatted output via path.Match if the gate
	// weren't in place.
	instances := []config.InstanceEntry{
		{Name: "first", Config: map[string]any{"repos": []any{"acme/*"}}},
		{Name: "second", Config: map[string]any{"repos": []any{"bob/*"}}},
	}
	_, err := pickInstance(p, instances, "https://example.test/acme/xyz")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnresolvedTemplatePlaceholder)
	// Error message must name the unresolved placeholder so the
	// plugin author can fix the url_pattern's named captures.
	assert.Contains(t, err.Error(), "{repo}")
	assert.Contains(t, err.Error(), "broken-template")
}

// --- ADR-0028 Cut 5: enabled flag in pickInstance ---

// TestPickInstance_EnabledFalse_SkipsDisabledInGlobWalk pins ADR-
// 0028 §7 (Cut 5): pickInstance filters disabled instances before
// the glob walk. A URL whose only matching glob lives on a
// disabled instance surfaces as ErrUnroutedURL — the operator
// turned that path off deliberately.
func TestPickInstance_EnabledFalse_SkipsDisabledInGlobWalk(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "github",
		URLPatterns:       []string{`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\d+`},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "glob_match",
			ConfigField:   "repos",
			MatchTemplate: "{owner}/{repo}",
		},
	})
	disabled := false
	instances := []config.InstanceEntry{
		{Name: "personal", Config: map[string]any{"repos": []any{"alice/*"}}, Enabled: &disabled},
		{Name: "acme-org", Config: map[string]any{"repos": []any{"acme-org/*"}}},
	}
	// URL matches the disabled instance's glob only; expect unrouted.
	_, err := pickInstance(p, instances, "https://github.com/alice/project/pull/1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnroutedURL,
		"disabled instance's glob must not satisfy URL routing")
}

// TestPickInstance_EnabledFalse_AllDisabled_Unrouted pins the
// all-disabled case for URL routing: when every configured
// instance is disabled, pickInstance returns ErrUnroutedURL with
// a diagnostic naming the count so the operator knows their
// config turned everything off.
func TestPickInstance_EnabledFalse_AllDisabled_Unrouted(t *testing.T) {
	t.Parallel()
	p := fixturePlugin(t, plugins.Capabilities{
		Name:              "github",
		URLPatterns:       []string{`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/\d+`},
		SupportsInstances: true,
		InstanceRouting: &plugins.InstanceRoutingSpec{
			Strategy:      "glob_match",
			ConfigField:   "repos",
			MatchTemplate: "{owner}/{repo}",
		},
	})
	disabled := false
	instances := []config.InstanceEntry{
		{Name: "personal", Config: map[string]any{"repos": []any{"alice/*"}}, Enabled: &disabled},
		{Name: "work", Config: map[string]any{"repos": []any{"bob/*"}}, Enabled: &disabled},
	}
	_, err := pickInstance(p, instances, "https://github.com/alice/project/pull/1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnroutedURL)
	assert.Contains(t, err.Error(), "no enabled instances")
}

// TestEnabledInstances_FiltersDisabled pins the helper's filter
// semantics: nil pointer = enabled (default); explicit false =
// disabled (filtered out).
func TestEnabledInstances_FiltersDisabled(t *testing.T) {
	t.Parallel()
	disabled := false
	enabled := true
	in := []config.InstanceEntry{
		{Name: "default-on"},                       // nil → enabled
		{Name: "explicit-on", Enabled: &enabled},   // true → enabled
		{Name: "explicit-off", Enabled: &disabled}, // false → filtered
	}
	out := enabledInstances(in)
	require.Len(t, out, 2)
	assert.Equal(t, "default-on", out[0].Name)
	assert.Equal(t, "explicit-on", out[1].Name)
}
