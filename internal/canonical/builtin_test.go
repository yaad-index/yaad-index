package canonical

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDaemonEntityKinds_IncludesDay pins the cut-1 entity-kind
// vocabulary: just `day` for v1.x. Week / month / year are
// deferred to a later layer per ADR-0025 § Entity kinds.
func TestDaemonEntityKinds_IncludesDay(t *testing.T) {
	t.Parallel()
	got := DaemonEntityKinds()
	assert.Equal(t, []string{DayKind}, got)
	assert.Equal(t, "day", DayKind, "slug-side spelling matches ADR-0025 § Entity kinds")
}

// TestDaemonEdgeTypes_FiveCanonicalNames pins the cut-1 edge type
// vocabulary per ADR-0025 § Edge types: the five names that
// describe time-bound relationships.
func TestDaemonEdgeTypes_FiveCanonicalNames(t *testing.T) {
	t.Parallel()
	want := []string{
		"due_on",
		"occurred_on",
		"is_about_day",
		"references_day",
		"ingested_on",
	}
	assert.Equal(t, want, DaemonEdgeTypes())
}

// TestNewGuardWithDaemonDefaults_FoldsInDaemonSet pins the
// load-bearing contract: the daemon-built-in `day` kind and the
// five canonical edge types are always allowed, regardless of
// operator config.
func TestNewGuardWithDaemonDefaults_FoldsInDaemonSet(t *testing.T) {
	t.Parallel()
	g := NewGuardWithDaemonDefaults(nil, nil)
	assert.True(t, g.AllowKind(DayKind), "day kind must be allowed even with empty operator config")
	for _, edge := range DaemonEdgeTypes() {
		assert.True(t, g.AllowEdgeType(edge),
			"canonical edge %q must be allowed even with empty operator config", edge)
	}
}

// TestNewGuardWithDaemonDefaults_PreservesOperatorEntries pins
// that operator-configured kinds + edges still flow through —
// the daemon set is additive, not replacing.
func TestNewGuardWithDaemonDefaults_PreservesOperatorEntries(t *testing.T) {
	t.Parallel()
	g := NewGuardWithDaemonDefaults(
		[]string{"person", "city"},
		[]string{"is_about", "lives_in"},
	)
	assert.True(t, g.AllowKind("person"))
	assert.True(t, g.AllowKind("city"))
	assert.True(t, g.AllowKind(DayKind), "daemon set also folded in")
	assert.True(t, g.AllowEdgeType("is_about"))
	assert.True(t, g.AllowEdgeType("lives_in"))
	assert.True(t, g.AllowEdgeType(EdgeTypeDueOn), "daemon edge also folded in")
	assert.False(t, g.AllowKind("unknown"), "unknown kinds still rejected")
}
