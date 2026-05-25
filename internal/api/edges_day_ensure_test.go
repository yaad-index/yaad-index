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
