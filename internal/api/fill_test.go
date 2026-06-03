package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// fillTestEntityID is the entity each fill test seeds + redeems against.
const fillTestEntityID = "boardgame:fill-test"

// fillTestGaps is the canonical gap set used by fill_test.go's setup.
// Per ADR-0008 these live in the vault file's frontmatter; the fill
// handler validates submitted field names against this list.
var fillTestGaps = []string{"summary", "tags", "complexity_assessment"}

func fillRequestBody(t *testing.T, body any) io.Reader {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err, "marshal fill request")
	return strings.NewReader(string(b))
}

// newFillFixture returns a handler over a vault-wired store with the
// fill-test entity seeded both in the DB (for the kind lookup) and in
// the vault (with the canonical gaps list). Returns the handler, the
// store, and the vault root so individual tests can inspect the
// post-fill markdown file directly.
func newFillFixture(t *testing.T) (http.Handler, store.Store, string) {
	t.Helper()
	h, st, root := newAPIWithVault(t)
	seedFillEntity(t, st, root, fillTestEntityID, "boardgame", fillTestGaps)
	return h, st, root
}

// seedFillEntity writes a partial entity to BOTH the DB (so the
// fill handler's kind lookup succeeds) and the vault file (so the
// gap set is canonical per ADR-0008). Mirrors what an ingest call
// would have produced post-a prior PR — useful as a deterministic test
// setup that doesn't depend on the long-poll simulator.
func seedFillEntity(t *testing.T, st store.Store, vaultRoot, id, kind string, gaps []string) {
	t.Helper()
	seedEntity(t, st, id, kind)
	w, err := vault.NewWriter(vaultRoot)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: kind,
		Source: []string{"test-fixture/default"},
		Data: map[string]any{"id": id},
		Gaps: gaps,
	}))
}

func postFill(t *testing.T, h http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/entities/" + id + "/fill"
	req := httptest.NewRequest(http.MethodPost, target, fillRequestBody(t, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validFillBody() map[string]any {
	return map[string]any{
		"fields": map[string]any{
			"summary": "Heavy economic euro by Martin Wallace.",
			"tags": []string{"heavy-euro", "economic"},
			"complexity_assessment": "Moderate-to-heavy depth; ~2h with experienced players.",
		},
	}
}

// readVaultByID is a small lookup helper reused across fill tests
// that need to inspect the post-fill markdown file directly.
func readVaultByID(t *testing.T, root, kind, id string) *vault.Entity {
	t.Helper()
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	got, err := r.ReadByID(kind, id)
	require.NoError(t, err, "read vault file for %s", id)
	return got
}

func Test_Fill_HappyPath_AllGapsInOneCall(t *testing.T) {
	t.Parallel()

	h, _, root := newFillFixture(t)

	rec := postFill(t, h, fillTestEntityID, validFillBody())
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got fillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, fillTestEntityID, got.Entity.ID)
	assert.NotNil(t, got.Gaps, "gaps must always be a non-nil slice; want [] not null")
	assert.Empty(t, got.Gaps, "all-gaps-filled call: response.gaps is empty")

	// All three filled fields appear under data (summary + tags are
	// projected from frontmatter into the API entity.data shape per
	// vaultEntityDataForDB; complexity_assessment is a regular data
	// field).
	for _, k := range []string{"summary", "tags", "complexity_assessment"} {
		assert.Contains(t, got.Entity.Data, k, "entity.data should have %q after merge", k)
	}

	// #358: one fill-provenance row stamped per fill call, surfaced
	// in the API response (via the post-fill DB reload).
	require.Len(t, got.Entity.Provenance, 1, "one provenance row per fill call")
	assert.True(t, got.Entity.Provenance[0].OK)
	assert.NotEmpty(t, got.Entity.Provenance[0].FilledAt)

	// Vault file is the source of truth.
	v := readVaultByID(t, root, "boardgame", fillTestEntityID)
	assert.Equal(t, "Heavy economic euro by Martin Wallace.", v.Summary,
		"vault frontmatter.summary")
	assert.ElementsMatch(t, []string{"heavy-euro", "economic"}, v.Tags,
		"vault frontmatter.tags")
	assert.Equal(t, "Moderate-to-heavy depth; ~2h with experienced players.",
		v.Data["complexity_assessment"], "vault data.complexity_assessment")
	assert.Empty(t, v.Gaps, "all gaps filled → empty list")
	// #358: vault frontmatter carries the same single fill-provenance row.
	require.Len(t, v.Provenance, 1, "vault frontmatter.provenance: one row per fill call")
}

func Test_Fill_PartialFill_RemainingGapsStay(t *testing.T) {
	t.Parallel()

	h, _, root := newFillFixture(t)
	body := map[string]any{
		"fields": map[string]any{
			"summary": "only summary, leaving tags + complexity_assessment for later",
		},
	}
	rec := postFill(t, h, fillTestEntityID, body)
	require.Equal(t, http.StatusOK, rec.Code,
		"partial fill is now a success per ADR-0008; body=%s", rec.Body.String())

	// Vault state.
	v := readVaultByID(t, root, "boardgame", fillTestEntityID)
	assert.Equal(t, "only summary, leaving tags + complexity_assessment for later", v.Summary)
	gotVaultGaps := append([]string(nil), v.Gaps...)
	sort.Strings(gotVaultGaps)
	assert.Equal(t, []string{"complexity_assessment", "tags"}, gotVaultGaps,
		"remaining gaps stay open for a future call")

	// Response surfaces the same remaining gaps so the agent can chain
	// the next partial fill without re-fetching via GET /v1/entities/{id}
	// (the source issue + the cold-reviewer's a prior PR review note 2).
	var got fillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.NotNil(t, got.Gaps)
	gotRespGaps := append([]string(nil), got.Gaps...)
	sort.Strings(gotRespGaps)
	assert.Equal(t, []string{"complexity_assessment", "tags"}, gotRespGaps,
		"response.gaps surfaces what's still pending for chained partial fills")
}

func Test_Fill_MultiCall_AccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	h, _, root := newFillFixture(t)

	// First call: fill summary only.
	first := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{"summary": "first call summary"},
	})
	require.Equal(t, http.StatusOK, first.Code)

	// Second call: fill the remaining two gaps.
	second := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{
			"tags": []string{"e2e", "multi-call"},
			"complexity_assessment": "moderate",
		},
	})
	require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())

	v := readVaultByID(t, root, "boardgame", fillTestEntityID)
	assert.Equal(t, "first call summary", v.Summary)
	assert.ElementsMatch(t, []string{"e2e", "multi-call"}, v.Tags)
	assert.Equal(t, "moderate", v.Data["complexity_assessment"])
	assert.Empty(t, v.Gaps, "all gaps filled across two calls")

	// #358: each successful fill call stamps one provenance row, so the
	// two-call sequence leaves two rows in the vault frontmatter.
	require.Len(t, v.Provenance, 2, "one provenance row per fill call, across two calls")
}

// Test_Fill_MissingFields_400 is the sole coverage for the unified
// handler's empty-body branch: an empty object rejects with 400 before
// any field parsing. The other legacy /v1/fill rejection-shape tests
// (409 conflict + rejected[] list / "fields is required") were removed
// in #421 as duplicates of operator_fill_test.go's unified-handler set;
// this one had no equivalent there, so it was rewritten to the unified
// message rather than deleted.
func Test_Fill_MissingFields_400(t *testing.T) {
	t.Parallel()

	h, _, _ := newFillFixture(t)
	rec := postFill(t, h, fillTestEntityID, map[string]any{})
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument",
		"body must be a non-empty object of field operations")
}

// Test_Fill_MalformedTags_Rejected pins the #359 validation boundary:
// the reserved `tags` field must reject non-array / non-string-element
// values rather than silently coercing to empty and still closing the
// gap.
func Test_Fill_MalformedTags_Rejected(t *testing.T) {
	t.Parallel()
	h, _, root := newFillFixture(t)

	// A bare string (not an array) rejects.
	rec := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{"tags": "one-tag"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// Non-string array elements reject too.
	rec2 := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{"tags": []any{123}},
	})
	require.Equal(t, http.StatusBadRequest, rec2.Code, "body=%s", rec2.Body.String())

	// The gap stays open — no silent empty fill.
	v := readVaultByID(t, root, "boardgame", fillTestEntityID)
	assert.Contains(t, v.Gaps, "tags", "malformed tags fill must NOT close the gap")
	assert.Empty(t, v.Tags)
}

func Test_Fill_MalformedJSON_400(t *testing.T) {
	t.Parallel()

	h, _, _ := newFillFixture(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v1/entities/"+fillTestEntityID+"/fill", strings.NewReader(`{`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "JSON")
}

func Test_Fill_UnknownEntity_404(t *testing.T) {
	t.Parallel()

	h, _, _ := newAPIWithVault(t) // no entity seeded
	rec := postFill(t, h, "boardgame:nope", map[string]any{
		"fields": map[string]any{"summary": "x"},
	})
	assertErrorEnvelope(t, rec, http.StatusNotFound, "not_found", "boardgame:nope")
}

// Test_Fill_VaultNotConfigured_503 — without WithVaultIO, the gap
// set has no source of truth; the handler returns 503 vault_required
// rather than silently no-op'ing into the DB. Asymmetric with PR
//'s ingest path (which is happy to stay DB-only); see fill.go's
// handler comment for the why.
func Test_Fill_VaultNotConfigured_503(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	seedEntity(t, st, fillTestEntityID, "boardgame")

	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, plugins.NewRegistry()) // NO WithVaultIO

	rec := postFill(t, h, fillTestEntityID, validFillBody())
	assertErrorEnvelope(t, rec, http.StatusServiceUnavailable, "vault_required", "vault.path")
}

// Test_Fill_DurableCallback_AcrossStoreReopen pins the durability
// claim from ADR-0008: the entity ID is the durable callback handle.
// Even after closing and re-opening the store (simulating server
// restart), a subsequent fill call against the same id succeeds —
// the vault file was the source of truth all along.
func Test_Fill_DurableCallback_AcrossStoreReopen(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	// Use a real on-disk SQLite file so we can close + re-open it.
	// In-memory stores wipe on close, which would test the wrong thing.
	dbPath := t.TempDir() + "/yaad.db"

	// First "session": seed entity + initial vault file.
	{
		st, err := store.New(dbPath)
		require.NoError(t, err)
		seedEntity(t, st, fillTestEntityID, "boardgame")
		require.NoError(t, w.Write(&vault.Entity{
			ID: fillTestEntityID,
			Kind: "boardgame",
			Source: []string{"test-fixture/default"},
			Data: map[string]any{"id": fillTestEntityID},
			Gaps: fillTestGaps,
		}))
		require.NoError(t, st.Close())
	}

	// Second "session": reopen and call fill against the same id.
	st, err := store.New(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	h := NewHandlerWithRegistry(logger, st, plugins.NewRegistry(),
		WithVaultIO(w, r))

	rec := postFill(t, h, fillTestEntityID, map[string]any{
		"fields": map[string]any{"summary": "post-restart fill"},
	})
	require.Equal(t, http.StatusOK, rec.Code,
		"durable-callback claim: same id works after store reopen; body=%s", rec.Body.String())

	v := readVaultByID(t, root, "boardgame", fillTestEntityID)
	assert.Equal(t, "post-restart fill", v.Summary)
}

// Test_Fill_MergedEntityIsKindRegistered keeps the closure invariant
// that mirrors ingest's. The merged entity's kind must be in
// /v1/kinds; any edges must reference declared edge kinds.
func Test_Fill_MergedEntityIsKindRegistered(t *testing.T) {
	t.Parallel()

	h, _, _ := newFillFixture(t)
	rec := postFill(t, h, fillTestEntityID, validFillBody())
	require.Equal(t, http.StatusOK, rec.Code)

	var got fillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))

	declared := make(map[string]struct{}, len(testSeedEntityKinds))
	for _, k := range testSeedEntityKinds {
		declared[k] = struct{}{}
	}
	_, ok := declared[got.Entity.Kind]
	assert.True(t, ok, "entity.kind=%q not in testSeedEntityKinds", got.Entity.Kind)

	declaredEdges := make(map[string]struct{}, len(testSeedEdgeKinds))
	for _, k := range testSeedEdgeKinds {
		declaredEdges[k] = struct{}{}
	}
	for i, e := range got.Entity.Edges {
		_, ok := declaredEdges[e.Type]
		assert.True(t, ok, "entity.edges[%d].type=%q not in testSeedEdgeKinds", i, e.Type)
	}
}
