package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
)

// TestCollectPluginEmittedEdgeTypes_UnionDedupes pins the
// ADR-0016 §plugin-driven-activation symmetry for edge types
// (yaad-index #9): walking the registry returns the deduped
// union of every plugin's Capabilities.CanonicalEdgeTypesEmitted,
// skipping empty strings.
func TestCollectPluginEmittedEdgeTypes_UnionDedupes(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		CapabilitiesValue: plugins.Capabilities{
			Name: "wikipedia",
			CanonicalEdgeTypesEmitted: []string{"is_about", ""},
		},
	})
	registry.Register(&fixture.Plugin{
		NameValue: "bgg",
		CapabilitiesValue: plugins.Capabilities{
			Name: "bgg",
			CanonicalEdgeTypesEmitted: []string{"is_about", "designed_by"},
		},
	})

	got := collectPluginEmittedEdgeTypes(registry)
	sort.Strings(got)
	assert.Equal(t, []string{"designed_by", "is_about"}, got,
		"plugin-emitted edge types must dedupe across plugins and drop empty strings")
}

// TestCollectPluginEmittedEdgeTypes_EmptyRegistry pins the
// nil-safe path: no registered plugins → empty slice (not nil panic).
func TestCollectPluginEmittedEdgeTypes_EmptyRegistry(t *testing.T) {
	t.Parallel()

	got := collectPluginEmittedEdgeTypes(plugins.NewRegistry())
	assert.Empty(t, got)
}

// TestUnionEdgeTypes_OperatorAndPluginMerge pins the union semantic:
// operator-config canonical_edge_types merge with plugin-declared
// CanonicalEdgeTypesEmitted, dedupe on equal entries, drop empty
// strings.
func TestUnionEdgeTypes_OperatorAndPluginMerge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op []string
		plugin []string
		want []string
	}{
		{
			name: "operator-only — no plugin emission",
			op: []string{"same_as", "is_about"},
			plugin: nil,
			want: []string{"is_about", "same_as"},
		},
		{
			name: "plugin-only — operator omitted (the #9 wikipedia path)",
			op: nil,
			plugin: []string{"is_about"},
			want: []string{"is_about"},
		},
		{
			name: "overlap deduped",
			op: []string{"is_about", "same_as"},
			plugin: []string{"is_about", "designed_by"},
			want: []string{"designed_by", "is_about", "same_as"},
		},
		{
			name: "empty strings dropped from both sides",
			op: []string{"is_about", ""},
			plugin: []string{"", "designed_by"},
			want: []string{"designed_by", "is_about"},
		},
		{
			name: "both empty → empty",
			op: nil,
			plugin: nil,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unionEdgeTypes(tc.op, tc.plugin)
			sort.Strings(got)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- buildCanonicalKindResolvers (#304 Cut A) ---------------------------

// TestBuildCanonicalKindResolvers_HappyPath pins the kind →
// []plugin-name shape: each plugin's ResolvesCanonicalKinds entries
// land in the map under the kind, valued by the plugin's name.
func TestBuildCanonicalKindResolvers_HappyPath(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "wikipedia",
			CanonicalKindsEmitted:  []string{"person", "city"},
			ResolvesCanonicalKinds: []string{"person", "city"},
		},
	})
	registry.Register(&fixture.Plugin{
		NameValue: "bgg",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "bgg",
			CanonicalKindsEmitted:  []string{"boardgame", "person"},
			ResolvesCanonicalKinds: []string{"boardgame"},
		},
	})

	got, err := buildCanonicalKindResolvers(registry, []string{"person", "city", "boardgame"}, nil)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.ElementsMatch(t, []string{"wikipedia"}, got["city"])
	assert.ElementsMatch(t, []string{"bgg"}, got["boardgame"])
	assert.ElementsMatch(t, []string{"wikipedia"}, got["person"],
		"bgg emits but does not resolve person; only wikipedia appears")
}

// TestBuildCanonicalKindResolvers_MultiResolverAllowed pins the
// Cut A cardinality decision: multiple plugins MAY claim the same
// kind. The map records all of them; one-resolver-per-kind
// enforcement is deferred to Cut C with routing.
func TestBuildCanonicalKindResolvers_MultiResolverAllowed(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "wikipedia",
			CanonicalKindsEmitted:  []string{"boardgame"},
			ResolvesCanonicalKinds: []string{"boardgame"},
		},
	})
	registry.Register(&fixture.Plugin{
		NameValue: "bgg",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "bgg",
			CanonicalKindsEmitted:  []string{"boardgame"},
			ResolvesCanonicalKinds: []string{"boardgame"},
		},
	})

	got, err := buildCanonicalKindResolvers(registry, []string{"boardgame"}, nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bgg", "wikipedia"}, got["boardgame"],
		"Cut A records multiple resolvers; Cut C decides cardinality")
}

// TestBuildCanonicalKindResolvers_SubsetViolation pins the strict
// per-plugin gate: declaring a resolver entry for a kind the
// plugin doesn't emit is a typo / contract violation, fail-fast.
func TestBuildCanonicalKindResolvers_SubsetViolation(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "typo",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "typo",
			CanonicalKindsEmitted:  []string{"person"},
			ResolvesCanonicalKinds: []string{"persn"}, // typo'd
		},
	})

	_, err := buildCanonicalKindResolvers(registry, []string{"person"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves_canonical_kinds")
	assert.Contains(t, err.Error(), "persn")
}

// TestBuildCanonicalKindResolvers_EmptyEntriesSkipped pins the
// defensive shape: empty strings in the slice are silently dropped
// (sloppy capabilities don't error or land in the map).
func TestBuildCanonicalKindResolvers_EmptyEntriesSkipped(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "wikipedia",
			CanonicalKindsEmitted:  []string{"person"},
			ResolvesCanonicalKinds: []string{"", "person", ""},
		},
	})

	got, err := buildCanonicalKindResolvers(registry, []string{"person"}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.ElementsMatch(t, []string{"wikipedia"}, got["person"])
}

// TestBuildCanonicalKindResolvers_NoOptInBackCompat pins the
// pre-#304 plugin path: a plugin that doesn't declare
// resolves_canonical_kinds (the case for every plugin today)
// produces an empty map. No errors; cross-plugin coverage WARN
// fires for each emitted kind without a resolver but the build
// completes.
func TestBuildCanonicalKindResolvers_NoOptInBackCompat(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "legacy",
		CapabilitiesValue: plugins.Capabilities{
			Name:                  "legacy",
			CanonicalKindsEmitted: []string{"person", "city"},
		},
	})

	got, err := buildCanonicalKindResolvers(registry, []string{"person", "city"}, nil)
	require.NoError(t, err, "legacy plugins without resolves_canonical_kinds must not error")
	assert.Empty(t, got, "no resolver claims → empty ownership map")
}

// TestBuildCanonicalKindResolvers_NilLoggerSafe pins that a nil
// logger argument doesn't panic; the function falls back to
// slog.Default() for the cross-plugin-coverage WARN.
func TestBuildCanonicalKindResolvers_NilLoggerSafe(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		CapabilitiesValue: plugins.Capabilities{
			Name:                   "wikipedia",
			CanonicalKindsEmitted:  []string{"person"},
			ResolvesCanonicalKinds: []string{"person"},
		},
	})

	got, err := buildCanonicalKindResolvers(registry, []string{"person", "uncovered"}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
}
