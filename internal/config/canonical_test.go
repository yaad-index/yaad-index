package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalGuard_NilAndEmptyAreEquivalent(t *testing.T) {
	t.Parallel()

	// Three observationally-equivalent constructions of the
	// "no canonical layer" state: nil + nil, empty + empty, mixed.
	cases := []struct {
		name string
		kinds []string
		edgeTypes []string
	}{
		{"nil/nil", nil, nil},
		{"empty/empty", []string{}, []string{}},
		{"nil/empty", nil, []string{}},
		{"empty/nil", []string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewCanonicalGuard(tc.kinds, tc.edgeTypes)
			assert.False(t, g.AllowKind("person"), "no canonical kinds → AllowKind=false")
			assert.False(t, g.AllowEdgeType("is_about"), "no canonical edge types → AllowEdgeType=false")
			assert.Empty(t, g.EnabledKinds(), "no enabled kinds → empty result")
		})
	}
}

func TestCanonicalGuard_AllowsConfiguredKinds(t *testing.T) {
	t.Parallel()

	g := NewCanonicalGuard([]string{"person", "city"}, []string{"is_about", "lives_in"})
	assert.True(t, g.AllowKind("person"))
	assert.True(t, g.AllowKind("city"))
	assert.False(t, g.AllowKind("country"), "kind not in config → not allowed")

	assert.True(t, g.AllowEdgeType("is_about"))
	assert.True(t, g.AllowEdgeType("lives_in"))
	assert.False(t, g.AllowEdgeType("designed"), "edge type not in config → not allowed")

	assert.ElementsMatch(t, []string{"person", "city"}, g.EnabledKinds())
}

func TestCanonicalGuard_DropsEmptyStringEntries(t *testing.T) {
	t.Parallel()

	// Empty entries can leak in from `canonical_edge_types: ["", "is_about"]`
	// (operator typo) or from a code path that derives the kinds slice
	// from raw input. Per ADR-0013 §1 the registry-map keys are validated
	// to match `[a-z][a-z0-9_]*` so the kinds slice from a real config
	// can't carry an empty string in production, but the guard still
	// dedupes defensively at the API boundary — this test pins that.
	g := NewCanonicalGuard([]string{"", "person", ""}, []string{"", "is_about"})
	assert.True(t, g.AllowKind("person"))
	assert.False(t, g.AllowKind(""))
	assert.True(t, g.AllowEdgeType("is_about"))
	assert.False(t, g.AllowEdgeType(""))
	assert.ElementsMatch(t, []string{"person"}, g.EnabledKinds())
}

func TestCanonicalGuard_NilReceiver(t *testing.T) {
	t.Parallel()

	// The tracker can be constructed with nil guard (the implicit
	// "no canonical layer" case). Methods on nil must not panic.
	var g *CanonicalGuard
	assert.False(t, g.AllowKind("person"))
	assert.False(t, g.AllowEdgeType("is_about"))
	assert.Empty(t, g.EnabledKinds())
}
