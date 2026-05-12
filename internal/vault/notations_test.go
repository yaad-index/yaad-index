package vault

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_NotationsEmittedInFrontmatter pins the wire shape: a
// non-empty Notations slice surfaces as a `notations:` YAML list
// in the frontmatter, in input order.
func TestMarshal_NotationsEmittedInFrontmatter(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Notations: []string{
			"https://en.wikipedia.org/wiki/Susanna_Clarke",
			"wikipedia: Susanna Clarke",
			"https://en.m.wikipedia.org/wiki/Susanna_Clarke",
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)
	assert.Contains(t, out, "notations:\n")
	assert.Contains(t, out, " - https://en.wikipedia.org/wiki/Susanna_Clarke\n")
	assert.Contains(t, out, " - 'wikipedia: Susanna Clarke'\n",
		"shorthand notation must be quoted (contains a colon → YAML scalar safety)")
	assert.Contains(t, out, " - https://en.m.wikipedia.org/wiki/Susanna_Clarke\n")
}

// TestMarshal_NotationsOmitemptyDropsField pins the no-noise rule:
// nil/empty Notations leaves no `notations:` key in the frontmatter
// at all (no `notations: []` artifact).
func TestMarshal_NotationsOmitemptyDropsField(t *testing.T) {
	t.Parallel()

	t.Run("nil slice", func(t *testing.T) {
		t.Parallel()
		e := &Entity{ID: "x:y", Kind: "x", Plugin: "p"}
		b, err := Marshal(e, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "notations")
	})
	t.Run("empty slice", func(t *testing.T) {
		t.Parallel()
		e := &Entity{ID: "x:y", Kind: "x", Plugin: "p", Notations: []string{}}
		b, err := Marshal(e, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "notations")
	})
}

// TestMarshal_NotationsRoundTrip — Marshal then Unmarshal preserves
// the Notations slice in order. End-to-end of the cache-key
// vault-roundtrip path that reindex relies on (per yaad-index issue
// a prior PR).
func TestMarshal_NotationsRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "Susanna Clarke"},
		Notations: []string{
			"https://en.wikipedia.org/wiki/Susanna_Clarke",
			"wikipedia: Susanna Clarke",
			"https://en.m.wikipedia.org/wiki/Susanna_Clarke",
		},
	}
	b, err := Marshal(original, nil)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(b), "notations:"),
		"marshal should emit notations: section")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, original.Notations, got.Notations,
		"Notations must round-trip in input order")
}

// TestWriter_NotationsEndToEnd — full Writer.Write → Reader.ReadByID
// roundtrip carries Notations through the on-disk file. Locks the
// production-path contract.
func TestWriter_NotationsEndToEnd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := NewWriter(root)
	require.NoError(t, err)

	require.NoError(t, w.Write(&Entity{
		ID: "wikipedia:tehran",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{"title": "Tehran"},
		Notations: []string{
			"https://en.wikipedia.org/wiki/Tehran",
			"wikipedia: Tehran",
		},
	}))

	r, err := NewReader(root)
	require.NoError(t, err)
	got, err := r.ReadByID("wikipedia-article", "wikipedia:tehran")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"https://en.wikipedia.org/wiki/Tehran",
		"wikipedia: Tehran",
	}, got.Notations)
}
