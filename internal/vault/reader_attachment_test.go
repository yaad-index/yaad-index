package vault

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// seedAttachmentAtMdPath hand-places an entity .md at an arbitrary
// on-disk path (any of the four layouts the resolver probes) plus its
// backing attachment under `<mdPath without .md>/attachments/`. Used to
// exercise the ct/ and #415-subfolder layouts that the Writer's default
// flat placement doesn't produce.
func seedAttachmentAtMdPath(t *testing.T, root, mdRelPath string, e *Entity, fileBody string) {
	t.Helper()
	mdAbs := filepath.Join(root, mdRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(mdAbs), 0o755))
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(mdAbs, b, 0o644))

	attachDir := filepath.Join(strings.TrimSuffix(mdAbs, ".md"), "attachments")
	require.NoError(t, os.MkdirAll(attachDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(attachDir, "thumb.jpg"), []byte(fileBody), 0o644))
}

// TestOpenAttachment_CanonicalLabelLayout pins #443: an entity living
// under the ADR-0021 `ct/<kind>/<slug>.md` layout must resolve its
// attachment. The old active-then-archive-only probe skipped ct/.
func TestOpenAttachment_CanonicalLabelLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seedAttachmentAtMdPath(t, root, filepath.Join("ct", "boardgame", "fixture-2024.md"),
		&Entity{
			ID:          "boardgame:fixture-2024",
			Kind:        "boardgame",
			Source:      []string{"fixture/default"},
			Data:        map[string]any{"name": "Fixture"},
			Attachments: []Attachment{{Name: "thumb.jpg", Kind: "image/jpeg", Path: "attachments/thumb.jpg"}},
		}, "ct-bytes")

	r, err := NewReader(root)
	require.NoError(t, err)
	rc, manifest, info, err := r.OpenAttachment("boardgame", "boardgame:fixture-2024", "thumb.jpg")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "ct-bytes", string(got))
	assert.Equal(t, "thumb.jpg", manifest.Name)
	assert.Equal(t, int64(len("ct-bytes")), info.Size())
}

// TestOpenAttachment_SubfolderLayout pins #443: a user-content entity in
// the #415 one-level-deep subfolder layout
// (`user-content/<subfolder>/<slug>.md`) must resolve its attachment.
func TestOpenAttachment_SubfolderLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seedAttachmentAtMdPath(t, root, filepath.Join("user-content", "journal", "note-abc.md"),
		&Entity{
			ID:          "user-content:note-abc",
			Kind:        "user-content",
			Source:      []string{"operator/default"},
			Data:        map[string]any{"title": "Note"},
			Attachments: []Attachment{{Name: "thumb.jpg", Kind: "image/jpeg", Path: "attachments/thumb.jpg"}},
		}, "sub-bytes")

	r, err := NewReader(root)
	require.NoError(t, err)
	rc, manifest, info, err := r.OpenAttachment("user-content", "user-content:note-abc", "thumb.jpg")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "sub-bytes", string(got))
	assert.Equal(t, "thumb.jpg", manifest.Name)
	assert.Equal(t, int64(len("sub-bytes")), info.Size())
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
