package canonical

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestParseDayID_HappyPath pins the canonical shape: `day:YYYY-MM-DD`
// returns the slug portion + ok=true.
func TestParseDayID_HappyPath(t *testing.T) {
	t.Parallel()
	slug, ok := ParseDayID("day:2026-11-11")
	assert.True(t, ok)
	assert.Equal(t, "2026-11-11", slug)
}

// TestParseDayID_RejectsMalformedShape covers the rejection set
// at the SHAPE level — missing prefix, wrong digit-count, wrong
// order, trailing characters. Calendar-validity is deliberately
// NOT enforced: `day:2026-13-99` matches the regex and returns
// ok=true (see ParseDayID's godoc for the rationale — operators
// who type a malformed date get a malformed day entity; the
// anchor still works).
func TestParseDayID_RejectsMalformedShape(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"2026-11-11",       // missing prefix
		"day:2026-11",      // missing day
		"day:2026-11-11Z",  // trailing zone
		"day:2026-11-11 ",  // trailing space
		"day:11-11-2026",   // wrong order
		"day:",             // empty slug
		"week:2026-W45",    // wrong kind
	}
	for _, c := range cases {
		_, ok := ParseDayID(c)
		assert.False(t, ok, "ParseDayID(%q) must reject", c)
	}
}

// TestScanDayRefs_DeterministicOrder pins that the emitted ref
// list is sorted by (field, day-id). Stable ordering matters for
// reproducible edge-emit downstream.
func TestScanDayRefs_DeterministicOrder(t *testing.T) {
	t.Parallel()
	data := map[string]any{
		"zeta_field":     "day:2026-11-11",
		"alpha_field":    "day:2026-11-11",
		"middle":         "day:2025-01-01",
		"not_a_day":      "this is just text",
		"number":         42,
		"absent":         nil,
	}
	got := ScanDayRefs(data)
	want := []DayRef{
		{Field: "alpha_field", DayID: "day:2026-11-11"},
		{Field: "middle", DayID: "day:2025-01-01"},
		{Field: "zeta_field", DayID: "day:2026-11-11"},
	}
	assert.Equal(t, want, got)
}

// TestScanDayRefs_EmptyData returns nil.
func TestScanDayRefs_EmptyData(t *testing.T) {
	t.Parallel()
	assert.Nil(t, ScanDayRefs(nil))
	assert.Nil(t, ScanDayRefs(map[string]any{}))
}

// TestScanDayRefs_SkipsNonStringValues pins that non-string values
// (numbers, nested maps, slices) don't trip the scanner. Nested
// walks are out-of-scope for cut 2.
func TestScanDayRefs_SkipsNonStringValues(t *testing.T) {
	t.Parallel()
	data := map[string]any{
		"plain":    "day:2026-11-11",
		"int":      99,
		"slice":    []any{"day:2025-01-01"},
		"nested":   map[string]any{"buried": "day:2024-06-06"},
	}
	got := ScanDayRefs(data)
	require.Len(t, got, 1)
	assert.Equal(t, "plain", got[0].Field)
}

// TestResolveDayEdgeType_PluginOverridesWin pins that a declared
// override beats the baseline.
func TestResolveDayEdgeType_PluginOverridesWin(t *testing.T) {
	t.Parallel()
	dateFields := map[string]string{
		"deadline":   "due_on",
		"happens_at": "occurred_on",
	}
	assert.Equal(t, "due_on", ResolveDayEdgeType("deadline", dateFields))
	assert.Equal(t, "occurred_on", ResolveDayEdgeType("happens_at", dateFields))
}

// TestResolveDayEdgeType_BaselineFallback pins that an undeclared
// field falls back to references_day.
func TestResolveDayEdgeType_BaselineFallback(t *testing.T) {
	t.Parallel()
	dateFields := map[string]string{"deadline": "due_on"}
	assert.Equal(t, EdgeTypeReferencesDay, ResolveDayEdgeType("note_about", dateFields))
	assert.Equal(t, EdgeTypeReferencesDay, ResolveDayEdgeType("anything", nil))
}

// TestResolveDayEdgeType_EmptyDeclaredFallsBack pins the defensive
// fallback when a plugin declares an empty edge type (author bug
// — shouldn't happen if the capability schema rejects empties).
func TestResolveDayEdgeType_EmptyDeclaredFallsBack(t *testing.T) {
	t.Parallel()
	dateFields := map[string]string{"deadline": ""}
	assert.Equal(t, EdgeTypeReferencesDay, ResolveDayEdgeType("deadline", dateFields),
		"empty declared edge type falls back to baseline rather than emitting empty-type")
}

// TestEmitDayRefs_BaselineEdge pins the load-bearing end-to-end:
// a plain frontmatter day reference materializes the day entity
// + the references_day edge.
func TestEmitDayRefs_BaselineEdge(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "boardgame:acme-game", Kind: "boardgame",
	}))

	emitted := EmitDayRefs(context.Background(), st,
		"boardgame:acme-game",
		map[string]any{"played_on": "day:2026-11-11"},
		nil,
		logger,
	)
	assert.Equal(t, 1, emitted)

	gotEdges, err := st.GetEdgesFor(context.Background(), "boardgame:acme-game", nil)
	require.NoError(t, err)
	require.Len(t, gotEdges, 1)
	assert.Equal(t, EdgeTypeReferencesDay, gotEdges[0].Type)
	assert.Equal(t, "day:2026-11-11", gotEdges[0].To)

	_, err = st.GetEntity(context.Background(), "day:2026-11-11")
	require.NoError(t, err, "day entity must exist post-emit")
}

// TestEmitDayRefs_PluginOverrideEdge pins that the plugin's
// DateFields override produces the declared edge type (and NOT
// the baseline — no double-edge).
func TestEmitDayRefs_PluginOverrideEdge(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "task:write-report", Kind: "task",
	}))

	emitted := EmitDayRefs(context.Background(), st,
		"task:write-report",
		map[string]any{"deadline": "day:2026-12-01"},
		map[string]string{"deadline": EdgeTypeDueOn},
		logger,
	)
	assert.Equal(t, 1, emitted)

	gotEdges, err := st.GetEdgesFor(context.Background(), "task:write-report", nil)
	require.NoError(t, err)
	require.Len(t, gotEdges, 1, "exactly one edge — no double-edge with references_day")
	assert.Equal(t, EdgeTypeDueOn, gotEdges[0].Type)
	assert.Equal(t, "day:2026-12-01", gotEdges[0].To)
}

// TestEmitDayRefs_Idempotent pins that re-emitting the same refs
// is safe — no duplicate edges, no entity churn.
func TestEmitDayRefs_Idempotent(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "task:t1", Kind: "task",
	}))

	data := map[string]any{"deadline": "day:2026-11-11"}
	EmitDayRefs(context.Background(), st, "task:t1", data, nil, logger)
	EmitDayRefs(context.Background(), st, "task:t1", data, nil, logger)
	EmitDayRefs(context.Background(), st, "task:t1", data, nil, logger)

	gotEdges, err := st.GetEdgesFor(context.Background(), "task:t1", nil)
	require.NoError(t, err)
	assert.Len(t, gotEdges, 1, "three emits → one edge (upsert idempotent)")
}

// TestEmitDayRefs_MixedDeclaredAndBaseline pins that a single
// frontmatter with both declared + undeclared day fields produces
// the correct mix: declared field gets its override, undeclared
// gets the baseline.
func TestEmitDayRefs_MixedDeclaredAndBaseline(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "task:t1", Kind: "task",
	}))

	emitted := EmitDayRefs(context.Background(), st, "task:t1",
		map[string]any{
			"deadline":     "day:2026-11-11",
			"random_note":  "day:2026-12-31",
		},
		map[string]string{"deadline": EdgeTypeDueOn},
		logger,
	)
	assert.Equal(t, 2, emitted)

	gotEdges, err := st.GetEdgesFor(context.Background(), "task:t1", nil)
	require.NoError(t, err)
	require.Len(t, gotEdges, 2)
	byTarget := make(map[string]string, len(gotEdges))
	for _, e := range gotEdges {
		byTarget[e.To] = e.Type
	}
	assert.Equal(t, EdgeTypeDueOn, byTarget["day:2026-11-11"])
	assert.Equal(t, EdgeTypeReferencesDay, byTarget["day:2026-12-31"])
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}
