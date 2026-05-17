package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newAPIWithVaultIO wires a handler with vault read + write so the
// entity-surface enrichment path activates. Returns the handler,
// the store, and the writer + reader so tests can seed both layers.
func newAPIWithVaultIO(t *testing.T) (http.Handler, store.Store, *vault.Writer, *vault.Reader) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, plugins.NewRegistry(), WithVaultIO(w, r))
	return h, st, w, r
}

// seedEntityWithVaultBody writes an entity with rich vault fields
// populated. The store gets the DB-mirrored slice; the vault gets
// the whole shape.
func seedEntityWithVaultBody(t *testing.T, st store.Store, w *vault.Writer, e *vault.Entity) {
	t.Helper()
	require.NoError(t, w.Write(e))

	// Mirror to the store so the GET handler's GetEntity probe finds
	// the entity. Provenance pinned to one row; the wire-side merge
	// overlays vault provenance via mergeVaultEntity.
	now := time.Now().UTC()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: e.ID,
		Kind: e.Kind,
		Data: e.Data,
		Provenance: []store.ProvenanceEntry{
			{Source: "seed", FetchedAt: &now, OK: true},
		},
	}))
}

func decodeEntityResponse(t *testing.T, rec *httptest.ResponseRecorder) entity {
	t.Helper()
	var got entity
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	return got
}

// TestGetEntity_VaultSurfaceMerged pins the source issue a prior PR addendum:
// GET /v1/entities/{id} returns the single-hop body — clean_content,
// summary, tags, gaps, aliases, plugin, notations, notes — when
// the vault is wired and the entity's vault file carries them.
func TestGetEntity_VaultSurfaceMerged(t *testing.T) {
	t.Parallel()

	h, st, w, _ := newAPIWithVaultIO(t)

	commentDate := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
	seedEntityWithVaultBody(t, st, w, &vault.Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "Susanna Clarke"},
		CleanContent: "Susanna Clarke is an English author.",
		Summary: "British author known for Jonathan Strange & Mr Norrell.",
		Tags: []string{"author", "fantasy"},
		Gaps: []string{"birth_date"},
		Aliases: []string{"Susanna Clarke"},
		Notations: []string{
			"https://en.wikipedia.org/wiki/Susanna_Clarke",
			"wikipedia: Susanna Clarke",
		},
		Notes: []vault.Note{
			{Date: commentDate, Text: "Met at a reading", Author: "alice"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/wikipedia:susanna-clarke", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := decodeEntityResponse(t, rec)
	assert.Equal(t, "wikipedia:susanna-clarke", got.ID)
	assert.Equal(t, "wikipedia", got.Plugin)
	// Vault round-trip canonicalizes a trailing newline on CleanContent.
	assert.Equal(t, "Susanna Clarke is an English author.\n", got.CleanContent)
	assert.Equal(t, "British author known for Jonathan Strange & Mr Norrell.", got.Summary)
	assert.Equal(t, []string{"author", "fantasy"}, got.Tags)
	assert.Equal(t, []string{"birth_date"}, got.Gaps)
	assert.Equal(t, []string{"Susanna Clarke"}, got.Aliases)
	assert.Equal(t, []string{
		"https://en.wikipedia.org/wiki/Susanna_Clarke",
		"wikipedia: Susanna Clarke",
	}, got.Notations)
	require.Len(t, got.Notes, 1)
	assert.Equal(t, "Met at a reading", got.Notes[0].Text)
	assert.Equal(t, "alice", got.Notes[0].Author)
}

// TestGetEntity_OmitsEmptyVaultFields pins the no-noise contract:
// a vault file with no body fields produces a wire entity with no
// `clean_content`/`summary`/`tags`/etc keys (omitempty across).
func TestGetEntity_OmitsEmptyVaultFields(t *testing.T) {
	t.Parallel()

	h, st, w, _ := newAPIWithVaultIO(t)
	// title equals slug → synthesizeAliases emits nothing, keeping
	// the omitempty assertion clean.
	seedEntityWithVaultBody(t, st, w, &vault.Entity{
		ID: "wikipedia:bare",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "bare"},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/wikipedia:bare", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	for _, key := range []string{
		`"clean_content"`,
		`"summary"`,
		`"tags"`,
		`"gaps"`,
		`"aliases"`,
		`"notations"`,
		`"notes"`,
	} {
		assert.NotContains(t, body, key,
			"empty vault fields must omit %s from the wire", key)
	}
	// `plugin` is the one field that DOES land on a bare entity (the
	// vault writer always sets it). Keep it on the wire.
	assert.Contains(t, body, `"plugin":"wikipedia"`)
}

// TestGetEntity_DBOnlyDeploymentSurfacesNothing pins the
// no-vault-wired path: GetEntity with WithVaultIO omitted returns
// the DB-mirrored slice only. Backwards compatible with deployments
// that don't run a vault.
func TestGetEntity_DBOnlyDeploymentSurfacesNothing(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "wikipedia:dbonly",
		Kind: "wikipedia-article",
		Data: map[string]any{"title": "DBOnly"},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed", FetchedAt: &now, OK: true},
		},
	}))

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, plugins.NewRegistry()) // NO WithVaultIO

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/wikipedia:dbonly", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	for _, key := range []string{
		`"clean_content"`,
		`"summary"`,
		`"tags"`,
		`"aliases"`,
		`"notations"`,
		`"plugin"`,
		`"notes"`,
	} {
		assert.NotContains(t, body, key,
			"DB-only deployment must NOT surface vault-only field %s", key)
	}
}

// TestIngest_CacheHitInheritsVaultBodyParity — the addendum closes
// the gap a prior PR left: ingest cache-hit response should carry the
// same single-hop body fields a fresh ingest's complete response
// would. Pre-seed a vault file + DB entity + notation row, then
// ingest the URL form — assert the cache-hit response carries the
// vault's clean_content + summary + tags etc.
func TestIngest_CacheHitInheritsVaultBodyParity(t *testing.T) {
	t.Parallel()

	h, st, w, _ := newAPIWithVaultIO(t)
	seedEntityWithVaultBody(t, st, w, &vault.Entity{
		ID: "wikipedia:parity",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "Parity"},
		CleanContent: "Body content from the cached vault file.",
		Summary: "Cached summary.",
		Tags: []string{"cached"},
		Notations: []string{
			"https://en.wikipedia.org/wiki/Parity",
			"wikipedia: Parity",
		},
	})
	// Register the notation in the cache table so lookup-first hits.
	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: "https://en.wikipedia.org/wiki/Parity",
		EntityID: "wikipedia:parity",
		Kind: store.NotationKindURL,
	}))

	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Parity",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got ingestCompleteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "complete", got.State)
	assert.Equal(t, "Body content from the cached vault file.\n", got.Entity.CleanContent,
		"cache-hit response must surface clean_content from the vault (trailing newline canonical)")
	assert.Equal(t, "Cached summary.", got.Entity.Summary)
	assert.Equal(t, []string{"cached"}, got.Entity.Tags)
	assert.Equal(t, "wikipedia", got.Entity.Plugin)

	// And the cache:notations provenance entry the a prior PR contract
	// requires must still be present.
	foundCacheProv := false
	for _, p := range got.Entity.Provenance {
		if p.Source == cacheNotationsSource {
			foundCacheProv = true
			break
		}
	}
	assert.True(t, foundCacheProv,
		"cache-hit response must still carry the cache:notations provenance entry")
}
