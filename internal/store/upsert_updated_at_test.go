package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpsertEntity_StampsUpdatedAt_ZeroCallerInput pins the
// internal-stamp contract: callers passing only CreatedAt + Data
// (the shape handleFill / handleEntityOperatorFill use today) get
// a current-time UpdatedAt on the resulting row, NOT the
// zero-value time.Time the caller's struct literal carried.
//
// Pre-existing behavior — `e.UpdatedAt = now` in UpsertEntity
// overrides any caller-supplied value. The test exists so a future
// refactor that drops or re-orders that line breaks visibly here
// rather than silently stamping 0001-01-01 on every fill row.
//
// Pinned per alice2-index a prior PR cold-reviewer carry-over (2026-05-08):
// reviewers initially read the call-site struct literal in
// handleEntityOperatorFill as "passes zero UpdatedAt" without
// noticing the store-layer's internal stamp. The test makes the
// stamp behavior unambiguous and self-documenting.
func TestUpsertEntity_StampsUpdatedAt_ZeroCallerInput(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Now().UTC()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "test:zero-updated-at",
		Kind: "test",
		Data: map[string]any{"name": "x"},
		CreatedAt: createdAt,
		// UpdatedAt deliberately zero — matches the call-site shape
		// in handleFill / handleEntityOperatorFill.
	}))
	after := time.Now().UTC()

	got, err := s.GetEntity(ctx, "test:zero-updated-at")
	require.NoError(t, err)
	assert.False(t, got.UpdatedAt.IsZero(),
		"UpsertEntity must stamp UpdatedAt even when caller passes zero")
	assert.True(t,
		!got.UpdatedAt.Before(before.Add(-time.Second)) &&
			!got.UpdatedAt.After(after.Add(time.Second)),
		"UpdatedAt %s should be near now (window [%s, %s])",
		got.UpdatedAt, before, after)
}
