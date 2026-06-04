package vault_test

import (
	"context"
	"os"
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

// TestSubfolder_MultipleMatches_CollisionProbe pins the multi-subfolder
// collision case (#415): when two same-slug files exist in different
// subfolders (e.g. hand-authored
// before reindex), ReadByID returns not-found (no unique match) but the
// create-collision probe UserContentSlugInSubfolder still reports the
// collision — so create can't write a third flat file and break the
// flat id's uniqueness.
func TestSubfolder_MultipleMatches_CollisionProbe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, _ := vault.NewWriter(root)
	r, _ := vault.NewReader(root)
	ctx := context.Background()

	require.NoError(t, w.WriteWithCommitInSubfolder(ctx, ucEntity("user-content:dup", "Dup"), "notes", "", ""))
	require.NoError(t, w.WriteWithCommitInSubfolder(ctx, ucEntity("user-content:dup", "Dup"), "drafts", "", ""))
	require.FileExists(t, filepath.Join(root, "user-content", "notes", "dup.md"))
	require.FileExists(t, filepath.Join(root, "user-content", "drafts", "dup.md"))

	// ReadByID can't resolve a unique file → not-found.
	_, err := r.ReadByID("user-content", "user-content:dup")
	require.Error(t, err)
	assert.True(t, vault.IsNotExist(err), "two matches → ReadByID is not-found")

	// The collision probe still flags it.
	exists, err := r.UserContentSlugInSubfolder("dup")
	require.NoError(t, err)
	assert.True(t, exists, "collision probe sees the slug regardless of match count")
}

// TestSubfolder_ScopedToUserContent pins the kind-scoping of #415: the
// subfolder fallback is user-content only. A non-UGC kind with a
// same-slug file nested one level deep must NOT be resolved by ReadByID
// — generic entity reads stay on the flat / ct / archive paths.
func TestSubfolder_ScopedToUserContent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, _ := vault.NewWriter(root)
	r, _ := vault.NewReader(root)
	ctx := context.Background()

	bg := &vault.Entity{
		ID:     "boardgame:catan",
		Kind:   "boardgame",
		Source: []string{"bgg/default"},
		Data:   map[string]any{"id": "boardgame:catan", "title": "Catan"},
		Tags:   []string{"x"},
	}
	require.NoError(t, w.WriteWithCommitInSubfolder(ctx, bg, "shelf", "", ""))
	require.FileExists(t, filepath.Join(root, "boardgame", "shelf", "catan.md"))

	_, err := r.ReadByID("boardgame", "boardgame:catan")
	require.Error(t, err)
	assert.True(t, vault.IsNotExist(err),
		"non-UGC kinds do not glob nested markdown — the subfolder fallback is user-content only")
}

// TestMoveToSubfolder_RelocatesFileAcrossLocations pins #425 Cut 1: a
// move relocates the vault file flat<->subfolder<->subfolder via rename,
// is an idempotent no-op when already at the target, and reports
// os.ErrNotExist for a missing entity.
func TestMoveToSubfolder_RelocatesFileAcrossLocations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, w.WriteWithCommit(ctx, ucEntity("user-content:mv", "Move Me"), "", ""))
	flat := filepath.Join(root, "user-content", "mv.md")
	notes := filepath.Join(root, "user-content", "notes", "mv.md")
	drafts := filepath.Join(root, "user-content", "drafts", "mv.md")
	require.FileExists(t, flat)

	moved, err := w.MoveToSubfolder(ctx, "user-content", "user-content:mv", "notes", "", "")
	require.NoError(t, err)
	assert.True(t, moved)
	assert.NoFileExists(t, flat)
	require.FileExists(t, notes)

	moved, err = w.MoveToSubfolder(ctx, "user-content", "user-content:mv", "notes", "", "")
	require.NoError(t, err)
	assert.False(t, moved, "same subfolder is an idempotent no-op")

	moved, err = w.MoveToSubfolder(ctx, "user-content", "user-content:mv", "drafts", "", "")
	require.NoError(t, err)
	assert.True(t, moved)
	assert.NoFileExists(t, notes)
	require.FileExists(t, drafts)

	moved, err = w.MoveToSubfolder(ctx, "user-content", "user-content:mv", "", "", "")
	require.NoError(t, err)
	assert.True(t, moved)
	require.FileExists(t, flat)
	assert.NoFileExists(t, drafts)

	_, err = w.MoveToSubfolder(ctx, "user-content", "user-content:ghost", "notes", "", "")
	require.Error(t, err)
	assert.True(t, vault.IsNotExist(err))
}

// TestMoveToSubfolder_MovesAttachmentSidecar pins the #425 review fix:
// the attachment sidecar (`<dir>/<slug>/`) rides along with the .md as
// part of the move contract, so manifest attachment paths (which resolve
// relative to the .md) stay valid after a move — no .md-moved-but-
// sidecar-orphaned silent break.
func TestMoveToSubfolder_MovesAttachmentSidecar(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, w.WriteWithCommit(ctx, ucEntity("user-content:att", "Att"), "", ""))
	// A sidecar dir with one attachment alongside the flat .md.
	sidecar := filepath.Join(root, "user-content", "att")
	require.NoError(t, os.MkdirAll(sidecar, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sidecar, "photo.png"), []byte("img"), 0o644))

	moved, err := w.MoveToSubfolder(ctx, "user-content", "user-content:att", "notes", "", "")
	require.NoError(t, err)
	assert.True(t, moved)

	// Both .md and the sidecar (with its file) are at the new location;
	// neither remains at the old.
	require.FileExists(t, filepath.Join(root, "user-content", "notes", "att.md"))
	require.FileExists(t, filepath.Join(root, "user-content", "notes", "att", "photo.png"))
	assert.NoFileExists(t, filepath.Join(root, "user-content", "att.md"))
	assert.NoDirExists(t, sidecar)
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
