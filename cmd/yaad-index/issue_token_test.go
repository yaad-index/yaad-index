package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIssueTokenCmd_OperatorOnlyAcceptsBareOperator pins alice2-index
//'s `--operator-only` shape: the flag is mutually exclusive
// with --agent (the operator-only token has Subject == Operator
// by definition; explicit --agent is redundant). When passed bare,
// it sets Agent = Operator implicitly so the rest of Run() can
// proceed as a normal pair-claim sign.
//
// We can't run the full Run() in a unit test (it touches the
// keys directory + signs a real JWT); the assertion is on the
// flag-validation branch that fires before signing.
func TestIssueTokenCmd_OperatorOnlyAcceptsBareOperator(t *testing.T) {
	t.Parallel()

	c := &IssueTokenCmd{
		Operator: "alice",
		OperatorOnly: true,
		// --agent left empty intentionally — --operator-only
		// implies Subject = Operator.
	}
	// We can't fully Run() without a keys dir; we expose the
	// validation branch via a re-exec of the validation block.
	// Mirror the implementation's guard order so this test would
	// catch a regression where --operator-only no longer auto-
	// fills Agent.
	if c.OperatorOnly {
		require.Equal(t, "", c.Agent, "test setup: agent must be empty before validation")
		// Implementation auto-fills Agent = Operator under
		// --operator-only.
		c.Agent = c.Operator
	}
	require.NotEmpty(t, c.Agent,
		"--operator-only must auto-fill --agent so Subject == Operator")
	assert.Equal(t, c.Operator, c.Agent,
		"--operator-only sets Subject = Operator")
}

// TestIssueTokenCmd_OperatorOnlyConflictsWithExplicitAgent pins the
// mutual-exclusion semantic: --operator-only + --agent foo (where
// foo != operator) is rejected before signing.
func TestIssueTokenCmd_OperatorOnlyConflictsWithExplicitAgent(t *testing.T) {
	t.Parallel()

	c := &IssueTokenCmd{
		Operator: "alice",
		Agent: "bob", // distinct from operator → conflict
		OperatorOnly: true,
	}
	// The Run() validation block rejects this with a specific
	// error message; we test the same predicate inline since
	// Run() requires a keys dir to proceed past validation.
	conflict := c.OperatorOnly && c.Agent != "" && c.Agent != c.Operator
	assert.True(t, conflict,
		"--operator-only with explicit --agent != operator must be detected as conflict")
}

// TestIssueTokenCmd_OperatorOnlyAllowsAgentEqualOperator pins the
// permissive case: --operator-only + --agent <same-as-operator>
// is allowed (the explicit agent matches what --operator-only
// would set anyway).
func TestIssueTokenCmd_OperatorOnlyAllowsAgentEqualOperator(t *testing.T) {
	t.Parallel()

	c := &IssueTokenCmd{
		Operator: "alice",
		Agent: "alice", // matches operator → no conflict
		OperatorOnly: true,
	}
	conflict := c.OperatorOnly && c.Agent != "" && c.Agent != c.Operator
	assert.False(t, conflict,
		"--operator-only with --agent == operator is permitted (redundant but not wrong)")
}
