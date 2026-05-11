package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newUGCFrontmatterEdgesFixture wires a vault-backed handler with
// RequireAuth enforcement + a canonical-kind registry covering the
// kinds the test mappings target. Returns a signer + the vault root
// so callers can mint tokens and probe the resulting state.
func newUGCFrontmatterEdgesFixture(
	t *testing.T,
	mappings map[string]config.UserContentFrontmatterEdgeMapping,
) (http.Handler, store.Store, string, auth.Signer) {
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

	// canonical-kind registry covers `boardgame`, `person`, and
	// `company` so the test mappings' target_kinds resolve.
	opPerKind := map[string]config.CanonicalKindConfig{
		"boardgame": {},
		"person": {},
		"company": {},
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
		WithUserContentFrontmatterEdges(mappings),
	)
	return h, st, root, signer
}

// defaultUGCMappings is the test fixture's canonical
// `user_content_frontmatter_edges` block — mirrors the BGG-flavored
// mapping documented in/.
func defaultUGCMappings() map[string]config.UserContentFrontmatterEdgeMapping {
	return map[string]config.UserContentFrontmatterEdgeMapping{
		"publisher": {EdgeType: "published_by", TargetKind: "company"},
		"designed_by": {EdgeType: "designed_by", TargetKind: "person"},
		"about": {EdgeType: "is_about", TargetKind: "boardgame"},
	}
}

// TestUserContentCreate_FrontmatterEdges_HappyPath_ObjectForm
// pins the load-bearing contract: declared frontmatter
// fields whose value is a `{name, kind}` object derive a typed
// canonical-edge from the new UGC entity to the daemon-derived
// canonical-label.
func TestUserContentCreate_FrontmatterEdges_HappyPath_ObjectForm(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Brass Birmingham Notes",
		"body": "Some thoughts.",
		"tags": []string{"games"},
		"data": map[string]any{
			"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			"publisher": map[string]any{"name": "Roxley", "kind": "company"},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	id := "user-content:brass-birmingham-notes"
	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, edges, 2, "want one edge per declared field with a value")

	byType := make(map[string]string, len(edges))
	for _, e := range edges {
		byType[e.Type] = e.To
	}
	assert.Equal(t, "boardgame:brass-birmingham", byType["is_about"],
		"daemon's slug.Slug derives the canonical-label slug")
	assert.Equal(t, "company:roxley", byType["published_by"])

	// Thin canonical-label rows materialized so the FK is
	// satisfied. Same shape as's path.
	for _, target := range []string{"boardgame:brass-birmingham", "company:roxley"} {
		row, err := st.GetEntity(context.Background(), target)
		require.NoError(t, err, "label row should exist for %q", target)
		assert.Empty(t, row.Data, "thin row carries no data; vault file materializes on operator-fill")
	}
}

// TestUserContentCreate_FrontmatterEdges_HappyPath_ListForm
// covers the list-of-objects shape: a single declared field
// produces one edge per element.
func TestUserContentCreate_FrontmatterEdges_HappyPath_ListForm(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Designer Roster",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			"designed_by": []any{
				map[string]any{"name": "Martin Wallace", "kind": "person"},
				map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
			},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), "user-content:designer-roster", nil)
	require.NoError(t, err)
	require.Len(t, edges, 2, "list-form value produces one edge per element")
	targets := make([]string, len(edges))
	for i, e := range edges {
		targets[i] = e.To
		assert.Equal(t, "designed_by", e.Type)
	}
	assert.ElementsMatch(t,
		[]string{"person:martin-wallace", "person:uwe-rosenberg"},
		targets,
	)
}

// TestUserContentCreate_FrontmatterEdges_HappyPath_PreformedLabels
// pins the dual-shape acceptance: UGC create accepts pre-formed
// `<kind>:<slug>` strings (operator-authored content; same as
// operator-fill). The slug is taken as-is — no
// re-canonicalization.
func TestUserContentCreate_FrontmatterEdges_HappyPath_PreformedLabels(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Pre Formed Labels Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			"about": "boardgame:brass-pittsburgh",
			"designed_by": []any{"person:martin-wallace"},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), "user-content:pre-formed-labels-test", nil)
	require.NoError(t, err)
	require.Len(t, edges, 2)
	byType := make(map[string]string, len(edges))
	for _, e := range edges {
		byType[e.Type] = e.To
	}
	assert.Equal(t, "boardgame:brass-pittsburgh", byType["is_about"],
		"pre-formed label slug round-trips as-is")
	assert.Equal(t, "person:martin-wallace", byType["designed_by"])
}

// TestUserContentCreate_FrontmatterEdges_EmptyList covers the
// empty-list edge case: `[]` for a declared field skips edge
// creation (no edges) without rejecting the create.
func TestUserContentCreate_FrontmatterEdges_EmptyList(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Empty List Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			"designed_by": []any{},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), "user-content:empty-list-test", nil)
	require.NoError(t, err)
	assert.Empty(t, edges)
}

// TestUserContentCreate_FrontmatterEdges_KindMismatch covers the
// kind-validation path: each declared mapping pins a single
// `target_kind`; values whose kind doesn't match reject with
// 400 `kind_not_allowed`.
func TestUserContentCreate_FrontmatterEdges_KindMismatch(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Kind Mismatch Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			// `about` mapping declares target_kind=boardgame; a
			// `person`-kind value rejects.
			"about": map[string]any{"name": "Martin Wallace", "kind": "person"},
		},
	}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")
}

// TestUserContentCreate_FrontmatterEdges_MalformedPreformedLabel
// covers pre-formed-label parse errors: a string without a `:` /
// empty kind / empty slug rejects.
func TestUserContentCreate_FrontmatterEdges_MalformedPreformedLabel(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Malformed Label Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			"about": "no-colon-here",
		},
	}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_canonical_label")
}

// TestUserContentCreate_FrontmatterEdges_UndeclaredFieldsLandInData
// pins the back-compat surface: data-map fields not declared in
// the operator's mapping flow through to vault.Entity.Data
// verbatim, with no edge derivation. Lets operators mix
// edge-driving fields with arbitrary metadata.
func TestUserContentCreate_FrontmatterEdges_UndeclaredFieldsLandInData(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Undeclared Field Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
			"year": 2018,
			"notes": "operator's freeform note",
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	id := "user-content:undeclared-field-test"
	ve := readVaultByID(t, root, "user-content", id)
	// Declared mapping `about` produced an `is_about` edge; vault
	// frontmatter mirrors with the canonical-label list.
	assert.Equal(t, []any{"boardgame:brass-birmingham"}, ve.Data["is_about"],
		"declared edge field stored in data as canonical-label list")
	// Undeclared fields land verbatim.
	assert.EqualValues(t, 2018, ve.Data["year"])
	assert.Equal(t, "operator's freeform note", ve.Data["notes"])
	// edges pin: only `is_about` lands.
	edges, err := st.GetEdgesFor(context.Background(), id, nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "is_about", edges[0].Type)
}

// TestUserContentCreate_FrontmatterEdges_NoMappings_NoOp pins the
// graceful-degradation path: when the operator hasn't declared any
// frontmatter-edge mappings, UGC create works exactly as it did
// legacy — the dead-config-field regression is sealed but the
// feature is opt-in.
func TestUserContentCreate_FrontmatterEdges_NoMappings_NoOp(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, nil)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "No Mappings Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			// Even with object-shape values, no mappings = no
			// edges + no validation. The fields land in vault
			// frontmatter as-is.
			"about": map[string]any{"name": "Whatever", "kind": "boardgame"},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), "user-content:no-mappings-test", nil)
	require.NoError(t, err)
	assert.Empty(t, edges, "no mappings → no edges")
}

// TestUserContentCreate_FrontmatterEdges_DeclaredFieldAbsentSkipped
// pins the per-field absence behavior: when a declared mapping
// has no value in the request's `data` map, no edge is created
// for that field. Operators don't need to set every declared
// mapping on every UGC create.
func TestUserContentCreate_FrontmatterEdges_DeclaredFieldAbsentSkipped(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newUGCFrontmatterEdgesFixture(t, defaultUGCMappings())
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Absent Field Test",
		"body": "",
		"tags": []string{"games"},
		"data": map[string]any{
			// `about` set; `publisher` + `designed_by` absent.
			"about": map[string]any{"name": "Brass Birmingham", "kind": "boardgame"},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), "user-content:absent-field-test", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "only the declared+set field produces an edge")
	assert.Equal(t, "is_about", edges[0].Type)
}

// TestWrapAsList covers the inline JSON wrap helper that
// normalizes the four declared-field shapes (single object, list
// of objects, single string, list of strings) into a single
// list-shape input for the shared parser.
func TestWrapAsList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in string
		want string
	}{
		{"object", `{"name":"X","kind":"person"}`, `[{"name":"X","kind":"person"}]`},
		{"string", `"person:martin-wallace"`, `["person:martin-wallace"]`},
		{"already_list_object", `[{"name":"X","kind":"person"}]`, `[{"name":"X","kind":"person"}]`},
		{"already_list_strings", `["person:m"]`, `["person:m"]`},
		{"empty_list", `[]`, `[]`},
		{"whitespace_object", ` {"a": 1} `, `[{"a": 1}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := wrapAsList([]byte(tc.in))
			assert.Equal(t, tc.want, strings.TrimSpace(string(got)))
		})
	}
}
