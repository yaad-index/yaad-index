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

// newCanonicalTypeFixture wires a vault-backed handler whose
// canonical-kind registry covers a `source` kind with one
// canonical_type gap `subjects` plus a wildcard variant on a
// second kind. Used by the canonical_type fill tests below.
//
// kindsCanonicalAllowList sits on the `subjects` gap: when the
// caller declared a specific list (`["person", "boardgame"]`),
// fills whose elements name a kind outside that list reject. When
// the caller declared the wildcard `["*"]`, the resolution set
// becomes the operator's full canonical_kinds registry per
// ADR-0008.
func newCanonicalTypeFixture(t *testing.T, gapKinds []string) (http.Handler, store.Store, string, auth.Signer) {
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

	// Operator config with a `source` kind carrying the
	// canonical_type `subjects` gap. The kind list also includes
	// `boardgame` and `person` so wildcard-resolution tests have
	// real entries in the operator's canonical_kinds registry.
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

// seedSourceForCanonicalTypeFill creates a vault entity + DB row
// for a `source`-kind entity ready to receive a canonical_type
// fill on the `subjects` gap. The gap appears in the open-gap
// list so the handler accepts ops on it.
func seedSourceForCanonicalTypeFill(t *testing.T, st store.Store, root, id string) {
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

// TestOperatorFill_CanonicalType_HappyPath_ObjectForm covers the
// primary canonical_type path per yaad-index: the operator
// submits a list of `{name, kind}` objects, the daemon slugifies
// each via slug.Slug, edges land from the source entity to the
// derived canonical-label endpoints.
func TestOperatorFill_CanonicalType_HappyPath_ObjectForm(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:newsletter-2026-04"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Brass: Birmingham (2018)", "kind": "boardgame"},
				map[string]any{"name": "Martin Wallace", "kind": "person"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, edges, 2, "want 2 edges, one per fill entry")

	targets := make([]string, len(edges))
	for i, e := range edges {
		targets[i] = e.To
	}
	assert.ElementsMatch(t,
		[]string{"boardgame:brass-birmingham-2018", "person:martin-wallace"},
		targets,
		"daemon's slug.Slug derives the canonical-label slug from the descriptive name",
	)

	// Thin canonical-label rows materialized so the FK on edges is
	// satisfied. Pre-existing data on those rows would survive
	// (ensureCanonicalLabelRow GetEntity-then-skip).
	bgRow, err := st.GetEntity(context.Background(), "boardgame:brass-birmingham-2018")
	require.NoError(t, err)
	assert.Equal(t, "boardgame", bgRow.Kind)
	personRow, err := st.GetEntity(context.Background(), "person:martin-wallace")
	require.NoError(t, err)
	assert.Equal(t, "person", personRow.Kind)
}

// TestOperatorFill_CanonicalType_HappyPath_PreformedLabels covers
// the operator-only second shape: pre-formed `<kind>:<slug>`
// strings. Daemon extracts the kind from the prefix and accepts
// the slug as-is — no re-canonicalization. Useful when the
// operator copies a label from a search result or from another
// entity's edges.
func TestOperatorFill_CanonicalType_HappyPath_PreformedLabels(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:preformed-labels"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				"boardgame:brass-pittsburgh",
				"person:martin-wallace",
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, edges, 2)
	targets := make([]string, len(edges))
	for i, e := range edges {
		targets[i] = e.To
	}
	assert.ElementsMatch(t,
		[]string{"boardgame:brass-pittsburgh", "person:martin-wallace"},
		targets,
		"pre-formed labels round-trip slug component as-is",
	)
}

// TestOperatorFill_CanonicalType_EmptyList covers the explicit
// "no recognized subjects in scope" fill per spec §Edge cases.
// Empty-list transitions the gap to filled (not unfilled, not
// deferred) so /v1/needs-fill stops resurfacing it; no edges are
// created.
func TestOperatorFill_CanonicalType_EmptyList(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:empty-list-fill"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{"subjects": []any{}}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// No edges for an empty-list fill.
	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	assert.Empty(t, edges, "empty-list fill must not create edges")

	// Gap left the open-set (filled state); subjects is no longer
	// in ve.Gaps.
	ve := readVaultByID(t, root, "source", id)
	assert.NotContains(t, ve.Gaps, "subjects", "empty-list fill transitions gap to filled")
}

// TestOperatorFill_CanonicalType_RefillReplacesEdges covers the
// idempotent-replace semantic per spec §Re-fill: a second fill
// deletes the prior edges and creates the new fill's edges. No
// edge appending; no partial diff.
func TestOperatorFill_CanonicalType_RefillReplacesEdges(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:refill-test"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	// First fill: two subjects.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Caverna", "kind": "boardgame"},
				map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "first fill body=%s", rec.Body.String())

	first, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, first, 2)

	// Second fill: one different subject. Prior edges must be
	// wiped; only the new subject's edge survives.
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Agricola", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "second fill body=%s", rec.Body.String())

	second, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, second, 1, "re-fill must replace, not append")
	assert.Equal(t, "boardgame:agricola", second[0].To)
}

// TestOperatorFill_CanonicalType_KindNotInResolution covers the
// gap's allowlist enforcement: a fill entry whose kind isn't in
// `gap.Kinds` rejects with 400. Per spec: "Fill-validation
// rejects fills where the kind isn't in the operator's declared
// canonical_kinds set."
func TestOperatorFill_CanonicalType_KindNotInResolution(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"boardgame"}) // person NOT allowed
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:kind-not-allowed"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Martin Wallace", "kind": "person"},
			},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")

	// No edges created; vault file unchanged (the parse-error
	// path rejects before the apply step).
	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	assert.Empty(t, edges)
}

// TestOperatorFill_CanonicalType_WildcardKinds covers the `kinds:
// "*"` resolution path: fills whose kind appears in the
// operator's full canonical_kinds registry pass; kinds outside
// the registry reject. Validates that the wildcard is NOT
// interpreted as "any string"; ADR-0008 limits it to declared
// canonical kinds.
func TestOperatorFill_CanonicalType_WildcardKinds(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{config.CanonicalTypeWildcard})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:wildcard-fill"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	// `boardgame` and `person` are in the operator's
	// canonical_kinds registry — both pass.
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Caverna", "kind": "boardgame"},
				map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "wildcard registered kinds body=%s", rec.Body.String())

	// `country` is NOT in the operator's canonical_kinds — wildcard
	// rejects.
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Germany", "kind": "country"},
			},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"wildcard rejects kinds not in operator registry; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")
}

// TestOperatorFill_CanonicalType_NotArray covers the wire-shape
// gate: the value MUST be a JSON array. A bare object (single-
// item without array wrapping) rejects per spec §Edge cases:
// "Single-item fill is always wrapped in an array. Never a bare
// object."
func TestOperatorFill_CanonicalType_NotArray(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:not-array"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	body := strings.NewReader(`{"subjects": {"name": "X", "kind": "boardgame"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/"+id+"/operator-fill", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
}

// TestOperatorFill_CanonicalType_MalformedPreformedLabel covers
// the pre-formed-string rejection path: a string without a `:`
// separator OR with empty kind/slug rejects.
func TestOperatorFill_CanonicalType_MalformedPreformedLabel(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:malformed-preformed"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	cases := []string{
		"no-colon-here",
		":missing-kind",
		"kind-only:",
	}
	for _, badLabel := range cases {
		t.Run(badLabel, func(t *testing.T) {
			rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
				map[string]any{"subjects": []any{badLabel}}, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid_canonical_label")
		})
	}
}

// TestOperatorFill_CanonicalType_ExtraFieldRejected covers the
// strict-decode contract on object-form entries: an extra field
// (e.g. typo'd `name`) rejects rather than silently ignoring.
// Catches caller-side bugs at the boundary.
func TestOperatorFill_CanonicalType_ExtraFieldRejected(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:extra-field"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	body := strings.NewReader(`{"subjects": [{"name": "X", "kind": "boardgame", "year": 2018}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/"+id+"/operator-fill", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
}

// TestOperatorFill_CanonicalType_DataPersists covers the data-
// landing side: after a canonical_type fill, the canonical-label
// list is persisted to ve.Data[<gap>] so subsequent reads
// (including agent context) see the fill.
func TestOperatorFill_CanonicalType_DataPersists(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:data-persists"
	seedSourceForCanonicalTypeFill(t, st, root, id)

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/operator-fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got operatorFillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)

	// Vault frontmatter carries the canonical-label list.
	ve := readVaultByID(t, root, "source", id)
	subjects, ok := ve.Data["subjects"].([]any)
	require.True(t, ok, "ve.Data[subjects] must be a list, got %T (%v)", ve.Data["subjects"], ve.Data["subjects"])
	require.Len(t, subjects, 1)
	assert.Equal(t, "boardgame:brass-birmingham", subjects[0])
}
