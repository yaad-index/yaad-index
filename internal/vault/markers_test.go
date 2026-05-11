package vault

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMergePluginBody_FirstWriteEmpty pins ADR-0015 §3's first-write
// path: existing body is empty, plugin content gets wrapped in the
// marker pair.
func TestMergePluginBody_FirstWriteEmpty(t *testing.T) {
	t.Parallel()

	const plugin = "# Brass: Birmingham\n\n![thumb](brass-birmingham-2018.thumb.jpg)\n\nAn economic strategy game."
	got, err := MergePluginBody("", plugin)
	require.NoError(t, err)
	assert.Empty(t, got.PriorMarkers, "first-write empty body must report no prior markers")

	want := PluginBodyStartMarker + "\n" + plugin + "\n" + PluginBodyEndMarker
	assert.Equal(t, want, got.Body)
}

// TestMergePluginBody_FirstWriteExistingUnmarkedBody covers ADR-0015
// §4 first bullet: operator hand-wrote content before the plugin
// started emitting body. Plugin region appears at the END so the
// operator's existing content stays where they put it.
func TestMergePluginBody_FirstWriteExistingUnmarkedBody(t *testing.T) {
	t.Parallel()

	const operator = "## My playthrough notes\nFirst time playing 2025-04-15.\n"
	const plugin = "# Brass: Birmingham\n\nAn economic strategy game."

	got, err := MergePluginBody(operator, plugin)
	require.NoError(t, err)
	assert.Empty(t, got.PriorMarkers, "no prior markers detected → empty PriorMarkers")

	// Operator content first, then plugin region.
	assert.True(t, strings.HasPrefix(got.Body, operator),
		"operator content must come first; got %q", got.Body)
	assert.Contains(t, got.Body, PluginBodyStartMarker)
	assert.Contains(t, got.Body, PluginBodyEndMarker)
	// And the plugin region is appended below operator content.
	startIdx := strings.Index(got.Body, PluginBodyStartMarker)
	assert.Greater(t, startIdx, len(operator)-1,
		"plugin marker must appear AFTER operator content")
}

// TestMergePluginBody_FirstWriteExistingNoTrailingNewline pins the
// separator-newline behavior: when existing body lacks a trailing
// newline, the merge inserts one before the plugin region so the
// markers don't fuse with operator content.
func TestMergePluginBody_FirstWriteExistingNoTrailingNewline(t *testing.T) {
	t.Parallel()

	const operator = "no trailing newline" // intentionally no \n
	const plugin = "plugin body"

	got, err := MergePluginBody(operator, plugin)
	require.NoError(t, err)

	// The marker should NOT be on the same line as operator content.
	startIdx := strings.Index(got.Body, PluginBodyStartMarker)
	require.Greater(t, startIdx, 0)
	assert.Equal(t, byte('\n'), got.Body[startIdx-1],
		"marker must be preceded by a newline even when operator content lacks one")
}

// TestMergePluginBody_ReingestCleanMarkers covers the happy re-
// ingest path: prior markers are well-formed, plugin re-emits new
// content, daemon replaces only the between-region. Operator content
// in `before` and `after` is preserved verbatim, byte-for-byte.
func TestMergePluginBody_ReingestCleanMarkers(t *testing.T) {
	t.Parallel()

	const before = "# pre-plugin operator note\n\nI added this BEFORE re-ingest.\n\n"
	const oldPlugin = "# Brass: Birmingham (OLD)\n\nold description.\n"
	const newPlugin = "# Brass: Birmingham (NEW)\n\n![thumb](brass-birmingham-2018.thumb.jpg)\n\nfresh description."
	const after = "\n\n## My playthrough notes\nRound 3 was the killer.\n"

	existing := before +
		PluginBodyStartMarker + "\n" + oldPlugin + "\n" + PluginBodyEndMarker +
		after

	got, err := MergePluginBody(existing, newPlugin)
	require.NoError(t, err)
	assert.Equal(t, "clean", got.PriorMarkers,
		"well-formed re-ingest must report PriorMarkers=clean")

	// Operator content preserved at both ends.
	assert.True(t, strings.HasPrefix(got.Body, before),
		"before-region must be preserved byte-for-byte at the start")
	assert.True(t, strings.HasSuffix(got.Body, after),
		"after-region must be preserved byte-for-byte at the end")

	// New plugin content lands inside the marker pair.
	assert.Contains(t, got.Body, newPlugin)
	assert.NotContains(t, got.Body, oldPlugin,
		"old plugin content must be fully replaced by the new emission")

	// Exactly one marker pair (no duplicates from the splice).
	assert.Equal(t, 1, strings.Count(got.Body, PluginBodyStartMarker),
		"exactly one start marker after merge")
	assert.Equal(t, 1, strings.Count(got.Body, PluginBodyEndMarker),
		"exactly one end marker after merge")
}

// TestMergePluginBody_MalformedFallbacks covers ADR-0015 §4: prior
// body has malformed markers (start without end, end without start,
// end before start). All three fall back to wholesale-replace + a
// non-empty PriorMarkers reason for the caller to log WARN.
func TestMergePluginBody_MalformedFallbacks(t *testing.T) {
	t.Parallel()

	const plugin = "fresh plugin content"

	cases := []struct {
		name string
		existing string
		wantReason string
	}{
		{
			name: "start_only_no_end",
			existing: "before\n" + PluginBodyStartMarker + "\nstale plugin\n",
			wantReason: "start_only_no_end",
		},
		{
			name: "end_only_no_start",
			existing: "before\nstale plugin\n" + PluginBodyEndMarker + "\nafter\n",
			wantReason: "end_only_no_start",
		},
		{
			name: "end_before_start",
			existing: PluginBodyEndMarker + "\nmiddle\n" + PluginBodyStartMarker + "\n",
			wantReason: "end_before_start",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := MergePluginBody(tc.existing, plugin)
			require.NoError(t, err)
			assert.Equal(t, tc.wantReason, got.PriorMarkers,
				"malformed case %s must report the reason", tc.name)

			// Fallback is wholesale-replace: body is just the wrapped plugin.
			want := PluginBodyStartMarker + "\n" + plugin + "\n" + PluginBodyEndMarker
			assert.Equal(t, want, got.Body)
		})
	}
}

// TestMergePluginBody_RejectsPluginEmittedMarkers covers ADR-0015
// §4 last bullet: plugin content containing the literal start or
// end marker substring is rejected with ErrPluginEmittedMarker.
// Fail-fast surfaces the plugin bug rather than silently mangling.
func TestMergePluginBody_RejectsPluginEmittedMarkers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		plugin string
	}{
		{"start_marker_in_plugin", "title\n" + PluginBodyStartMarker + "\nbody"},
		{"end_marker_in_plugin", "title\nbody\n" + PluginBodyEndMarker},
		{"both_markers_in_plugin", PluginBodyStartMarker + "\nfoo\n" + PluginBodyEndMarker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := MergePluginBody("", tc.plugin)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrPluginEmittedMarker),
				"want ErrPluginEmittedMarker, got %v", err)
		})
	}
}

// TestMergePluginBody_MultipleMarkerPairs covers ADR-0015 §4: the
// daemon picks the first pair, treats anything after the first end
// marker as `after` (preserved). Single-block contract for v1.
//
// Construction: a stray second pair appears AFTER the first end
// marker, simulating either a daemon bug or operator hand-edit
// corruption. The merge should:
// - splice between the FIRST pair (replacing only its inner content)
// - keep the second pair verbatim in `after`
// - log "clean" since the first pair was well-formed; the second
// pair survives as preserved content the operator can clean up.
func TestMergePluginBody_MultipleMarkerPairs(t *testing.T) {
	t.Parallel()

	const newPlugin = "fresh"
	const before = "before\n"
	const oldFirst = "first stale\n"
	// Stray second pair in `after`. Treated as preserved operator
	// content per the single-block contract; the daemon doesn't
	// strip them.
	const strayAfter = "\nstray content\n" +
		PluginBodyStartMarker + "\nstray plugin\n" + PluginBodyEndMarker +
		"\ntrailing\n"

	existing := before +
		PluginBodyStartMarker + "\n" + oldFirst + "\n" + PluginBodyEndMarker +
		strayAfter

	got, err := MergePluginBody(existing, newPlugin)
	require.NoError(t, err)
	assert.Equal(t, "clean", got.PriorMarkers)
	assert.True(t, strings.HasSuffix(got.Body, strayAfter),
		"stray second pair must be preserved verbatim in after-region")
	assert.Contains(t, got.Body, newPlugin)
	assert.NotContains(t, got.Body, oldFirst, "first pair's inner content replaced")
}

// TestMergePluginBody_UnicodeInPluginRegion pins byte-safety: a
// plugin emitting non-ASCII content (BGG alternate-language titles,
// Wikipedia article excerpts) must round-trip without corruption.
func TestMergePluginBody_UnicodeInPluginRegion(t *testing.T) {
	t.Parallel()

	const plugin = "# Бирмингем\n\n经济战略游戏。\n\nDescription with — em-dash and 🎲 emoji."
	got, err := MergePluginBody("", plugin)
	require.NoError(t, err)
	assert.Contains(t, got.Body, plugin,
		"unicode plugin content must round-trip byte-for-byte")
}
