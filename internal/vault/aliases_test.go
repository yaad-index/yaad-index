package vault

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSynthesizeAliases_SourceShape exercises the source-shape path
// (Kind not in canonicalKinds). The alias is sourced from data.title.
//
// Spec: ADR-0011 §"Synthesis rule" — source-shape entities use
// `data.title`; nil/empty canonicalKinds defaults to source-shape.
func TestSynthesizeAliases_SourceShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		entity *Entity
		kinds []string
		expected []string
	}{
		{
			name: "title differs from slug → single-element alias",
			entity: &Entity{
				ID: "wikipedia:martin-wallace",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Data: map[string]any{"title": "Martin Wallace (game designer)"},
			},
			kinds: []string{"person", "city"},
			expected: []string{"Martin Wallace (game designer)"},
		},
		{
			name: "title equals slug → no alias",
			entity: &Entity{
				ID: "wikipedia:foo",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Data: map[string]any{"title": "foo"},
			},
			kinds: []string{"person"},
			expected: nil,
		},
		{
			name: "missing title → no alias",
			entity: &Entity{
				ID: "wikipedia:foo",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Data: map[string]any{"lang": "en"},
			},
			kinds: []string{"person"},
			expected: nil,
		},
		{
			name: "empty title → no alias",
			entity: &Entity{
				ID: "wikipedia:foo",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Data: map[string]any{"title": " "},
			},
			kinds: []string{"person"},
			expected: nil,
		},
		{
			name: "non-string title → no alias",
			entity: &Entity{
				ID: "wikipedia:foo",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Data: map[string]any{"title": 42},
			},
			kinds: []string{"person"},
			expected: nil,
		},
		{
			name: "nil canonicalKinds → still reads data.title",
			entity: &Entity{
				ID: "wikipedia:foo",
				Kind: "wikipedia-article",
				Plugin: "wikipedia",
				Data: map[string]any{"title": "Foo Bar"},
			},
			kinds: nil,
			expected: []string{"Foo Bar"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := synthesizeAliases(tc.entity, tc.kinds)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestSynthesizeAliases_CanonicalShape exercises the canonical-shape
// path (Kind ∈ canonicalKinds). The alias is sourced from data.name.
//
// Spec: ADR-0011 §"Synthesis rule" — canonical-shape entities use
// `data.name`. The kind is identified via the operator's
// canonical_kinds set (CanonicalGuard.EnabledKinds()).
func TestSynthesizeAliases_CanonicalShape(t *testing.T) {
	t.Parallel()

	canonicalKinds := []string{"person", "city", "country"}

	cases := []struct {
		name string
		entity *Entity
		expected []string
	}{
		{
			name: "name differs from slug → single-element alias",
			entity: &Entity{
				ID: "person:martin-wallace",
				Kind: "person",
				Plugin: "wikipedia",
				Data: map[string]any{"name": "Martin Wallace"},
			},
			expected: []string{"Martin Wallace"},
		},
		{
			name: "name equals slug → no alias",
			entity: &Entity{
				ID: "city:tehran",
				Kind: "city",
				Plugin: "wikipedia",
				Data: map[string]any{"name": "tehran"},
			},
			expected: nil,
		},
		{
			name: "missing name → no alias (data.title NOT consulted on canonical kind)",
			entity: &Entity{
				ID: "person:martin-wallace",
				Kind: "person",
				Plugin: "wikipedia",
				Data: map[string]any{"title": "Martin Wallace"},
			},
			expected: nil,
		},
		{
			name: "empty name → no alias",
			entity: &Entity{
				ID: "person:foo",
				Kind: "person",
				Plugin: "wikipedia",
				Data: map[string]any{"name": ""},
			},
			expected: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := synthesizeAliases(tc.entity, canonicalKinds)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestMarshal_AliasesEmittedInFrontmatter pins the on-disk shape of
// the aliases field: a single-element YAML list ordered between
// `plugin:` and `data:` per the schema, omitted entirely (no key in
// the file) when the synthesized list is empty.
func TestMarshal_AliasesEmittedInFrontmatter(t *testing.T) {
	t.Parallel()

	t.Run("source-shape with title emits aliases", func(t *testing.T) {
		t.Parallel()
		e := &Entity{
			ID: "wikipedia:martin-wallace",
			Kind: "wikipedia-article",
			Plugin: "wikipedia",
			Data: map[string]any{"title": "Martin Wallace (designer)"},
		}
		b, err := Marshal(e, []string{"person"})
		require.NoError(t, err)
		out := string(b)
		assert.Contains(t, out, "aliases:\n - Martin Wallace (designer)\n")
	})

	t.Run("canonical-shape with name emits aliases", func(t *testing.T) {
		t.Parallel()
		e := &Entity{
			ID: "person:martin-wallace",
			Kind: "person",
			Plugin: "wikipedia",
			Data: map[string]any{"name": "Martin Wallace"},
		}
		b, err := Marshal(e, []string{"person", "city"})
		require.NoError(t, err)
		out := string(b)
		assert.Contains(t, out, "aliases:\n - Martin Wallace\n")
	})

	t.Run("title equals slug → omitempty drops the key", func(t *testing.T) {
		t.Parallel()
		e := &Entity{
			ID: "wikipedia:foo",
			Kind: "wikipedia-article",
			Plugin: "wikipedia",
			Data: map[string]any{"title": "foo"},
		}
		b, err := Marshal(e, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "aliases:")
	})

	t.Run("missing title → omitempty drops the key", func(t *testing.T) {
		t.Parallel()
		e := &Entity{
			ID: "wikipedia:foo",
			Kind: "wikipedia-article",
			Plugin: "wikipedia",
		}
		b, err := Marshal(e, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "aliases:")
	})
}

// TestMarshal_AliasesRoundTrip verifies that an entity ingested with
// data.title → marshaled → reparsed retains the synthesized alias on
// the parsed Entity. End-to-end check anchoring the wikipedia-article
// flow named in ADR-0011's "Worked example".
func TestMarshal_AliasesRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Entity{
		ID: "wikipedia:martin-wallace",
		Kind: "wikipedia-article",
		Plugin: "wikipedia",
		Data: map[string]any{
			"title": "Martin Wallace (designer)",
			"lang": "en",
		},
	}
	b, err := Marshal(original, []string{"person", "city"})
	require.NoError(t, err)
	require.True(t, strings.Contains(string(b), "aliases:"),
		"marshal should emit aliases for source-shape title")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"Martin Wallace (designer)"}, got.Aliases)
}

// TestWriter_WithCanonicalKinds threads the option through Writer →
// Marshal so a canonical-shape entity reads `data.name` end-to-end.
// Locks the wiring contract added for the source issue — a regression that
// drops the option silently would still produce frontmatter, just
// without the alias field, so this test reads the on-disk file.
func TestWriter_WithCanonicalKinds(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, err := NewWriter(root, WithCanonicalKinds([]string{"person"}))
	require.NoError(t, err)

	require.NoError(t, w.Write(&Entity{
		ID: "person:martin-wallace",
		Kind: "person",
		Plugin: "wikipedia",
		Data: map[string]any{"name": "Martin Wallace"},
	}))

	r, err := NewReader(root)
	require.NoError(t, err)
	got, err := r.ReadByID("person", "person:martin-wallace")
	require.NoError(t, err)
	assert.Equal(t, []string{"Martin Wallace"}, got.Aliases)
}
