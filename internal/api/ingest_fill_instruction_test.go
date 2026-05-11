package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Per ADR-0013 §2 a prior PR: when `fill_instruction` is unset on the server
// config, the wire field is absent (omitempty) on every needs_fill
// response. When set, it appears verbatim — byte-identical to the
// configured string. Tested across both the fresh-fetch needs_fill
// path (fixture sentinel) and the cache-hit needs_fill path
// (notation cache + open gaps in vault frontmatter).

func newAPIWithFillInstruction(t *testing.T, instruction string) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{}
	if instruction != "" {
		opts = append(opts, WithFillInstruction(instruction))
	}
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), opts...)
}

// rawJSONForRecorder decodes an httptest.ResponseRecorder body as a
// generic map so tests can assert key presence/absence on the wire.
// Decoding into the typed `ingestNeedsFillResponse` doesn't tell us
// whether `instruction` was emitted vs zero-value-omitted.
func rawJSONForRecorder(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got),
		"decode response body as raw map")
	return got
}

func TestIngest_NeedsFill_FillInstructionAbsent_OmittedFromWire(t *testing.T) {
	t.Parallel()

	h := newAPIWithFillInstruction(t, "")
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())

	got := rawJSONForRecorder(t, rec)
	assert.Equal(t, "needs_fill", got["state"])
	_, has := got["instruction"]
	assert.False(t, has, "instruction must be omitted from wire when fill_instruction unset; got body=%s", rec.Body.String())
}

func TestIngest_NeedsFill_FillInstructionSet_PassedThroughVerbatim(t *testing.T) {
	t.Parallel()

	const instruction = "Extract canonical companions only when the article's text " +
		"clearly supports them. Skip-if-absent on every gap."
	h := newAPIWithFillInstruction(t, instruction)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)

	got := rawJSONForRecorder(t, rec)
	assert.Equal(t, instruction, got["instruction"], "instruction must be verbatim from config")
}

// Cache-hit needs_fill path — assert the same omit-vs-verbatim
// contract on the cache-served response. Mirrors the cache-hit
// pattern in ingest_notation_cache_test.go but seeds an entity
// with open gaps so respondFromCacheHit takes the needs_fill
// branch (per ingest.go::respondFromCacheHit's openGaps check).
func TestIngest_CacheHit_NeedsFill_FillInstructionAbsent_OmittedFromWire(t *testing.T) {
	t.Parallel()
	h, _, _ := newCacheHitNeedsFillAPI(t, "")
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/cache-hit-needs-fill/seeded",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := rawJSONForRecorder(t, rec)
	assert.Equal(t, "needs_fill", got["state"])
	_, has := got["instruction"]
	assert.False(t, has, "instruction omitted on cache-hit needs_fill when fill_instruction unset")
}

func TestIngest_CacheHit_NeedsFill_FillInstructionSet_PassedThroughVerbatim(t *testing.T) {
	t.Parallel()

	const instruction = "Treat every gap as best-effort; agent fills only what " +
		"the source clearly supports."
	h, _, _ := newCacheHitNeedsFillAPI(t, instruction)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/cache-hit-needs-fill/seeded",
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	got := rawJSONForRecorder(t, rec)
	assert.Equal(t, instruction, got["instruction"], "instruction verbatim on cache-hit needs_fill")
}

// newCacheHitNeedsFillAPI seeds an entity in the DB + vault with
// open gaps, registers a notation pointing at it, and returns a
// handler whose subsequent ingest of that notation hits the cache
// → respondFromCacheHit's needs_fill branch.
//
// Empty instruction → no WithFillInstruction option (validates
// the omitted-field default).
func newCacheHitNeedsFillAPI(t *testing.T, instruction string) (http.Handler, store.Store, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, vErr := vault.NewWriter(root)
	require.NoError(t, vErr)
	r, vErr := vault.NewReader(root)
	require.NoError(t, vErr)

	// Seed the entity in both DB and vault. The vault frontmatter
	// carries the open `gaps:` list that respondFromCacheHit reads
	// from to decide complete vs needs_fill.
	const entityID = "cached-kind:seeded"
	const notation = "https://example.test/cache-hit-needs-fill/seeded"
	now := time.Now().UTC()
	fetchedAt := &now
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: entityID,
		Kind: "cached-kind",
		Data: map[string]any{"title": "seeded"},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: entityID,
		Kind: "cached-kind",
		Plugin: "seed",
		Data: map[string]any{"title": "seeded"},
		Gaps: []string{"summary", "tags"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))

	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: notation,
		EntityID: entityID,
		Kind: "cached-kind",
	}))

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{WithVaultIO(w, r)}
	if instruction != "" {
		opts = append(opts, WithFillInstruction(instruction))
	}
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), opts...), st, notation
}

// Sanity: every needs_fill response carries the wire shape the
// agent expects (`state`, `gaps`) — assert the new field doesn't
// silently displace others. Belt-and-suspenders against an
// accidental marshal-tag regression.
func TestIngest_NeedsFill_WireShapeStillCarriesGapsAndState(t *testing.T) {
	t.Parallel()

	h := newAPIWithFillInstruction(t, "anything goes")
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	body := rec.Body.String()
	assert.True(t, strings.Contains(body, `"state":"needs_fill"`))
	assert.True(t, strings.Contains(body, `"gaps":`))
	assert.True(t, strings.Contains(body, `"instruction":"anything goes"`))
}
