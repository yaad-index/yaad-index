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
	"strings"
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
			// #350: needs_fill total counts entities whose gap_state
			// carries at least one unfilled entry. Mirror the
			// workflow add_gap shape — a single `summary` gap entry
			// with no filled_at + not deferred — so the entity
			// passes both the DB-side count predicate and the
			// vault-side listing filter.
			GapState: map[string]store.GapStateEntry{
				"summary": {},
			},
			Provenance: []store.ProvenanceEntry{
				{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
			},
		}))
		require.NoError(t, w.Write(&vault.Entity{
			ID: id,
			Kind: "boardgame",
			Source: []string{"seed/default"},
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
		Source: []string{"seed/default"},
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
	assert.Equal(t, config.LegacyRegistryWireShape(reg), got.CanonicalVocabulary,
		"canonical_vocabulary surfaced at response-root (post-#275; pre-#275 was per-entry)")
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
		ID: id, Kind: "boardgame", Source: []string{"seed/default"},
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

// TestNeedsFill_GapMetadata_DataSchemaSurfaces pins the #117
// contract: when a gap's GapStateEntry carries DataSchema
// (workflow-injected via add_gap.data_schema), needs-fill
// surfaces it on GapMetadata so the agent's fill-prompt builder
// can include the per-key extraction guidance.
func TestNeedsFill_GapMetadata_DataSchemaSurfaces(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"hiring_alert_for": {
					Type:         "canonical_type",
					Description:  "company that's hiring per this alert",
					Kinds:        []string{"company"},
					FillStrategy: "agent",
				},
			},
		},
	}
	schema := map[string]string{
		"role":      "the role title in the hiring alert",
		"salary":    "salary range if mentioned, else omit",
		"work_mode": "remote / hybrid / onsite if mentioned, else omit",
	}
	gapState := map[string]vault.GapStateEntry{
		"hiring_alert_for": {DataSchema: schema},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:schema-test",
		[]string{"hiring_alert_for"},
		gapState, reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	require.Contains(t, got.Entities[0].GapMetadata, "hiring_alert_for")
	meta := got.Entities[0].GapMetadata["hiring_alert_for"]
	assert.Equal(t, "canonical_type", meta.Type)
	assert.Equal(t, "agent", meta.FillStrategy)
	assert.Equal(t, []string{"company"}, meta.Kinds)
	assert.Equal(t, schema, meta.DataSchema)
}

// TestNeedsFill_GapMetadata_NoDataSchemaWhenAbsent pins the
// omitempty wire shape: a gap without DataSchema produces no
// `data_schema` key in the JSON response.
func TestNeedsFill_GapMetadata_NoDataSchemaWhenAbsent(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"rating": {Type: "int", Description: "rating", Range: []int{1, 10}, FillStrategy: "both"},
			},
		},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:no-schema",
		[]string{"rating"},
		nil, reg, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"data_schema"`,
		"gaps without workflow-injected schema must not surface the field")
}

// TestNeedsFill_AllGapsFiltered_EntityExcluded: an entity whose
// TestNeedsFill_NoFillableEntitiesReturnsExhaustedInOneCall pins the
// #112 contract: a DB full of non-fillable entities (here: vault
// frontmatter has empty Gaps) resolves to a single round-trip with
// `entities: []` AND no cursor — the handler scans the candidate
// stream end-to-end (bounded by needsFillMaxCandidateScan) rather
// than returning one empty page per `limit` rows with an advancing
// cursor. Pre-#112 the agent's loop would call this endpoint
// 325/limit times for a 325-entity DB before realizing nothing was
// fillable; post-fix it's one call.
func TestNeedsFill_NoFillableEntitiesReturnsExhaustedInOneCall(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	// Seed 60 entities, all gap_call_done_at=NULL (DB candidates)
	// but with vault Gaps empty (filtered out by the handler).
	// 60 > needsFillCandidateBatch (200) is overkill; pick a count
	// well above the agent's default limit (50) so pre-fix would
	// have returned multiple empty pages before exhausting.
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("boardgame:nonfillable-%02d", i)
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID: id, Kind: "boardgame",
			Data: map[string]any{"id": id},
		}))
		require.NoError(t, w.Write(&vault.Entity{
			ID: id, Kind: "boardgame", Source: []string{"seed/default"},
			Data: map[string]any{"id": id},
			Gaps: nil,
		}))
	}

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithCanonicalKindRegistry(nfRegistryWithBoardgameSummary()),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill?limit=20", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities,
		"60 non-fillable entities → 0 fillable; pre-fix surfaced empty pages with advancing cursor")
	assert.Empty(t, got.NextCursor,
		"scan exhausted entire DB → no cursor (client stops paginating)")
}

// TestNeedsFill_FillablePastNonFillableReturnsInOneCall pins the
// adjacent #112 shape: interleaved fillable + non-fillable rows
// past the start. The handler keeps scanning past the non-fillable
// prefix and surfaces the fillable rows in the same call, not in a
// later page.
func TestNeedsFill_FillablePastNonFillableReturnsInOneCall(t *testing.T) {
	t.Parallel()
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
	// 30 non-fillable rows with ids `boardgame:nf-00..29`, then
	// 3 fillable rows with ids `boardgame:yes-0..2`. `id ASC`
	// ordering puts the non-fillable prefix first; the fillable
	// rows trail well past the agent's `limit=10` if the handler
	// stopped after the first batch.
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("boardgame:nf-%02d", i)
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID: id, Kind: "boardgame", Data: map[string]any{"id": id},
		}))
		require.NoError(t, w.Write(&vault.Entity{
			ID: id, Kind: "boardgame", Source: []string{"seed/default"},
			Data: map[string]any{"id": id},
			Gaps: nil,
		}))
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("boardgame:yes-%d", i)
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID: id, Kind: "boardgame", Data: map[string]any{"id": id},
			Provenance: []store.ProvenanceEntry{
				{Source: "seed", FetchedAt: fetchedAt, OK: true},
			},
		}))
		require.NoError(t, w.Write(&vault.Entity{
			ID: id, Kind: "boardgame", Source: []string{"seed/default"},
			Data: map[string]any{"id": id},
			Gaps: []string{"summary"},
			Provenance: []vault.ProvenanceEntry{
				{Source: "seed", FetchedAt: fetchedAt, OK: true},
			},
			CleanContent: "stub-clean " + id,
		}))
	}

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithCanonicalKindRegistry(nfRegistryWithBoardgameSummary()),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill?limit=10", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 3,
		"all 3 fillable rows must surface in one call despite 30-row non-fillable prefix")
	for i, e := range got.Entities {
		assert.Equal(t, fmt.Sprintf("boardgame:yes-%d", i), e.ID,
			"entities[%d].id", i)
	}
	assert.Empty(t, got.NextCursor,
		"scan exhausted DB after surfacing the trailing 3 fillable rows")
}

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

// TestNeedsFill_GapMetadata_WorkflowInjectedSpec_StandsAlone:
// when a workflow-injected gap (GapStateEntry with inline
// Type / FillStrategy / Kinds / ...) is present on the entity
// AND the operator-config canonical_kinds does NOT register
// the gap, /v1/needs-fill surfaces the workflow's shape
// standalone (per #142).
func TestNeedsFill_GapMetadata_WorkflowInjectedSpec_StandsAlone(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			// No registration for hiring_alert_for in canonical_kinds.
			Gaps: map[string]config.GapSpec{},
		},
	}
	gapState := map[string]vault.GapStateEntry{
		"hiring_alert_for": {
			Type:         "canonical_type",
			FillStrategy: "agent",
			Kinds:        []string{"company"},
			Description:  "the company that's hiring",
		},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:wf-standalone",
		[]string{"hiring_alert_for"},
		gapState, reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	require.Contains(t, got.Entities[0].GapMetadata, "hiring_alert_for")
	meta := got.Entities[0].GapMetadata["hiring_alert_for"]
	assert.Equal(t, "canonical_type", meta.Type)
	assert.Equal(t, "agent", meta.FillStrategy)
	assert.Equal(t, []string{"company"}, meta.Kinds)
	assert.Equal(t, "the company that's hiring",
		got.Entities[0].Gaps["hiring_alert_for"])
}

// TestNeedsFill_GapMetadata_WorkflowOverridesConfigPerField:
// when both operator-config AND workflow-injected spec are
// present, non-empty workflow fields win per-field (per #142).
func TestNeedsFill_GapMetadata_WorkflowOverridesConfigPerField(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"hiring_alert_for": {
					Type:         "canonical_type",
					FillStrategy: "operator",
					Kinds:        []string{"person", "company"},
					Description:  "config description",
				},
			},
		},
	}
	gapState := map[string]vault.GapStateEntry{
		"hiring_alert_for": {
			Description:  "workflow override",
			FillStrategy: "agent",
		},
	}
	h, _, _ := nfFixtureWithGaps(t,
		"boardgame:override",
		[]string{"hiring_alert_for"},
		gapState, reg, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	meta := got.Entities[0].GapMetadata["hiring_alert_for"]
	assert.Equal(t, "canonical_type", meta.Type, "type falls through from config (not overridden)")
	assert.Equal(t, "agent", meta.FillStrategy, "workflow overrides fill_strategy")
	assert.Equal(t, []string{"person", "company"}, meta.Kinds, "kinds falls through")
	assert.Equal(t, "workflow override",
		got.Entities[0].Gaps["hiring_alert_for"], "workflow overrides description")
}

// TestNeedsFill_KindNotInRegistry_WorkflowInjectedGap_Surfaces
// is the #156 regression test for symptom 2: a source-shape
// entity (kind not in the canonical-LABEL registry per ADR-0016
// — sources aren't carried there, only canonical-label kinds
// are) with a workflow-injected GapStateEntry must surface on
// /v1/needs-fill. The prior strict-mode early-return in
// buildNeedsFillEntry dropped these entities entirely, leaving
// e.g. linkedin-hiring-classify workflow fills unreachable.
//
// Kind=gmail is plugin-emitted source-shape (yaad-gmail's
// canonical_kinds_emitted is email + email-address + label,
// not gmail itself). The workflow's add_gap injects the full
// spec inline per #142; needs_fill must accept that as the
// authoritative shape regardless of whether the kind has an
// operator/plugin canonical_kinds entry.
func TestNeedsFill_KindNotInRegistry_WorkflowInjectedGap_Surfaces(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	id := "gmail:msg-abc"
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: id, Kind: "gmail",
		Data:       map[string]any{"id": id},
		Provenance: []store.ProvenanceEntry{{Source: "seed", FetchedAt: &now, OK: true}},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "gmail", Source: []string{"gmail/default"},
		Data: map[string]any{"id": id, "subject": "linkedin notification"},
		Gaps: []string{"hiring_alert_for"},
		GapState: map[string]vault.GapStateEntry{
			"hiring_alert_for": {
				Type:         "canonical_type",
				FillStrategy: "agent",
				Kinds:        []string{"company"},
				Description:  "the company that's hiring",
			},
		},
		Provenance:   []vault.ProvenanceEntry{{Source: "seed", FetchedAt: &now, OK: true}},
		CleanContent: "linkedin email body",
	}))

	// Registry has NO entry for gmail — sources aren't canonical
	// labels per ADR-0016. The workflow-injected GapStateEntry is
	// the only shape source.
	reg := map[string]config.CanonicalKindConfig{}

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r), WithCanonicalKindRegistry(reg),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1,
		"source-shape entity with workflow-injected gap should surface (per #156)")
	require.Equal(t, id, got.Entities[0].ID)
	require.Equal(t, "gmail", got.Entities[0].Kind)
	require.Contains(t, got.Entities[0].GapMetadata, "hiring_alert_for")
	meta := got.Entities[0].GapMetadata["hiring_alert_for"]
	assert.Equal(t, "canonical_type", meta.Type)
	assert.Equal(t, "agent", meta.FillStrategy)
	assert.Equal(t, []string{"company"}, meta.Kinds)
	assert.Equal(t, "the company that's hiring",
		got.Entities[0].Gaps["hiring_alert_for"])
}

// TestNeedsFill_KindNotInRegistry_NoWorkflowShape_StillDropped
// pins the per-gap strict-mode preserved by #156: an entity
// whose kind is not in the registry AND has no workflow-injected
// shape on any gap still drops — there's no prompt source for
// any of its gaps, so it shouldn't surface as a zero-prompt row.
// This is the prior `TestNeedsFill_KindNotInRegistry_EntitySkipped`
// invariant from #4 strict-mode, re-asserted post-#156 to
// confirm the per-gap loop's `!hasCfgSpec && !workflowGapEntryHasShape`
// guard still handles the no-shape-anywhere case.
func TestNeedsFill_KindNotInRegistry_NoWorkflowShape_StillDropped(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	id := "gmail:msg-no-shape"
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: id, Kind: "gmail",
		Data:       map[string]any{"id": id},
		Provenance: []store.ProvenanceEntry{{Source: "seed", FetchedAt: &now, OK: true}},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "gmail", Source: []string{"gmail/default"},
		Data:         map[string]any{"id": id},
		Gaps:         []string{"summary"}, // gap exists but no shape anywhere
		Provenance:   []vault.ProvenanceEntry{{Source: "seed", FetchedAt: &now, OK: true}},
		CleanContent: "body",
	}))

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithCanonicalKindRegistry(map[string]config.CanonicalKindConfig{}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities,
		"kind not in registry + no workflow-injected shape → entity drops (per-gap strict mode preserved)")
}

// TestNeedsFill_VaultNotExist_QuietDebugSkip is the #156
// symptom-1 regression: a candidate row whose vault file genuinely
// doesn't exist (e.g. pure-pointer canonical-label thin row per
// ADR-0021 — DB row created during ingest/fill, vault file never
// materialized) should be skipped without a WARN. The prior shape
// logged WARN on every scan, flooding the daemon log for every
// thin pointer in the DB. Correctness was already fine; this
// pins the silence contract.
//
// Test shape: seed a DB row but skip the vault.Write. Confirm the
// scan returns no entities (the missing-file row is skipped) AND
// that no WARN was emitted. The logger captures into a buffer so
// the test inspects emitted log records.
func TestNeedsFill_VaultNotExist_QuietDebugSkip(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	_ = w // unused — intentional: no vault.Write so ReadByID returns IsNotExist

	now := time.Now().UTC()
	// DB row exists (thin-pointer canonical-label shape) — no vault file.
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: "email-address:notify-at-example",
		Kind: "email-address",
		Data: map[string]any{"id": "email-address:notify-at-example"},
		Provenance: []store.ProvenanceEntry{{Source: "seed", FetchedAt: &now, OK: true}},
	}))

	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := NewHandlerWithRegistry(
		logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithCanonicalKindRegistry(map[string]config.CanonicalKindConfig{
			"email-address": {Gaps: map[string]config.GapSpec{}},
		}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.Entities, "thin-pointer row with no vault file is skipped")

	logOutput := logBuf.String()
	assert.NotContains(t, logOutput, `"level":"WARN"`,
		"missing-vault-file is the expected pure-pointer shape; should NOT emit WARN.\nlog dump: %s", logOutput)
	assert.Contains(t, logOutput, "no vault file (pure-pointer row)",
		"should still emit a debug record so operators can trace the skip if needed")
}

// TestNeedsFill_CanonicalVocabularyDeDupedAtResponseRoot pins
// the core #275 acceptance: across a multi-entity response, the
// canonical_vocabulary block lives at the response root (one
// copy) rather than per-entry (N copies). Pre-#275 every entry
// carried the full registry, which blew agent-context windows
// once the kind set grew past a handful.
func TestNeedsFill_CanonicalVocabularyDeDupedAtResponseRoot(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{"summary": "Game summary."}),
		},
	}
	ids := []string{"boardgame:a", "boardgame:b", "boardgame:c"}
	h, _ := nfFixture(t, ids, "", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 3)
	assert.NotEmpty(t, got.CanonicalVocabulary,
		"canonical_vocabulary must be present at response root")

	// Per-entry canonical_vocabulary field is gone from the
	// struct, but probe the raw JSON to be sure no entity
	// payload accidentally re-emits it.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	entities, _ := raw["entities"].([]any)
	require.Len(t, entities, 3)
	for i, e := range entities {
		entMap, _ := e.(map[string]any)
		_, has := entMap["canonical_vocabulary"]
		assert.False(t, has, "entity[%d] must NOT carry canonical_vocabulary (deduped to root)", i)
	}
}

// TestNeedsFill_ExcludeCanonicalVocabulary pins the
// `?exclude=canonical_vocabulary` opt-out for caching agents
// that have already fetched the registry from /v1/structure or
// /v1/kinds.
func TestNeedsFill_ExcludeCanonicalVocabulary(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {Gaps: config.GapsFromMap(map[string]string{"summary": "s"})},
	}
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?exclude=canonical_vocabulary", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.CanonicalVocabulary,
		"?exclude=canonical_vocabulary must drop the registry block")
	require.Len(t, got.Entities, 1, "entities still surface; only the vocab is stripped")
}

// TestNeedsFill_ExcludeCleanContent pins the
// `?exclude=clean_content` opt-out for callers that have
// cached the body via /v1/entities.
func TestNeedsFill_ExcludeCleanContent(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {Gaps: config.GapsFromMap(map[string]string{"summary": "s"})},
	}
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?exclude=clean_content", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	require.Len(t, got.Entities, 1)
	assert.Empty(t, got.Entities[0].CleanContent,
		"?exclude=clean_content must blank the per-entry body")
}

// TestNeedsFill_ExcludeBothFields exercises the multi-field
// shape `?exclude=canonical_vocabulary,clean_content` to
// confirm comma-separated parsing.
func TestNeedsFill_ExcludeBothFields(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {Gaps: config.GapsFromMap(map[string]string{"summary": "s"})},
	}
	h, _ := nfFixture(t, []string{"boardgame:a"}, "", reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?exclude=canonical_vocabulary,clean_content", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Empty(t, got.CanonicalVocabulary)
	require.Len(t, got.Entities, 1)
	assert.Empty(t, got.Entities[0].CleanContent)
}

// TestNeedsFill_TotalReflectsGapCallableCount pins the #338
// queue-depth contract: `total` surfaces the count of DB-side
// gap-callable entities regardless of the per-page cursor /
// limit. Five candidates → total=5 across every page; final
// page still reports 5 even when only 1 entity returns.
func TestNeedsFill_TotalReflectsGapCallableCount(t *testing.T) {
	t.Parallel()
	ids := []string{"boardgame:a", "boardgame:b", "boardgame:c", "boardgame:d", "boardgame:e"}
	h, _ := nfFixture(t, ids, "", nfRegistryWithBoardgameSummary())

	// Page 1: limit=2.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill?limit=2", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	page1 := decodeNFResponse(t, rec)
	assert.Equal(t, 5, page1.Total,
		"total reflects DB-side gap-callable count regardless of per-page limit")

	// Last page after cursor traversal: total stays anchored at 5.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?limit=2&cursor="+page1.NextCursor, nil))
	page2 := decodeNFResponse(t, rec)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/needs-fill?limit=2&cursor="+page2.NextCursor, nil))
	finalPage := decodeNFResponse(t, rec)
	require.Len(t, finalPage.Entities, 1)
	assert.Equal(t, 5, finalPage.Total,
		"total is queue-depth, not per-page count")
}

// TestNeedsFill_TotalEmptyStore_IsZero pins the empty-DB
// case: no gap-callable rows → total=0.
func TestNeedsFill_TotalEmptyStore_IsZero(t *testing.T) {
	t.Parallel()
	h, _ := nfFixture(t, nil, "", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeNFResponse(t, rec)
	assert.Equal(t, 0, got.Total)
}

// TestNeedsFill_TotalVaultNil_IsZero pins the no-vault path:
// total stays 0 because the gap-callable count without a
// vault to resolve gaps is meaningless.
func TestNeedsFill_TotalVaultNil_IsZero(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "boardgame:vn-1",
		Kind: "boardgame",
		Data: map[string]any{"id": "boardgame:vn-1"},
	}))
	// No WithVaultIO → vaultReader stays nil.
	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	assert.Equal(t, 0, got.Total,
		"vault-nil → total=0 (gap count without vault is meaningless)")
}

// TestNeedsFill_TotalIgnoresEmptyGapState pins the #350 fix
// for the over-count: entities with gap_call_done_at IS NULL
// but no gap_state populated (i.e., no workflow ever added a
// gap to them) must NOT inflate the queue-depth count.
func TestNeedsFill_TotalIgnoresEmptyGapState(t *testing.T) {
	t.Parallel()
	// Seed two entities with vault gaps + gap_state populated
	// (via nfFixture). They should count.
	h, st := nfFixture(t,
		[]string{"boardgame:a", "boardgame:b"},
		"", nfRegistryWithBoardgameSummary())

	// Seed ten extra "noise" entities with gap_call_done_at IS NULL
	// but empty gap_state — mirrors the staging shape that
	// motivated #350 (pre-workflow rows that never had a gap
	// added). These must NOT inflate the count.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("boardgame:noise-%d", i)
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID:   id,
			Kind: "boardgame",
			Data: map[string]any{"id": id},
		}))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	assert.Equal(t, 2, got.Total,
		"total counts only entities with populated gap_state — empty gap_state rows excluded")
}

// TestNeedsFill_TotalIgnoresAllFilledGapState pins that an
// entity whose gap_state has every entry already filled
// (filled_at stamped) doesn't count — the JSON1 EXISTS clause
// requires at least one unfilled entry.
func TestNeedsFill_TotalIgnoresAllFilledGapState(t *testing.T) {
	t.Parallel()
	h, st := nfFixture(t,
		[]string{"boardgame:active"},
		"", nfRegistryWithBoardgameSummary())

	// Add an entity whose gap_state entry IS populated but
	// already filled — operator-filled at some point. Must NOT
	// count as queue depth.
	filledAt := time.Date(2026, 5, 30, 6, 0, 0, 0, time.UTC)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "boardgame:filled",
		Kind: "boardgame",
		Data: map[string]any{"id": "boardgame:filled"},
		GapState: map[string]store.GapStateEntry{
			"summary": {Source: "operator", FilledAt: &filledAt},
		},
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	assert.Equal(t, 1, got.Total,
		"total excludes entities whose gap_state has every entry already filled")
}

// TestNeedsFill_TotalIgnoresDeferredGapState pins that
// deferred entries (operator-deferred per ADR-0019) don't
// count toward the queue depth either — the COALESCE check
// on the `deferred` field treats deferred as "not in queue".
func TestNeedsFill_TotalIgnoresDeferredGapState(t *testing.T) {
	t.Parallel()
	h, st := nfFixture(t,
		[]string{"boardgame:active"},
		"", nfRegistryWithBoardgameSummary())

	deferredAt := time.Date(2026, 5, 30, 6, 0, 0, 0, time.UTC)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "boardgame:deferred",
		Kind: "boardgame",
		Data: map[string]any{"id": "boardgame:deferred"},
		GapState: map[string]store.GapStateEntry{
			"summary": {Deferred: true, DeferredAt: &deferredAt},
		},
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	assert.Equal(t, 1, got.Total,
		"total excludes deferred-only entities — only genuinely queued counts")
}

// TestNeedsFill_TotalIncludesMixedFilledAndUnfilled pins the
// "at least one unfilled" semantic: an entity with two gap
// entries — one filled, one still pending — counts because
// the EXISTS clause finds the unfilled one.
func TestNeedsFill_TotalIncludesMixedFilledAndUnfilled(t *testing.T) {
	t.Parallel()
	h, st := nfFixture(t, nil, "", nfRegistryWithBoardgameSummary())

	filledAt := time.Date(2026, 5, 30, 6, 0, 0, 0, time.UTC)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "boardgame:mixed",
		Kind: "boardgame",
		Data: map[string]any{"id": "boardgame:mixed"},
		GapState: map[string]store.GapStateEntry{
			"summary": {Source: "agent", FilledAt: &filledAt},
			"rating":  {}, // still unfilled
		},
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil))
	got := decodeNFResponse(t, rec)
	assert.Equal(t, 1, got.Total,
		"total counts an entity when at least one gap entry remains unfilled")
}
