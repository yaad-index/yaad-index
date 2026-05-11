package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedActiveEntityWithAttachment writes one entity in the active
// layout plus a single attachment file under
// `<root>/<kind>/<slug>/attachments/<name>`. Returns the writer +
// the resolved file path so tests can assert post-cascade state.
func seedActiveEntityWithAttachment(t *testing.T, kind, id, name, body string) (*Writer, string, string) {
	t.Helper()
	root := t.TempDir()
	w, err := NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&Entity{
		ID: id,
		Kind: kind,
		Plugin: "fixture",
		Data: map[string]any{"name": "Cascade-Fixture"},
		Attachments: []Attachment{
			{Name: name, Kind: "image/jpeg", Path: filepath.Join("attachments", name)},
		},
	}))
	slug, err := slugFromID(id)
	require.NoError(t, err)
	dir := filepath.Join(root, kind, slug, "attachments")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	filePath := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(filePath, []byte(body), 0o644))
	return w, root, filePath
}

func TestArchiveWithCommit_CascadesAttachmentSubdir(t *testing.T) {
	t.Parallel()
	const id = "boardgame:cascade-archive-2024"
	w, root, activeFile := seedActiveEntityWithAttachment(t, "boardgame", id, "thumb.jpg", "active-bytes")

	require.NoError(t, w.ArchiveWithCommit(context.Background(), "boardgame", id, "archive: "+id, ""))

	// Active subdir gone, archive subdir present with the file.
	_, err := os.Stat(activeFile)
	assert.True(t, os.IsNotExist(err), "active attachment file must be gone after archive")
	archiveFile := filepath.Join(root, ArchiveDir, "boardgame", "cascade-archive-2024", "attachments", "thumb.jpg")
	got, err := os.ReadFile(archiveFile)
	require.NoError(t, err, "archive attachment must exist")
	assert.Equal(t, "active-bytes", string(got))

	// Active .md gone, archive .md present.
	_, err = os.Stat(filepath.Join(root, "boardgame", "cascade-archive-2024.md"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, ArchiveDir, "boardgame", "cascade-archive-2024.md"))
	assert.NoError(t, err)
}

func TestArchiveWithCommit_NoSubdir_StillSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := NewWriter(root)
	require.NoError(t, err)
	const id = "boardgame:no-attach-2024"
	require.NoError(t, w.Write(&Entity{
		ID: id,
		Kind: "boardgame",
		Plugin: "fixture",
		Data: map[string]any{"name": "No-Attach"},
	}))

	// No subdir to cascade — must still archive cleanly.
	require.NoError(t, w.ArchiveWithCommit(context.Background(), "boardgame", id, "archive: "+id, ""))

	_, err = os.Stat(filepath.Join(root, ArchiveDir, "boardgame", "no-attach-2024.md"))
	assert.NoError(t, err)
}

func TestRestoreWithCommit_CascadesAttachmentSubdir(t *testing.T) {
	t.Parallel()
	const id = "boardgame:cascade-restore-2024"
	w, root, _ := seedActiveEntityWithAttachment(t, "boardgame", id, "thumb.jpg", "round-trip")

	// Archive first.
	require.NoError(t, w.ArchiveWithCommit(context.Background(), "boardgame", id, "archive: "+id, ""))
	// Then restore — both .md and subdir must move back.
	require.NoError(t, w.RestoreWithCommit(context.Background(), "boardgame", id, "restore: "+id, ""))

	activeFile := filepath.Join(root, "boardgame", "cascade-restore-2024", "attachments", "thumb.jpg")
	got, err := os.ReadFile(activeFile)
	require.NoError(t, err, "active attachment must reappear after restore")
	assert.Equal(t, "round-trip", string(got))

	// Archive subdir gone after restore.
	_, err = os.Stat(filepath.Join(root, ArchiveDir, "boardgame", "cascade-restore-2024", "attachments"))
	assert.True(t, os.IsNotExist(err), "archive subdir must be empty after restore")
}

func TestDestroyArchivedWithCommit_RemovesAttachmentSubdir(t *testing.T) {
	t.Parallel()
	const id = "boardgame:cascade-destroy-2024"
	w, root, _ := seedActiveEntityWithAttachment(t, "boardgame", id, "thumb.jpg", "soon-gone")

	// Archive, then destroy.
	require.NoError(t, w.ArchiveWithCommit(context.Background(), "boardgame", id, "archive: "+id, ""))
	require.NoError(t, w.DestroyArchivedWithCommit(context.Background(), "boardgame", id, "destroy: "+id, ""))

	// Both .md and subdir gone from the archive layout.
	_, err := os.Stat(filepath.Join(root, ArchiveDir, "boardgame", "cascade-destroy-2024.md"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, ArchiveDir, "boardgame", "cascade-destroy-2024"))
	assert.True(t, os.IsNotExist(err), "archive subdir must be gone after destroy")
}

func TestDestroyArchivedWithCommit_NoSubdir_StillRemovesMD(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := NewWriter(root)
	require.NoError(t, err)
	const id = "boardgame:no-attach-destroy-2024"
	require.NoError(t, w.Write(&Entity{
		ID: id,
		Kind: "boardgame",
		Plugin: "fixture",
		Data: map[string]any{"name": "Plain"},
	}))
	require.NoError(t, w.ArchiveWithCommit(context.Background(), "boardgame", id, "archive: "+id, ""))

	// No subdir to remove — destroy must still succeed.
	require.NoError(t, w.DestroyArchivedWithCommit(context.Background(), "boardgame", id, "destroy: "+id, ""))

	_, err = os.Stat(filepath.Join(root, ArchiveDir, "boardgame", "no-attach-destroy-2024.md"))
	assert.True(t, os.IsNotExist(err))
}

// Round-trip test: archived attachment is reachable via the
// active-then-archive fallback in OpenAttachment, so an operator
// inspecting an archived entity's attachment still gets the bytes.
func TestOpenAttachment_FallsBackToArchive(t *testing.T) {
	t.Parallel()
	const id = "boardgame:archived-readable-2024"
	w, root, _ := seedActiveEntityWithAttachment(t, "boardgame", id, "thumb.jpg", "still-readable")

	require.NoError(t, w.ArchiveWithCommit(context.Background(), "boardgame", id, "archive: "+id, ""))

	r, err := NewReader(root)
	require.NoError(t, err)
	rc, manifest, _, err := r.OpenAttachment("boardgame", id, "thumb.jpg")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got := make([]byte, 32)
	n, _ := rc.Read(got)
	assert.Equal(t, "still-readable", string(got[:n]))
	assert.Equal(t, "thumb.jpg", manifest.Name)
}
