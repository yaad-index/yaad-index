package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestGetEntity_ResolvesAlias pins #392: an id-taking handler resolves a
// human-readable alias to the canonical id (exact-id fast path, then
// alias-by-kind, then 404). Mirrors the issue's `boardgame:Brass:
// Birmingham` repro.
func TestGetEntity_ResolvesAlias(t *testing.T) {
	t.Parallel()
	h, st, _ := newAPIWithVault(t)
	const canonical = "boardgame:brass-birmingham"
	seedEntity(t, st, canonical, "boardgame")
	require.NoError(t, st.ReplaceAliases(context.Background(), canonical,
		[]store.Alias{{Alias: "Brass: Birmingham"}}))

	getID := func(target string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&body)
		return rec.Code, body.ID
	}

	// The human-readable alias resolves to the canonical id.
	code, gotID := getID("/v1/entities/boardgame:Brass:%20Birmingham")
	require.Equal(t, http.StatusOK, code, "alias lookup should resolve")
	assert.Equal(t, canonical, gotID, "alias resolves to the canonical id")

	// Exact slug fast path is unchanged.
	code, gotID = getID("/v1/entities/" + canonical)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, canonical, gotID)

	// Unknown reference still 404s.
	code, _ = getID("/v1/entities/boardgame:Nonexistent%20Game")
	assert.Equal(t, http.StatusNotFound, code)

	// Kind-scoped: the same alias text under a different kind does NOT
	// resolve (the alias belongs to a boardgame, not a person).
	code, _ = getID("/v1/entities/person:Brass:%20Birmingham")
	assert.Equal(t, http.StatusNotFound, code, "alias must not cross kind namespaces")
}

// TestGetEntityContext_ResolvesAlias covers the second alias-aware read
// tool (get_entity_with_context) — the route a SKILL/PR-body claim made
// alias-aware but the handler had originally missed.
func TestGetEntityContext_ResolvesAlias(t *testing.T) {
	t.Parallel()
	h, st, _ := newAPIWithVault(t)
	const canonical = "boardgame:brass-birmingham"
	seedEntity(t, st, canonical, "boardgame")
	require.NoError(t, st.ReplaceAliases(context.Background(), canonical,
		[]store.Alias{{Alias: "Brass: Birmingham"}}))

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/boardgame:Brass:%20Birmingham/context?depth=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "context lookup by alias should resolve; body=%s", rec.Body.String())

	var body struct {
		Root struct {
			ID string `json:"id"`
		} `json:"root"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, canonical, body.Root.ID, "context root resolves to the canonical id")
}
