package vault

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestVault(t *testing.T) (*Writer, *Reader, string) {
	t.Helper()
	root := t.TempDir()
	w, err := NewWriter(root)
	require.NoError(t, err, "NewWriter")
	r, err := NewReader(root)
	require.NoError(t, err, "NewReader")
	return w, r, root
}

func TestNewWriter_RejectsRelativeRoot(t *testing.T) {
	t.Parallel()
	_, err := NewWriter("relative/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestNewWriter_RejectsMissingRoot(t *testing.T) {
	t.Parallel()
	_, err := NewWriter("/this/does/not/exist/test-vault")
	require.Error(t, err)
}

func TestNewWriter_RejectsFileAsRoot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	notDir := filepath.Join(tmp, "file")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o644))
	_, err := NewWriter(notDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestWriter_WriteCreatesFolderPerKindFile(t *testing.T) {
	t.Parallel()

	w, _, root := newTestVault(t)
	e := fixtureEntity(t)

	require.NoError(t, w.Write(e))

	want := filepath.Join(root, "wikipedia-article", "martin-wallace.md")
	info, err := os.Stat(want)
	require.NoError(t, err, "destination file should exist at %q", want)
	assert.True(t, info.Mode().IsRegular())
}

// TestWriter_WriteCanonicalLabelLandsUnderCT pins the
// auto-materialize layout per ADR-0021's amendment (yaad-index
// phase D): a canonical-label entity's vault file lands at
// `<root>/ct/<kind>/<slug>.md` rather than the per-kind default.
// Reader.ReadByID's chained fallback (active → canonical-label →
// archive) finds the file on subsequent reads.
func TestWriter_WriteCanonicalLabelLandsUnderCT(t *testing.T) {
	t.Parallel()

	w, r, root := newTestVault(t)
	e := &Entity{
		ID: "boardgame:brass-birmingham",
		Kind: "boardgame",
		Source: []string{"operator-fill/default"},
		Data: map[string]any{"rating": 8},
		Gaps: []string{"want", "played"},
	}
	require.NoError(t, w.WriteCanonicalLabelWithCommit(context.Background(), e, "fill: rating", ""))

	// File at the canonical-label path.
	wantCT := filepath.Join(root, "ct", "boardgame", "brass-birmingham.md")
	info, err := os.Stat(wantCT)
	require.NoError(t, err, "canonical-label vault file should exist at %q", wantCT)
	assert.True(t, info.Mode().IsRegular())

	// NO file at the per-kind default path.
	wantDefault := filepath.Join(root, "boardgame", "brass-birmingham.md")
	_, err = os.Stat(wantDefault)
	assert.True(t, os.IsNotExist(err),
		"canonical-label write must NOT land at the per-kind default %q", wantDefault)

	// Reader.ReadByID finds the file via the canonical-label
	// probe in its chained fallback.
	got, err := r.ReadByID("boardgame", "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, e.ID, got.ID)
	assert.Equal(t, e.Kind, got.Kind)
}

func TestWriter_WriteRoundTripsViaReader(t *testing.T) {
	t.Parallel()

	w, r, _ := newTestVault(t)
	want := fixtureEntity(t)
	require.NoError(t, w.Write(want))

	got, err := r.ReadByID(want.Kind, want.ID)
	require.NoError(t, err, "ReadByID")

	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Kind, got.Kind)
	assert.Equal(t, want.Source, got.Source)
	assert.Equal(t, want.Summary, got.Summary)
	assert.Equal(t, want.Tags, got.Tags)
	assert.Equal(t, want.Edges, got.Edges)
	require.Len(t, got.Notes, len(want.Notes))
	for i := range want.Notes {
		assert.True(t, want.Notes[i].Date.Equal(got.Notes[i].Date),
			"notes[%d].date", i)
		assert.Equal(t, want.Notes[i].Text, got.Notes[i].Text, "notes[%d].text", i)
	}
}

// TestWriter_AtomicWriteLeavesNoTempFileOnSuccess pins the post-condition
// of Write: after a successful return, the destination file exists and
// no `.<slug>.md.tmp-*` file remains in the kind directory.
func TestWriter_AtomicWriteLeavesNoTempFileOnSuccess(t *testing.T) {
	t.Parallel()

	w, _, root := newTestVault(t)
	e := fixtureEntity(t)
	require.NoError(t, w.Write(e))

	kindDir := filepath.Join(root, e.Kind)
	entries, err := os.ReadDir(kindDir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), "."),
			"kind dir should not contain temp files after success, found %q", entry.Name())
	}
	require.Len(t, entries, 1, "kind dir should have exactly the destination file")
	assert.Equal(t, "martin-wallace.md", entries[0].Name())
}

// TestWriter_AtomicWriteRejectsInvalidEntityIDPriorToWrite locks the
// other half of the atomic-write contract: validation errors prevent
// any file from appearing on disk. Together with the success-path test
// this proves the destination is never partially written.
func TestWriter_AtomicWriteRejectsInvalidEntityIDPriorToWrite(t *testing.T) {
	t.Parallel()

	w, _, root := newTestVault(t)
	bad := &Entity{ID: "no-colon", Kind: "wikipedia-article", Source: []string{"wikipedia/default"}}

	err := w.Write(bad)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEntityID)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "no files should have been created on validation failure")
}

// TestWriter_AtomicSwapVisibility uses the os.Rename guarantee on the
// same filesystem: at no observable point should the destination file
// exist but be empty / partially written. We can't easily race the
// rename in-process, so the assertion here is structural — the body
// is wholly written before rename, so any reader observing the
// destination sees the complete file.
func TestWriter_AtomicSwapVisibility(t *testing.T) {
	t.Parallel()

	w, r, root := newTestVault(t)
	e := fixtureEntity(t)
	require.NoError(t, w.Write(e))

	dst := filepath.Join(root, e.Kind, "martin-wallace.md")
	contents, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.NotEmpty(t, contents, "destination must be non-empty after Write")
	require.True(t, strings.HasPrefix(string(contents), "---\n"),
		"destination must start with frontmatter delimiter")
	require.Contains(t, string(contents), "\n---\n",
		"destination must contain closing frontmatter delimiter")

	got, err := r.ReadFile(dst)
	require.NoError(t, err, "destination must parse as a complete entity")
	assert.Equal(t, e.ID, got.ID)
}

// TestWriter_OverwritesExistingDestination verifies a second write to
// the same entity replaces the file content (re-ingest semantics from
// ADR-0008: existing entity updated in place).
func TestWriter_OverwritesExistingDestination(t *testing.T) {
	t.Parallel()

	w, r, _ := newTestVault(t)
	e := fixtureEntity(t)
	require.NoError(t, w.Write(e), "first write")

	e.Summary = "Updated summary on re-ingest."
	require.NoError(t, w.Write(e), "second write")

	got, err := r.ReadByID(e.Kind, e.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated summary on re-ingest.", got.Summary)
}

func TestWriter_RejectsSlugWithPathSeparators(t *testing.T) {
	t.Parallel()

	w, _, _ := newTestVault(t)
	bad := &Entity{ID: "wikipedia:foo/bar", Kind: "wikipedia-article", Source: []string{"wikipedia/default"}}
	err := w.Write(bad)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEntityID)
}

// TestWriter_ConcurrentWritesToDifferentEntitiesAreSafe — writers on
// different entities don't interfere. (Per the type comment, same-entity
// concurrency is undefined; we only assert the safe case here.)
func TestWriter_ConcurrentWritesToDifferentEntitiesAreSafe(t *testing.T) {
	t.Parallel()

	w, r, root := newTestVault(t)
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			e := &Entity{
				ID: "wikipedia:concurrent-" + string(rune('a'+i)),
				Kind: "wikipedia-article",
				Source: []string{"wikipedia/default"},
			}
			assert.NoError(t, w.Write(e))
		}()
	}
	wg.Wait()

	entries, err := os.ReadDir(filepath.Join(root, "wikipedia-article"))
	require.NoError(t, err)
	require.Len(t, entries, n)
	for i := 0; i < n; i++ {
		got, err := r.ReadByID("wikipedia-article", "wikipedia:concurrent-"+string(rune('a'+i)))
		require.NoError(t, err)
		assert.Equal(t, "wikipedia:concurrent-"+string(rune('a'+i)), got.ID)
	}
}

func TestReader_ReadByIDMissingReturnsNotExist(t *testing.T) {
	t.Parallel()

	_, r, _ := newTestVault(t)
	_, err := r.ReadByID("wikipedia-article", "wikipedia:nope")
	require.Error(t, err)
	assert.True(t, IsNotExist(err) || errors.Is(err, fs.ErrNotExist),
		"want os.ErrNotExist via IsNotExist, got %v", err)
}

func TestReader_ReadByIDRejectsInvalidID(t *testing.T) {
	t.Parallel()

	_, r, _ := newTestVault(t)
	_, err := r.ReadByID("wikipedia-article", "no-colon")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEntityID)
}

func TestReader_ReadFileMalformedReturnsError(t *testing.T) {
	t.Parallel()

	_, r, root := newTestVault(t)
	bad := filepath.Join(root, "bogus.md")
	require.NoError(t, os.WriteFile(bad, []byte("no frontmatter here"), 0o644))

	_, err := r.ReadFile(bad)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMalformedFrontmatter), "want ErrMalformedFrontmatter, got %v", err)
}
