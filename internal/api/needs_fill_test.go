package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Per ADR-0013 §6 / yaad-index: `GET /v1/needs-fill` returns
// gap-callable entities (DB `gap_call_done_at IS NULL` AND vault
// `gaps:` non-empty) with the full needs_fill payload — same shape
// as the cache-hit ingest envelope. Pagination uses an opaque
// base64(last-seen-id) cursor over `id ASC` ordering.

// nfRegistryWithBoardgameSummary returns a canonical-kind registry
// containing the `boardgame` kind with a `summary` gap configured.
// Used by tests that seed boardgame entities with a `summary` gap
// — post-yaad-index #4 the registry is the canonical source for
// AI-prompts, so the test fixtures need the gap declared there to
// see it surface in needs-fill responses.
func nfRegistryWithBoardgameSummary() map[string]config.CanonicalKindConfig {
	return map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{
				"summary": "Game summary prompt.",
			}),
		},
	}
}

// nfFixture wires a handler with vault IO + optional global
// fill_instruction + optional canonical_kinds registry, and seeds
// `count` entities with given ids each carrying open gaps. Returns
// the handler + store + vault IO so callers can mutate.
func nfFixture(
	t *testing.T,
	ids []string,
	instruction string,
	reg map[string]config.CanonicalKindConfig,
) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	fetchedAt := &now
	for _, id := range ids {
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID: id,
			Kind: "boardgame",
			Data: map[string]any{"id": id},
			Provenance: []store.ProvenanceEntry{
				{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
			},
		}))
		require.NoError(t, w.Write(&vault.Entity{
			ID: id,
			Kind: "boardgame",
			Plugin: "seed",
			Data: map[string]any{"id": id},
			Gaps: []string{"summary"},
			Provenance: []vault.ProvenanceEntry{
				{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
			},
			CleanContent: "stub-clean-content for " + id,
		}))
	}

	opts := []HandlerOption{WithVaultIO(w, r)}
	if instruction != "" {
		opts = append(opts, WithFillInstruction(instruction))
	}
	if reg != nil {
		opts = append(opts, WithCanonicalKindRegistry(reg))
	}
	return NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		opts...,
	), st
}

func decodeNFResponse(t *testing.T, rec *httptest.ResponseRecorder) needsFillResponse {
	t.Helper()
	var got needsFillResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got),
		"decode needs-fill response; body=%s", rec.Body.String())
	return got
}

// Empty store → empty result, no cursor.
func TestNeedsFill_EmptyStore_EmptyEntities(t *testing.T) {
	t.Parallel()
	h, _ := nfFixture(t, nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.True(t, got.OK)
	assert.Empty(t, got.Entities)
	assert.Empty(t, got.NextCursor)
}

// Single entity with unfilled gaps + flag NULL → returned.
func TestNeedsFill_SingleCandidate_Returned(t *testing.T) {
	t.Parallel()
	h, _ := nfFixture(t, []string{"boardgame:a"}, "G", nfRegistryWithBoardgameSummary())
	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Equal(t, "boardgame:a", got.Entities[0].ID)
	assert.Contains(t, got.Entities[0].Gaps, "summary")
	assert.Equal(t, "G", got.Entities[0].Instruction)
	assert.Empty(t, got.NextCursor, "fewer than limit candidates → no cursor")
}

// Pagination: 5 candidates, limit=2 → 2 entities + cursor; cursor's
// next page returns next 2; final page (1 entity) has no cursor.
func TestNeedsFill_PaginationCursor(t *testing.T) {
	t.Parallel()
	ids := []string{"boardgame:a", "boardgame:b", "boardgame:c", "boardgame:d", "boardgame:e"}
	h, _ := nfFixture(t, ids, "", nfRegistryWithBoardgameSummary())

	// Page 1: limit=2 → ["a", "b"] + cursor.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill?limit=2", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	page1 := decodeNFResponse(t, rec)
	require.Len(t, page1.Entities, 2)
	assert.Equal(t, "boardgame:a", page1.Entities[0].ID)
	assert.Equal(t, "boardgame:b", page1.Entities[1].ID)
	require.NotEmpty(t, page1.NextCursor, "full page → cursor must be set")

	// Decoded cursor should be the last-considered id.
	decoded, err := base64.URLEncoding.DecodeString(page1.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, "boardgame:b", string(decoded))

	// Page 2: limit=2 + cursor → ["c", "d"] + cursor.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?limit=2&cursor="+page1.NextCursor, nil))
	page2 := decodeNFResponse(t, rec)
	require.Len(t, page2.Entities, 2)
	assert.Equal(t, "boardgame:c", page2.Entities[0].ID)
	assert.Equal(t, "boardgame:d", page2.Entities[1].ID)
	require.NotEmpty(t, page2.NextCursor)

	// Page 3 (last): limit=2 + cursor → ["e"], no cursor.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?limit=2&cursor="+page2.NextCursor, nil))
	page3 := decodeNFResponse(t, rec)
	require.Len(t, page3.Entities, 1)
	assert.Equal(t, "boardgame:e", page3.Entities[0].ID)
	assert.Empty(t, page3.NextCursor, "fewer than limit → no cursor")
}

// Entity with all gaps filled (vault `gaps:` empty) → not in response.
func TestNeedsFill_AllGapsFilled_Excluded(t *testing.T) {
	t.Parallel()
	// Build a custom fixture: entity exists in DB (flag NULL by default)
	// but vault frontmatter has empty Gaps. The handler's vault-read
	// filter should exclude it.
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: "boardgame:filled",
		Kind: "boardgame",
		Data: map[string]any{"id": "boardgame:filled"},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: "boardgame:filled",
		Kind: "boardgame",
		Plugin: "seed",
		Data: map[string]any{"id": "boardgame:filled"},
		Gaps: nil, // no unfilled gaps
	}))
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities, "entity with no unfilled gaps → excluded from list")
}

// Entity with unfilled gaps but flag SET → not in response (lifecycle
// suppression per ADR-0013 §4).
func TestNeedsFill_FlagSet_Excluded(t *testing.T) {
	t.Parallel()
	h, st := nfFixture(t, []string{"boardgame:flagged"}, "", nil)
	require.NoError(t, st.MarkGapCallDone(context.Background(), "boardgame:flagged"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities, "flag set → excluded by SQL filter")
}

// Limit handling: 0 / negative / non-integer → default 50.
func TestNeedsFill_LimitDefaulting(t *testing.T) {
	t.Parallel()
	// Seed exactly 60 entities so we can prove the default-50 cap is
	// applied (returned count = 50, cursor non-empty).
	ids := make([]string, 60)
	for i := range ids {
		ids[i] = fmt.Sprintf("boardgame:%03d", i)
	}
	h, _ := nfFixture(t, ids, "", nfRegistryWithBoardgameSummary())

	for _, raw := range []string{"", "0", "-3", "abc"} {
		raw := raw
		t.Run("limit="+raw, func(t *testing.T) {
			t.Parallel()
			url := "/v1/needs-fill"
			if raw != "" {
				url += "?limit=" + raw
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			got := decodeNFResponse(t, rec)
			assert.Len(t, got.Entities, 50, "default limit should clamp to 50")
		})
	}
}

// Limit > cap → silently clamps to 200.
func TestNeedsFill_LimitOverCap_ClampedTo200(t *testing.T) {
	t.Parallel()
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("boardgame:%04d", i)
	}
	h, _ := nfFixture(t, ids, "", nfRegistryWithBoardgameSummary())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill?limit=999", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Len(t, got.Entities, 200, "limit 999 → clamp to cap 200")
}

// Malformed cursor → 400 invalid_argument.
func TestNeedsFill_MalformedCursor_400(t *testing.T) {
	t.Parallel()
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?cursor=not-base64!!!", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.False(t, body.OK)
	assert.Equal(t, "invalid_argument", body.Error)
	assert.Contains(t, body.Message, "cursor")
}

// Per-entity payload shape parity with the cache-hit needs_fill
// envelope. Both endpoints must produce the same instruction
// resolution + canonical_vocabulary layout for the same inputs;
// the shared `buildNeedsFillEntry` helper guarantees this. This
// test exercises the helper directly with a per-kind override and
// asserts the resolved instruction picks the per-kind value.
func TestNeedsFill_PerKindInstruction_Resolves(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{"summary": "Game summary."}),
			Instruction: config.InstructionFromString("PER_KIND"),
		},
	}
	h, _ := nfFixture(t, []string{"boardgame:a"}, "GLOBAL", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Equal(t, "PER_KIND", got.Entities[0].Instruction,
		"per-kind override wins over global, same as cache-hit envelope")
	// Wire shape per ADR-0016: registry projects through to the
	// legacy flat `gaps: map[string]string` + `instruction: string`
	// shape on the wire, preserving pre-ADR-0016 yaad-mcp client
	// compat. The internal registry (typed) is converted via
	// LegacyRegistryWireShape on emit.
	assert.Equal(t, config.LegacyRegistryWireShape(reg), got.Entities[0].CanonicalVocabulary,
		"canonical_vocabulary surfaced via legacy wire-shape projection")
}

// vault-nil deploy: even when the DB carries flag-NULL candidates,
// without a vault reader the handler can't filter on Gaps-non-empty
// (the canonical source per ADR-0008). The cold-reviewer's a prior PR catch: the
// prior shape advanced lastConsidered through the DB candidates
// without emitting entries, so `len(candidates) >= limit` fired
// and emitted a non-empty next_cursor — a DB-only client got
// infinite empty pages. This test pins the corrected short-
// circuit: empty entities, no cursor, regardless of how many
// flag-NULL rows the DB has.
func TestNeedsFill_VaultReaderNil_EmptyResultNoCursor(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	// Seed enough candidates that the prior bug would have triggered
	// `len(candidates) >= limit` with the default limit=50.
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("boardgame:vn-%03d", i)
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID: id,
			Kind: "boardgame",
			Data: map[string]any{"id": id},
		}))
	}
	// No WithVaultIO → vaultReader stays nil on the handlerConfig.
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.True(t, got.OK)
	assert.Empty(t, got.Entities, "vault-nil → empty entities array")
	assert.Empty(t, got.NextCursor,
		"vault-nil → no next_cursor (the cold-reviewer's infinite-empty-pages catch)")
}

// Empty registry + no global → instruction omitted, canonical_vocabulary
// omitted (omitempty on both per ADR-0013 §2 a prior PR + a prior PR).
// TestNeedsFill_KindNotInRegistry_EntitySkipped pins the
// yaad-index #4 strict-mode skip: an entity whose kind isn't
// declared in the operator's canonical_kinds registry has no
// prompts to surface, so it's dropped from the needs-fill list.
// (Pre-#4 the entity surfaced with empty-string prompts under the
// plugin-emit fallback; the strict semantic now requires explicit
// operator-config to participate in fill.)
func TestNeedsFill_KindNotInRegistry_EntitySkipped(t *testing.T) {
	t.Parallel()
	// Empty registry — the seeded boardgame entity has no kind
	// entry in `canonical_kinds:`.
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities,
		"kind not in registry → entity dropped from needs-fill list (yaad-index #4 strict mode)")
}

// TestNeedsFill_GapNotInRegistry_GapSkipped pins the strict-mode
// per-gap skip: an entity whose kind IS in the registry but the
// gap-name isn't declared in the registry's per-kind Gaps map is
// dropped (no plugin-side prompt fallback). When that's the only
// gap, the entity itself drops too.
func TestNeedsFill_GapNotInRegistry_GapSkipped(t *testing.T) {
	t.Parallel()
	// Registry declares boardgame but only the `tags` gap, not
	// the `summary` gap the fixture seeds.
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{
				"tags": "Tag prompt.",
			}),
		},
	}
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities,
		"gap not in registry → gap dropped; sole-gap entity drops from list")
}

// TestNeedsFill_RegistryPromptSurfaces pins the post-#4 happy
// path: the registry's per-gap Description becomes the AI prompt
// on the wire — replacing the pre-#4 empty-string sentinel that
// signaled "plugin generated the prompt but the daemon lost it."
func TestNeedsFill_RegistryPromptSurfaces(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{
				"summary": "Write a one-paragraph summary of the game.",
			}),
		},
	}
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Equal(t, "Write a one-paragraph summary of the game.",
		got.Entities[0].Gaps["summary"],
		"registry Description surfaces as the AI prompt on the wire")
}

// nfFixtureWithGaps is a richer fixture variant that lets the test
// caller specify per-entity gaps + GapState so the ADR-0019 step 6
// audience + defer filters can be exercised end-to-end.
func nfFixtureWithGaps(
	t *testing.T,
	id string,
	gaps []string,
	gapState map[string]vault.GapStateEntry,
	reg map[string]config.CanonicalKindConfig,
	authed bool,
) (http.Handler, auth.Signer, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: id, Kind: "boardgame",
		Data: map[string]any{"id": id},
		Provenance: []store.ProvenanceEntry{{Source: "seed", FetchedAt: &now, OK: true}},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "boardgame", Plugin: "seed",
		Data: map[string]any{"id": id},
		Gaps: gaps,
		GapState: gapState,
		Provenance: []vault.ProvenanceEntry{
			{Source: "seed", FetchedAt: &now, OK: true},
		},
		CleanContent: "x",
	}))

	opts := []HandlerOption{WithVaultIO(w, r), WithCanonicalKindRegistry(reg)}
	var signer auth.Signer
	if authed {
		keyDir := t.TempDir()
		require.NoError(t, auth.GenerateKeypair(keyDir, false))
		signer, err = auth.LoadSigner(keyDir)
		require.NoError(t, err)
		verifier, err := auth.LoadVerifier(keyDir)
		require.NoError(t, err)
		opts = append(opts, WithAuthVerifier(verifier), WithAuthRequired(true))
	}
	return NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		opts...,
	), signer, st
}

// TestNeedsFill_DeferredGap_Excluded pins the ADR-0019 step 6
// defer filter: a deferred gap doesn't surface to either audience.
func TestNeedsFill_DeferredGap_Excluded(t *testing.T) {
	t.Parallel()
	deferAt := time.Now().UTC()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"summary": {Type: "string", Description: "summary"},
				"played": {Type: "bool", Description: "played", FillStrategy: "operator"},
			},
		},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:def-1",
		[]string{"summary", "played"},
		map[string]vault.GapStateEntry{
			"played": {Deferred: true, DeferredAt: &deferAt},
		},
		reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	// `played` deferred → not in gaps map. `summary` (fill_strategy
	// empty → "both") survives; agent-default audience.
	assert.Contains(t, got.Entities[0].Gaps, "summary")
	assert.NotContains(t, got.Entities[0].Gaps, "played",
		"deferred gap must be excluded")
}

// TestNeedsFill_AgentAudience_SkipsOperatorOnlyGaps: agent caller
// (no auth or Subject != Operator) doesn't see operator-only gaps.
func TestNeedsFill_AgentAudience_SkipsOperatorOnlyGaps(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"summary": {Type: "string", Description: "summary"},
				"rating": {Type: "int", Description: "rating", FillStrategy: "operator"},
			},
		},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:agent-audience",
		[]string{"summary", "rating"},
		nil, reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Contains(t, got.Entities[0].Gaps, "summary")
	assert.NotContains(t, got.Entities[0].Gaps, "rating",
		"agent caller must not see operator-only gap")
}

// TestNeedsFill_OperatorAudience_SkipsAgentOnlyGaps: operator
// caller (Subject == Operator) doesn't see agent-only gaps.
func TestNeedsFill_OperatorAudience_SkipsAgentOnlyGaps(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"summary": {Type: "string", Description: "summary", FillStrategy: "agent"},
				"rating": {Type: "int", Description: "rating", FillStrategy: "operator"},
			},
		},
	}
	h, signer, _ := nfFixtureWithGaps(t,
		"boardgame:op-audience",
		[]string{"summary", "rating"},
		nil, reg, true)
	tok := mintToken(t, signer, "alice", "alice") // Subject == Operator

	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Contains(t, got.Entities[0].Gaps, "rating")
	assert.NotContains(t, got.Entities[0].Gaps, "summary",
		"operator caller must not see agent-only gap")
}

// TestNeedsFill_BothAudiences_SeeBothStrategy: a gap with
// fill_strategy="both" (or empty) appears to both audiences.
func TestNeedsFill_BothAudiences_SeeBothStrategy(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"shared": {Type: "string", Description: "either-fill", FillStrategy: "both"},
			},
		},
	}
	h, signer, _ := nfFixtureWithGaps(t,
		"boardgame:both",
		[]string{"shared"},
		nil, reg, true)

	// Agent caller (Subject != Operator).
	agentTok := mintToken(t, signer, "the implementer", "alice")
	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	req.Header.Set("Authorization", "Bearer "+agentTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Contains(t, got.Entities[0].Gaps, "shared", "agent sees both-strategy")

	// Operator caller (Subject == Operator).
	opTok := mintToken(t, signer, "alice", "alice")
	req = httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	req.Header.Set("Authorization", "Bearer "+opTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got = decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Contains(t, got.Entities[0].Gaps, "shared", "operator sees both-strategy")
}

// TestNeedsFill_GapMetadata_Surfaces: ADR-0019 step 6 — typed
// metadata appears alongside the prompt map.
func TestNeedsFill_GapMetadata_Surfaces(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"rating": {
					Type: "int", Description: "rating",
					Range: []int{1, 10}, FillStrategy: "both",
				},
			},
		},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:meta-test",
		[]string{"rating"},
		nil, reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	require.Contains(t, got.Entities[0].GapMetadata, "rating")
	meta := got.Entities[0].GapMetadata["rating"]
	assert.Equal(t, "int", meta.Type)
	assert.Equal(t, "both", meta.FillStrategy)
	assert.Equal(t, []int{1, 10}, meta.Range)
}

// TestNeedsFill_AllGapsFiltered_EntityExcluded: an entity whose
// ALL gaps are deferred or wrong-audience is dropped from the
// response entirely (not surfaced as an empty-gaps row).
func TestNeedsFill_AllGapsFiltered_EntityExcluded(t *testing.T) {
	t.Parallel()
	deferAt := time.Now().UTC()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"played": {Type: "bool", Description: "played", FillStrategy: "operator"},
			},
		},
	}
	// Deferred + only-strategy=operator + agent caller.
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:all-filtered",
		[]string{"played"},
		map[string]vault.GapStateEntry{
			"played": {Deferred: true, DeferredAt: &deferAt},
		},
		reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities,
		"entity with only deferred / wrong-audience gaps must be dropped, not surfaced empty")
}
