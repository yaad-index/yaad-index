package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// TestIngest_CacheHit_NeedsFill_RegistryPromptsSurface pins the
// yaad-index #4 behavior on the cache-hit needs_fill path: when
// the entity's kind has a registry entry with per-gap Descriptions,
// the wire `gaps` map carries those Descriptions verbatim — not the
// pre-#4 empty-string sentinel.
func TestIngest_CacheHit_NeedsFill_RegistryPromptsSurface(t *testing.T) {
	t.Parallel()

	reg := map[string]config.CanonicalKindConfig{
		"cached-kind": {
			Gaps: config.GapsFromMap(map[string]string{
				"summary": "Cache-hit summary prompt.",
				"tags":    "Cache-hit tags prompt.",
			}),
		},
	}
	h, _, _ := newCacheHitNeedsFillWithRegistry(t, reg)
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/cache-hit-needs-fill/seeded",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := rawJSONForRecorder(t, rec)
	require.Equal(t, "needs_fill", got["state"])

	gaps, ok := got["gaps"].(map[string]any)
	require.True(t, ok, "gaps must be a JSON object")
	assert.Equal(t, "Cache-hit summary prompt.", gaps["summary"],
		"registry Description surfaces as the AI prompt on the cache-hit wire")
	assert.Equal(t, "Cache-hit tags prompt.", gaps["tags"])
}

// TestIngest_CacheHit_NeedsFill_KindNotInRegistry_EmptyGaps pins
// the strict-mode acceptance: when the entity's kind isn't in the
// registry, the cache-hit path returns needs_fill state with an
// empty gaps map (not switched to complete; the entity surfaces
// but no fill work). Operator must enable the kind to surface
// prompts.
func TestIngest_CacheHit_NeedsFill_KindNotInRegistry_EmptyGaps(t *testing.T) {
	t.Parallel()

	// Empty registry — cached-kind has no entry. Same fixture
	// seeds entity with open gaps so respondFromCacheHit takes the
	// needs_fill branch (openGaps non-empty).
	h, _, _ := newCacheHitNeedsFillWithRegistry(t, nil)
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/cache-hit-needs-fill/seeded",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := rawJSONForRecorder(t, rec)
	assert.Equal(t, "needs_fill", got["state"],
		"kind-not-in-registry must NOT flip state to complete — agent gets entity, no work")

	gaps, ok := got["gaps"].(map[string]any)
	require.True(t, ok, "gaps must be a JSON object even when empty")
	assert.Empty(t, gaps,
		"kind not in registry → empty gaps map (yaad-index #4 strict mode)")
}

// newCacheHitNeedsFillWithRegistry mirrors
// newCacheHitNeedsFillAPI's setup but wires an optional
// canonical-kind registry so the #4 cache-hit branches can be
// exercised cleanly. Returns handler + store + notation URL.
func newCacheHitNeedsFillWithRegistry(t *testing.T, reg map[string]config.CanonicalKindConfig) (http.Handler, store.Store, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, vErr := vault.NewWriter(root)
	require.NoError(t, vErr)
	r, vErr := vault.NewReader(root)
	require.NoError(t, vErr)

	const entityID = "cached-kind:seeded"
	const notation = "https://example.test/cache-hit-needs-fill/seeded"
	now := time.Now().UTC()
	fetchedAt := &now
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   entityID,
		Kind: "cached-kind",
		Data: map[string]any{"title": "seeded"},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID:     entityID,
		Kind:   "cached-kind",
		Source: []string{"seed/default"},
		Data:   map[string]any{"title": "seeded"},
		Gaps:   []string{"summary", "tags"},
		Provenance: []vault.ProvenanceEntry{
			{Source: "seed:fixture", FetchedAt: fetchedAt, OK: true},
		},
	}))
	require.NoError(t, st.UpsertNotation(context.Background(), store.Notation{
		Notation: notation,
		EntityID: entityID,
		Kind:     "cached-kind",
	}))

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{WithVaultIO(w, r)}
	if reg != nil {
		opts = append(opts, WithCanonicalKindRegistry(reg))
	}
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), opts...), st, notation
}
