package canonical

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDaemonEntityKinds pins the entity-kind vocabulary the
// daemon always allows. Set today: `day` per ADR-0025 cut 1,
// `task` per the ADR-0024 alignment landed in #268, and the
// gmail-emitted `email` / `email-address` / `label` kinds per
// #272. Week / month / year are deferred to a later layer per
// ADR-0025 § Entity kinds.
func TestDaemonEntityKinds(t *testing.T) {
	t.Parallel()
	got := DaemonEntityKinds()
	assert.Equal(t, []string{DayKind, TaskKind, EmailKind, EmailAddressKind, LabelKind}, got)
	assert.Equal(t, "day", DayKind, "slug-side spelling matches ADR-0025 § Entity kinds")
	assert.Equal(t, "task", TaskKind, "slug-side spelling matches ADR-0024 §Task")
	assert.Equal(t, "email", EmailKind)
	assert.Equal(t, "email-address", EmailAddressKind)
	assert.Equal(t, "label", LabelKind)
}

// TestDaemonEdgeTypes pins the edge type vocabulary the daemon
// always allows. Cut-1 set per ADR-0025 § Edge types (the five
// time-bound relationships) plus `triggered_by` per #268 for
// the task → source attribution edge plus the gmail-emitted
// from/to/cc/bcc/tagged_as set per #272.
func TestDaemonEdgeTypes(t *testing.T) {
	t.Parallel()
	want := []string{
		"due_on",
		"occurred_on",
		"is_about_day",
		"references_day",
		"ingested_on",
		"triggered_by",
		"from",
		"to",
		"cc",
		"bcc",
		"tagged_as",
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
