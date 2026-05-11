package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
)

// silentLogger discards every record. Tests that don't assert on
// reprobe's stderr-bound progress logger don't want them in the
// test output stream either.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Test_reprobePlugins_FreshFirstWrite covers the no-prior-cache
// path: the plugin row didn't exist before reprobe; running it
// writes a fresh row and the summary line reports `(none) ->
// <version> [shape: first-write]`.
func Test_reprobePlugins_FreshFirstWrite(t *testing.T) {
	st := newSeededStore(t)
	cfg := newFakePluginConfig(t, fakeModeOKv1)

	var out bytes.Buffer
	err := reprobePlugins(context.Background(), silentLogger(), st, cfg.Plugins, "", &out)
	require.NoError(t, err)
	got := out.String()
	assert.Contains(t, got, "fake: (none) -> 0.1.0 [shape: first-write]",
		"first-write summary line; got %q", got)

	// Fresh row landed in the cache.
	row, found, err := st.GetPluginCapabilities(context.Background(), "fake")
	require.NoError(t, err)
	require.True(t, found, "reprobe must upsert the fresh row")
	assert.Equal(t, "0.1.0", row.Version)
}

// Test_reprobePlugins_VersionAndShapeChanged covers the recovery
// case the command exists for: seed a cache row with version
// 0.1.0 + minimal caps, run reprobe against a binary that now
// emits 0.2.0 + different caps, confirm the row is overwritten +
// the summary line reports the version transition + shape:changed.
func Test_reprobePlugins_VersionAndShapeChanged(t *testing.T) {
	st := newSeededStore(t)
	ctx := context.Background()
	require.NoError(t, st.UpsertPluginCapabilities(ctx, "fake", "0.1.0",
		[]byte(`{"name":"fake","version":"0.1.0","url_patterns":["^https?://example\\.test/.*"],"entity_kinds":[]}`)))
	cfg := newFakePluginConfig(t, fakeModeOKv2)

	var out bytes.Buffer
	require.NoError(t, reprobePlugins(ctx, silentLogger(), st, cfg.Plugins, "", &out))
	got := out.String()
	assert.Contains(t, got, "fake: 0.1.0 -> 0.2.0",
		"version transition reported; got %q", got)
	assert.Contains(t, got, "[shape: changed]",
		"version + shape both changed; got %q", got)

	row, _, _ := st.GetPluginCapabilities(ctx, "fake")
	assert.Equal(t, "0.2.0", row.Version, "cache row reflects fresh version after reprobe")
}

// Test_reprobePlugins_ShapeUnchanged covers the no-op-but-still-
// cycled path: cache row matches what --init re-emits exactly,
// summary reports shape:unchanged. Useful sanity that operators
// running reprobe defensively (no actual change) get a clean
// "nothing to see" signal.
func Test_reprobePlugins_ShapeUnchanged(t *testing.T) {
	st := newSeededStore(t)
	ctx := context.Background()
	// Seed with the EXACT JSON the fake plugin emits on --init
	// (modes OKv1 + NoVer share the v1 caps shape; we use OKv1
	// here so --version probe also matches 0.1.0).
	// Seed JSON must match exactly what fake plugin's --init
	// emits — otherwise the shape-compare's Marshal round-trip
	// surfaces incidental differences (nil vs empty slice
	// re-marshaling to null vs []) as `changed`. Cribbing the
	// fake plugin's own emit shape (entity_kinds: [] empty
	// array) keeps the comparison honest.
	require.NoError(t, st.UpsertPluginCapabilities(ctx, "fake", "0.1.0",
		[]byte(`{"name":"fake","version":"0.1.0","url_patterns":["^https?://example\\.test/.*"],"entity_kinds":[]}`)))
	cfg := newFakePluginConfig(t, fakeModeOKv1)

	var out bytes.Buffer
	require.NoError(t, reprobePlugins(ctx, silentLogger(), st, cfg.Plugins, "", &out))
	got := out.String()
	assert.Contains(t, got, "fake: 0.1.0 -> 0.1.0",
		"version stable across reprobe; got %q", got)
	assert.Contains(t, got, "[shape: unchanged]",
		"shape compare reports unchanged; got %q", got)
}

// Test_reprobePlugins_NameNotInConfig pins the operator-typo path:
// --name <wrong> errors out with a clear message naming the
// available plugins, instead of silently reprobing nothing.
func Test_reprobePlugins_NameNotInConfig(t *testing.T) {
	st := newSeededStore(t)
	cfg := newFakePluginConfig(t, fakeModeOKv1)

	var out bytes.Buffer
	err := reprobePlugins(context.Background(), silentLogger(), st, cfg.Plugins, "typo", &out)
	require.Error(t, err, "missing plugin name must be a hard error, not a silent no-op")
	assert.Contains(t, err.Error(), `plugin "typo" not found`)
	assert.Contains(t, err.Error(), "fake",
		"error must list the available plugin names so operator can correct the typo")
	assert.Empty(t, out.String(), "no per-plugin summary line emitted on the typo path")
}

// Test_reprobePlugins_NameTargetsOneOfMany covers --name dispatch:
// two plugins in config, --name targets only one, the other is
// untouched (its cache row stays whatever it was).
func Test_reprobePlugins_NameTargetsOneOfMany(t *testing.T) {
	st := newSeededStore(t)
	ctx := context.Background()
	// Seed an unrelated plugin's cache row that should NOT be
	// touched by a targeted reprobe.
	require.NoError(t, st.UpsertPluginCapabilities(ctx, "untouched", "9.9.9",
		[]byte(`{"name":"untouched","version":"9.9.9","url_patterns":["^x://"],"entity_kinds":[]}`)))

	cfg := newFakePluginConfig(t, fakeModeOKv1)
	// Add a second entry that points at a non-existent path.
	// reprobe will only target "fake"; the other entry isn't
	// touched so its missing path doesn't error.
	cfg.Plugins = append(cfg.Plugins, config.PluginEntry{Name: "untouched", Path: "/nonexistent/binary"})

	var out bytes.Buffer
	require.NoError(t, reprobePlugins(ctx, silentLogger(), st, cfg.Plugins, "fake", &out))

	got := out.String()
	assert.Contains(t, got, "fake:")
	assert.NotContains(t, got, "untouched", "targeted reprobe must not surface the untargeted plugin")

	// Untouched plugin's cache row survives at its original
	// version (no overwrite).
	row, found, _ := st.GetPluginCapabilities(ctx, "untouched")
	require.True(t, found)
	assert.Equal(t, "9.9.9", row.Version,
		"untouched plugin's cache row must survive a targeted reprobe of a different plugin")
}

// Test_reprobePlugins_AggregatesFailures pins the multi-plugin
// failure path: with several plugins configured and one binary
// missing, reprobe surfaces the per-plugin error line + returns a
// non-zero error naming the failure count and which names failed.
// The other plugins still successfully reprobe — one broken
// binary doesn't block recovery for the rest.
func Test_reprobePlugins_AggregatesFailures(t *testing.T) {
	st := newSeededStore(t)
	cfg := newFakePluginConfig(t, fakeModeOKv1)
	cfg.Plugins = append(cfg.Plugins, config.PluginEntry{Name: "broken", Path: "/nonexistent/binary"})

	var out bytes.Buffer
	err := reprobePlugins(context.Background(), silentLogger(), st, cfg.Plugins, "", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken")
	got := out.String()
	assert.Contains(t, got, "fake: (none) -> 0.1.0",
		"good plugin still reprobed despite sibling failure")
	assert.Contains(t, got, "broken: ERROR",
		"broken plugin emits a per-plugin error line on stdout")
	// And the broken plugin's cache row stays empty (no upsert
	// happened since --init failed).
	_, found, _ := st.GetPluginCapabilities(context.Background(), "broken")
	assert.False(t, found, "broken plugin must not get a cache row")
}

// Test_sameCapabilitiesShape pins the shape-compare helper:
// reordered keys + whitespace differences round-trip identical;
// real shape changes (added field) report different.
func Test_sameCapabilitiesShape(t *testing.T) {
	t.Parallel()

	a := []byte(`{"name":"fake","version":"0.1.0","url_patterns":["^x://"],"entity_kinds":[]}`)
	b := []byte(`{"version":"0.1.0","entity_kinds":[],"url_patterns":["^x://"],"name":"fake"}`)
	assert.True(t, sameCapabilitiesShape(a, b),
		"key-order shuffle must compare equal after Marshal round-trip")

	c := []byte(`{"name":"fake","version":"0.1.0","url_patterns":["^x://"],"entity_kinds":[],"source_namespace":"fake"}`)
	assert.False(t, sameCapabilitiesShape(a, c),
		"added field on the second blob must compare different (the regression-recovery signal)")
}

// Test_pluginEntryNames_Empty pins the nil-config edge case for
// the error helper.
func Test_pluginEntryNames_Empty(t *testing.T) {
	t.Parallel()
	got := pluginEntryNames(nil)
	assert.Empty(t, got)
	got = pluginEntryNames([]config.PluginEntry{})
	assert.Empty(t, got)
	got = pluginEntryNames([]config.PluginEntry{{Name: "a"}, {Name: "b"}})
	assert.Equal(t, []string{"a", "b"}, got)

	// Sanity: the error message includes the whole list, not just
	// the count. Operator can read the names directly.
	formatted := strings.Join(got, ",")
	assert.Equal(t, "a,b", formatted)
}
