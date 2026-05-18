package actions

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/yaad-index/internal/config"
)

// TestWrapWorkflow_AlwaysWrapsNonEmpty: any non-empty input
// returns `[[<input>]]`. The caller is expected to know the
// input names a workflow file (workflows live under
// vault/workflows/), so no detection is performed.
func TestWrapWorkflow_AlwaysWrapsNonEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"linkedin-hiring-classify", "[[linkedin-hiring-classify]]"},
		{"github-pr-watch", "[[github-pr-watch]]"},
		{"single", "[[single]]"},
		{"with-hyphens-and-numbers-42", "[[with-hyphens-and-numbers-42]]"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, wrapWorkflow(c.in))
	}
}

// TestWrapWorkflow_EmptyPassesThrough: empty input returns
// empty — the caller can invoke unconditionally without
// pre-checking, and `[[]]` is not a useful Obsidian link.
func TestWrapWorkflow_EmptyPassesThrough(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", wrapWorkflow(""))
}

// canonicalKindsForTest returns a small registry used by the
// maybeWrapEntity tests — three operator-known kinds:
// boardgame, person, gmail.
func canonicalKindsForTest() map[string]config.CanonicalKindConfig {
	return map[string]config.CanonicalKindConfig{
		"boardgame": {},
		"person":    {},
		"gmail":     {},
	}
}

// TestMaybeWrapEntity_WrapsKnownKindShape: `<kind>:<id>` with
// kind in registry → wrapped in [[ ]].
func TestMaybeWrapEntity_WrapsKnownKindShape(t *testing.T) {
	t.Parallel()
	kinds := canonicalKindsForTest()
	cases := []struct{ in, want string }{
		{"boardgame:acme-game-2026", "[[boardgame:acme-game-2026]]"},
		{"person:alex-example", "[[person:alex-example]]"},
		{"gmail:msg-abc-123", "[[gmail:msg-abc-123]]"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, maybeWrapEntity(c.in, kinds))
	}
}

// TestMaybeWrapEntity_UnknownKindPassesThrough: `<kind>:<id>`
// where the kind is NOT in the registry passes through
// unchanged. Prevents wrapping `pkg:something` /
// `scheme:value` literals that share the format but aren't
// entity references.
func TestMaybeWrapEntity_UnknownKindPassesThrough(t *testing.T) {
	t.Parallel()
	kinds := canonicalKindsForTest()
	cases := []string{
		"package:something-not-an-entity",
		"http:example",
		"unknown-kind:42",
	}
	for _, in := range cases {
		assert.Equal(t, in, maybeWrapEntity(in, kinds),
			"unknown kind %q must pass through unwrapped", in)
	}
}

// TestMaybeWrapEntity_NoColonPassesThrough: a plain string
// with no colon separator is not entity-shaped — pass through.
func TestMaybeWrapEntity_NoColonPassesThrough(t *testing.T) {
	t.Parallel()
	kinds := canonicalKindsForTest()
	cases := []string{
		"plain-string",
		"hello world",
		"123",
		"text with no colons",
	}
	for _, in := range cases {
		assert.Equal(t, in, maybeWrapEntity(in, kinds))
	}
}

// TestMaybeWrapEntity_MultipleColonsPassesThrough: a string
// with multiple colons isn't a single `<kind>:<id>` shape —
// pass through. Guards against timestamp embeddings + RFC3339
// strings + ipv6 + nested namespaces.
func TestMaybeWrapEntity_MultipleColonsPassesThrough(t *testing.T) {
	t.Parallel()
	kinds := canonicalKindsForTest()
	cases := []string{
		"2026-05-18T19:00:00Z",  // RFC3339 timestamp
		"boardgame:x:y",          // accidental triple
		"::empty-kind-segment",
		"a:b:c:d",
	}
	for _, in := range cases {
		assert.Equal(t, in, maybeWrapEntity(in, kinds))
	}
}

// TestMaybeWrapEntity_EmptySegmentsPassThrough: the kind or
// id half being empty isn't a valid entity shape — pass
// through. `:trailing` and `leading:` shapes hit this guard.
func TestMaybeWrapEntity_EmptySegmentsPassThrough(t *testing.T) {
	t.Parallel()
	kinds := canonicalKindsForTest()
	cases := []string{
		":trailing-only",
		"leading-only:",
	}
	for _, in := range cases {
		assert.Equal(t, in, maybeWrapEntity(in, kinds))
	}
}

// TestMaybeWrapEntity_EmptyStringPassesThrough: empty input
// returns empty.
func TestMaybeWrapEntity_EmptyStringPassesThrough(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", maybeWrapEntity("", canonicalKindsForTest()))
}

// TestMaybeWrapEntity_EmptyRegistryPassesAll: with an empty
// registry, every input passes through — there are no known
// kinds to match against. Useful guard for tests that wire
// no canonical config.
func TestMaybeWrapEntity_EmptyRegistryPassesAll(t *testing.T) {
	t.Parallel()
	empty := map[string]config.CanonicalKindConfig{}
	assert.Equal(t, "boardgame:acme-game", maybeWrapEntity("boardgame:acme-game", empty),
		"empty registry → no kind matches → no wrap")
}
