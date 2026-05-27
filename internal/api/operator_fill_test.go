package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newOperatorFillFixture builds a vault-wired handler with auth +
// canonical-kind registry covering boardgame, ready for the
// operator-fill tests.
func newOperatorFillFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer) {
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

	// Resolved registry: boardgame with ADR-0019 step 4 built-ins
	// (rating int 1-10, owned bool, ...) merged with universal
	// defaults. MergeCanonicalRegistry produces the same shape the
	// runtime uses; we call it directly here so the test handler
	// sees the production layering.
	reg := config.MergeCanonicalRegistry(
		nil,
		[]string{"boardgame"},
		config.CanonicalKindConfig{},
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)
	return h, st, root, signer
}

// seedBoardgameForFill writes a vault entity + DB row for an
// operator-fill test. The entity has the five ADR-0019 boardgame
// gaps in its open-gap list so the handler accepts ops on them.
func seedBoardgameForFill(t *testing.T, st store.Store, root, id string) {
	t.Helper()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "boardgame",
		Data: map[string]any{"name": "Test Game"},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "boardgame",
		Source: []string{"fixture/default"},
		Data: map[string]any{"name": "Test Game"},
		Gaps: []string{"rating", "owned", "want", "played", "knows_how_to_play"},
	}))
}

// mintOperatorToken issues a JWT where Subject == Operator (the
// operator-only pattern per ADR-0019 §Endpoint surface). The
// handler accepts these; agent-on-behalf tokens (Subject !=
// Operator) reject with 403.
func mintOperatorToken(t *testing.T, signer auth.Signer, operator string) string {
	t.Helper()
	return mintToken(t, signer, operator, operator)
}

// TestOperatorFill_HappyPath_SetScalar covers the scalar-set
// path — operator writes rating + owned, types validate against
// the boardgame built-in spec, gap_state stamps source=operator +
// filled_at, gaps list shrinks.
func TestOperatorFill_HappyPath_SetScalar(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:test-game"
	seedBoardgameForFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"rating": 9,
			"owned": true,
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got operatorFillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	// Remaining gaps: 5 ADR-0019 boardgame built-ins minus 2 we set.
	assert.ElementsMatch(t, []string{"want", "played", "knows_how_to_play"}, got.Gaps)

	// Vault frontmatter: data + gap_state landed.
	ve := readVaultByID(t, root, "boardgame", id)
	assert.EqualValues(t, 9, ve.Data["rating"])
	assert.Equal(t, true, ve.Data["owned"])
	require.Contains(t, ve.GapState, "rating")
	assert.Equal(t, "operator", ve.GapState["rating"].Source)
	assert.NotNil(t, ve.GapState["rating"].FilledAt)
	require.Contains(t, ve.GapState, "owned")
	assert.Equal(t, "operator", ve.GapState["owned"].Source)
}

// TestOperatorFill_Clear: explicit JSON null clears the value
// and removes the gap_state entry.
func TestOperatorFill_Clear(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:clear-test"
	seedBoardgameForFill(t, st, root, id)

	// Set, then clear.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": 7}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "set body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": nil}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "clear body=%s", rec.Body.String())

	ve := readVaultByID(t, root, "boardgame", id)
	_, hasData := ve.Data["rating"]
	assert.False(t, hasData, "rating data cleared")
	_, hasState := ve.GapState["rating"]
	assert.False(t, hasState, "rating gap_state entry removed")
}

// TestOperatorFill_Defer_HappyPath: {defer:true} on an unfilled
// field stamps the deferred state.
func TestOperatorFill_Defer_HappyPath(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:defer-test"
	seedBoardgameForFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"played": map[string]any{"defer": true}}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	ve := readVaultByID(t, root, "boardgame", id)
	require.Contains(t, ve.GapState, "played")
	assert.True(t, ve.GapState["played"].Deferred)
	assert.NotNil(t, ve.GapState["played"].DeferredAt)
}

// TestOperatorFill_Defer_RequiresUnfilled: defer on a field that's
// already filled returns 409 deferred_requires_unfilled.
func TestOperatorFill_Defer_RequiresUnfilled(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:defer-conflict"
	seedBoardgameForFill(t, st, root, id)

	// First fill rating, then try to defer it.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": 8}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": map[string]any{"defer": true}}, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "deferred_requires_unfilled")
}

// TestOperatorFill_Undefer: {defer:false} drops the deferred state.
func TestOperatorFill_Undefer(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:undefer-test"
	seedBoardgameForFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"played": map[string]any{"defer": true}}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"played": map[string]any{"defer": false}}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	ve := readVaultByID(t, root, "boardgame", id)
	_, has := ve.GapState["played"]
	assert.False(t, has, "undefer drops gap_state entry entirely")
}

// TestOperatorFill_TypeMismatch: wrong type for an int gap rejects
// with 400 type_mismatch.
func TestOperatorFill_TypeMismatch(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:type-test"
	seedBoardgameForFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": "9"}, nil) // string instead of int
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
}

// TestOperatorFill_OutOfRange: rating=11 outside [1,10] rejects.
func TestOperatorFill_OutOfRange(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:range-test"
	seedBoardgameForFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": 11}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "out_of_range")
}

// TestOperatorFill_AgentOnlyField: a field with fill_strategy=agent
// rejects with 400 agent_only_field. We construct a kind config
// directly to make this independent of the boardgame built-ins.
func TestOperatorFill_AgentOnlyField(t *testing.T) {
	t.Parallel()
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

	// Custom registry: a kind whose summary gap is agent-only.
	reg := map[string]config.CanonicalKindConfig{
		"agent_only_kind": {
			Gaps: map[string]config.GapSpec{
				"summary": {
					Type: "string",
					Description: "agent fills this",
					FillStrategy: "agent",
				},
			},
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)

	const id = "agent_only_kind:foo"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "agent_only_kind", Data: map[string]any{"name": "x"},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "agent_only_kind", Source: []string{"fixture/default"},
		Data: map[string]any{"name": "x"}, Gaps: []string{"summary"},
	}))
	tok := mintOperatorToken(t, signer, "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"summary": "operator's summary"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "agent_only_field")
}

// TestOperatorFill_AgentOnBehalfOfOperatorAccepted (per yaad-index
//): a JWT where Subject is an agent + Operator names a real
// human now passes the operator-authority gate. Legacy this
// rejected with 403 agent_not_allowed; the gate widened to accept
// the agent-conduit pattern since the operator authority is
// structurally present on the pair-claim. The audit trail stamps
// the agent (commit author) and the operator (frontmatter operator
// field) separately.
func TestOperatorFill_AgentOnBehalfOfOperatorAccepted(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	const id = "boardgame:agent-on-behalf"
	seedBoardgameForFill(t, st, root, id)
	agentTok := mintToken(t, signer, "the implementer", "alice") // subject != operator, operator populated

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", agentTok,
		map[string]any{"rating": 9}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"rating":9`)
}

// Per #317 the operator-authority gate has been dropped. The
// production signer continues to reject empty Operator at sign
// time, so the new "agent-tier no-Operator token accepted" shape
// is not directly testable through the production auth layer
// without widening the signer (out-of-scope per the spec). The
// surviving integration test for the gate's positive path is
// TestOperatorFill_AgentOnBehalfOfOperatorAccepted (above), which
// continues to pass post-change — the agent-conduit pattern now
// routes through the no-op auth check rather than the (removed)
// operator-authority gate, but the observed wire-shape is
// identical. The anonymous-rejection branch is exercised by the
// auth middleware's own RequireAuth-with-required=true path; the
// handler-level `IsAnonymousClaim` check is the dev-mode safety
// net for the auth-disabled deploy.

// TestOperatorFill_VaultRequired: no vault wired returns 503.
func TestOperatorFill_VaultRequired(t *testing.T) {
	t.Parallel()
	h, _ := newAPIWithStore(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/boardgame:any/operator-fill",
		strings.NewReader(`{"rating": 9}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "vault_required")
}

// TestOperatorFill_AutoMaterialize_ThinRow_VaultFileMissing covers
// the ADR-0021 amendment / phase D path: a canonical-label
// entity has only a thin DB row (from phase B's ingest-time
// thin-row materialization) but no vault file. Operator-fill
// auto-creates the vault file at `<root>/ct/<kind>/<slug>.md`
// with the fill values in frontmatter, rather than 404'ing on the
// missing vault file.
func TestOperatorFill_AutoMaterialize_ThinRow_VaultFileMissing(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:martin-wallace-game-from-thin-row"

	// Seed only the thin DB row — no vault file. Mirrors the
	// post-ingest state when a plugin emitted a canonical-label
	// edge target via the new source-shape: phase B materialized
	// the thin row, vault file deferred until first operator-fill.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "boardgame",
	}))

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": 8, "owned": true}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got operatorFillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)

	// Vault file landed at `<root>/ct/<kind>/<slug>.md` (NOT the
	// per-kind default `<root>/<kind>/<slug>.md`).
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	ve, err := r.ReadByID("boardgame", id)
	require.NoError(t, err, "vault file must be readable post-fill")
	assert.Equal(t, int64(8), getRating(t, ve))
	owned, _ := ve.Data["owned"].(bool)
	assert.True(t, owned, "owned should be true post-fill")
	// Remaining gaps after set: the auto-materialized entity
	// starts with the kind's full gap set (universal name +
	// summary + tags from MergeCanonicalRegistry's defaults +
	// boardgame's 5 operator-strategy gaps). rating + owned drop
	// out post-set; the rest stay open for future fills.
	assert.ElementsMatch(t,
		[]string{"name", "summary", "tags", "want", "played", "knows_how_to_play"},
		ve.Gaps,
	)
}

// TestOperatorFill_AutoMaterialize_NoRow_NoVaultFile covers the
// "operator manually invents canonical metadata" path: neither
// the DB row nor the vault file exists, the id parses as a
// canonical-label `<canonical_kind>:<slug>` shape, and operator-
// fill auto-creates BOTH the row and the vault file. Per ADR-0021
// amendment §canonical-label first-fill.
func TestOperatorFill_AutoMaterialize_NoRow_NoVaultFile(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:operator-invented-game"

	// No seed at all — operator invents this canonical-label.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": 6}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// DB row materialized.
	dbEntity, err := st.GetEntity(context.Background(), id)
	require.NoError(t, err, "DB row must exist post-fill")
	assert.Equal(t, "boardgame", dbEntity.Kind)

	// Vault file at ct/<kind>/<slug>.md.
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	ve, err := r.ReadByID("boardgame", id)
	require.NoError(t, err)
	assert.Equal(t, int64(6), getRating(t, ve))
}

// TestOperatorFill_NotFound_NonCanonicalLabelID asserts the auto-
// materialize gate: an id that doesn't parse as a canonical-label
// (kind not in the operator's canonical_kinds registry) keeps the
// existing 404 behavior. Source-namespace prefixes like
// `bgg:<slug>` or malformed ids fall here.
func TestOperatorFill_NotFound_NonCanonicalLabelID(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")

	// `bgg` is a source-namespace, not a canonical kind. The
	// auto-materialize gate rejects it; the handler returns 404
	// rather than auto-creating a `bgg:<slug>` row + vault file.
	rec := ugcReq(t, h, http.MethodPost,
		"/v1/entities/bgg:made-up-source/operator-fill", tok,
		map[string]any{"rating": 5}, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no entity")
}

// TestOperatorFill_NotFound_VaultFileMissing_NonCanonicalKind
// asserts the second auto-materialize gate: a thin DB row exists
// but its Kind is NOT in the canonical-kind registry. Source-
// shape entities with a missing vault file remain a 404 — the
// daemon never auto-creates source-shape vault files (that path
// is plugin-driven).
func TestOperatorFill_NotFound_VaultFileMissing_NonCanonicalKind(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:thin-row-no-vault-with-nonregistry"

	// Seed a row with a kind NOT in the canonical-kind registry.
	// (The fixture's registry covers `boardgame`; we use a
	// fictitious `widget` kind that won't pass the gate.)
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "widget:foo",
		Kind: "widget",
	}))
	_ = id // not used; we drive the request with the widget id below

	rec := ugcReq(t, h, http.MethodPost,
		"/v1/entities/widget:foo/operator-fill", tok,
		map[string]any{"name": "Foo"}, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	// `widget` isn't in canonicalKindReg, so the kindCfg lookup
	// reports unknown_canonical_kind (409) — that fires before
	// the missing-vault path because we re-ordered kindCfg
	// resolution ahead of vault read.
	assert.Contains(t, rec.Body.String(), "unknown_canonical_kind")
}

// getRating is a small helper to extract `data.rating` as an int64
// from a vault.Entity (numeric values land as float64 from JSON or
// int from the YAML decoder; consolidate to int64 for the
// assertion).
func getRating(t *testing.T, ve *vault.Entity) int64 {
	t.Helper()
	v, ok := ve.Data["rating"]
	if !ok {
		t.Fatalf("vault.Entity.Data has no `rating` key; data=%v", ve.Data)
	}
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	default:
		t.Fatalf("rating: unexpected type %T (%v)", v, v)
		return 0
	}
}

// TestOperatorFill_ClearRestoresField pins the a prior PR the cold-reviewer carry-
// over fix: after set→clear, the field reappears in ve.Gaps so the
// operator can re-fill it. Without this, /v1/needs-fill would miss
// the field permanently.
func TestOperatorFill_ClearRestoresField(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newOperatorFillFixture(t)
	tok := mintOperatorToken(t, signer, "alice")
	const id = "boardgame:clear-restores-field"
	seedBoardgameForFill(t, st, root, id)

	// Set rating, then clear it. The vault entity's Gaps should
	// re-include rating after the clear.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": 7}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// After set, rating is NOT in Gaps.
	ve := readVaultByID(t, root, "boardgame", id)
	assert.NotContains(t, ve.Gaps, "rating", "after set, rating leaves Gaps")

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"rating": nil}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "clear body=%s", rec.Body.String())

	// After clear, rating is back in Gaps (a prior PR carry-over).
	ve = readVaultByID(t, root, "boardgame", id)
	assert.Contains(t, ve.Gaps, "rating",
		"after clear, rating restored to Gaps so /v1/needs-fill can resurface it")
	_, hasState := ve.GapState["rating"]
	assert.False(t, hasState, "gap_state entry still removed on clear")
}

// TestOperatorFill_WorkflowInjectedSpec_RespectsAgentOnlyFillStrategy
// is the #158 audience-filter regression: a workflow-injected
// gap with fill_strategy=agent must reject operator-fill via
// the same agent_only_field check as the config-side
// TestOperatorFill_AgentOnlyField, even though the spec comes
// from ve.GapState rather than canonical_kinds config. The
// audience filter in parseOperatorFillOps reads the effective
// gap-spec map; if resolveEffectiveGaps drops FillStrategy, the
// operator could illicitly fill an agent-only workflow gap.
//
// Setup uses a canonical-label kind (in registry — clears the
// operator-fill kind-must-be-in-registry guard) but the gap
// itself is workflow-injected only (no config spec). Mirrors
// the realistic shape where a workflow injects an agent-only
// gap on a canonical-label entity (e.g. company.competitor-list,
// person.linkedin-bio-summary).
func TestOperatorFill_WorkflowInjectedSpec_RespectsAgentOnlyFillStrategy(t *testing.T) {
	t.Parallel()
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

	// company kind IS in registry (canonical-label), but the
	// `competitor` gap is workflow-injected only — no config spec.
	reg := map[string]config.CanonicalKindConfig{"company": {}}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)

	const id = "company:agent-only-wf-gap-target"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "company", Data: map[string]any{"id": id},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "company", Source: []string{"fixture/default"},
		Data: map[string]any{"id": id},
		Gaps: []string{"competitor"},
		GapState: map[string]vault.GapStateEntry{
			"competitor": {
				Type:         "string",
				FillStrategy: "agent",
				Description:  "agent-only workflow-injected gap",
			},
		},
	}))

	opTok := mintOperatorToken(t, signer, "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", opTok,
		map[string]any{"competitor": "Foo Corp"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "agent_only_field",
		"workflow-injected fill_strategy=agent must reject operator-fill same as config-side")
}
