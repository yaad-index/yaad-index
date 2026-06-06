package attachments

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCopyCapped pins the size-cap boundary: a source within the cap
// copies in full, a source exactly at the cap succeeds, and a source
// over the cap returns ErrAttachmentTooLarge.
func TestCopyCapped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, copyCapped(&buf, strings.NewReader("hello"), 10),
		"5 bytes under a 10-byte cap copies fine")
	assert.Equal(t, "hello", buf.String(), "all bytes written when within cap")

	buf.Reset()
	require.NoError(t, copyCapped(&buf, strings.NewReader(strings.Repeat("y", 8)), 8),
		"exactly-at-cap is allowed")
	assert.Len(t, buf.String(), 8)

	buf.Reset()
	err := copyCapped(&buf, strings.NewReader(strings.Repeat("x", 20)), 8)
	require.ErrorIs(t, err, ErrAttachmentTooLarge, "20 bytes over an 8-byte cap is refused")
}

// TestDispatch_FileScheme_OverSizeCap verifies the cap is wired into the
// file:// handler end-to-end: a staged source larger than the dispatcher's
// max is skipped (fail-soft per ADR-0014 §5 — empty FetchAttachments, no
// dispatch error) and the partial dest file is removed.
func TestDispatch_FileScheme_OverSizeCap(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	src := filepath.Join(stagingDir, "oversized.bin")
	require.NoError(t, os.WriteFile(src, []byte(strings.Repeat("x", 20)), 0o644),
		"stage a 20-byte source")

	d, err := New(stagingDir, WithMaxAttachmentBytes(8))
	require.NoError(t, err)

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "doc", URI: "file://" + src, Extension: "bin"},
		},
		VaultRoot: vaultRoot,
		Kind:      "boardgame",
		LocalID:   "test-2099",
	})
	require.NoError(t, err, "over-cap attachment is fail-soft, not a dispatch error")
	assert.Empty(t, res.FetchAttachments, "over-cap attachment is skipped, not recorded")

	dest := filepath.Join(vaultRoot, "boardgame", "test-2099", "attachments", "doc.bin")
	_, statErr := os.Stat(dest)
	assert.True(t, os.IsNotExist(statErr), "partial dest is removed on over-cap copy")
}
