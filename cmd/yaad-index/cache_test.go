package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/vault"
)

// cacheEntryExpired — sentinel-rule predicate per alice2-index
// (post-PR-B: cache_expires-only; the legacy cache_ttl_seconds
// fallback is gone).
func TestCacheEntryExpired_SentinelRules(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name string
		expires *vault.CacheExpires
		includeInfinite bool
		want bool
	}{
		{"nil CacheExpires not expired (no opinion)", nil, false, false},
		{"future expiry not expired", vault.CacheExpiresAt(future), false, false},
		{"past expiry expired", vault.CacheExpiresAt(past), false, true},
		{"Never sentinel excluded by default", vault.CacheExpiresNever(), false, false},
		{"Never sentinel surfaced when --include-infinite", vault.CacheExpiresNever(), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &vault.Entity{
				ID: "test:foo",
				Kind: "test",
				Plugin: "test-plugin",
				CacheExpires: tc.expires,
			}
			got := cacheEntryExpired(e, now, tc.includeInfinite)
			assert.Equal(t, tc.want, got)
		})
	}
}

// findExpiredCacheEntries walks a vault root, applies filters, and
// returns the expired set. End-to-end-style: write entities to a
// real vault directory + read them back through the discovery path.
func TestFindExpiredCacheEntries_HappyPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	fresh := now.Add(-30 * time.Minute)
	pastExpiry := now.Add(-time.Hour)
	futureExpiry := now.Add(time.Hour)

	// Three entities — one expired (cache_expires in past), one
	// fresh (cache_expires in future), one with no stamp.
	entities := []*vault.Entity{
		{
			ID: "test:expired-one",
			Kind: "test",
			Plugin: "wikipedia",
			Data: map[string]any{"title": "Expired One"},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			CacheExpires: vault.CacheExpiresAt(pastExpiry),
		},
		{
			ID: "test:fresh-one",
			Kind: "test",
			Plugin: "wikipedia",
			Data: map[string]any{"title": "Fresh One"},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &fresh, OK: true},
			},
			CacheExpires: vault.CacheExpiresAt(futureExpiry),
		},
		{
			ID: "test:no-stamp",
			Kind: "test",
			Plugin: "wikipedia",
			Data: map[string]any{"title": "No Stamp"},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			// no CacheExpires stamped → cache forever → not expired
		},
	}
	for _, e := range entities {
		require.NoError(t, w.Write(e))
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := findExpiredCacheEntries(logger, root, "", "", false, now)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the expired-one should match")
	assert.Equal(t, "test:expired-one", got[0].ID)
	assert.Equal(t, "wikipedia", got[0].Plugin)
	assert.False(t, got[0].Never)
	assert.True(t, pastExpiry.Equal(got[0].ExpiresAt))
	assert.Equal(t, 2*time.Hour, got[0].Age)
}

// --plugin filter restricts to entities whose Plugin field matches.
func TestFindExpiredCacheEntries_PluginFilter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	for _, plug := range []string{"wikipedia", "boardgamegeek"} {
		require.NoError(t, w.Write(&vault.Entity{
			ID: "test:" + plug,
			Kind: "test",
			Plugin: plug,
			Data: map[string]any{"title": plug},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			CacheExpires: vault.CacheExpiresAt(pastExpiry),
		}))
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := findExpiredCacheEntries(logger, root, "wikipedia", "", false, now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "wikipedia", got[0].Plugin)
}

// --kind filter restricts to entities of the given kind.
func TestFindExpiredCacheEntries_KindFilter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	for _, kind := range []string{"person", "boardgame"} {
		require.NoError(t, w.Write(&vault.Entity{
			ID: kind + ":foo",
			Kind: kind,
			Plugin: "wikipedia",
			Data: map[string]any{"title": kind},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			CacheExpires: vault.CacheExpiresAt(pastExpiry),
		}))
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := findExpiredCacheEntries(logger, root, "", "person", false, now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "person", got[0].Kind)
}

// --include-infinite surfaces entries with negative TTL.
func TestFindExpiredCacheEntries_IncludeInfinite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)

	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:never-expires",
		Kind: "test",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "Never"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &stale, OK: true},
		},
		CacheExpires: vault.CacheExpiresNever(),
	}))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Default: --include-infinite=false → empty.
	got, err := findExpiredCacheEntries(logger, root, "", "", false, now)
	require.NoError(t, err)
	assert.Empty(t, got, "negative-TTL entries excluded by default")

	// With --include-infinite=true → surfaced.
	got, err = findExpiredCacheEntries(logger, root, "", "", true, now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Never, "Never-sentinel entries surface with Never=true")
}

// Hidden / non-.md files in the vault tree are ignored (defensive
// against the writer's `.<slug>.md.tmp-*` temp files leaking).
func TestFindExpiredCacheEntries_IgnoresHiddenAndNonMarkdown(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:real",
		Kind: "test",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "Real"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &stale, OK: true},
		},
		CacheExpires: vault.CacheExpiresAt(pastExpiry),
	}))

	// Spurious files: hidden tmp + a .txt non-markdown.
	require.NoError(t, writeFile(filepath.Join(root, "test", ".hidden.tmp-fake"), "garbage"))
	require.NoError(t, writeFile(filepath.Join(root, "test", "notes.txt"), "not markdown"))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := findExpiredCacheEntries(logger, root, "", "", false, now)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the real .md file should match; spurious files ignored")
	assert.Equal(t, "test:real", got[0].ID)
}

// writeFile is a tiny helper for the spurious-files test.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// findOlderThanCacheEntries — duration-based predicate variant
// (purge --older-than). Ignores cache_ttl_seconds entirely; matches
// only on freshest_fetched_at age.
func TestFindOlderThanCacheEntries_DurationPredicate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Hour)
	fresh := now.Add(-30 * time.Minute)

	// Three entities — only the stale one should match --older-than 1h.
	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:stale", Kind: "test", Plugin: "wikipedia",
		Data: map[string]any{"title": "Stale"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &stale, OK: true},
		},
		// No TTL stamped — would be excluded by find_expired but
		// older-than ignores TTL.
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:fresh", Kind: "test", Plugin: "wikipedia",
		Data: map[string]any{"title": "Fresh"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &fresh, OK: true},
		},
		// Future expiry — the older-than predicate ignores stamp, only
		// looks at fetched_at age. fresh is 30min old → not stale-by-1h.
		CacheExpires: vault.CacheExpiresAt(now.Add(time.Hour)),
	}))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := findOlderThanCacheEntries(logger, root, "", "", time.Hour, now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "test:stale", got[0].ID)
}

// pickRefetchNotation picks an http(s) URL when present, falls back
// to first non-empty otherwise, returns "" on empty / all-empty.
func TestPickRefetchNotation(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in []string
		want string
	}{
		"http URL preferred over shorthand": {
			in: []string{"wikipedia: Tehran", "https://en.wikipedia.org/wiki/Tehran"},
			want: "https://en.wikipedia.org/wiki/Tehran",
		},
		"shorthand fallback when no URL": {in: []string{"wikipedia: Tehran"}, want: "wikipedia: Tehran"},
		"empty slice returns empty": {in: nil, want: ""},
		"all-empty returns empty": {in: []string{"", ""}, want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, pickRefetchNotation(tc.in))
		})
	}
}

// CachePurgeCmd dry-run: prints matching entries, does NOT delete
// vault files.
func TestCachePurgeCmd_DryRunDoesNotDelete(t *testing.T) {
	t.Parallel()
	root, dbPath := writePurgeFixture(t)

	cmd := CachePurgeCmd{
		DBPath: dbPath,
		VaultPath: root,
		DryRun: true,
	}
	require.NoError(t, cmd.Run())

	// Vault files still present.
	_, err := os.Stat(filepath.Join(root, "test", "expired.md"))
	require.NoError(t, err, "dry-run must NOT delete the expired vault file")
}

// CachePurgeCmd real purge: deletes vault files for expired entries.
func TestCachePurgeCmd_RealPurgeDeletesVaultFiles(t *testing.T) {
	t.Parallel()
	root, dbPath := writePurgeFixture(t)

	cmd := CachePurgeCmd{
		DBPath: dbPath,
		VaultPath: root,
		DryRun: false,
	}
	require.NoError(t, cmd.Run())

	// Expired file gone, fresh file still present.
	_, err := os.Stat(filepath.Join(root, "test", "expired.md"))
	assert.True(t, os.IsNotExist(err), "expired vault file must be deleted; err=%v", err)
	_, err = os.Stat(filepath.Join(root, "test", "fresh.md"))
	require.NoError(t, err, "fresh vault file must survive purge")
}

// CachePurgeCmd --plugin filter: only matching plugin's entries
// purged.
func TestCachePurgeCmd_PluginFilter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "alice2.db")
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	for _, plug := range []string{"wikipedia", "boardgamegeek"} {
		require.NoError(t, w.Write(&vault.Entity{
			ID: "test:" + plug, Kind: "test", Plugin: plug,
			Data: map[string]any{"title": plug},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			CacheExpires: vault.CacheExpiresAt(pastExpiry),
		}))
	}

	cmd := CachePurgeCmd{
		DBPath: dbPath,
		VaultPath: root,
		Plugin: "wikipedia",
		DryRun: false,
	}
	require.NoError(t, cmd.Run())

	_, err = os.Stat(filepath.Join(root, "test", "wikipedia.md"))
	assert.True(t, os.IsNotExist(err), "wikipedia entity purged")
	_, err = os.Stat(filepath.Join(root, "test", "boardgamegeek.md"))
	require.NoError(t, err, "non-matching plugin's entity survives purge")
}

// CachePurgeCmd --older-than override: ignores cache_ttl_seconds
// and uses age-based predicate. Catches entries with no TTL stamped
// (legacy deployments).
func TestCachePurgeCmd_OlderThanOverride(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "alice2.db")
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	ancient := now.Add(-90 * 24 * time.Hour)

	// Entity with NO TTL stamped (legacy). Wouldn't match TTL
	// predicate, but matches --older-than 30d.
	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:ancient", Kind: "test", Plugin: "wikipedia",
		Data: map[string]any{"title": "Ancient"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &ancient, OK: true},
		},
	}))

	cmd := CachePurgeCmd{
		DBPath: dbPath,
		VaultPath: root,
		OlderThan: "720h", // 30 days
		DryRun: false,
	}
	require.NoError(t, cmd.Run())

	_, err = os.Stat(filepath.Join(root, "test", "ancient.md"))
	assert.True(t, os.IsNotExist(err),
		"--older-than must purge entries regardless of cache_ttl_seconds stamp")
}

// CachePurgeCmd --older-than parse error.
func TestCachePurgeCmd_OlderThanInvalidDuration(t *testing.T) {
	t.Parallel()

	cmd := CachePurgeCmd{
		ConfigPath: filepath.Join(t.TempDir(), "missing.yaml"),
		VaultPath: t.TempDir(),
		OlderThan: "not-a-duration",
		DryRun: true,
	}
	err := cmd.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse --older-than")
}

// writePurgeFixture sets up a vault root + db path with one
// expired entity and one fresh entity. Used by the purge tests
// to share the seed pattern.
func writePurgeFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "alice2.db")
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)
	fresh := now.Add(-30 * time.Minute)

	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:expired", Kind: "test", Plugin: "wikipedia",
		Data: map[string]any{"title": "Expired"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &stale, OK: true},
		},
		CacheExpires: vault.CacheExpiresAt(pastExpiry),
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: "test:fresh", Kind: "test", Plugin: "wikipedia",
		Data: map[string]any{"title": "Fresh"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &fresh, OK: true},
		},
		// Future expiry — fresh entity must NOT be purged.
		CacheExpires: vault.CacheExpiresAt(now.Add(time.Hour)),
	}))
	return root, dbPath
}

// CacheRefetchCmd happy path: posts force_refetch=true to the
// daemon for each expired entity. httptest.Server stands in for
// the daemon and asserts the request shape.
func TestCacheRefetchCmd_HappyPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	require.NoError(t, w.Write(&vault.Entity{
		ID: "wikipedia:foo", Kind: "wikipedia-article", Plugin: "wikipedia",
		Data: map[string]any{"title": "Foo"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "fake:fetch", FetchedAt: &stale, OK: true},
		},
		Notations: []string{"https://en.wikipedia.org/wiki/Foo", "wikipedia: Foo"},
		CacheExpires: vault.CacheExpiresAt(pastExpiry),
	}))

	var seenReqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/v1/ingest", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		seenReqs = append(seenReqs, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"state":"complete"}`))
	}))
	t.Cleanup(srv.Close)

	cmd := CacheRefetchCmd{
		ConfigPath: filepath.Join(t.TempDir(), "missing.yaml"),
		VaultPath: root,
		Server: srv.URL,
	}
	require.NoError(t, cmd.Run())

	require.Len(t, seenReqs, 1, "one refetch POST per expired entity")
	assert.Equal(t, "https://en.wikipedia.org/wiki/Foo", seenReqs[0]["url"],
		"prefers the http(s) URL over shorthand")
	assert.Equal(t, true, seenReqs[0]["force_refetch"],
		"force_refetch=true must be set")
}

// --limit clamps the number of refetch POSTs.
func TestCacheRefetchCmd_LimitClamps(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	for _, slug := range []string{"a", "b", "c"} {
		require.NoError(t, w.Write(&vault.Entity{
			ID: "wikipedia:" + slug, Kind: "wikipedia-article", Plugin: "wikipedia",
			Data: map[string]any{"title": slug},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			Notations: []string{"https://en.wikipedia.org/wiki/" + slug},
			CacheExpires: vault.CacheExpiresAt(pastExpiry),
		}))
	}

	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"state":"complete"}`))
	}))
	t.Cleanup(srv.Close)

	cmd := CacheRefetchCmd{
		ConfigPath: filepath.Join(t.TempDir(), "missing.yaml"),
		VaultPath: root,
		Server: srv.URL,
		Limit: 2,
	}
	require.NoError(t, cmd.Run())
	assert.Equal(t, 2, count, "--limit=2 must cap refetch POSTs at 2 even though 3 expired")
}

// Daemon error: failed refetch counted, loop continues.
func TestCacheRefetchCmd_DaemonErrorContinues(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	pastExpiry := now.Add(-time.Hour)

	for _, slug := range []string{"a", "b"} {
		require.NoError(t, w.Write(&vault.Entity{
			ID: "wikipedia:" + slug, Kind: "wikipedia-article", Plugin: "wikipedia",
			Data: map[string]any{"title": slug},
			Provenance: []vault.ProvenanceEntry{
				{Source: "fake:fetch", FetchedAt: &stale, OK: true},
			},
			Notations: []string{"https://en.wikipedia.org/wiki/" + slug},
			CacheExpires: vault.CacheExpiresAt(pastExpiry),
		}))
	}

	var seen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"boom","message":"upstream failure"}`))
	}))
	t.Cleanup(srv.Close)

	cmd := CacheRefetchCmd{
		ConfigPath: filepath.Join(t.TempDir(), "missing.yaml"),
		VaultPath: root,
		Server: srv.URL,
	}
	// Run returns nil even on per-entity failures — partial-success
	// is the documented contract.
	require.NoError(t, cmd.Run())
	assert.Equal(t, 2, seen, "loop must continue past per-entity 500s")
}
