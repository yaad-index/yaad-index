package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// nfFilterSeed is one (id, kind, source) tuple for the #385 filter
// fixture. source is the vault `source[0]` slash-form; the plugin
// namespace (the bit before `/`) is what the source filter matches.
type nfFilterSeed struct {
	id, kind, source string
	// filled marks the seed as already-filled: its gap_state carries a
	// filled_at (so the gap-state-aware count excludes it) and its vault
	// Gaps list is empty. Used to pin that a source-matching but
	// non-gap-callable row doesn't inflate the source total (#439).
	filled bool
}

// nfFilterFixture seeds heterogeneous gap-callable entities across
// multiple kinds + source plugins, so the #385 kind/source filters can
// be exercised. Both `boardgame` and `person` carry a `summary` gap in
// the registry so every seed surfaces absent a filter.
func nfFilterFixture(t *testing.T, seeds []nfFilterSeed) http.Handler {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	now := time.Now().UTC()
	for _, s := range seeds {
		gapState := store.GapStateEntry{}
		vaultGaps := []string{"summary"}
		if s.filled {
			gapState = store.GapStateEntry{FilledAt: &now}
			vaultGaps = nil
		}
		require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
			ID:       s.id,
			Kind:     s.kind,
			Data:     map[string]any{"id": s.id},
			GapState: map[string]store.GapStateEntry{"summary": gapState},
			Provenance: []store.ProvenanceEntry{
				{Source: "seed:fixture", FetchedAt: &now, OK: true},
			},
		}))
		require.NoError(t, w.Write(&vault.Entity{
			ID:           s.id,
			Kind:         s.kind,
			Source:       []string{s.source},
			Data:         map[string]any{"id": s.id},
			Gaps:         vaultGaps,
			CleanContent: "stub for " + s.id,
		}))
	}

	reg := map[string]config.CanonicalKindConfig{
		"boardgame": {Gaps: config.GapsFromMap(map[string]string{"summary": "Game summary."})},
		"person":    {Gaps: config.GapsFromMap(map[string]string{"summary": "Person summary."})},
	}
	return NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithCanonicalKindRegistry(reg),
	)
}

func nfGet(t *testing.T, h http.Handler, target string) needsFillResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got needsFillResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

func nfEntityIDs(resp needsFillResponse) []string {
	ids := make([]string, 0, len(resp.Entities))
	for _, e := range resp.Entities {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	return ids
}

var nfFilterSeeds = []nfFilterSeed{
	{id: "boardgame:a", kind: "boardgame", source: "bgg/default"},
	{id: "boardgame:b", kind: "boardgame", source: "wikipedia/default"},
	{id: "person:x", kind: "person", source: "bgg/default"},
	{id: "person:y", kind: "person", source: "wikipedia/default"},
}

// TestNeedsFill_SourceTotal_ExcludesFilled pins the #439 review fix: the
// source total enumerates the gap-state-aware candidate set (matching
// CountGapCallableCandidates), so a source-matching but already-filled row
// is NOT counted — using the loose gap_call_done_at-only list would have
// over-counted it.
func TestNeedsFill_SourceTotal_ExcludesFilled(t *testing.T) {
	t.Parallel()
	seeds := append(append([]nfFilterSeed{}, nfFilterSeeds...),
		nfFilterSeed{id: "boardgame:filled", kind: "boardgame", source: "bgg/default", filled: true})
	h := nfFilterFixture(t, seeds)

	got := nfGet(t, h, "/v1/needs-fill?source=bgg")
	// Two unfilled bgg entities surface; the filled bgg row is gap-state
	// non-callable, so it inflates neither the list nor the total.
	assert.Equal(t, []string{"boardgame:a", "person:x"}, nfEntityIDs(got))
	assert.Equal(t, 2, got.Total, "filled source-matching row excluded from total (#439)")
}

// TestNeedsFill_NoFilters_Unchanged pins the no-params behavior: every
// gap-callable entity surfaces, total counts them all.
func TestNeedsFill_NoFilters_Unchanged(t *testing.T) {
	t.Parallel()
	h := nfFilterFixture(t, nfFilterSeeds)
	got := nfGet(t, h, "/v1/needs-fill")
	assert.Equal(t, []string{"boardgame:a", "boardgame:b", "person:x", "person:y"}, nfEntityIDs(got))
	assert.Equal(t, 4, got.Total)
}

// TestNeedsFill_KindFilter pins the store-level kind filter: only the
// requested kind surfaces, and total reflects it (exact, DB-side).
func TestNeedsFill_KindFilter(t *testing.T) {
	t.Parallel()
	h := nfFilterFixture(t, nfFilterSeeds)
	got := nfGet(t, h, "/v1/needs-fill?kind=boardgame")
	assert.Equal(t, []string{"boardgame:a", "boardgame:b"}, nfEntityIDs(got))
	assert.Equal(t, 2, got.Total, "total reflects the kind filter")
}

// TestNeedsFill_SourceFilter pins the vault-side source filter on the
// plugin namespace (PluginName — bit before `/` in source[0]) AND that
// total now reflects it (#439): the source-aware count scans the
// gap-callable set and counts vault source matches, so total drops to the
// filtered count instead of overcounting at the kind-unfiltered anchor.
func TestNeedsFill_SourceFilter(t *testing.T) {
	t.Parallel()
	h := nfFilterFixture(t, nfFilterSeeds)
	got := nfGet(t, h, "/v1/needs-fill?source=bgg")
	assert.Equal(t, []string{"boardgame:a", "person:x"}, nfEntityIDs(got))
	assert.Equal(t, 2, got.Total, "total reflects the source filter (#439): 2 bgg entities, not the kind-anchor 4")
}

// TestNeedsFill_KindAndSource_AND pins that the two filters compose with
// AND for both the entity list and total (#439): only person+wikipedia
// surfaces, and total counts exactly that intersection.
func TestNeedsFill_KindAndSource_AND(t *testing.T) {
	t.Parallel()
	h := nfFilterFixture(t, nfFilterSeeds)
	got := nfGet(t, h, "/v1/needs-fill?kind=person&source=wikipedia")
	assert.Equal(t, []string{"person:y"}, nfEntityIDs(got))
	assert.Equal(t, 1, got.Total, "total reflects kind=person AND source=wikipedia (#439): just person:y")
}

// TestNeedsFill_SourceFilter_NoMatch pins that an unmatched source yields
// zero entities AND total 0 (#439) — not the kind anchor.
func TestNeedsFill_SourceFilter_NoMatch(t *testing.T) {
	t.Parallel()
	h := nfFilterFixture(t, nfFilterSeeds)
	got := nfGet(t, h, "/v1/needs-fill?source=gmail")
	assert.Empty(t, got.Entities)
	assert.Equal(t, 0, got.Total, "unmatched source → total 0 (#439)")
}
