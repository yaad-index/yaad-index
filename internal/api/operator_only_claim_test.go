package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// newOperatorOnlyFixture builds a vault-less handler with a fixture
// plugin declaring `commands: ["fetch"]` so command-shape inputs
// pass routing-time validation and reach the operator-only-claim
// gate. Returns the handler + signer so tests can mint claims with
// various Subject/Operator shapes.
func newOperatorOnlyFixture(t *testing.T) (http.Handler, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	d := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(d, false))
	signer, err := auth.LoadSigner(d)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(d)
	require.NoError(t, err)

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, _ string, onEnvelope plugins.EnvelopeFunc, _ plugins.ControlFunc) error {
			return nil // 0-envelope (no_results); test only cares about pre-spawn gating
		},
		CapabilitiesValue: plugins.Capabilities{
			Name: "gmail",
			SourceNamespace: "gmail",
			EntityKinds: []plugins.KindSpec{{Name: "source"}},
			Commands: []string{"fetch"},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry,
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
	)
	return h, signer
}

// signClaim is a small helper to produce a Bearer header for a
// specific (Subject, Operator) pair.
func signClaim(t *testing.T, s auth.Signer, subject, operator string) string {
	t.Helper()
	now := time.Now().UTC()
	tok, err := s.Sign(auth.Claim{
		Subject: subject,
		Operator: operator,
		IssuedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	return "Bearer " + tok
}

func postIngestWithAuth(t *testing.T, h http.Handler, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestOperatorOnly_PairClaimRejectsCommandShape pins the load-bearing
// contract: a pair-claim token (Subject=agent, Operator=human,
// distinct values) on a command-shape input returns 403
// operator_only_required, no subprocess spawn.
func TestOperatorOnly_PairClaimRejectsCommandShape(t *testing.T) {
	t.Parallel()
	h, signer := newOperatorOnlyFixture(t)
	bearer := signClaim(t, signer, "bob", "alice") // Subject != Operator

	rec := postIngestWithAuth(t, h,
		`{"url":"gmail: !fetch","wait_seconds":0}`, bearer)

	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_only_required")
}

// TestOperatorOnly_OperatorOnlyTokenPassesCommandShape pins the
// happy path: a token with Subject==Operator passes the gate.
// Downstream behavior is whatever the dispatch produces (here
// not_found because the fixture plugin's MatchFunc returns false +
// returns no envelopes); the assertion is "not 403."
func TestOperatorOnly_OperatorOnlyTokenPassesCommandShape(t *testing.T) {
	t.Parallel()
	h, signer := newOperatorOnlyFixture(t)
	bearer := signClaim(t, signer, "alice", "alice") // Subject == Operator

	rec := postIngestWithAuth(t, h,
		`{"url":"gmail: !fetch","wait_seconds":0}`, bearer)

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"operator-only token must pass the gate (body=%s)", rec.Body.String())
}

// TestOperatorOnly_OperatorOnlyTokenPassesURLShape pins privilege
// widening: an operator-only token can also call URL-shape
// endpoints (everything pair-claim can call, operator-only can
// too).
func TestOperatorOnly_OperatorOnlyTokenPassesURLShape(t *testing.T) {
	t.Parallel()
	h, signer := newOperatorOnlyFixture(t)
	bearer := signClaim(t, signer, "alice", "alice")

	rec := postIngestWithAuth(t, h,
		`{"url":"https://example.com/foo","wait_seconds":0}`, bearer)

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"operator-only token must work on URL-shape too (body=%s)", rec.Body.String())
}

// TestOperatorOnly_PairClaimPassesURLShape pins existing behavior:
// pair-claim tokens still work on URL-shape inputs (the gate only
// fires on command-shape).
func TestOperatorOnly_PairClaimPassesURLShape(t *testing.T) {
	t.Parallel()
	h, signer := newOperatorOnlyFixture(t)
	bearer := signClaim(t, signer, "bob", "alice")

	rec := postIngestWithAuth(t, h,
		`{"url":"https://example.com/foo","wait_seconds":0}`, bearer)

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"pair-claim on URL-shape must keep working (body=%s)", rec.Body.String())
}

// TestOperatorOnly_NoTokenStill401 pins that the gate doesn't
// short-circuit existing 401 behavior — a missing-token request
// rejects at the auth middleware as 401, not 403.
func TestOperatorOnly_NoTokenStill401(t *testing.T) {
	t.Parallel()
	h, _ := newOperatorOnlyFixture(t)

	rec := postIngestWithAuth(t, h,
		`{"url":"gmail: !fetch","wait_seconds":0}`, "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"missing token still returns 401, not 403 (body=%s)", rec.Body.String())
}

// TestOperatorOnly_AnonymousModePassesCommandShape pins the
// dev-mode permissive path: when auth.required=false, the
// anonymous claim passes the operator-only gate so command-shape
// inputs continue to work without a real token.
func TestOperatorOnly_AnonymousModePassesCommandShape(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, _ string, onEnvelope plugins.EnvelopeFunc, _ plugins.ControlFunc) error {
			return nil
		},
		CapabilitiesValue: plugins.Capabilities{Name: "gmail", SourceNamespace: "gmail", Commands: []string{"fetch"}},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithAuthRequired(false))

	rec := postIngestWithAuth(t, h,
		`{"url":"gmail: !fetch","wait_seconds":0}`, "")

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"anonymous (dev-mode) must pass the gate (body=%s)", rec.Body.String())
}

// TestClaimIsOperatorOnly_DirectShapes pins the helper at the unit
// level so a regression in the gating predicate surfaces as a
// focused test failure rather than only via end-to-end HTTP tests.
func TestClaimIsOperatorOnly_DirectShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		c *auth.Claim
		want bool
	}{
		{"nil claim", nil, false},
		{"operator-only", &auth.Claim{Subject: "alice", Operator: "alice"}, true},
		{"pair-claim", &auth.Claim{Subject: "bob", Operator: "alice"}, false},
		{"empty subject", &auth.Claim{Subject: "", Operator: "alice"}, false},
		{"empty operator", &auth.Claim{Subject: "alice", Operator: ""}, false},
		{"both empty", &auth.Claim{}, false},
		{"anonymous", newAnonymousClaim(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClaimIsOperatorOnly(tc.c)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestClaim_IsOperatorOnly_AuthPackagePin pins the auth-package
// helper. ClaimIsOperatorOnly in the api package wraps this with
// an anonymous-claim permissive shim; the auth.Claim method itself
// is the structural check.
func TestClaim_IsOperatorOnly_AuthPackagePin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		c *auth.Claim
		want bool
	}{
		{"nil", nil, false},
		{"operator-only", &auth.Claim{Subject: "alice", Operator: "alice"}, true},
		{"pair-claim", &auth.Claim{Subject: "bob", Operator: "alice"}, false},
		{"empty subject", &auth.Claim{Subject: "", Operator: "alice"}, false},
		{"empty operator", &auth.Claim{Subject: "alice", Operator: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.c.IsOperatorOnly())
		})
	}
}
