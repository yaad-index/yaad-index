package vault

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAttachmentVault writes one entity (active layout) with a
// single-attachment manifest plus the backing file. Returns the
// vault root path. Helper kept package-local to avoid bleeding test
// fixtures into the public Reader/Writer surface.
func seedAttachmentVault(t *testing.T, manifestPath, fileBody string) string {
	t.Helper()
	root := t.TempDir()
	w, err := NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&Entity{
		ID: "boardgame:fixture-2024",
		Kind: "boardgame",
		Source: []string{"fixture/default"},
		Data: map[string]any{"name": "Fixture"},
		Attachments: []Attachment{
			{Name: "thumb.jpg", Kind: "image/jpeg", Path: manifestPath},
		},
	}))
	dir := filepath.Join(root, "boardgame", "fixture-2024", "attachments")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "thumb.jpg"), []byte(fileBody), 0o644))
	return root
}

func TestOpenAttachment_HappyPath(t *testing.T) {
	t.Parallel()
	root := seedAttachmentVault(t, "attachments/thumb.jpg", "hello-bytes")

	r, err := NewReader(root)
	require.NoError(t, err)
	rc, manifest, info, err := r.OpenAttachment("boardgame", "boardgame:fixture-2024", "thumb.jpg")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello-bytes", string(got))
	assert.Equal(t, "thumb.jpg", manifest.Name)
	assert.Equal(t, "image/jpeg", manifest.Kind)
	assert.Equal(t, int64(len("hello-bytes")), info.Size())
}

// a prior PR cold-reviewer carry-over: a manifest entry whose Path traverses out
// of the entity dir must reject. The validation lives in
// validateAttachmentPath.
func TestOpenAttachment_ManifestPathTraversal_Rejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		"../../etc/passwd",
		"../sibling-entity/secret.md",
		"/absolute/path",
		"..",
		// Resolves to the entity directory itself — not an escape,
		// but os.Open on a directory would fail at the ServeContent
		// seek step. The cold-reviewer flagged this on a prior PR; rejecting upfront
		// gives a clean 400 instead of a confusing 500.
		".",
		"./",
		"foo/..",
	}
	for _, p := range cases {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			root := seedAttachmentVault(t, p, "ignored")
			r, err := NewReader(root)
			require.NoError(t, err)

			_, _, _, err = r.OpenAttachment("boardgame", "boardgame:fixture-2024", "thumb.jpg")
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidAttachmentName),
				"manifest path %q must reject as invalid; got %v", p, err)
		})
	}
}

func TestOpenAttachment_NameValidation(t *testing.T) {
	t.Parallel()
	root := seedAttachmentVault(t, "attachments/thumb.jpg", "ignored")
	r, err := NewReader(root)
	require.NoError(t, err)

	cases := []struct {
		name string
	}{
		{""},
		{"../escape"},
		{"sub/dir.jpg"},
		{".hidden"},
		{".."},
		{"./dotcurrent"},
	}
	for _, c := range cases {
		c := c
		t.Run("name="+c.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := r.OpenAttachment("boardgame", "boardgame:fixture-2024", c.name)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidAttachmentName),
				"name %q must reject; got %v", c.name, err)
		})
	}
}

func TestOpenAttachment_NotInManifest(t *testing.T) {
	t.Parallel()
	root := seedAttachmentVault(t, "attachments/thumb.jpg", "ignored")
	// Stash a sibling file that ISN'T in the manifest — must not be
	// reachable via OpenAttachment per the aggregate-root contract.
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "boardgame", "fixture-2024", "attachments", "stowaway.txt"),
		[]byte("ignored"), 0o644))

	r, err := NewReader(root)
	require.NoError(t, err)
	_, _, _, err = r.OpenAttachment("boardgame", "boardgame:fixture-2024", "stowaway.txt")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAttachmentNotInManifest))
}
