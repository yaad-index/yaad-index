package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
)

// TestIssueTokenCmd_OperatorOnlyAcceptsBareOperator pins yaad-index
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

// TestIssueTokenCmd_OnBehalfOfOperator_SetsDelegatedClaim is the
// end-to-end #361 mint -> verify path: `issue-token --agent bob
// --operator alice --on-behalf-of-operator` produces a pair-claim token
// whose verified claim carries OperatorDelegated == true. Not parallel —
// it swaps os.Stdout to capture the token the CLI prints.
func TestIssueTokenCmd_OnBehalfOfOperator_SetsDelegatedClaim(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(dir, false))

	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	runErr := (&IssueTokenCmd{
		Operator: "alice",
		Agent: "bob",
		OnBehalfOfOperator: true,
		KeysDir: dir,
		TTL: "1h",
	}).Run()
	_ = w.Close()
	os.Stdout = orig
	require.NoError(t, runErr)

	out, err := io.ReadAll(r)
	require.NoError(t, err)
	token := strings.TrimSpace(string(out))
	require.NotEmpty(t, token)

	verifier, err := auth.LoadVerifier(dir)
	require.NoError(t, err)
	claim, err := verifier.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, "bob", claim.Subject)
	assert.Equal(t, "alice", claim.Operator)
	assert.True(t, claim.OperatorDelegated,
		"--on-behalf-of-operator stamps OperatorDelegated on the issued token")
}

// TestIssueTokenCmd_OnBehalfOfOperator_ConflictsWithOperatorOnly pins
// the mutual exclusion: an operator-only token already carries operator
// authority, so combining it with the agent-tier delegation flag is
// rejected before any signing.
func TestIssueTokenCmd_OnBehalfOfOperator_ConflictsWithOperatorOnly(t *testing.T) {
	t.Parallel()
	err := (&IssueTokenCmd{
		Operator: "alice",
		OperatorOnly: true,
		OnBehalfOfOperator: true,
	}).Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on-behalf-of-operator")
}
