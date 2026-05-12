package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginCapabilities_GetMissReturnsNotFound asserts the cache-miss
// signal: no row → (zero-value, false, nil). Callers (the loader)
// take this as "no cache; fall through to --init."
func TestPluginCapabilities_GetMissReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	got, found, err := s.GetPluginCapabilities(context.Background(), "wikipedia")
	require.NoError(t, err, "GetPluginCapabilities on empty cache")
	assert.False(t, found, "found: want false on empty cache (entry=%+v)", got)
}

// TestPluginCapabilities_UpsertThenGet round-trips a single row and
// asserts every column survives. cached_at must be parseable as a
// recent UTC timestamp.
func TestPluginCapabilities_UpsertThenGet(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	const name = "wikipedia"
	const version = "0.1.0"
	caps := []byte(`{"name":"wikipedia","version":"0.1.0","url_patterns":["^https?://...wikipedia.org/wiki/.+"]}`)

	before := time.Now().UTC()
	require.NoError(t,
		s.UpsertPluginCapabilities(context.Background(), name, version, caps),
		"UpsertPluginCapabilities")
	after := time.Now().UTC()

	got, found, err := s.GetPluginCapabilities(context.Background(), name)
	require.NoError(t, err, "GetPluginCapabilities")
	require.True(t, found, "found: want true after upsert")
	assert.Equal(t, version, got.Version)
	assert.Equal(t, string(caps), string(got.CapabilitiesJSON))
	assert.False(t, got.CachedAt.Before(before), "cached_at: want >= before (%s), got %s", before, got.CachedAt)
	assert.False(t, got.CachedAt.After(after.Add(time.Second)),
		"cached_at: want <= after+1s (%s), got %s", after, got.CachedAt)
}

// TestPluginCapabilities_UpsertOverwrites is the version-mismatch
// invalidation path the loader exercises: bumping a plugin's version
// triggers UpsertPluginCapabilities, which overwrites the existing
// row in place. Subsequent Get returns the new version.
func TestPluginCapabilities_UpsertOverwrites(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t,
		s.UpsertPluginCapabilities(ctx, "wikipedia", "0.1.0", []byte(`{"version":"0.1.0"}`)),
		"first upsert")
	require.NoError(t,
		s.UpsertPluginCapabilities(ctx, "wikipedia", "0.2.0", []byte(`{"version":"0.2.0"}`)),
		"second upsert")

	got, found, err := s.GetPluginCapabilities(ctx, "wikipedia")
	require.NoError(t, err, "GetPluginCapabilities")
	require.True(t, found, "found: want true")
	assert.Equal(t, "0.2.0", got.Version)
	assert.Equal(t, `{"version":"0.2.0"}`, string(got.CapabilitiesJSON))
}

// TestPluginCapabilities_DeleteRemovesOne covers the
// `yaad-index plugins clear-cache --name <n>` path. A delete on an
// existing row reports rows-deleted=true; subsequent Get returns
// not-found. Other rows are untouched.
func TestPluginCapabilities_DeleteRemovesOne(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t,
		s.UpsertPluginCapabilities(ctx, "wikipedia", "0.1.0", []byte(`{}`)),
		"seed wikipedia")
	require.NoError(t,
		s.UpsertPluginCapabilities(ctx, "bgg", "1.0", []byte(`{}`)),
		"seed bgg")

	dropped, err := s.DeletePluginCapabilities(ctx, "wikipedia")
	require.NoError(t, err, "DeletePluginCapabilities(wikipedia)")
	assert.True(t, dropped, "dropped: want true on existing row")

	_, found, _ := s.GetPluginCapabilities(ctx, "wikipedia")
	assert.False(t, found, "wikipedia: want absent after delete")

	_, found, _ = s.GetPluginCapabilities(ctx, "bgg")
	assert.True(t, found, "bgg: want still present (delete must not leak)")

	// Idempotent: re-deleting reports false (no row).
	dropped2, err := s.DeletePluginCapabilities(ctx, "wikipedia")
	require.NoError(t, err, "DeletePluginCapabilities(wikipedia, second)")
	assert.False(t, dropped2, "dropped on second delete: want false (no row)")
}

// TestPluginCapabilities_ClearAllEmptiesTable covers the
// `yaad-index plugins clear-cache` (no --name) path. Every row goes;
// the count returned matches what was there.
func TestPluginCapabilities_ClearAllEmptiesTable(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	for _, name := range []string{"wikipedia", "bgg", "books"} {
		require.NoError(t,
			s.UpsertPluginCapabilities(ctx, name, "1.0", []byte(`{}`)),
			"seed %s", name)
	}

	n, err := s.ClearAllPluginCapabilities(ctx)
	require.NoError(t, err, "ClearAllPluginCapabilities")
	assert.Equal(t, 3, n, "rows-deleted")
	for _, name := range []string{"wikipedia", "bgg", "books"} {
		_, found, _ := s.GetPluginCapabilities(ctx, name)
		assert.False(t, found, "%s: want absent after ClearAll", name)
	}

	// On an already-empty table, Clear reports 0 rows.
	n2, err := s.ClearAllPluginCapabilities(ctx)
	require.NoError(t, err, "ClearAllPluginCapabilities (second)")
	assert.Equal(t, 0, n2, "rows-deleted on empty table")
}

// TestPluginCapabilities_UpsertRejectsEmptyArgs guards the contract
// (name, version, capabilities_json must all be non-empty). A plugin
// with no version doesn't get cached — callers fall through to a
// fresh --init each time. The loader currently skips the upsert when
// caps.Version is empty; this test pins the store-side check too.
func TestPluginCapabilities_UpsertRejectsEmptyArgs(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	assert.Error(t, s.UpsertPluginCapabilities(ctx, "", "1.0", []byte(`{}`)), "empty name")
	assert.Error(t, s.UpsertPluginCapabilities(ctx, "wikipedia", "", []byte(`{}`)), "empty version")
	assert.Error(t, s.UpsertPluginCapabilities(ctx, "wikipedia", "1.0", nil), "nil capabilities_json")
	assert.Error(t, s.UpsertPluginCapabilities(ctx, "wikipedia", "1.0", []byte{}), "empty capabilities_json")
}
