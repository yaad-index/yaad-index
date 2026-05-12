package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per ADR-0013 §3 / yaad-index a prior PR: ConfigHash() produces a
// deterministic SHA over the canonical_kinds + canonical_edge_types
// subset, surfaced as `config_hash` on /v1/cv-status. Tests pin
// determinism + change-detection invariants.

// hash is a small wrapper that keeps the test bodies readable —
// ConfigHash is a (string, error) two-return signature (the cold-reviewer's
// a prior PR catch on the error-shaped string contract); these tests
// only care about the success path.
func hash(t *testing.T, kinds map[string]CanonicalKindConfig, edges []string) string {
	t.Helper()
	h, err := ConfigHash(kinds, edges)
	require.NoError(t, err)
	return h
}

func TestConfigHash_DeterministicAcrossMapInsertionOrder(t *testing.T) {
	t.Parallel()
	regA := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "Full name."})},
		"boardgame": {Gaps: GapsFromMap(map[string]string{"name": "Game title."})},
	}
	regB := map[string]CanonicalKindConfig{
		"boardgame": {Gaps: GapsFromMap(map[string]string{"name": "Game title."})},
		"person": {Gaps: GapsFromMap(map[string]string{"name": "Full name."})},
	}
	hA := hash(t, regA, []string{"is_about"})
	hB := hash(t, regB, []string{"is_about"})
	assert.Equal(t, hA, hB, "map insertion order must not affect hash")
	assert.Len(t, hA, 16, "hash truncated to 16 hex chars (matches /v1/structure version width)")
}

func TestConfigHash_BumpsOnKindAdded(t *testing.T) {
	t.Parallel()
	regA := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "Full name."})},
	}
	regB := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "Full name."})},
		"city": {Gaps: GapsFromMap(map[string]string{"name": "City name."})},
	}
	hA := hash(t, regA, nil)
	hB := hash(t, regB, nil)
	assert.NotEqual(t, hA, hB, "adding a kind must bump hash")
}

func TestConfigHash_BumpsOnEdgeTypeAdded(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "Full name."})},
	}
	hA := hash(t, reg, []string{"is_about"})
	hB := hash(t, reg, []string{"is_about", "lives_in"})
	assert.NotEqual(t, hA, hB, "adding an edge type must bump hash")
}

func TestConfigHash_BumpsOnGapPromptChanged(t *testing.T) {
	t.Parallel()
	regA := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "Full name."})},
	}
	regB := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "First and last."})},
	}
	hA := hash(t, regA, nil)
	hB := hash(t, regB, nil)
	assert.NotEqual(t, hA, hB, "gap-prompt content change must bump hash")
}

func TestConfigHash_BumpsOnInstructionChanged(t *testing.T) {
	t.Parallel()
	regA := map[string]CanonicalKindConfig{
		"person": {
			Gaps: GapsFromMap(map[string]string{"name": "Full name."}),
			Instruction: InstructionFromString("Skip if absent."),
		},
	}
	regB := map[string]CanonicalKindConfig{
		"person": {
			Gaps: GapsFromMap(map[string]string{"name": "Full name."}),
			Instruction: InstructionFromString("Always include."),
		},
	}
	hA := hash(t, regA, nil)
	hB := hash(t, regB, nil)
	assert.NotEqual(t, hA, hB, "per-kind instruction change must bump hash")
}

// Edge-type slice is sorted before hashing — same contract as
// `/v1/structure`'s `version` field from a prior PR (yaad-index).
// Operator reorder of the YAML list does NOT bump the hash; both
// observability surfaces agree on what counts as a canonical-
// vocabulary config change. Pinned per yaad's a prior PR review note
// on cross-surface consistency.
func TestConfigHash_EdgeTypeReorderDoesNotBumpHash(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "x"})},
	}
	hA := hash(t, reg, []string{"is_about", "lives_in"})
	hB := hash(t, reg, []string{"lives_in", "is_about"})
	assert.Equal(t, hA, hB,
		"edge_types is sorted before hashing — operator reorder of the "+
			"YAML list must not bump the hash. Matches /v1/structure version's "+
			"pre-hash sort (a prior PR). If a future iteration introduces order-as-"+
			"semantics, this test updates deliberately alongside the structure "+
			"version's contract.")
}

func TestConfigHash_NilAndEmptyEquivalent(t *testing.T) {
	t.Parallel()
	hNil := hash(t, nil, nil)
	hEmpty := hash(t, map[string]CanonicalKindConfig{}, []string{})
	assert.Equal(t, hNil, hEmpty,
		"nil and empty inputs are observationally equivalent; "+
			"hash must reflect that")
	assert.Len(t, hNil, 16)
}

// Success path always returns nil error + 16-char hex output.
// Pins the contract so a future refactor that changes the return
// shape trips this test before it reaches the cv-status caller.
func TestConfigHash_SuccessReturnsNilError(t *testing.T) {
	t.Parallel()
	h, err := ConfigHash(map[string]CanonicalKindConfig{
		"person": {Gaps: GapsFromMap(map[string]string{"name": "x"})},
	}, []string{"is_about"})
	require.NoError(t, err)
	assert.Len(t, h, 16)
	// Hex sanity — every character is 0-9 or a-f.
	for _, c := range h {
		assert.True(t,
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"non-hex character %q in hash %q", c, h)
	}
}
