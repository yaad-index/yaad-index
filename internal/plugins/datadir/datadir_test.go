package datadir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolve_DefaultUnderUserCacheDir pins the default path
// shape: when no override is set, Resolve returns
// `<UserCacheDir>/yaad-<plugin>/<instance>`. UserCacheDir maps
// to XDG_CACHE_HOME when set, so the test sets it to a temp dir
// and asserts the join shape.
//
// t.Setenv-using tests intentionally don't call t.Parallel.
func TestResolve_DefaultUnderUserCacheDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	got, err := Resolve("github", "personal", "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(tmp, "yaad-github", "personal"), got)
}

// TestResolve_OverridePassesThrough pins the operator-override
// path: an explicit instances[*].data_dir is returned verbatim,
// no UserCacheDir join.
func TestResolve_OverridePassesThrough(t *testing.T) {
	t.Parallel()
	got, err := Resolve("github", "personal", "/srv/yaad/state/github-personal")
	require.NoError(t, err)
	assert.Equal(t, "/srv/yaad/state/github-personal", got)
}

// TestResolve_InstancesHaveDistinctPaths pins multi-instance
// isolation: two instances of the same plugin resolve to
// different paths under the default scheme. Required for the
// #282 cookie-jar contract (each BGG account's cookies must be
// stored separately).
func TestResolve_InstancesHaveDistinctPaths(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	personal, err := Resolve("gmail", "personal", "")
	require.NoError(t, err)
	work, err := Resolve("gmail", "work", "")
	require.NoError(t, err)
	assert.NotEqual(t, personal, work)
}

// TestEnsure_CreatesDirAt0700 pins the create-with-perms path:
// a non-existent dir is created with the secrets-grade 0700
// perm. Parent dirs are created too (MkdirAll).
func TestEnsure_CreatesDirAt0700(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	target := filepath.Join(tmp, "nested", "yaad-test", "personal")
	require.NoError(t, Ensure(target))
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// TestEnsure_IdempotentOnExistingDir pins that re-calling Ensure
// on an existing directory is a no-op (no error, perms left
// alone). The daemon never re-perms operator-owned state per the
// #284 contract.
func TestEnsure_IdempotentOnExistingDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	target := filepath.Join(tmp, "existing")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, Ensure(target))
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(),
		"Ensure must not chmod an existing dir; operator-set perms stay")
}

// TestEnsure_RejectsNonDirAtPath pins the failure path: a file
// existing at the resolved path returns an error rather than
// proceeding with a misconfigured plugin.
func TestEnsure_RejectsNonDirAtPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	target := filepath.Join(tmp, "not-a-dir")
	require.NoError(t, os.WriteFile(target, []byte("file"), 0o600))
	err := Ensure(target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}
