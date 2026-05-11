package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ts(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// fixtureEntity returns a fully-populated entity matching the historical
// brass-birmingham wire shape: two provenance entries (one success +
// fetched_at, one failure + fetched_at + error fields) and a small data
// blob. Edges intentionally empty — the entity-cutover PR doesn't load
// them yet (`with_edges` lands later).
func fixtureEntity() *Entity {
	return &Entity{
		ID: "boardgame:brass-birmingham",
		Kind: "boardgame",
		Data: map[string]any{
			"title": "Brass: Birmingham",
			"year": float64(2018), // JSON numbers round-trip as float64
		},
		Provenance: []ProvenanceEntry{
			{
				Source: "bgg:14-2024-04-12",
				FetchedAt: ts("2024-04-12T15:03:11Z"),
				OK: true,
			},
			{
				Source: "bgg:14-2024-04-13",
				FetchedAt: ts("2024-04-13T06:00:00Z"),
				OK: false,
				Error: "extractor_timeout",
				ErrorMessage: "AI extraction did not complete within 60s",
			},
		},
		Edges: []EdgeRef{},
	}
}

func TestSaveAndGetEntity_RoundTrip(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	want := fixtureEntity()
	require.NoError(t, s.SaveEntity(ctx, want), "SaveEntity")

	got, err := s.GetEntity(ctx, want.ID)
	require.NoError(t, err, "GetEntity")

	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Kind, got.Kind)
	assert.Equal(t, want.Data, got.Data)
	assert.False(t, got.CreatedAt.IsZero(), "created_at: want non-zero after save")
	assert.False(t, got.UpdatedAt.IsZero(), "updated_at: want non-zero after save")
	assert.Empty(t, got.Edges, "edges: want empty (with_edges expansion not yet wired)")

	assert.True(t, provenanceEqualByValue(got.Provenance, want.Provenance),
		"provenance round-trip mismatch:\n got %#v\nwant %#v", got.Provenance, want.Provenance)
}

func TestGetEntity_MissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	_, err := s.GetEntity(ctx, "boardgame:does-not-exist")
	assert.ErrorIs(t, err, ErrNotFound, "GetEntity on unknown id: want ErrNotFound")
}

func TestGetEntities_PreservesOrderAndCollectsMissing(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	a := &Entity{
		ID: "boardgame:a",
		Kind: "boardgame",
		Data: map[string]any{"title": "A"},
		Provenance: []ProvenanceEntry{
			{Source: "bgg:a", FetchedAt: ts("2026-01-01T00:00:00Z"), OK: true},
		},
		Edges: []EdgeRef{},
	}
	b := &Entity{
		ID: "person:b",
		Kind: "person",
		Data: map[string]any{"name": "B"},
		Provenance: []ProvenanceEntry{
			{Source: "books:b", FetchedAt: ts("2026-01-02T00:00:00Z"), OK: true},
		},
		Edges: []EdgeRef{},
	}
	require.NoError(t, s.SaveEntity(ctx, a), "SaveEntity a")
	require.NoError(t, s.SaveEntity(ctx, b), "SaveEntity b")

	// Mixed order, with one not-saved id sandwiched between.
	ids := []string{"person:b", "boardgame:nope", "boardgame:a", "boardgame:also-nope"}
	matched, missing, err := s.GetEntities(ctx, ids)
	require.NoError(t, err, "GetEntities")

	gotMatchedIDs := make([]string, len(matched))
	for i, m := range matched {
		gotMatchedIDs[i] = m.ID
	}
	assert.Equal(t, []string{"person:b", "boardgame:a"}, gotMatchedIDs,
		"matched ids: want input-order present-only")
	assert.Equal(t, []string{"boardgame:nope", "boardgame:also-nope"}, missing,
		"missing: want input order")
}

func TestGetEntities_EmptyInputReturnsEmptyOutputs(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	matched, missing, err := s.GetEntities(context.Background(), nil)
	require.NoError(t, err, "GetEntities(nil)")
	assert.Empty(t, matched, "matched")
	assert.Empty(t, missing, "missing")
}

func TestSaveEntity_OnReSaveReplacesProvenance(t *testing.T) {
	t.Parallel()

	// Wipe-and-rewrite semantics for the entity-cutover PR. When ingest
	// lands its real append-on-re-save behaviour the rule changes — this
	// test will be updated alongside.
	s := newMemoryStore(t)
	ctx := context.Background()

	first := fixtureEntity()
	require.NoError(t, s.SaveEntity(ctx, first), "first SaveEntity")

	second := fixtureEntity()
	second.Provenance = []ProvenanceEntry{
		{Source: "bgg:14-2024-05-01", FetchedAt: ts("2024-05-01T00:00:00Z"), OK: true},
	}
	require.NoError(t, s.SaveEntity(ctx, second), "second SaveEntity")

	got, err := s.GetEntity(ctx, second.ID)
	require.NoError(t, err, "GetEntity")
	require.Len(t, got.Provenance, 1, "provenance after re-save: want 1 (wiped + rewritten)")
	assert.Equal(t, "bgg:14-2024-05-01", got.Provenance[0].Source)
}

func TestSaveEntity_RoundTripsAgentFillProvenance(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	e := &Entity{
		ID: "boardgame:agent-fill-test",
		Kind: "boardgame",
		Data: map[string]any{"title": "Filled By Agent"},
		Provenance: []ProvenanceEntry{
			{
				Source: "bgg:af",
				FetchedAt: ts("2026-01-01T00:00:00Z"),
				OK: true,
			},
			{
				Source: "agent:bob",
				FilledAt: ts("2026-01-01T00:05:00Z"),
				OK: true,
			},
		},
		Edges: []EdgeRef{},
	}
	require.NoError(t, s.SaveEntity(ctx, e), "SaveEntity")

	got, err := s.GetEntity(ctx, e.ID)
	require.NoError(t, err, "GetEntity")
	require.Len(t, got.Provenance, 2, "provenance: want 2 entries")

	plugin := got.Provenance[0]
	assert.NotNil(t, plugin.FetchedAt, "provenance[0].fetched_at: want set on plugin entry")
	assert.Nil(t, plugin.FilledAt, "provenance[0].filled_at: want nil on plugin entry")

	fill := got.Provenance[1]
	assert.NotNil(t, fill.FilledAt, "provenance[1].filled_at: want set on fill entry")
	assert.Nil(t, fill.FetchedAt, "provenance[1].fetched_at: want nil on fill entry")
}

// provenanceEqualByValue compares two slices ignoring trivial pointer
// differences in *time.Time. Round-tripped times are compared by
// .Equal() (so RFC3339Nano fidelity isn't required) and nilness.
func provenanceEqualByValue(a, b []ProvenanceEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Source != b[i].Source ||
			a[i].OK != b[i].OK ||
			a[i].Error != b[i].Error ||
			a[i].ErrorMessage != b[i].ErrorMessage {
			return false
		}
		if (a[i].FetchedAt == nil) != (b[i].FetchedAt == nil) {
			return false
		}
		if a[i].FetchedAt != nil && !a[i].FetchedAt.Equal(*b[i].FetchedAt) {
			return false
		}
		if (a[i].FilledAt == nil) != (b[i].FilledAt == nil) {
			return false
		}
		if a[i].FilledAt != nil && !a[i].FilledAt.Equal(*b[i].FilledAt) {
			return false
		}
	}
	return true
}

// TestSaveEntity_GapStateRoundTrip pins the ADR-0019 §Storage
// gap_state JSON column round-trip through SaveEntity → GetEntity.
// Three semantic shapes coexist on the wire and all must survive:
//
// - filled-by-agent → {Source: "agent", FilledAt: <ts>}
// - filled-by-operator → {Source: "operator", FilledAt: <ts>}
// - deferred → {Deferred: true, DeferredAt: <ts>}
//
// Empty + nil maps round-trip as nil (the column stays NULL on
// disk so existing-rows shape stays clean).
func TestSaveEntity_GapStateRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	filledAtAgent := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	filledAtOperator := time.Date(2026, 5, 8, 16, 30, 0, 0, time.UTC)
	deferredAt := time.Date(2026, 5, 8, 16, 35, 0, 0, time.UTC)

	want := fixtureEntity()
	want.GapState = map[string]GapStateEntry{
		"summary": {Source: "agent", FilledAt: &filledAtAgent},
		"rating": {Source: "operator", FilledAt: &filledAtOperator},
		"played": {Deferred: true, DeferredAt: &deferredAt},
	}
	require.NoError(t, s.SaveEntity(ctx, want), "SaveEntity")

	got, err := s.GetEntity(ctx, want.ID)
	require.NoError(t, err, "GetEntity")

	require.Len(t, got.GapState, 3, "gap_state entries round-trip")
	assert.Equal(t, "agent", got.GapState["summary"].Source)
	require.NotNil(t, got.GapState["summary"].FilledAt)
	assert.True(t, filledAtAgent.Equal(*got.GapState["summary"].FilledAt))
	assert.Equal(t, "operator", got.GapState["rating"].Source)
	assert.True(t, got.GapState["played"].Deferred)
	require.NotNil(t, got.GapState["played"].DeferredAt)
	assert.True(t, deferredAt.Equal(*got.GapState["played"].DeferredAt))
}

// Empty / nil GapState must round-trip as nil — the column stays
// NULL so existing-row reads see "no metadata" cleanly.
func TestSaveEntity_GapStateNilStaysNil(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	want := fixtureEntity()
	want.GapState = nil
	require.NoError(t, s.SaveEntity(ctx, want))

	got, err := s.GetEntity(ctx, want.ID)
	require.NoError(t, err)
	assert.Nil(t, got.GapState, "nil gap_state stays nil through round-trip")

	// Same shape with explicit empty map.
	want.GapState = map[string]GapStateEntry{}
	require.NoError(t, s.SaveEntity(ctx, want))
	got, err = s.GetEntity(ctx, want.ID)
	require.NoError(t, err)
	assert.Nil(t, got.GapState, "empty map collapses to nil on read (no metadata)")
}
