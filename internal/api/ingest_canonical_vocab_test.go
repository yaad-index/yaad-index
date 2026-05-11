package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Per ADR-0013 §2 a prior PR: the parsed `canonical_kinds:` registry is
// wired through to needs_fill responses with two interactions:
//
// 1. Per-kind `instruction:` overrides global `fill_instruction:`
// for entities whose kind is in the registry. Resolution order:
// per-kind set → per-kind wins; per-kind unset → global wins;
// both unset → field omitted.
// 2. The full registry surfaces verbatim under
// `canonical_vocabulary` on every needs_fill response.
// Empty / nil registry → field omitted (omitempty).
//
// Tests cover the matrix on both response paths (fresh ingest +
// cache-hit) and verify filled responses do NOT carry these fields.

// newAPIWithCVRegistry wires both global fill_instruction AND the
// canonical_kinds registry. Either may be empty; `""` for instruction
// or nil for registry skips the corresponding option.
func newAPIWithCVRegistry(
	t *testing.T,
	instruction string,
	reg map[string]config.CanonicalKindConfig,
) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{}
	if instruction != "" {
		opts = append(opts, WithFillInstruction(instruction))
	}
	if reg != nil {
		opts = append(opts, WithCanonicalKindRegistry(reg))
	}
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), opts...)
}

// rawJSONResponse decodes an httptest.ResponseRecorder body as a
// generic map for omit/present assertions on top-level fields.
func rawJSONResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

// 1a. Per-kind set + global set → per-kind wins.
//
// The fixture-fresh path produces an entity with kind "boardgame".
// We register that kind in the CV registry with a per-kind instruction; the
// resolved instruction on the response should be the per-kind value.
func TestIngest_NeedsFill_InstructionResolution_PerKindWinsOverGlobal(t *testing.T) {
	t.Parallel()

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{"name": "Override gap."}),
			Instruction: config.InstructionFromString("PER_KIND_INSTRUCTION"),
		},
	}
	h := newAPIWithCVRegistry(t, "GLOBAL_INSTRUCTION", reg)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := rawJSONResponse(t, rec)
	assert.Equal(t, "PER_KIND_INSTRUCTION", got["instruction"])
}

// 1b. Per-kind unset + global set → global wins.
//
// The entity's kind has NO per-kind instruction; resolution falls back
// to the global `fill_instruction:`.
func TestIngest_NeedsFill_InstructionResolution_FallsBackToGlobal(t *testing.T) {
	t.Parallel()

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{"name": "x"}),
			// Instruction left empty — per-kind unset.
		},
	}
	h := newAPIWithCVRegistry(t, "GLOBAL_INSTRUCTION", reg)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONResponse(t, rec)
	assert.Equal(t, "GLOBAL_INSTRUCTION", got["instruction"])
}

// 1c. Per-kind set + global unset → per-kind wins.
func TestIngest_NeedsFill_InstructionResolution_PerKindAlone(t *testing.T) {
	t.Parallel()

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{"name": "x"}),
			Instruction: config.InstructionFromString("PER_KIND_ALONE"),
		},
	}
	h := newAPIWithCVRegistry(t, "", reg)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONResponse(t, rec)
	assert.Equal(t, "PER_KIND_ALONE", got["instruction"])
}

// 1d. Both unset → field absent.
func TestIngest_NeedsFill_InstructionResolution_BothUnsetOmits(t *testing.T) {
	t.Parallel()

	h := newAPIWithCVRegistry(t, "", nil)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONResponse(t, rec)
	_, has := got["instruction"]
	assert.False(t, has, "instruction omitted when both per-kind and global unset")
}

// 1e. Source-shape entity (kind NOT in registry) with global set →
// global wins, per-kind path is skipped (the kind isn't in the
// registry, so the lookup misses).
func TestIngest_NeedsFill_SourceEntityOnlySeesGlobalPath(t *testing.T) {
	t.Parallel()

	// Registry contains "person" — a different kind from the fixture's
	// "boardgame". The lookup should miss; global wins.
	reg := map[string]config.CanonicalKindConfig{
		"person": {
			Gaps: config.GapsFromMap(map[string]string{"name": "Full name."}),
			Instruction: config.InstructionFromString("PERSON_INSTRUCTION"),
		},
	}
	h := newAPIWithCVRegistry(t, "GLOBAL_FOR_SOURCE", reg)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONResponse(t, rec)
	assert.Equal(t, "GLOBAL_FOR_SOURCE", got["instruction"],
		"source entity (kind not in registry) should fall back to global")
}

// 2a. canonical_vocabulary populated → field appears verbatim.
func TestIngest_NeedsFill_CanonicalVocabulary_Populated(t *testing.T) {
	t.Parallel()

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {
			Gaps: config.GapsFromMap(map[string]string{
				"name": "Full name.",
				"summary": "One-paragraph summary.",
			}),
			Instruction: config.InstructionFromString("Skip if absent."),
		},
		"person": {
			Gaps: config.GapsFromMap(map[string]string{"name": "Person name."}),
			Instruction: config.InstructionFromString(""),
		},
	}
	h := newAPIWithCVRegistry(t, "", reg)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONResponse(t, rec)

	cv, ok := got["canonical_vocabulary"].(map[string]any)
	require.True(t, ok, "canonical_vocabulary must appear on response; body=%s", rec.Body.String())
	require.Len(t, cv, 2)

	personEntry, ok := cv["person"].(map[string]any)
	require.True(t, ok)
	personGaps := personEntry["gaps"].(map[string]any)
	assert.Equal(t, "Person name.", personGaps["name"])
	// Empty per-kind instruction should be omitempty-dropped from the wire.
	_, hasInst := personEntry["instruction"]
	assert.False(t, hasInst, "empty per-kind instruction should be omitted from wire")

	nffEntry, ok := cv["boardgame"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Skip if absent.", nffEntry["instruction"])
}

// 2b. canonical_vocabulary empty registry → field absent. AND the
// global instruction still surfaces correctly when the registry is
// nil (the cold-reviewer's a prior PR catch — separates the omit-CV signal from the
// instruction-resolution signal so a regression in either is caught
// independently).
func TestIngest_NeedsFill_CanonicalVocabulary_EmptyRegistry_Omitted(t *testing.T) {
	t.Parallel()

	h := newAPIWithCVRegistry(t, "GLOBAL_ONLY", nil)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONResponse(t, rec)
	_, has := got["canonical_vocabulary"]
	assert.False(t, has, "canonical_vocabulary omitted when registry is empty / nil")
	assert.Equal(t, "GLOBAL_ONLY", got["instruction"],
		"global instruction must still surface when registry is nil")
}

// 2c. Cache-hit needs_fill carries both fields too. Mirrors the
// fresh-ingest path's contract on respondFromCacheHit.
func TestIngest_CacheHit_NeedsFill_CarriesInstructionAndCanonicalVocabulary(t *testing.T) {
	t.Parallel()

	const entityKind = "cached-kind"
	const entityID = "cached-kind:seeded"
	const notation = "https://example.test/cv-cache-hit/seeded"

	reg := map[string]config.CanonicalKindConfig{
		entityKind: {
			Gaps: config.GapsFromMap(map[string]string{"name": "Seed name."}),
			Instruction: config.InstructionFromString("PER_KIND_CACHE_INSTRUCTION"),
		},
	}

	// Seed entity with open gaps so respondFromCacheHit takes the
	// needs_fill branch.
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	rdr, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	fetchedAt := &now
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: entityID,
		Kind: entityKind,
		Data: map[string]any{"title": "seeded"},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: entityID,
		Kind: entityKind,
		Plugin: "seed",
		Data: map[string]any{"title": "seeded"},
		Gaps: []string{"name"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: notation,
		EntityID: entityID,
		Kind: entityKind,
	}))

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, rdr),
		WithFillInstruction("GLOBAL_CACHE"),
		WithCanonicalKindRegistry(reg),
	)

	rec := postIngest(t, h, map[string]any{"url": notation})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := rawJSONResponse(t, rec)

	// Per-kind wins on resolution.
	assert.Equal(t, "PER_KIND_CACHE_INSTRUCTION", got["instruction"])
	// canonical_vocabulary surfaces verbatim.
	cv, ok := got["canonical_vocabulary"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, cv, entityKind)
}

// 2d. Filled-state response (state == "complete") does NOT carry
// instruction or canonical_vocabulary — the fields are needs_fill-only.
//
// Hits the cache-hit complete branch by seeding an entity with EMPTY
// gaps in the vault frontmatter; respondFromCacheHit's openGaps==0
// path encodes ingestCompleteResponse, which doesn't have the wire
// fields at all.
func TestIngest_CompleteCacheHit_DoesNotCarryNeedsFillFields(t *testing.T) {
	t.Parallel()

	const entityKind = "complete-kind"
	const entityID = "complete-kind:seeded"
	const notation = "https://example.test/cv-complete/seeded"

	reg := map[string]config.CanonicalKindConfig{
		entityKind: {
			Gaps: config.GapsFromMap(map[string]string{"name": "x"}),
			Instruction: config.InstructionFromString("would-not-leak"),
		},
	}

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	rdr, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	fetchedAt := &now
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: entityID,
		Kind: entityKind,
		Data: map[string]any{"title": "seeded"},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	// Empty Gaps list → respondFromCacheHit treats as complete.
	require.NoError(t, w.Write(&vault.Entity{
		ID: entityID,
		Kind: entityKind,
		Plugin: "seed",
		Data: map[string]any{"title": "seeded"},
		Gaps: nil,
		Provenance: []vault.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: notation,
		EntityID: entityID,
		Kind: entityKind,
	}))

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, rdr),
		WithFillInstruction("GLOBAL_LEAK_WOULD_FAIL"),
		WithCanonicalKindRegistry(reg),
	)

	rec := postIngest(t, h, map[string]any{"url": notation})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := rawJSONResponse(t, rec)
	assert.Equal(t, "complete", got["state"])
	_, hasInst := got["instruction"]
	assert.False(t, hasInst, "instruction must not appear on complete responses")
	_, hasCV := got["canonical_vocabulary"]
	assert.False(t, hasCV, "canonical_vocabulary must not appear on complete responses")
}

// resolveInstruction unit tests — direct coverage of the resolution
// helper. Saves having to spin a full HTTP handler for every matrix
// cell that's already covered above.
func TestResolveInstruction_Matrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		global string
		regKind string
		regInst string
		want string
	}{
		{"per_kind_wins_over_global", "person", "G", "person", "P", "P"},
		{"falls_back_to_global", "person", "G", "person", "", "G"},
		{"per_kind_alone", "person", "", "person", "P", "P"},
		{"both_unset", "person", "", "", "", ""},
		{"kind_not_in_registry_uses_global", "boardgame", "G", "person", "P", "G"},
		{"kind_not_in_registry_both_unset", "boardgame", "", "person", "P", ""},
		// The cold-reviewer's a prior PR catch: nil-registry + non-empty global must
		// return global. The empty-string regKind below leaves `reg`
		// at its nil default, so this exercises the
		// no-config-block-at-all branch (operator deploys without
		// `canonical_kinds:` but with `fill_instruction:`).
		{"nil_registry_with_global", "any-kind", "G", "", "", "G"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reg map[string]config.CanonicalKindConfig
			if tc.regKind != "" {
				reg = map[string]config.CanonicalKindConfig{
					tc.regKind: {
						Gaps: config.GapsFromMap(map[string]string{"x": "x"}),
						Instruction: config.InstructionFromString(tc.regInst),
					},
				}
			}
			got := resolveInstruction(tc.kind, tc.global, reg)
			assert.Equal(t, tc.want, got)
		})
	}
}
