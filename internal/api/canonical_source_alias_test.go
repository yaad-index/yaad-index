package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newNameGapFixture wires the create-canonical endpoint with a `person`
// kind that declares a scalar `name` gap, so a create can seed data.name
// and exercise the #405 alias mirror. The Writer is canonical-kind-aware
// (matching the serve path) so the vault frontmatter alias synthesizes
// from data.name too.
func newNameGapFixture(t *testing.T) (http.Handler, store.Store, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root, vault.WithCanonicalKinds([]string{"person"}))
	require.NoError(t, err)
	rd, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	reg := map[string]config.CanonicalKindConfig{
		"person": {
			Gaps: map[string]config.GapSpec{
				"name":     {Type: "string", Description: "display name"},
				"relation": {Type: "string", Description: "relation to the operator"},
			},
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, rd),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)
	return h, st, signer
}

// TestCreateCanonicalEntity_SeedsNameRegistersAlias pins #405 decision 2:
// create_canonical_entity registers a resolver alias when (and only when)
// `data` seeds a `name` field — the operator-supplied slug has no
// name→slug derivation of its own to capture.
func TestCreateCanonicalEntity_SeedsNameRegistersAlias(t *testing.T) {
	t.Parallel()
	h, st, signer := newNameGapFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	ctx := context.Background()

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "person", "slug": "alex-example", "data": map[string]any{"name": "Alex Example"}}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	resolved, err := st.ResolveAlias(ctx, "Alex Example", "person")
	require.NoError(t, err)
	assert.Equal(t, "person:alex-example", resolved, "seeded name resolves to the created id")

	// End-to-end: the read endpoint resolves the alias too (resolveEntityID).
	getRec := ugcReq(t, h, http.MethodGet,
		"/v1/entities/person:"+url.PathEscape("Alex Example"), tok, nil, nil)
	require.Equal(t, http.StatusOK, getRec.Code, "GET by alias resolves; body=%s", getRec.Body.String())
}

// TestCreateCanonicalEntity_NoName_NoAlias pins the other half of
// decision 2: a create that seeds no `name` registers no alias (the slug
// alone carries no source name).
func TestCreateCanonicalEntity_NoName_NoAlias(t *testing.T) {
	t.Parallel()
	h, st, signer := newNameGapFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	ctx := context.Background()

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "person", "slug": "bob", "data": map[string]any{"relation": "friend"}}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	aliases, err := st.ListAliasesForEntity(ctx, "person:bob")
	require.NoError(t, err)
	assert.Empty(t, aliases, "no name seeded → no alias registered")
}

// TestOperatorFill_CanonicalType_MaterializeRegistersNameAlias pins #405
// for the primary path: filling a canonical_type gap with a
// `{name, kind, data}` entry materializes the target on the dataview
// path (per-entry data is the materialize trigger) and registers the
// source name as a resolver alias — the issue's bgg example, where a
// freshly-mentioned game resolves by its title immediately.
func TestOperatorFill_CanonicalType_MaterializeRegistersNameAlias(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintOperatorToken(t, signer, "alice")
	const id = "source:names-alias"
	seedSourceForCanonicalTypeFill(t, st, root, id)
	ctx := context.Background()

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{
			"subjects": []any{
				map[string]any{"name": "Moon Colony Bloodbath", "kind": "boardgame", "data": map[string]any{"rating": "10"}},
			},
		}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resolved, err := st.ResolveAlias(ctx, "Moon Colony Bloodbath", "boardgame")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:moon-colony-bloodbath", resolved,
		"the mentioned game resolves by its source name after materialize")

	// The captured name also lands as data.name on the materialized file.
	ve := readVaultByID(t, root, "boardgame", "boardgame:moon-colony-bloodbath")
	assert.Equal(t, "Moon Colony Bloodbath", ve.Data["name"], "source name captured as data.name")
}

// TestUserContentCreate_TitleRegistersAlias pins #405 for the UGC create
// path: the title the slug derived from is registered as a resolver
// alias (source-shape, so synthesizeAliases reads data.title).
func TestUserContentCreate_TitleRegistersAlias(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")
	ctx := context.Background()

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Moon Colony Bloodbath",
		"tags":  []string{"games"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	resolved, err := st.ResolveAlias(ctx, "Moon Colony Bloodbath", "user-content")
	require.NoError(t, err)
	assert.Equal(t, "user-content:moon-colony-bloodbath", resolved, "UGC title resolves to its id")
}
