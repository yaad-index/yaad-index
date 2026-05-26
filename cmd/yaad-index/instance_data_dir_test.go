package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins/datadir"
)

// TestEnsurePluginInstanceDataDirs_CreatesDefault pins the
// startup-pass happy path: every configured plugin instance
// gets its `<XDG_CACHE>/yaad-<plugin>/<instance>/` dir created
// at 0700 perms before plugin subprocesses spawn.
//
// t.Setenv-using tests intentionally don't call t.Parallel.
func TestEnsurePluginInstanceDataDirs_CreatesDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal"},
			{Name: "acme-org"},
		},
		"gmail": {
			{Name: "default"},
		},
	}
	require.NoError(t, ensurePluginInstanceDataDirs(cfg))
	for _, want := range []string{
		filepath.Join(tmp, "yaad-github", "personal"),
		filepath.Join(tmp, "yaad-github", "acme-org"),
		filepath.Join(tmp, "yaad-gmail", "default"),
	} {
		info, err := os.Stat(want)
		require.NoError(t, err, "dir %s must exist", want)
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
}

// TestEnsurePluginInstanceDataDirs_HonorsOperatorOverride pins
// that an explicit `instances[*].data_dir` is created verbatim
// (not joined under userCacheDir).
func TestEnsurePluginInstanceDataDirs_HonorsOperatorOverride(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	override := filepath.Join(tmp, "custom", "github-personal")
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal", DataDir: override},
		},
	}
	require.NoError(t, ensurePluginInstanceDataDirs(cfg))
	info, err := os.Stat(override)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestEnsurePluginInstanceDataDirs_RejectsNonDir pins the
// fail-fast path: a file squatting the resolved data-dir path
// surfaces a clear error at boot rather than confusing the
// plugin subprocess at first dispatch.
func TestEnsurePluginInstanceDataDirs_RejectsNonDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	squatter := filepath.Join(tmp, "squatter")
	require.NoError(t, os.WriteFile(squatter, []byte("not a dir"), 0o600))
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal", DataDir: squatter},
		},
	}
	err := ensurePluginInstanceDataDirs(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github")
	assert.Contains(t, err.Error(), "personal")
	assert.Contains(t, err.Error(), "not a directory")
}

// TestEnsurePluginInstanceDataDirs_EmptyConfigOK pins that an
// empty configured-instances map is a no-op (no error). The
// daemon starts with no plugins in test/dev paths.
func TestEnsurePluginInstanceDataDirs_EmptyConfigOK(t *testing.T) {
	t.Parallel()
	require.NoError(t, ensurePluginInstanceDataDirs(nil))
	require.NoError(t, ensurePluginInstanceDataDirs(map[string][]config.InstanceEntry{}))
}

// TestEnsurePluginInstanceDataDirs_HonorsPluginDataRoot pins the
// PR-291 fold-in for #287: when SetRoot is called BEFORE the
// ensure pass (the correct ordering in main.go's run loop), the
// startup MkdirAll creates dirs under the operator-configured
// `plugin_data_root`. The earlier ordering bug stamped a
// different path on YAAD_PLUGIN_DATA_DIR per dispatch than the
// daemon had MkdirAll'd at boot; this test pins the fix.
func TestEnsurePluginInstanceDataDirs_HonorsPluginDataRoot(t *testing.T) {
	// Mutates package-global datadir.pluginDataRoot; can't
	// t.Parallel.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "operator-plugin-data-root")
	datadir.SetRoot(root)
	t.Cleanup(func() { datadir.SetRoot("") })

	cfg := map[string][]config.InstanceEntry{
		"github": {{Name: "personal"}},
	}
	require.NoError(t, ensurePluginInstanceDataDirs(cfg))

	want := filepath.Join(root, "yaad-github", "personal")
	info, err := os.Stat(want)
	require.NoError(t, err, "dir under plugin_data_root must exist after ensure pass; got SetRoot/ensure ordering bug if missing")
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
