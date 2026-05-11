package api

import (
	"context"
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

// newAgentFillCanonicalTypeFixture wires a vault-backed handler
// covering a `source` kind with one canonical_type gap `subjects`,
// configured with the caller-supplied kinds allowlist (specific
// list OR `["*"]` for the wildcard variant). Used by the agent-
// fill canonical_type tests below — the handler enforces
// auth-required so callers mint anonymous tokens via mintToken.
func newAgentFillCanonicalTypeFixture(t *testing.T, gapKinds []string) (http.Handler, store.Store, string, auth.Signer) {
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

	opPerKind := map[string]config.CanonicalKindConfig{
		"source": {
			Gaps: map[string]config.GapSpec{
				"subjects": {
					Type: config.CanonicalTypeName,
					Description: "Canonical entities mentioned in this source.",
					FillStrategy: "both",
					Kinds: gapKinds,
				},
			},
		},
		"boardgame": {},
		"person": {},
	}
	reg := config.MergeCanonicalRegistry(
		nil,
		nil,
		config.CanonicalKindConfig{},
		opPerKind,
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

// seedSourceForAgentCanonicalTypeFill creates a vault entity + DB
// row for a `source`-kind entity ready to receive an agent-fill
// canonical_type op on the `subjects` gap.
func seedSourceForAgentCanonicalTypeFill(t *testing.T, st store.Store, root, id string) {
	t.Helper()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "source",
		Data: map[string]any{"name": "Test Source"},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "source",
		Plugin: "fixture",
		Data: map[string]any{"name": "Test Source"},
		Gaps: []string{"subjects"},
	}))
}

// agentFillReq POSTs to /v1/entities/{id}/fill with the canonical
// agent-fill body shape `{"fields": {...}}` + a bearer token. A
// thin wrapper around the existing ugcReq helper.
func agentFillReq(t *testing.T, h http.Handler, id, tok string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{"fields": fields}, nil)
}

// TestAgentFill_CanonicalType_HappyPath_ObjectForm covers the
// primary canonical_type path on agent-fill per alice2-index:
// agent submits a list of `{name, kind}` objects, daemon
// slugifies each via slug.Slug, edges land from the source entity
// to the derived canonical-label endpoints. Mirrors the
// operator-fill canonical_type happy-path test from.
func TestAgentFill_CanonicalType_HappyPath_ObjectForm(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:newsletter-2026-04"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Brass: Birmingham (2018)", "kind": "boardgame"},
			map[string]any{"name": "Martin Wallace", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, edges, 2)
	targets := make([]string, len(edges))
	for i, e := range edges {
		targets[i] = e.To
	}
	assert.ElementsMatch(t,
		[]string{"boardgame:brass-birmingham-2018", "person:martin-wallace"},
		targets,
		"daemon's slug.Slug derives canonical-label slugs from descriptive names",
	)
}

// TestAgentFill_CanonicalType_RejectsPreformedLabels covers the
// agent-only-shape gate per spec §Operator-fill: pre-formed
// canonical-label strings are operator-only. An agent-fill body
// containing `["<kind>:<slug>", ...]` rejects with
// type_mismatch + the "pre-formed canonical-label string only
// accepted on operator-fill" hint.
func TestAgentFill_CanonicalType_RejectsPreformedLabels(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-rejects-preformed"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{"boardgame:brass-pittsburgh"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
	assert.Contains(t, rec.Body.String(), "operator-fill")
}

// TestAgentFill_CanonicalType_EmptyList covers the explicit
// empty-list path per spec §Edge cases: `[]` transitions the gap
// to filled state with no edges.
func TestAgentFill_CanonicalType_EmptyList(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-empty-list"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{"subjects": []any{}})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	assert.Empty(t, edges, "empty-list fill must not create edges")

	ve := readVaultByID(t, root, "source", id)
	assert.NotContains(t, ve.Gaps, "subjects",
		"empty-list fill transitions gap to filled")
}

// TestAgentFill_CanonicalType_RefillReplacesEdges covers the
// idempotent-replace semantic per spec §Re-fill: a second
// agent-fill deletes the prior edges and creates the new fill's
// edges. Uses two distinct subjects between fills to confirm.
func TestAgentFill_CanonicalType_RefillReplacesEdges(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-refill"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Caverna", "kind": "boardgame"},
			map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "first fill body=%s", rec.Body.String())

	first, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, first, 2)

	// Re-add `subjects` to the open-gap list so the agent-fill
	// handler accepts a second call. (The vault gap-set tracks
	// what's currently open; re-fill from the agent path requires
	// the field to be re-opened, e.g. via a re-ingest. This test
	// drives the re-open via a direct vault mutation to keep the
	// scope on the edge-replace semantics, not the re-open flow.)
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ve := readVaultByID(t, root, "source", id)
	ve.Gaps = append(ve.Gaps, "subjects")
	require.NoError(t, w.Write(ve))

	rec = agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Agricola", "kind": "boardgame"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "second fill body=%s", rec.Body.String())

	second, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, second, 1, "re-fill must replace, not append")
	assert.Equal(t, "boardgame:agricola", second[0].To)
}

// TestAgentFill_CanonicalType_KindNotInResolution covers the
// gap's allowlist enforcement on the agent path: a fill whose
// kind isn't in `gap.Kinds` rejects.
func TestAgentFill_CanonicalType_KindNotInResolution(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-kind-not-allowed"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Martin Wallace", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	assert.Empty(t, edges, "no edges should land on a rejected fill")
}

// TestAgentFill_CanonicalType_WildcardKinds covers the wildcard
// resolution: `kinds: "*"` accepts any kind in the operator's
// canonical_kinds registry; kinds outside the registry reject.
func TestAgentFill_CanonicalType_WildcardKinds(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{config.CanonicalTypeWildcard})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-wildcard"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	// `boardgame` and `person` are in the operator's
	// canonical_kinds → both pass.
	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Caverna", "kind": "boardgame"},
			map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "wildcard registered kinds body=%s", rec.Body.String())

	// `country` is NOT in the operator's canonical_kinds —
	// wildcard rejects per ADR-0008's "any THIS operator declared"
	// semantic.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ve := readVaultByID(t, root, "source", id)
	ve.Gaps = append(ve.Gaps, "subjects")
	require.NoError(t, w.Write(ve))

	rec = agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Germany", "kind": "country"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"wildcard rejects kinds not in operator registry; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")
}

// TestAgentFill_CanonicalType_NotArray covers the wire-shape
// gate: a bare object (single-item without array wrapping)
// rejects per spec §Edge cases.
func TestAgentFill_CanonicalType_NotArray(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-not-array"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	body := strings.NewReader(`{"fields": {"subjects": {"name": "X", "kind": "boardgame"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/"+id+"/fill", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
}

// TestAgentFill_LegacyFieldsStillWork covers the back-compat
// guarantee: fields without a typed gap-spec entry (e.g. summary
// + tags on a source-shape entity that doesn't declare them in
// the canonical-kind registry) keep flowing through the
// untyped applyFieldsToVaultEntity path. The agent-fill canonical_type
// branch is purely additive.
func TestAgentFill_LegacyFieldsStillWork(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-legacy-fields"

	// Seed an entity whose open-gap list includes both the typed
	// `subjects` canonical_type gap AND the legacy untyped
	// `summary` + `tags` fields. The `subjects` field is left
	// unfilled in this test; the legacy fields are the focus.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "source",
		Data: map[string]any{"name": "Test Source"},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "source",
		Plugin: "fixture",
		Data: map[string]any{"name": "Test Source"},
		Gaps: []string{"subjects", "summary", "tags"},
	}))

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"summary": "A short narrative.",
		"tags": []any{"alpha", "beta"},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	ve := readVaultByID(t, root, "source", id)
	assert.Equal(t, "A short narrative.", ve.Summary,
		"legacy summary lands on top-level vault.Entity.Summary, not Data")
	assert.Equal(t, []string{"alpha", "beta"}, ve.Tags,
		"legacy tags lands on top-level vault.Entity.Tags")
}

// TestNeedsFill_GapMetadataKindsSurfaces pins the nice-to-have
// per alice2-index: when a canonical_type gap is open, the
// `gap_metadata` wire field surfaces the gap's `kinds` allowlist
// so the agent's UI can render the resolution set at fill-prompt
// time.
func TestNeedsFill_GapMetadataKindsSurfaces(t *testing.T) {
	t.Parallel()
	gap := config.GapSpec{
		Type: config.CanonicalTypeName,
		Description: "subjects",
		FillStrategy: "both",
		Kinds: []string{"person", "boardgame"},
	}
	reg := map[string]config.CanonicalKindConfig{
		"source": {Gaps: map[string]config.GapSpec{"subjects": gap}},
	}
	ve := &vault.Entity{
		ID: "source:gap-metadata-kinds",
		Kind: "source",
		Plugin: "fixture",
		Gaps: []string{"subjects"},
	}
	entry, ok := buildNeedsFillEntry(ve.ID, ve.Kind, ve, "", reg, false)
	require.True(t, ok, "buildNeedsFillEntry: want true for an open canonical_type gap")

	require.NotNil(t, entry.GapMetadata)
	meta, has := entry.GapMetadata["subjects"]
	require.True(t, has, "gap_metadata must contain the subjects gap")
	assert.Equal(t, config.CanonicalTypeName, meta.Type)
	assert.Equal(t, []string{"person", "boardgame"}, meta.Kinds,
		"agent-side UI uses meta.Kinds to render the allowlist at fill-prompt time")
}
