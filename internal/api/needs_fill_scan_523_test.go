package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// TestNeedsFill_ScanBudgetNotEatenByNonCandidates pins the #523 fix: runs
// of never-surfacing rows that sort before the real candidates must not
// consume the per-request scan bound. The surfaceable candidate query
// drops them at the DB layer: pure-pointer stubs (nil data → "null") and
// all-filled rows. It KEEPS config-gap rows (NULL gap_state) so the list
// still surfaces them (the deliberate #439 list-vs-count divergence).
func TestNeedsFill_ScanBudgetNotEatenByNonCandidates(t *testing.T) {
	// Lower the per-request scan bound so the leading non-candidates would
	// exhaust it under the old bare query.
	prev := needsFillMaxCandidateScan
	needsFillMaxCandidateScan = 3
	t.Cleanup(func() { needsFillMaxCandidateScan = prev })

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	ctx := context.Background()

	// 5 pure-pointer stubs: nil Data → data column is the JSON literal
	// "null"; no vault file. They sort BEFORE the real rows by id and
	// outnumber the scan bound. The surfaceable query drops them.
	for i := 0; i < 5; i++ {
		require.NoError(t, st.SaveEntity(ctx, &store.Entity{
			ID:   fmt.Sprintf("aaa-stub:%02d", i),
			Kind: "boardgame",
			// Data nil → "null"; no GapState; no vault file.
		}))
	}

	// An all-filled row (data + gap_state with a filled entry): a
	// candidate under the old query that's scanned-then-skipped; the
	// surfaceable query drops it (gap_state present, no unfilled gap).
	filledAt := time.Now().UTC()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:       "bbb-filled:1",
		Kind:     "boardgame",
		Data:     map[string]any{"id": "filled"},
		GapState: map[string]store.GapStateEntry{"summary": {Source: "agent", FilledAt: &filledAt}},
	}))

	// A config-gap row: data present, NULL gap_state, an open vault gap
	// declared only in config. The list MUST still surface it.
	const cfgGapID = "yyy-configgap:1"
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   cfgGapID,
		Kind: "boardgame",
		Data: map[string]any{"id": "configgap"},
		// no GapState — gap_state column NULL.
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: cfgGapID, Kind: "boardgame", Source: []string{"seed/default"},
		Data: map[string]any{"id": "configgap"}, Gaps: []string{"summary"},
	}))

	// A real row with an unfilled gap + a vault file, sorting last.
	const realID = "zzz-real:1"
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID: realID, Kind: "boardgame",
		Data:     map[string]any{"id": "real"},
		GapState: map[string]store.GapStateEntry{"summary": {}},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: realID, Kind: "boardgame", Source: []string{"seed/default"},
		Data: map[string]any{"id": "real"}, Gaps: []string{"summary"},
	}))

	h := NewHandlerWithRegistry(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithCanonicalKindRegistry(nfRegistryWithBoardgameSummary()),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/needs-fill", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeNFResponse(t, rec)
	ids := make([]string, 0, len(got.Entities))
	for _, e := range got.Entities {
		ids = append(ids, e.ID)
	}
	// Both surfaceable rows appear despite the 6 non-candidates sorting
	// before them under a scan bound of 3 — proving the stubs + all-filled
	// row did not consume the budget.
	assert.Contains(t, ids, realID, "real unfilled entity must surface")
	assert.Contains(t, ids, cfgGapID, "config-gap (NULL gap_state) entity must still surface")
	assert.NotContains(t, ids, "bbb-filled:1", "all-filled entity must not surface")
}
