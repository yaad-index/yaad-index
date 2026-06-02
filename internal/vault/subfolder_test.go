package vault_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/vault"
)

func ucEntity(id, title string) *vault.Entity {
	return &vault.Entity{
		ID:     id,
		Kind:   "user-content",
		Source: []string{"user/default"},
		Data:   map[string]any{"id": id, "title": title},
		Tags:   []string{"x"},
	}
}

// TestSubfolder_CreateReadEditPreservesLocation pins #415's load-bearing
// invariant: a file created in a subfolder is read back by its flat id,
// and a later edit (via the flat-path WriteWithCommit the edit handlers
// use) writes BACK to the subfolder rather than orphaning the entity to
// the flat path.
func TestSubfolder_CreateReadEditPreservesLocation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	ctx := context.Background()

	e := ucEntity("user-content:my-note", "My Note")
	require.NoError(t, w.WriteWithCommitInSubfolder(ctx, e, "notes", "", ""))

	subPath := filepath.Join(root, "user-content", "notes", "my-note.md")
	flatPath := filepath.Join(root, "user-content", "my-note.md")
	require.FileExists(t, subPath, "create lands in the subfolder")
	assert.NoFileExists(t, flatPath, "create does not write the flat path")

	// Read resolves by flat id via the subfolder glob fallback.
	got, err := r.ReadByID("user-content", "user-content:my-note")
	require.NoError(t, err)
	assert.Equal(t, "user-content:my-note", got.ID)

	// An edit via the flat-path WriteWithCommit (what the section
	// handlers call) must stay in the subfolder.
	e.Data["title"] = "My Note (edited)"
	require.NoError(t, w.WriteWithCommit(ctx, e, "", ""))
	require.FileExists(t, subPath, "edit stays in the subfolder")
	assert.NoFileExists(t, flatPath, "edit does not orphan to the flat path")

	got2, err := r.ReadByID("user-content", "user-content:my-note")
	require.NoError(t, err)
	assert.Equal(t, "My Note (edited)", got2.Data["title"])
}

// TestSubfolder_FlatUnaffected pins that a flat (no-subfolder) entity is
// written + read at the flat path, unchanged by #415.
func TestSubfolder_FlatUnaffected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, _ := vault.NewWriter(root)
	r, _ := vault.NewReader(root)
	ctx := context.Background()

	require.NoError(t, w.WriteWithCommit(ctx, ucEntity("user-content:flat", "Flat"), "", ""))
	require.FileExists(t, filepath.Join(root, "user-content", "flat.md"))
	got, err := r.ReadByID("user-content", "user-content:flat")
	require.NoError(t, err)
	assert.Equal(t, "user-content:flat", got.ID)
}

// TestSubfolder_ArchiveFindsSubfolderSource pins that archiving resolves
// a subfoldered source file (else delete, which routes through
// archive→destroy, can't find it). The archive destination is flat.
func TestSubfolder_ArchiveFindsSubfolderSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, _ := vault.NewWriter(root)
	ctx := context.Background()

	require.NoError(t, w.WriteWithCommitInSubfolder(ctx, ucEntity("user-content:arch-me", "Arch"), "drafts", "", ""))
	subPath := filepath.Join(root, "user-content", "drafts", "arch-me.md")
	require.FileExists(t, subPath)

	require.NoError(t, w.ArchiveWithCommit(ctx, "user-content", "user-content:arch-me", "", ""))
	assert.NoFileExists(t, subPath, "archive moves the subfolder source")
	assert.FileExists(t, filepath.Join(root, "_archive", "user-content", "arch-me.md"),
		"archive destination is flat")
}
