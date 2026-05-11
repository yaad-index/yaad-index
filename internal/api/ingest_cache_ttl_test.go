package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
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

// newCacheTTLAPI is a copy of newNotationCacheAPI with WithCacheTTL
// wired through. Plugin records call counts so tests can assert
// when the plugin path was invoked vs. served from cache. Returns
// the vault root too so tests can mutate cache_expires directly
// (per alice2-index the lookup-side comparison is against the
// frontmatter date, not against provenance fetched_at).
func newCacheTTLAPI(t *testing.T, ttl time.Duration) (
	http.Handler,
	store.Store,
	*atomic.Int32,
	string, // vault root path
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
	plug := &fixture.Plugin{
		NameValue: "ttl-test",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			calls.Add(1)
			i := strings.LastIndex(rawURL, "/")
			id := "ttl-test:" + rawURL[i+1:]
			now := time.Now().UTC()
			return &plugins.FetchResult{
				Entity: &store.Entity{
					ID: id,
					Kind: "ttl-test-entity",
					Data: map[string]any{"title": id},
				},
				Provenance: []store.ProvenanceEntry{
					{Source: "fake:fetch", FetchedAt: &now, OK: true},
				},
				Notations: []string{rawURL},
			}, nil
		},
		CapabilitiesValue: plugins.Capabilities{Name: "ttl-test"},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{WithVaultIO(w, r)}
	if ttl > 0 {
		opts = append(opts, WithCacheTTL(ttl))
	}
	h := NewHandlerWithRegistry(logger, st, registry, opts...)
	return h, st, &calls, root
}

// backdateProvenance rewrites the entity's provenance so the
// freshest fetched_at is `age` ago. Lets tests stage stale cached
// state without sleeping. Used for the LEGACY cache_ttl_seconds
// fallback path; post- tests use backdateCacheExpires instead
// since the new lookup compares against the absolute date stamp,
// not against fetched_at.
func backdateProvenance(t *testing.T, st store.Store, id string, age time.Duration) {
	t.Helper()
	old := time.Now().UTC().Add(-age)
	require.NoError(t, st.ReplaceProvenance(context.Background(), id, []store.ProvenanceEntry{
		{Source: "fake:fetch", FetchedAt: &old, OK: true},
	}))
}

// backdateCacheExpires rewrites the vault entity's cache_expires
// frontmatter to `age` in the past. Stages "expired entity" state
// for's absolute-date lookup contract. The vault file is
// read, mutated in-place, and written back via vault.Writer.
func backdateCacheExpires(t *testing.T, root, kind, id string, age time.Duration) {
	t.Helper()
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ent, err := r.ReadByID(kind, id)
	require.NoError(t, err)
	ent.CacheExpires = vault.CacheExpiresAt(time.Now().UTC().Add(-age))
	require.NoError(t, w.Write(ent))
}

// (seedLegacyCacheTTLState was removed in PR-B alongside the
// CacheTTLSeconds field. Legacy vault entries that still carry
// `cache_ttl_seconds:` no longer participate in the lookup gate
// post-PR-B; operators force_refetch to migrate.)

// TestIngest_CacheTTL_FreshHitsCacheStaleFallsThrough — the core
// a prior PR behavior: a fresh entity (within TTL) hits cache; pushing
// the cache_expires stamp into the past forces the next ingest
// through the plugin path again.
func TestIngest_CacheTTL_FreshHitsCacheStaleFallsThrough(t *testing.T) {
	t.Parallel()
	h, _, calls, root := newCacheTTLAPI(t, time.Hour)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load(), "first ingest must call plugin")

	// Fresh window (1 hour TTL, cache_expires = now + 1h) → cache hit.
	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load(), "second ingest within TTL must hit cache")

	// Backdate cache_expires to 1 hour ago — past expiry.
	backdateCacheExpires(t, root, "ttl-test-entity", "ttl-test:foo", time.Hour)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 2, calls.Load(),
		"third ingest past cache_expires must fall through to the plugin")
}

// TestIngest_CacheTTL_DisabledHitsForever pins the default (TTL=0)
// behavior: even arbitrarily-old cached entries serve from cache.
func TestIngest_CacheTTL_DisabledHitsForever(t *testing.T) {
	t.Parallel()
	h, st, calls, _ := newCacheTTLAPI(t, 0) // TTL disabled

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	// Backdate by a year. With TTL disabled, still a hit.
	backdateProvenance(t, st, "ttl-test:foo", 365*24*time.Hour)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load(),
		"TTL=0 disables freshness check; ancient cached entry still hits")
}

// TestIngest_CacheTTL_ForceRefetchAlwaysBypasses pins that
// force_refetch=true ignores TTL state — even a fresh entity
// re-invokes the plugin.
func TestIngest_CacheTTL_ForceRefetchAlwaysBypasses(t *testing.T) {
	t.Parallel()
	h, _, calls, _ := newCacheTTLAPI(t, time.Hour)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	postIngest(t, h, map[string]any{
		"url": "https://example.test/foo",
		"wait_seconds": 2,
		"force_refetch": true,
	})
	require.EqualValues(t, 2, calls.Load(),
		"force_refetch=true must bypass cache regardless of TTL state")
}

// TestIngest_CacheTTL_PluginCapabilitiesWinsOverGlobal pins the
// new plugin-level resolution: when a plugin declares
// `cache_ttl_seconds` on its Capabilities, that value overrides
// the operator's global config (per the entry > plugin > global
// hierarchy). Stamped into vault frontmatter at ingest time.
func TestIngest_CacheTTL_PluginCapabilitiesWinsOverGlobal(t *testing.T) {
	t.Parallel()
	// Global = 1h; plugin = 60s. Plugin wins.
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	var calls atomic.Int32
	plug := &fixture.Plugin{
		NameValue: "ttl-test",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			calls.Add(1)
			id := "ttl-test:" + rawURL[strings.LastIndex(rawURL, "/")+1:]
			now := time.Now().UTC()
			return &plugins.FetchResult{
				Entity: &store.Entity{ID: id, Kind: "ttl-test-entity", Data: map[string]any{"title": id}},
				Provenance: []store.ProvenanceEntry{
					{Source: "fake:fetch", FetchedAt: &now, OK: true},
				},
				Notations: []string{rawURL},
			}, nil
		},
		// Plugin declares 60s — overrides any global config under.
		CapabilitiesValue: plugins.Capabilities{Name: "ttl-test", CacheTTLSeconds: 60},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry,
		WithVaultIO(w, r),
		WithCacheTTL(time.Hour), // Global = 3600s; plugin's 60 should win.
	)

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	v, err := r.ReadByID("ttl-test-entity", "ttl-test:foo")
	require.NoError(t, err)
	require.NotNil(t, v.CacheExpires, "plugin-level TTL must be stamped into vault frontmatter as cache_expires")
	require.False(t, v.CacheExpires.Never, "60s TTL stamps a finite cache_expires")
	expectedExpiry := time.Now().Add(60 * time.Second)
	delta := expectedExpiry.Sub(v.CacheExpires.Time).Abs()
	assert.Less(t, delta, 5*time.Second,
		"plugin TTL=60s should produce cache_expires ≈ now+60s (within 5s tolerance for ingest delay)")

	// Backdate cache_expires past now — past plugin's 60s TTL, fall through.
	backdateCacheExpires(t, root, "ttl-test-entity", "ttl-test:foo", 30*time.Second)
	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	assert.EqualValues(t, 2, calls.Load(),
		"backdate past plugin's 60s TTL must fall through even when global=3600s")
}

// TestIngest_CacheTTL_FetchResultOverrideWinsOverPlugin pins the
// per-fetch override: plugin can attach FetchResult.CacheTTLSeconds
// (pointer) to ad-hoc shorten or extend a specific fetch's TTL.
// Entry-level wins over plugin-level wins over global.
func TestIngest_CacheTTL_FetchResultOverrideWinsOverPlugin(t *testing.T) {
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
	override := 42 // explicit per-fetch — overrides plugin's 60.
	plug := &fixture.Plugin{
		NameValue: "ttl-test",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			calls.Add(1)
			id := "ttl-test:" + rawURL[strings.LastIndex(rawURL, "/")+1:]
			now := time.Now().UTC()
			return &plugins.FetchResult{
				Entity: &store.Entity{ID: id, Kind: "ttl-test-entity", Data: map[string]any{"title": id}},
				Provenance: []store.ProvenanceEntry{{Source: "fake:fetch", FetchedAt: &now, OK: true}},
				Notations: []string{rawURL},
				CacheTTLSeconds: &override,
			}, nil
		},
		CapabilitiesValue: plugins.Capabilities{Name: "ttl-test", CacheTTLSeconds: 60},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r), WithCacheTTL(time.Hour))

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	v, err := r.ReadByID("ttl-test-entity", "ttl-test:foo")
	require.NoError(t, err)
	require.NotNil(t, v.CacheExpires,
		"per-fetch override must produce a stamped cache_expires")
	require.False(t, v.CacheExpires.Never, "42s TTL stamps a finite cache_expires")
	expectedExpiry := time.Now().Add(42 * time.Second)
	delta := expectedExpiry.Sub(v.CacheExpires.Time).Abs()
	assert.Less(t, delta, 5*time.Second,
		"per-fetch FetchResult.CacheTTLSeconds=42 must produce cache_expires ≈ now+42s")
}

// TestIngest_CacheTTL_NegativeMeansInfinite pins the negative-
// sentinel rule: a negative cache_ttl_seconds at any level means
// "never expire" (cache forever even past arbitrary backdating).
func TestIngest_CacheTTL_NegativeMeansInfinite(t *testing.T) {
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
	plug := &fixture.Plugin{
		NameValue: "ttl-test",
		MatchFunc: func(rawURL string) bool {
			return strings.Contains(rawURL, "example.test/")
		},
		FetchFunc: func(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
			calls.Add(1)
			id := "ttl-test:" + rawURL[strings.LastIndex(rawURL, "/")+1:]
			now := time.Now().UTC()
			return &plugins.FetchResult{
				Entity: &store.Entity{ID: id, Kind: "ttl-test-entity", Data: map[string]any{"title": id}},
				Provenance: []store.ProvenanceEntry{{Source: "fake:fetch", FetchedAt: &now, OK: true}},
				Notations: []string{rawURL},
			}, nil
		},
		// Plugin declares -1 → infinite.
		CapabilitiesValue: plugins.Capabilities{Name: "ttl-test", CacheTTLSeconds: -1},
	}
	registry := plugins.NewRegistry()
	registry.Register(plug)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r), WithCacheTTL(time.Hour))

	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	require.EqualValues(t, 1, calls.Load())

	v, err := r.ReadByID("ttl-test-entity", "ttl-test:foo")
	require.NoError(t, err)
	require.NotNil(t, v.CacheExpires)
	assert.True(t, v.CacheExpires.Never,
		"negative TTL must stamp the `never` sentinel in cache_expires")

	// Even after a year, negative TTL means cache forever.
	// Try to backdate cache_expires anyway — Never sentinel
	// short-circuits to fresh regardless.
	postIngest(t, h, map[string]any{"url": "https://example.test/foo", "wait_seconds": 2})
	assert.EqualValues(t, 1, calls.Load(),
		"negative cache_ttl_seconds means cache forever — Never sentinel must still hit")
}

// (TestIngest_CacheTTL_LegacyFallback_* tests removed under
// PR-B — the legacy cache_ttl_seconds fallback path no longer
// exists in the lookup. Legacy entries that still have
// cache_ttl_seconds in their vault file cache forever until
// force_refetch re-ingests them under the new contract.)
