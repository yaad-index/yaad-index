package vault

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_AliasesMergesPluginEmittedWithSynthesized pins the
// a prior PR contract: plugin-emitted aliases on the Entity merge
// with the ADR-0011 title-synthesized one. Synthesized first
// (deterministic), plugin entries appended in input order,
// duplicates dropped.
func TestMarshal_AliasesMergesPluginEmittedWithSynthesized(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Susanna Clarke"},
		Aliases: []string{
			"S. Clarke",
			"author_of: Jonathan Strange & Mr Norrell",
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)

	// Order: synthesized first, then plugin-emitted in input order.
	assert.Contains(t, out, "aliases:\n  - Susanna Clarke\n  - S. Clarke\n  - 'author_of: Jonathan Strange & Mr Norrell'\n",
		"merged order must be synthesized first, then plugin entries in input order")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"Susanna Clarke",
		"S. Clarke",
		"author_of: Jonathan Strange & Mr Norrell",
	}, got.Aliases)
}

// TestMarshal_AliasesDedupesSynthesizedAgainstPlugin pins the
// dedupe rule: when the title-synthesized alias also appears in
// the plugin-emitted slice, only one entry survives (synthesized
// position wins).
func TestMarshal_AliasesDedupesSynthesizedAgainstPlugin(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Susanna Clarke"},
		Aliases: []string{
			// Plugin re-emits the same title — must dedupe.
			"Susanna Clarke",
			"S. Clarke",
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)

	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"Susanna Clarke", "S. Clarke"}, got.Aliases,
		"dedupe drops the second (plugin) occurrence; synthesized position is preserved")
}

// TestMarshal_AliasesEmptyPluginPreservesSynthesizedOnly pins the
// legacy behavior: plugin emits no aliases, only the title-
// synthesized entry surfaces.
func TestMarshal_AliasesEmptyPluginPreservesSynthesizedOnly(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Susanna Clarke"},
		// No Aliases — legacy shape.
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"Susanna Clarke"}, got.Aliases,
		"empty plugin slice must yield exactly the synthesized alias (current behavior preserved)")
}

// TestMarshal_AliasesEmptyEverythingDropsField pins the no-noise
// path: no synthesized entry (title equals slug) AND no plugin
// entries → frontmatter omits the field entirely.
func TestMarshal_AliasesEmptyEverythingDropsField(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "foo"}, // title==slug → no synth
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "aliases",
		"omitempty must drop the field entirely when both sources are empty")
}

// TestMarshal_AliasesPluginOnlyNoSynthesized pins the path where
// the synthesized entry is dropped (title equals slug) but the
// plugin still emits aliases — the plugin entries land alone, in
// their input order.
func TestMarshal_AliasesPluginOnlyNoSynthesized(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:foo",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "foo"}, // title==slug → no synth
		Aliases: []string{
			"Foo Bar Baz",
			"a: b",
		},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"Foo Bar Baz", "a: b"}, got.Aliases)
}

// TestMarshal_AliasesPluginEmptyStringEntriesIgnored pins the
// defensive trim: plugin-emitted empty strings are dropped on
// merge (they'd render as `aliases: - ”` which is noise).
func TestMarshal_AliasesPluginEmptyStringEntriesIgnored(t *testing.T) {
	t.Parallel()

	e := &Entity{
		ID: "wikipedia:susanna-clarke",
		Kind: "wikipedia-article",
		Source: []string{"wikipedia/default"},
		Data: map[string]any{"title": "Susanna Clarke"},
		Aliases: []string{"S. Clarke", "", "author: SC"},
	}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)
	require.True(t, strings.Contains(out, "aliases:"))
	got, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"Susanna Clarke", "S. Clarke", "author: SC"}, got.Aliases,
		"empty-string plugin entries must be dropped on merge")
}
