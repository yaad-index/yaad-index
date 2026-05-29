// Tests for #276 — canonical_type fill resolver_plugin gate.
// Covers both fill paths (agent-fill /v1/entities/{id}/fill and
// operator-fill) against a canonical_kinds registry that pins one
// kind to a resolver plugin and leaves another unbound.

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
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// resolverFillFixture wires a handler whose canonical-kinds
// registry has `boardgame` pinned to resolver_plugin=bgg and
// `person` left unbound (free agent creation OK). The fill-test
// entity carries a `mentions` canonical_type gap accepting both
// kinds so a single fill payload can exercise both branches.
// The handler is auth-required so operator-fill tests can mint
// an operator-authority token.
func resolverFillFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer) {
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

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"mentions": {
					Type:  config.CanonicalTypeName,
					Kinds: []string{"boardgame", "person"},
				},
			},
			ResolverPlugin: "bgg",
		},
		"person": {},
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)

	// Seed the source entity (the one being filled).
	src := "boardgame:fill-source"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: src, Kind: "boardgame",
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID:     src,
		Kind:   "boardgame",
		Source: []string{"test-fixture/default"},
		Data:   map[string]any{"id": src},
		Gaps:   []string{"mentions"},
	}))
	return h, st, root, signer
}

// authedFillPost issues a POST /v1/entities/<id>/fill with the
// auth token attached so the auth-required handler accepts it.
func authedFillPost(t *testing.T, h http.Handler, id string, body any, signer auth.Signer) *httptest.ResponseRecorder {
	t.Helper()
	token := mintToken(t, signer, "test-agent", "test-operator")
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/"+id+"/fill", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// authedOperatorFillPost issues a POST /v1/entities/<id>/operator-fill
// with an operator-authority token attached and an optional
// query string (used for `allow_unresolved=true`).
func authedOperatorFillPost(t *testing.T, h http.Handler, id string, body any, query string, signer auth.Signer) *httptest.ResponseRecorder {
	t.Helper()
	token := mintOperatorToken(t, signer, "test-operator")
	target := "/v1/entities/" + id + "/operator-fill"
	if query != "" {
		target += "?" + query
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestFill_ResolverPlugin_AgentBoundKindMissingTarget422 pins
// the core #276 acceptance: agent-fill against a kind with
// resolver_plugin set, naming a target that doesn't exist in
// the store, rejects with 422 `unresolved_target` and a hint
// naming the resolver plugin.
func TestFill_ResolverPlugin_AgentBoundKindMissingTarget422(t *testing.T) {
	t.Parallel()
	h, _, _, signer := resolverFillFixture(t)

	body := map[string]any{
		"fields": map[string]any{
			"mentions": []map[string]any{
				{"name": "Unknown Game", "kind": "boardgame"},
			},
		},
	}
	rec := authedFillPost(t, h, "boardgame:fill-source", body, signer)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unresolved_target")
	assert.Contains(t, rec.Body.String(), "bgg",
		"error message must name the resolver plugin to ingest through")
}

// TestFill_ResolverPlugin_AgentBoundKindExistingTarget200 pins
// the happy path: agent-fill against a resolver-bound kind
// whose target IS in the store (operator pre-ingested via the
// plugin) succeeds.
func TestFill_ResolverPlugin_AgentBoundKindExistingTarget200(t *testing.T) {
	t.Parallel()
	h, st, _, signer := resolverFillFixture(t)

	// Pre-resolve the target (the way an agent should — ingest
	// through bgg first, then fill).
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "boardgame:placeholder-game", Kind: "boardgame",
	}))

	body := map[string]any{
		"fields": map[string]any{
			"mentions": []map[string]any{
				{"name": "Placeholder Game", "kind": "boardgame"},
			},
		},
	}
	rec := authedFillPost(t, h, "boardgame:fill-source", body, signer)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// TestFill_ResolverPlugin_AgentUnboundKindAutoMaterializes pins
// the carve-out: kinds without resolver_plugin set continue to
// auto-materialize as today. Agent-fill of a `person` target
// that doesn't exist still succeeds (the `person` kind is
// operator-managed; new entries are fine).
func TestFill_ResolverPlugin_AgentUnboundKindAutoMaterializes(t *testing.T) {
	t.Parallel()
	h, _, _, signer := resolverFillFixture(t)

	body := map[string]any{
		"fields": map[string]any{
			"mentions": []map[string]any{
				{"name": "alice", "kind": "person"},
			},
		},
	}
	rec := authedFillPost(t, h, "boardgame:fill-source", body, signer)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// TestFill_ResolverPlugin_OperatorBoundKindMissingTarget422 pins
// that operator-fill follows the same gate as agent-fill by
// default — no implicit bypass. The explicit
// ?allow_unresolved=true override is required to land an
// unresolved target.
func TestFill_ResolverPlugin_OperatorBoundKindMissingTarget422(t *testing.T) {
	t.Parallel()
	h, _, _, signer := resolverFillFixture(t)

	body := map[string]any{
		"mentions": []map[string]any{
			{"name": "Unknown Game", "kind": "boardgame"},
		},
	}
	rec := authedOperatorFillPost(t, h, "boardgame:fill-source", body, "", signer)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"operator-fill without allow_unresolved must 422 same as agent-fill — body=%s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unresolved_target")
}

// TestFill_ResolverPlugin_OperatorBoundKindAllowUnresolved200
// pins the explicit-override path: operator passes
// ?allow_unresolved=true and the fill succeeds + auto-
// materializes the target like the pre-#276 shape.
func TestFill_ResolverPlugin_OperatorBoundKindAllowUnresolved200(t *testing.T) {
	t.Parallel()
	h, _, _, signer := resolverFillFixture(t)

	body := map[string]any{
		"mentions": []map[string]any{
			{"name": "Homebrew Game", "kind": "boardgame"},
		},
	}
	rec := authedOperatorFillPost(t, h, "boardgame:fill-source", body, "allow_unresolved=true", signer)
	require.Equal(t, http.StatusOK, rec.Code,
		"operator-fill with allow_unresolved=true must succeed — body=%s",
		rec.Body.String())
}

// fakeResolveAndUpsertNameResolver simulates a resolver plugin
// that, on ResolveCanonicalEntity, materializes the target
// canonical row in the store + returns the same id back to the
// caller. Used by the #325 fill-gate auto-fetch tests: the
// gate's MaybeDispatchResolverAutoFetch invokes this fake →
// the target row lands → the gate's re-probe succeeds → the
// fill proceeds.
type fakeResolveAndUpsertNameResolver struct {
	st store.Store
	// failWith, if non-nil, makes the resolver return this error
	// (simulating a plugin timeout / transport failure).
	failWith error
	// disambiguationOptions, if non-empty, makes the resolver
	// return options instead of a single-match resolution
	// (exercises the resolution-task spawn path).
	disambiguationOptions map[string]plugins.DisambiguationOption
}

func (f *fakeResolveAndUpsertNameResolver) ResolveCanonicalEntity(ctx context.Context, pluginName, targetKind, name string) (string, map[string]plugins.DisambiguationOption, error) {
	if f.failWith != nil {
		return "", nil, f.failWith
	}
	if len(f.disambiguationOptions) > 0 {
		return "", f.disambiguationOptions, nil
	}
	id := targetKind + ":" + name
	if err := f.st.UpsertEntity(ctx, &store.Entity{ID: id, Kind: targetKind}); err != nil {
		return "", nil, err
	}
	return id, nil, nil
}

// resolverFillFixtureWithAutoFetch wires #325's shared auto-
// fetch path: an edgewrite.Service with the boardgame resolver
// map + a NameResolver shim that materializes the target on
// resolve. Returns the same handler tuple as resolverFillFixture
// plus the resolver fake so individual tests can flip its
// behavior (success / disambiguation / error).
func resolverFillFixtureWithAutoFetch(t *testing.T, fake *fakeResolveAndUpsertNameResolver) (http.Handler, store.Store, auth.Signer) {
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

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: map[string]config.GapSpec{
				"mentions": {
					Type:  config.CanonicalTypeName,
					Kinds: []string{"boardgame", "person"},
				},
			},
			ResolverPlugin: "bgg",
		},
		"person": {},
	}

	// Build the edge-write service with the resolver map + the
	// fake NameResolver wired so MaybeDispatchResolverAutoFetch
	// invokes our fake on dispatch.
	fake.st = st
	svc, err := edgewrite.New(st, map[string][]string{"boardgame": {"bgg"}})
	require.NoError(t, err)
	svc.SetCanonicalKinds(map[string]struct{}{"boardgame": {}, "person": {}})
	svc.SetNameResolver(fake)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
		WithEdgeWriter(svc),
	)

	// Seed the source entity (the one being filled).
	src := "boardgame:fill-source"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: src, Kind: "boardgame",
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID:     src,
		Kind:   "boardgame",
		Source: []string{"test-fixture/default"},
		Data:   map[string]any{"id": src},
		Gaps:   []string{"mentions"},
	}))
	return h, st, signer
}

// TestFill_ResolverPlugin_AgentBoundKindMissingTargetAutoFetchSucceeds
// pins the #325 fill-gate auto-fetch happy path: agent-fill
// against a kind with resolver_plugin set, naming a target
// that doesn't exist, now triggers the shared auto-fetch
// path. The fake plugin resolves it (creates the row), the
// gate's re-probe succeeds, the fill proceeds with 200.
// Pre-#325 this was always 422.
func TestFill_ResolverPlugin_AgentBoundKindMissingTargetAutoFetchSucceeds(t *testing.T) {
	t.Parallel()
	fake := &fakeResolveAndUpsertNameResolver{}
	h, st, signer := resolverFillFixtureWithAutoFetch(t, fake)

	body := map[string]any{
		"fields": map[string]any{
			"mentions": []map[string]any{
				{"name": "auto-fetch-game", "kind": "boardgame"},
			},
		},
	}
	rec := authedFillPost(t, h, "boardgame:fill-source", body, signer)
	require.Equal(t, http.StatusOK, rec.Code,
		"fill must succeed after auto-fetch materializes the target — body=%s", rec.Body.String())

	got, err := st.GetEntity(context.Background(), "boardgame:auto-fetch-game")
	require.NoError(t, err, "auto-fetched canonical must exist in the store after the gate dispatched the plugin")
	assert.Equal(t, "boardgame", got.Kind)
}

// TestFill_ResolverPlugin_AgentBoundKind_AutoFetchError_Still422
// pins the dispatch-error branch: plugin returns an error →
// err-task spawned (out of band) → canonical still doesn't
// exist → gate falls through to 422. Agent's error path is
// preserved; they follow the err-task surface to recover.
func TestFill_ResolverPlugin_AgentBoundKind_AutoFetchError_Still422(t *testing.T) {
	t.Parallel()
	fake := &fakeResolveAndUpsertNameResolver{failWith: assertErr("plugin transport timeout")}
	h, _, signer := resolverFillFixtureWithAutoFetch(t, fake)

	body := map[string]any{
		"fields": map[string]any{
			"mentions": []map[string]any{
				{"name": "missing-game", "kind": "boardgame"},
			},
		},
	}
	rec := authedFillPost(t, h, "boardgame:fill-source", body, signer)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"plugin error → target stays missing → gate must 422 — body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unresolved_target")
}

// TestFill_ResolverPlugin_AgentBoundKind_AutoFetchDisambiguation_Still422
// pins the disambiguation branch: plugin returns options →
// resolution-task spawned (out of band) → canonical still
// doesn't exist → gate falls through to 422. The agent's
// follow-up flow is the same as today (now backed by a real
// task surface rather than a dead-end 422).
func TestFill_ResolverPlugin_AgentBoundKind_AutoFetchDisambiguation_Still422(t *testing.T) {
	t.Parallel()
	fake := &fakeResolveAndUpsertNameResolver{
		disambiguationOptions: map[string]plugins.DisambiguationOption{
			"boardgame:brass-birmingham": {Label: "Brass: Birmingham"},
			"boardgame:brass-lancashire": {Label: "Brass: Lancashire"},
		},
	}
	h, _, signer := resolverFillFixtureWithAutoFetch(t, fake)

	body := map[string]any{
		"fields": map[string]any{
			"mentions": []map[string]any{
				{"name": "brass", "kind": "boardgame"},
			},
		},
	}
	rec := authedFillPost(t, h, "boardgame:fill-source", body, signer)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"disambiguation → target stays missing → gate must 422 — body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unresolved_target")
}

// TestOperatorFill_ResolverPlugin_AutoFetchSucceeds pins the
// same auto-fetch happy path on the operator-fill endpoint:
// no `allow_unresolved=true` query → gate runs → plugin
// materializes target → fill proceeds.
func TestOperatorFill_ResolverPlugin_AutoFetchSucceeds(t *testing.T) {
	t.Parallel()
	fake := &fakeResolveAndUpsertNameResolver{}
	h, st, signer := resolverFillFixtureWithAutoFetch(t, fake)

	body := map[string]any{
		"mentions": []map[string]any{
			{"name": "operator-fetched", "kind": "boardgame"},
		},
	}
	rec := authedOperatorFillPost(t, h, "boardgame:fill-source", body, "", signer)
	require.Equal(t, http.StatusOK, rec.Code,
		"operator-fill auto-fetch must succeed when plugin resolves — body=%s", rec.Body.String())

	_, err := st.GetEntity(context.Background(), "boardgame:operator-fetched")
	require.NoError(t, err)
}

// assertErr returns a trivial error with the given message.
// Inline alternative to errors.New for the test fixtures.
func assertErr(msg string) error { return errAutoFetchTest(msg) }

type errAutoFetchTest string

func (e errAutoFetchTest) Error() string { return string(e) }
