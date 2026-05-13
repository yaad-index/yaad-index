package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

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
