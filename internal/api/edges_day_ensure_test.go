package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/store"
)

// TestHandleCreateEdge_TaskTargetDoesNotLazyMaterialize pins
// the carve-out from #272's lazy-ensure generalization: `task`
// is daemon-managed but task rows index vault/tasks files per
// ADR-0024 §Task and only materialize on first-create.
// Auto-creating a phantom `task:<slug>` row from a manual edge
// would land an entity with no backing vault file — so the
// handler must NOT lazy-ensure for task targets. An edge to
// an unknown task:id correctly 422s with missing_entity.
func TestHandleCreateEdge_TaskTargetDoesNotLazyMaterialize(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)

	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "boardgame:bz", Kind: "boardgame",
	}))

	body := edgeRequestBody(t, edgeRequest{
		Type: canonical.EdgeTypeTriggeredBy,
		From: "boardgame:bz",
		To:   "task:nonexistent-task",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"task target without backing file must 422, not silently materialize a phantom row")

	_, err := st.GetEntity(context.Background(), "task:nonexistent-task")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"task row must NOT be lazy-materialized — vault file is authoritative")
}

// TestHandleCreateEdge_LazyMaterializesDaemonManagedTarget_Email
// pins #272: when POST /v1/edges names an `email-address:<addr>`
// target that hasn't been materialized yet, the handler ensures
// the entity row first so the CreateEdge FK holds. Same lazy-
// on-write pattern as day (#268), generalized to thin-label
// daemon-managed kinds (excluding task — see the test above).
func TestHandleCreateEdge_LazyMaterializesDaemonManagedTarget_Email(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)

	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "boardgame:bx", Kind: "boardgame",
	}))

	body := edgeRequestBody(t, edgeRequest{
		Type: canonical.EdgeTypeFrom,
		From: "boardgame:bx",
		To:   "email-address:noreply-at-example-com",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"email-address target should lazy-materialize, not 422 with missing_entity")

	got, err := st.GetEntity(context.Background(), "email-address:noreply-at-example-com")
	require.NoError(t, err, "email-address entity row materialized on demand")
	assert.Equal(t, canonical.EmailAddressKind, got.Kind)
}

// TestHandleCreateEdge_LazyMaterializesDayTarget pins #268: when
// POST /v1/edges names a `day:YYYY-MM-DD` target that hasn't
// been materialized yet, the handler ensures the day entity
// row first so the CreateEdge FK holds. Mirrors the lazy-on-
// write pattern the ingest / fill / workflow paths already
// follow via EmitDayRefs.
func TestHandleCreateEdge_LazyMaterializesDayTarget(t *testing.T) {
	t.Parallel()
	h, st := newAPIWithStore(t)

	// Seed a source entity so the edge from-side resolves.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "boardgame:b1", Kind: "boardgame",
	}))

	body := edgeRequestBody(t, edgeRequest{
		Type: canonical.EdgeTypeReferencesDay,
		From: "boardgame:b1",
		To:   "day:2099-11-11",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/edges", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"day target should lazy-materialize, not 422 with missing_entity")

	got, err := st.GetEntity(context.Background(), "day:2099-11-11")
	require.NoError(t, err, "day entity row materialized on demand")
	assert.Equal(t, canonical.DayKind, got.Kind)
}
