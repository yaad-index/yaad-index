package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newNotationCacheAPI wires a handler with vault IO + a single
// fixture plugin whose Fetch records call counts and emits a
// canned FetchResult including a Notations list. Tests assert the
// orchestrator's lookup-first behavior (per the source issue a prior PR).
func newNotationCacheAPI(t *testing.T) (
	http.Handler,
	store.Store,
	*vault.Reader,
	*vault.Writer,
	*atomic.Int32,
) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	var calls atomic.Int32
	now := time.Now().UTC()
	plug := &fixture.Plugin{
		NameValue: "notation-test",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			calls.Add(1)
			// Slug derived from the last path segment so different
			// inputs produce stable distinct entities.
			i := strings.LastIndex(rawURL, "/")
			id := "notation-test:" + rawURL[i+1:]
			return &plugins.FetchResult{
				Entity: &store.Entity{
					ID: id,
					Kind: "notation-test-entity",
					Data: map[string]any{"title": id},
				},
				Provenance: []store.ProvenanceEntry{
					{Source: "fake:fetch", FetchedAt: &now, OK: true},
				},
				Notations: []string{
					rawURL,
					"shorthand: " + rawURL[i+1:],
				},
			}, nil
		},
		CapabilitiesValue: plugins.Capabilities{Name: "notation-test"},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r))
	return h, st, r, w, &calls
}

func decodeCompleteIngest(t *testing.T, rec *httptest.ResponseRecorder) ingestCompleteResponse {
	t.Helper()
	var got ingestCompleteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	return got
}

// TestIngest_LookupFirst_CacheMissThenWritesNotations covers the
// happy-path post-Fetch wiring: a fresh ingest invokes the plugin
// once, then registers every notation the plugin emitted in the
// entity_notations table.
func TestIngest_LookupFirst_CacheMissThenWritesNotations(t *testing.T) {
	t.Parallel()
	h, st, _, _, calls := newNotationCacheAPI(t)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.EqualValues(t, 1, calls.Load(), "first ingest must call plugin")

	// Both notations the plugin emitted now resolve.
	for _, n := range []string{
		"https://example.test/foo",
		"shorthand: foo",
	} {
		got, err := st.GetNotation(context.Background(), n)
		require.NoError(t, err, "GetNotation(%q)", n)
		assert.Equal(t, "notation-test:foo", got.EntityID)
		assert.Equal(t, store.NotationKindURL, got.Kind)
	}
}

// TestIngest_LookupFirst_SecondIngestHitsCache pins the self-
// roundtrip behavior: re-ingesting the same URL within the same
// session does NOT re-invoke the plugin.
func TestIngest_LookupFirst_SecondIngestHitsCache(t *testing.T) {
	t.Parallel()
	h, _, _, _, calls := newNotationCacheAPI(t)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load(), "first ingest must call plugin")

	rec := postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.Equal(t, http.StatusOK, rec.Code)
	require.EqualValues(t, 1, calls.Load(), "second ingest must NOT call plugin (cache hit)")

	got := decodeCompleteIngest(t, rec)
	assert.Equal(t, "complete", got.State)
	assert.Equal(t, "notation-test:foo", got.Entity.ID)
	// Cache-hit response must include the ephemeral cache:notations
	// provenance entry so agents distinguish cached from fresh.
	foundCacheProv := false
	for _, p := range got.Entity.Provenance {
		if p.Source == cacheNotationsSource {
			foundCacheProv = true
			assert.True(t, p.OK)
			break
		}
	}
	assert.True(t, foundCacheProv,
		"cache-hit response must include a cache:notations provenance entry")
}

// TestIngest_LookupFirst_AlternateNotationHits proves the lookup
// table covers equivalence: ingest URL form first, then shorthand
// form — second call hits the cache without invoking the plugin.
func TestIngest_LookupFirst_AlternateNotationHits(t *testing.T) {
	t.Parallel()
	h, _, _, _, calls := newNotationCacheAPI(t)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	// Re-ingest with the equivalent shorthand the plugin registered.
	rec := postIngest(t, h, map[string]any{"url": "shorthand: foo", "wait_seconds": 2})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.EqualValues(t, 1, calls.Load(),
		"shorthand notation must resolve to the same cached entity, no plugin call")

	got := decodeCompleteIngest(t, rec)
	assert.Equal(t, "notation-test:foo", got.Entity.ID)
}

// TestIngest_LookupFirst_ForceRefetchBypassesCache pins that
// force_refetch=true skips the notation cache lookup and re-
// invokes the plugin even when the entity is fresh.
func TestIngest_LookupFirst_ForceRefetchBypassesCache(t *testing.T) {
	t.Parallel()
	h, _, _, _, calls := newNotationCacheAPI(t)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/foo",
		"wait_seconds": 2,
		"force_refetch": true,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.EqualValues(t, 2, calls.Load(),
		"force_refetch=true must bypass cache and re-invoke plugin")
}

// TestIngest_LookupFirst_PluginEmitsNoNotationsLandsCleanly covers
// the backwards-compat path: a plugin that emits no Notations on
// FetchResult lands the entity cleanly without writing to the
// notation table; subsequent ingests of the same URL re-invoke the
// plugin (no cache hit possible).
func TestIngest_LookupFirst_PluginEmitsNoNotationsLandsCleanly(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	var calls atomic.Int32
	now := time.Now().UTC()
	plug := &fixture.Plugin{
		NameValue: "no-notations",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			calls.Add(1)
			return &plugins.FetchResult{
				Entity: &store.Entity{
					ID: "no-notations:foo",
					Kind: "no-notations-entity",
					Data: map[string]any{"title": "Foo"},
				},
				Provenance: []store.ProvenanceEntry{
					{Source: "fake:fetch", FetchedAt: &now, OK: true},
				},
				// Notations deliberately empty/nil.
			}, nil
		},
		CapabilitiesValue: plugins.Capabilities{Name: "no-notations"},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r))

	rec := postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.EqualValues(t, 1, calls.Load())

	// Notation table empty for this entity (skip-the-write path).
	_, err = st.GetNotation(context.Background(), "https://example.test/foo")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"plugin emitting no Notations must NOT register cache rows")

	// Re-ingest re-invokes the plugin (no cache to hit).
	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 2, calls.Load(),
		"second ingest re-invokes the plugin since no notation cache row was registered")
}

// TestIngest_LookupFirst_DisambiguationSkipsNotationWrite pins that
// the disambiguation path (plugin returns Options instead of an
// Entity) doesn't try to write to entity_notations — there's no
// entity yet to register against.
func TestIngest_LookupFirst_DisambiguationSkipsNotationWrite(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	plug := &fixture.Plugin{
		NameValue: "disamb",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			return &plugins.FetchResult{
				Options: map[string]plugins.DisambiguationOption{
					"option-a": {Label: "Option A"},
					"option-b": {Label: "Option B"},
				},
				// Notations populated even on disambiguation just to
				// prove the orchestrator skips the write path; a real
				// plugin probably wouldn't but the contract should
				// hold either way.
				Notations: []string{"https://example.test/disamb"},
			}, nil
		},
		CapabilitiesValue: plugins.Capabilities{Name: "disamb"},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r))

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/disamb",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Disambiguation response must NOT have written notations —
	// there's no entity_id to point them at.
	_, err = st.GetNotation(context.Background(), "https://example.test/disamb")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"disambiguation path must NOT write notation rows")
}
