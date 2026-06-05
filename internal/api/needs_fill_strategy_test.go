package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// nfStrategyFixture seeds a single auth-required handler whose one
// gap-callable entity carries three gaps of differing fill_strategy
// (agent / operator / both) so the #459 fill_strategy audience filter
// can be exercised against operator- and agent-authed callers.
//
// Registry gaps:
//   - summary  → fill_strategy "agent"    (agent-fillable only)
//   - rating   → fill_strategy "operator" (operator-fillable only)
//   - tags     → fill_strategy "both"     (both audiences)
func nfStrategyFixture(t *testing.T) (http.Handler, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	now := time.Now().UTC()
	const id = "boardgame:example-one"
	require.NoError(t, st.SaveEntity(t.Context(), &store.Entity{
		ID:   id,
		Kind: "boardgame",
		Data: map[string]any{"id": id},
		GapState: map[string]store.GapStateEntry{
			"summary": {},
			"rating":  {},
			"tags":    {},
		},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: &now, OK: true},
		},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID:           id,
		Kind:         "boardgame",
		Source:       []string{"fixture/default"},
		Data:         map[string]any{"id": id},
		Gaps:         []string{"summary", "rating", "tags"},
		CleanContent: "stub for " + id,
	}))

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {Gaps: map[string]config.GapSpec{
			"summary": {Description: "Game summary.", FillStrategy: "agent"},
			"rating":  {Description: "Operator rating.", FillStrategy: "operator"},
			"tags":    {Description: "Tags.", FillStrategy: "both"},
		}},
	}
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)
	return h, signer
}

// nfStrategyGet issues an authed GET and decodes the response, asserting 200.
func nfStrategyGet(t *testing.T, h http.Handler, target, bearer string) needsFillResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got needsFillResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

// nfGapNames returns the sorted gap field names on the single seeded
// entity (the fixture seeds exactly one gap-callable entity).
func nfGapNames(t *testing.T, resp needsFillResponse) []string {
	t.Helper()
	require.Len(t, resp.Entities, 1)
	names := make([]string, 0, len(resp.Entities[0].Gaps))
	for k := range resp.Entities[0].Gaps {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// TestNeedsFill_FillStrategyAgent_OverridesOperatorAuth pins #459: an
// operator-authed caller passing ?fill_strategy=agent sees the agent-
// fillable slice (agent + both gaps), NOT the operator-only gap — the
// param overrides the auth-derived audience.
func TestNeedsFill_FillStrategyAgent_OverridesOperatorAuth(t *testing.T) {
	t.Parallel()
	h, signer := nfStrategyFixture(t)
	opTok := mintToken(t, signer, "alice", "alice") // Subject==Operator

	got := nfStrategyGet(t, h, "/v1/needs-fill?fill_strategy=agent", opTok)
	assert.Equal(t, []string{"summary", "tags"}, nfGapNames(t, got),
		"agent view: agent + both gaps, operator gap excluded (override beats operator auth)")
}

// TestNeedsFill_FillStrategyOperator_OverridesAgentAuth pins #459: an
// agent-authed caller passing ?fill_strategy=operator sees the operator-
// fillable slice (operator + both gaps), NOT the agent-only gap.
func TestNeedsFill_FillStrategyOperator_OverridesAgentAuth(t *testing.T) {
	t.Parallel()
	h, signer := nfStrategyFixture(t)
	agentTok := mintToken(t, signer, "agent-one", "alice") // Subject!=Operator

	got := nfStrategyGet(t, h, "/v1/needs-fill?fill_strategy=operator", agentTok)
	assert.Equal(t, []string{"rating", "tags"}, nfGapNames(t, got),
		"operator view: operator + both gaps, agent gap excluded (override beats agent auth)")
}

// TestNeedsFill_NoStrategy_OperatorAuthUnchanged pins the omitted-param
// default for an operator caller: auth-derived operator view (operator +
// both gaps).
func TestNeedsFill_NoStrategy_OperatorAuthUnchanged(t *testing.T) {
	t.Parallel()
	h, signer := nfStrategyFixture(t)
	opTok := mintToken(t, signer, "alice", "alice")

	got := nfStrategyGet(t, h, "/v1/needs-fill", opTok)
	assert.Equal(t, []string{"rating", "tags"}, nfGapNames(t, got),
		"operator auth, no override: operator + both gaps")
}

// TestNeedsFill_NoStrategy_AgentAuthUnchanged pins the omitted-param
// default for an agent caller: auth-derived agent view (agent + both gaps).
func TestNeedsFill_NoStrategy_AgentAuthUnchanged(t *testing.T) {
	t.Parallel()
	h, signer := nfStrategyFixture(t)
	agentTok := mintToken(t, signer, "agent-one", "alice")

	got := nfStrategyGet(t, h, "/v1/needs-fill", agentTok)
	assert.Equal(t, []string{"summary", "tags"}, nfGapNames(t, got),
		"agent auth, no override: agent + both gaps")
}

// TestNeedsFill_FillStrategyInvalid_400 pins that a bogus fill_strategy
// value is rejected with 400 invalid_argument.
func TestNeedsFill_FillStrategyInvalid_400(t *testing.T) {
	t.Parallel()
	h, signer := nfStrategyFixture(t)
	opTok := mintToken(t, signer, "alice", "alice")

	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill?fill_strategy=bogus", nil)
	req.Header.Set("Authorization", "Bearer "+opTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	var errResp errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, "invalid_argument", errResp.Error)
}
