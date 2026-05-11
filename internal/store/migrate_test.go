package store

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_LoadMigrations_RejectsDuplicateVersion locks in the explicit
// duplicate-version check in loadMigrationsFromFS. Two files at the same
// version would silently skip the second after the first applied — a
// drift hazard as the migration count grows. The test pre-empts that by
// failing CI the moment a collision is introduced anywhere in the
// embedded migrations tree.
func Test_LoadMigrations_RejectsDuplicateVersion(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"migs/001_init.sql": &fstest.MapFile{Data: []byte("CREATE TABLE a (x INTEGER);")},
		"migs/001_dup.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (x INTEGER);")},
		"migs/002_after_collision.sql": &fstest.MapFile{Data: []byte("CREATE TABLE c (x INTEGER);")},
	}

	_, err := loadMigrationsFromFS(fsys, "migs")
	require.Error(t, err, "want error on duplicate version")
	assert.Contains(t, err.Error(), "duplicate migration version 1",
		"error message should mention 'duplicate migration version 1'")
}

func Test_LoadMigrations_AcceptsDistinctVersions(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"migs/001_init.sql": &fstest.MapFile{Data: []byte("CREATE TABLE a (x INTEGER);")},
		"migs/002_more.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (x INTEGER);")},
		"migs/010_skip.sql": &fstest.MapFile{Data: []byte("CREATE TABLE c (x INTEGER);")},
		"migs/README.md": &fstest.MapFile{Data: []byte("ignore non-sql files")},
		"migs/notes.txt": &fstest.MapFile{Data: []byte("also ignored")},
	}

	got, err := loadMigrationsFromFS(fsys, "migs")
	require.NoError(t, err)
	require.Len(t, got, 3, "want 3 migrations (sql files only)")
	// Sorted by version, gaps allowed.
	wantVersions := []int{1, 2, 10}
	for i, m := range got {
		assert.Equal(t, wantVersions[i], m.version, "migration[%d].version", i)
	}
}
