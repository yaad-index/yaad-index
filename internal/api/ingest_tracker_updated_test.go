// Unit tests for the entity.updated emission helper added per
// ADR-0024's 2026-05-21 amendment. The per-field delta detection
// + per-changed-field emission lives in queueEntityUpdated;
// preUpsertSnapshot decides the new-vs-rewrite branch upstream.
// These tests exercise both directly so the rules are pinned
// without needing the full handler integration path.

package api

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
)

// trackerForEmit constructs a minimal ingestTracker wired to a
// fresh bus + a recording subscriber. The store / vault / etc.
// stay nil — the helpers under test only touch the bus.
func trackerForEmit(t *testing.T) (*ingestTracker, eventbus.Bus, func() []eventbus.Event) {
	t.Helper()
	bus := eventbus.NewMemoryBus()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	tr := &ingestTracker{
		logger: logger,
		bus:    bus,
	}
	var mu sync.Mutex
	var got []eventbus.Event
	bus.Subscribe(eventbus.TopicEntityUpdated, func(_ context.Context, e eventbus.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})
	bus.Subscribe(eventbus.TopicEntityCreated, func(_ context.Context, e eventbus.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})
	snapshot := func() []eventbus.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]eventbus.Event, len(got))
		copy(out, got)
		return out
	}
	return tr, bus, snapshot
}

// TestQueueEntityUpdated_ChangedField_EmitsOne: one field
// changed → one entity.updated event with the right Field,
// Old, New, EntityID, Kind.
func TestQueueEntityUpdated_ChangedField_EmitsOne(t *testing.T) {
	t.Parallel()
	tr, _, snapshot := trackerForEmit(t)
	tr.queueEntityUpdated(context.Background(), nil,
		"github:acme_proj_pr_42", "github-pr",
		map[string]any{"state": "open", "number": int64(42)},
		map[string]any{"state": "closed", "number": int64(42)},
	)
	events := snapshot()
	require.Len(t, events, 1)
	ev, ok := events[0].(eventbus.EntityUpdatedEvent)
	require.True(t, ok)
	assert.Equal(t, "github:acme_proj_pr_42", ev.EntityID)
	assert.Equal(t, "github-pr", ev.Kind)
	assert.Equal(t, "data.state", ev.Field)
	assert.Equal(t, "open", ev.Old)
	assert.Equal(t, "closed", ev.New)
	assert.Equal(t, eventbus.SourceAgent, ev.SourceTag)
}

// TestQueueEntityUpdated_MultipleChangedFields_EmitsOnePerField:
// the ADR-pinned per-field-not-list rule — two changed fields
// surface as two separate events.
func TestQueueEntityUpdated_MultipleChangedFields_EmitsOnePerField(t *testing.T) {
	t.Parallel()
	tr, _, snapshot := trackerForEmit(t)
	tr.queueEntityUpdated(context.Background(), nil,
		"github:acme_proj_pr_42", "github-pr",
		map[string]any{"state": "open", "comment_count": int64(1)},
		map[string]any{"state": "closed", "comment_count": int64(3)},
	)
	events := snapshot()
	require.Len(t, events, 2, "two fields changed → two separate events")
	fields := []string{}
	for _, e := range events {
		fields = append(fields, e.(eventbus.EntityUpdatedEvent).Field)
	}
	// Sorted-key emission order — comment_count then state.
	assert.Equal(t, []string{"data.comment_count", "data.state"}, fields)
}

// TestQueueEntityUpdated_NoChange_NoEmission: equal values
// produce zero events. Pins the deep-equal skip.
func TestQueueEntityUpdated_NoChange_NoEmission(t *testing.T) {
	t.Parallel()
	tr, _, snapshot := trackerForEmit(t)
	tr.queueEntityUpdated(context.Background(), nil,
		"github:acme_proj_pr_42", "github-pr",
		map[string]any{"state": "open", "number": int64(42)},
		map[string]any{"state": "open", "number": int64(42)},
	)
	assert.Empty(t, snapshot())
}

// TestQueueEntityUpdated_AddedField_EmitsWithNilOld: a field
// added in the re-fetch (absent in old) emits with Old=nil.
func TestQueueEntityUpdated_AddedField_EmitsWithNilOld(t *testing.T) {
	t.Parallel()
	tr, _, snapshot := trackerForEmit(t)
	tr.queueEntityUpdated(context.Background(), nil,
		"github:acme_proj_pr_42", "github-pr",
		map[string]any{"state": "open"},
		map[string]any{"state": "open", "merged_at": "2026-05-22T06:00:00Z"},
	)
	events := snapshot()
	require.Len(t, events, 1)
	ev := events[0].(eventbus.EntityUpdatedEvent)
	assert.Equal(t, "data.merged_at", ev.Field)
	assert.Nil(t, ev.Old)
	assert.Equal(t, "2026-05-22T06:00:00Z", ev.New)
}

// TestQueueEntityUpdated_DroppedField_EmitsWithNilNew: a field
// dropped in the re-fetch (absent in new) emits with New=nil.
func TestQueueEntityUpdated_DroppedField_EmitsWithNilNew(t *testing.T) {
	t.Parallel()
	tr, _, snapshot := trackerForEmit(t)
	tr.queueEntityUpdated(context.Background(), nil,
		"github:acme_proj_pr_42", "github-pr",
		map[string]any{"state": "open", "draft": true},
		map[string]any{"state": "open"},
	)
	events := snapshot()
	require.Len(t, events, 1)
	ev := events[0].(eventbus.EntityUpdatedEvent)
	assert.Equal(t, "data.draft", ev.Field)
	assert.Equal(t, true, ev.Old)
	assert.Nil(t, ev.New)
}

// TestQueueEntityUpdated_NilBus_Noop: no bus wired → no panic,
// no event. Mirrors queueEntityCreated's nil-bus guard.
func TestQueueEntityUpdated_NilBus_Noop(t *testing.T) {
	t.Parallel()
	tr := &ingestTracker{bus: nil}
	tr.queueEntityUpdated(context.Background(), nil, "id", "kind",
		map[string]any{"a": 1}, map[string]any{"a": 2})
	// No assertion needed — coverage is "doesn't panic / doesn't error".
}

// TestPreUpsertSnapshot_NotFound_FlagsAsNew: ErrNotFound from
// GetEntity returns (nil, true, true) so the caller takes the
// entity.created branch.
func TestPreUpsertSnapshot_NotFound_FlagsAsNew(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	defer func() { _ = st.Close() }()

	tr := &ingestTracker{store: st}
	prior, isNew, ok := tr.preUpsertSnapshot(context.Background(), "missing:id")
	assert.Nil(t, prior)
	assert.True(t, isNew)
	assert.True(t, ok)
}

// TestPreUpsertSnapshot_ExistingRow_FlagsAsReFetch: the existing
// row's Data round-trips so the caller can diff against the
// incoming Entity.Data.
func TestPreUpsertSnapshot_ExistingRow_FlagsAsReFetch(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   "github:p_42",
		Kind: "github-pr",
		Data: map[string]any{"state": "open"},
	}))

	tr := &ingestTracker{store: st}
	prior, isNew, ok := tr.preUpsertSnapshot(context.Background(), "github:p_42")
	require.True(t, ok)
	assert.False(t, isNew)
	require.NotNil(t, prior)
	assert.Equal(t, "open", prior.Data["state"])
}
