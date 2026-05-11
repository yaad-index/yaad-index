package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// Test_clearPluginCache_NoNameClearsAll covers the no-flag path:
// `alice2-index plugins clear-cache` drops every cached row and prints
// the count. After the call, GetPluginCapabilities for any seeded
// plugin returns not-found.
func Test_clearPluginCache_NoNameClearsAll(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, name := range []string{"wikipedia", "bgg"} {
		require.NoError(t, st.UpsertPluginCapabilities(ctx, name, "0.1.0", []byte(`{}`)),
			"seed %s", name)
	}

	var buf bytes.Buffer
	require.NoError(t, clearPluginCache(ctx, st, "", &buf))
	assert.Contains(t, buf.String(), "cleared 2 plugin_capabilities row")
	for _, name := range []string{"wikipedia", "bgg"} {
		_, found, _ := st.GetPluginCapabilities(ctx, name)
		assert.False(t, found, "%s should be absent after clear-all", name)
	}
}

// Test_clearPluginCache_NameDropsSingleRow covers the targeted path:
// `--name <plugin>` drops only that one row and reports the action.
// Other plugins' rows survive.
func Test_clearPluginCache_NameDropsSingleRow(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, name := range []string{"wikipedia", "bgg"} {
		require.NoError(t, st.UpsertPluginCapabilities(ctx, name, "0.1.0", []byte(`{}`)),
			"seed %s", name)
	}

	var buf bytes.Buffer
	require.NoError(t, clearPluginCache(ctx, st, "wikipedia", &buf))
	assert.Contains(t, buf.String(), `cleared 1 plugin_capabilities row for "wikipedia"`)

	_, found, _ := st.GetPluginCapabilities(ctx, "wikipedia")
	assert.False(t, found, "wikipedia should be absent after targeted clear")
	_, found, _ = st.GetPluginCapabilities(ctx, "bgg")
	assert.True(t, found, "bgg should still be present (targeted clear must not leak)")
}

// Test_clearPluginCache_NameAbsentReportsAlreadyAbsent guards the
// no-op case: clearing a plugin that was never cached emits an
// informational message rather than an error. Operators routinely
// run clear-cache after a plugin removal; the no-op should be quiet,
// not loud.
func Test_clearPluginCache_NameAbsentReportsAlreadyAbsent(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	var buf bytes.Buffer
	require.NoError(t, clearPluginCache(context.Background(), st, "never-cached", &buf))
	assert.Contains(t, buf.String(), "already absent")
}
