package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestStoreArchiveStateProbe_HasUnfilledGapsFalseOnAllFilled pins
// the ADR-0030 §2 `all_gaps_resolved` primitive against the
// production probe: an entity whose GapState carries only filled
// or deferred entries reports HasUnfilledGaps=false.
func TestStoreArchiveStateProbe_HasUnfilledGapsFalseOnAllFilled(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "gmail:msg-filled",
		Kind: "gmail",
		Data: map[string]any{"subject": "ok"},
		GapState: map[string]store.GapStateEntry{
			"is_about":       {Source: "agent", FilledAt: &now},
			"is_actionable":  {Source: "operator", FilledAt: &now},
			"deferred_field": {Deferred: true, DeferredAt: &now},
		},
	}))

	p := &storeArchiveStateProbe{st: st}
	view, err := p.EntityArchiveState(ctx, "gmail:msg-filled")
	require.NoError(t, err)
	assert.False(t, view.HasUnfilledGaps,
		"all gaps either filled-or-deferred must report HasUnfilledGaps=false")
	assert.Equal(t, "ok", view.Data["subject"])
}

// TestStoreArchiveStateProbe_HasUnfilledGapsTrueOnAnyUnfilled pins
// the inverse: one unfilled-and-undeferred gap flips the signal.
func TestStoreArchiveStateProbe_HasUnfilledGapsTrueOnAnyUnfilled(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "gmail:msg-partial",
		Kind: "gmail",
		GapState: map[string]store.GapStateEntry{
			"is_about":  {Source: "agent", FilledAt: &now},
			"is_open":   {}, // neither filled nor deferred
		},
	}))

	p := &storeArchiveStateProbe{st: st}
	view, err := p.EntityArchiveState(context.Background(), "gmail:msg-partial")
	require.NoError(t, err)
	assert.True(t, view.HasUnfilledGaps,
		"any unfilled-and-undeferred gap must report HasUnfilledGaps=true")
}

// TestStoreArchiveStateProbe_OutgoingEdgeTypesDedup pins that the
// probe projects unique edge types from the entity's outbound
// edges (the predicate's HasEdges primitive is set membership,
// not multiset).
func TestStoreArchiveStateProbe_OutgoingEdgeTypesDedup(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	// SaveEntity does NOT persist Edges (edges live in a separate
	// table written via CreateEdge). Save the source + each
	// referenced target + the edges themselves.
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "gmail:msg-edges",
		Kind: "gmail",
	}))
	for _, target := range []string{"person:alice", "topic:budget", "person:bob"} {
		require.NoError(t, st.SaveEntity(ctx, &store.Entity{
			ID:   target,
			Kind: target[:len("person")], // crude prefix-as-kind, ok for the test
		}))
	}
	for _, e := range []store.Edge{
		{Type: "is_about", From: "gmail:msg-edges", To: "person:alice"},
		{Type: "is_about", From: "gmail:msg-edges", To: "topic:budget"},
		{Type: "is_actionable_for", From: "gmail:msg-edges", To: "person:bob"},
	} {
		require.NoError(t, st.CreateEdge(ctx, &e))
	}

	p := &storeArchiveStateProbe{st: st}
	view, err := p.EntityArchiveState(ctx, "gmail:msg-edges")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"is_about", "is_actionable_for"}, view.OutgoingEdgeTypes,
		"OutgoingEdgeTypes must be the unique set of edge types")
}

// TestStoreArchiveStateProbe_NotFoundReturnsZeroView pins the
// race-degrade design: when the entity vanished between fill and
// archive, the probe returns a zero view + nil error so the
// predicate evaluates "false" cleanly + the engine skips archive.
func TestStoreArchiveStateProbe_NotFoundReturnsZeroView(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	p := &storeArchiveStateProbe{st: st}
	view, err := p.EntityArchiveState(context.Background(), "gmail:never-existed")
	require.NoError(t, err, "not-found must NOT propagate as error")
	assert.False(t, view.HasUnfilledGaps)
	assert.Empty(t, view.OutgoingEdgeTypes)
	assert.Empty(t, view.Data)
}
