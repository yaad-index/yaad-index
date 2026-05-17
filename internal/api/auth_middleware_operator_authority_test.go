// Tests for the ClaimHasOperatorAuthority helper added in
// yaad-index — the operator-authority gate replaces the
// brittle Subject==Operator check at operator-fill + add_note
// call sites. The gate accepts agent-on-behalf-of-operator
// pair-claims (Subject is an agent, Operator names the human),
// which the prior check rejected even though the operator
// authority was structurally present.

package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/yaad-index/internal/auth"
)

func TestClaimHasOperatorAuthority(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		c *auth.Claim
		want bool
	}{
		{
			name: "nil claim",
			c: nil,
			want: false,
		},
		{
			name: "anonymous dev-mode claim",
			c: &auth.Claim{Subject: anonymousSubject, Operator: anonymousOperator},
			want: false,
		},
		{
			name: "operator acting directly (Subject == Operator)",
			c: &auth.Claim{Subject: "alice", Operator: "alice"},
			want: true,
		},
		{
			name: "agent acting on behalf of operator (Subject != Operator, Operator populated)",
			c: &auth.Claim{Subject: "the implementer", Operator: "alice"},
			want: true,
		},
		{
			name: "agent-only token with no operator (theoretical; signer rejects)",
			c: &auth.Claim{Subject: "the implementer", Operator: ""},
			want: false,
		},
		{
			name: "empty subject + operator populated (defensive)",
			c: &auth.Claim{Subject: "", Operator: "alice"},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ClaimHasOperatorAuthority(tc.c))
		})
	}
}
