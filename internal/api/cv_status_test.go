package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// Per ADR-0013 §3 / yaad-index a prior PR: `/v1/cv-status` surfaces
// canonical-vocabulary drift — counters of plugin emissions the
// operator's config dropped, plus the config_hash for change
// detection and last_reindex_at for the operator-prompted reindex
// hint.

func newCVStatusAPI(
	t *testing.T,
	reg map[string]config.CanonicalKindConfig,
	edgeTypes []string,
) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts := []HandlerOption{}
	if reg != nil {
		opts = append(opts, WithCanonicalKindRegistry(reg))
	}
	if len(edgeTypes) > 0 {
		opts = append(opts, WithCanonicalEdgeTypes(edgeTypes))
	}
	return NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), opts...), st
}

func decodeCVStatus(t *testing.T, rec *httptest.ResponseRecorder) cvStatusResponse {
	t.Helper()
	var got cvStatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got),
		"decode /v1/cv-status response; body=%s", rec.Body.String())
	return got
}

// Empty store + empty config → all drift arrays empty, config_hash
// non-empty + 16 hex chars, last_reindex_at null, reindex_hint
// populated.
func TestCVStatus_EmptyStore_EmptyDrift(t *testing.T) {
	t.Parallel()
	h, _ := newCVStatusAPI(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeCVStatus(t, rec)
	assert.True(t, got.OK)
	assert.Len(t, got.ConfigHash, 16)
	assert.Empty(t, got.Drift.KindsEmittedNotEnabled)
	assert.Empty(t, got.Drift.KindsEnabledNotEmitted)
	assert.Empty(t, got.Drift.EdgeTypesEmittedNotEnabled)
	assert.Empty(t, got.Drift.EdgeTypesEnabledNotEmitted)
	assert.Nil(t, got.LastReindexAt, "no reindex run yet → null on wire")
	assert.Equal(t, cvStatusReindexHint, got.ReindexHint)

	// Raw-JSON pin per yaad-index a prior PR / yaad-mcp a prior PR/14
	// pattern: empty arrays serialize as `[]`, not `null`.
	body := rec.Body.String()
	assert.Contains(t, body, `"kinds_emitted_not_enabled":[]`)
	assert.Contains(t, body, `"kinds_enabled_not_emitted":[]`)
	assert.Contains(t, body, `"edge_types_emitted_not_enabled":[]`)
	assert.Contains(t, body, `"edge_types_enabled_not_emitted":[]`)
	assert.NotContains(t, body, `"kinds_emitted_not_enabled":null`)
	assert.NotContains(t, body, `"edge_types_emitted_not_enabled":null`)
}

// Populated drift: directly stamp counters via the store (decouples
// from the ingest hookup test below) and verify the wire shape.
func TestCVStatus_PopulatedDrift_VerbatimRows(t *testing.T) {
	t.Parallel()
	h, st := newCVStatusAPI(t, nil, nil)
	ctx := context.Background()
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person"))
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person"))
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person")) // count = 3
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "boardgame"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeCVStatus(t, rec)
	require.Len(t, got.Drift.KindsEmittedNotEnabled, 2)

	// Sorted (plugin, kind) ASC per the store's contract.
	assert.Equal(t, "wikipedia", got.Drift.KindsEmittedNotEnabled[0].Plugin)
	assert.Equal(t, "boardgame", got.Drift.KindsEmittedNotEnabled[0].Kind)
	assert.Equal(t, int64(1), got.Drift.KindsEmittedNotEnabled[0].WouldMaterializeCount)
	assert.Equal(t, "person", got.Drift.KindsEmittedNotEnabled[1].Kind)
	assert.Equal(t, int64(3), got.Drift.KindsEmittedNotEnabled[1].WouldMaterializeCount)

	require.Len(t, got.Drift.EdgeTypesEmittedNotEnabled, 1)
	assert.Equal(t, "wikipedia", got.Drift.EdgeTypesEmittedNotEnabled[0].Plugin)
	assert.Equal(t, "is_about", got.Drift.EdgeTypesEmittedNotEnabled[0].EdgeType)
	assert.Equal(t, int64(2), got.Drift.EdgeTypesEmittedNotEnabled[0].WouldMaterializeCount)

	// kinds_enabled_not_emitted / edge_types_enabled_not_emitted
	// are stubbed empty for v1 per the issue spec.
	assert.Empty(t, got.Drift.KindsEnabledNotEmitted)
	assert.Empty(t, got.Drift.EdgeTypesEnabledNotEmitted)
}

// config_hash matches the standalone helper output for the same
// inputs. Pins the cross-call consistency guarantee — operator
// tooling can pre-compute the expected hash and verify the
// surface returns the same value.
func TestCVStatus_ConfigHashMatchesHelper(t *testing.T) {
	t.Parallel()
	reg := map[string]config.CanonicalKindConfig{
		"person": {
			Gaps: config.GapsFromMap(map[string]string{"name": "Full name."}),
			Instruction: config.InstructionFromString("Skip if absent."),
		},
	}
	edges := []string{"is_about", "lives_in"}
	h, _ := newCVStatusAPI(t, reg, edges)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeCVStatus(t, rec)

	expected, err := config.ConfigHash(reg, edges)
	require.NoError(t, err)
	assert.Equal(t, expected, got.ConfigHash)
}

// last_reindex_at populates from the reindex_files table after a
// reindex landed. Mirrors the store-side timestamp aggregation
// via direct UpsertReindexFile so the test doesn't have to spin
// up a real reindex run.
func TestCVStatus_LastReindexAtAfterUpsert(t *testing.T) {
	t.Parallel()
	h, st := newCVStatusAPI(t, nil, nil)
	ctx := context.Background()

	// Reindex sentinel: write one bookkeeping row with a known
	// timestamp; LastReindexAt should surface its last_indexed_at
	// on the cv-status response.
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, st.UpsertReindexFile(ctx, store.ReindexFile{
		Path: "/tmp/test-vault/wikipedia/foo.md",
		Mtime: now,
		ContentHash: "deadbeef",
		LastIndexedAt: now,
		EntityID: "wikipedia:foo",
		EntityKind: "wikipedia-article",
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeCVStatus(t, rec)
	require.NotNil(t, got.LastReindexAt, "after reindex, last_reindex_at must be non-null")
	// RFC-3339 UTC matches what the handler formats.
	assert.Equal(t, now.Format("2006-01-02T15:04:05Z"), *got.LastReindexAt)
}

// LastReindexAt: nil on the wire when no reindex has ever run.
// Distinct from the empty-store test above (which already covers
// this) so the failure mode is independently named in the test
// suite.
func TestCVStatus_LastReindexAt_NullWhenNeverReindexed(t *testing.T) {
	t.Parallel()
	h, _ := newCVStatusAPI(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// Decode as raw JSON to assert the field is literally null on
	// the wire (not `""` or absent).
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	v, has := raw["last_reindex_at"]
	require.True(t, has, "last_reindex_at must be present on the wire even when null")
	assert.Nil(t, v, "last_reindex_at must be null when no reindex has run")
}

// Integration: ingest-time drops bump the counters, which then
// surface on cv-status. End-to-end exercise of the a prior PR hookup.
// Under the post- ADR-0021 shape, the daemon's thin-row
// materialize path bumps the kind drop counter when an edge
// target's kind isn't in the operator's canonical_kinds; the
// edge-type drop counter fires from the existing
// persistCanonicalEdges path.
func TestCVStatus_IngestDropsBumpCounters(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// Source-shape fixture: a `wikipedia`-namespaced source node
	// emits one is_about edge to person:martin-wallace AND one
	// linked_in edge to city:bristol. With empty canonical_kinds +
	// canonical_edge_types config both kinds + the linked_in edge
	// type drop, bumping counters as the cv-status drift surface
	// expects.
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		MatchFunc: func(rawURL string) bool { return rawURL != "" },
		CapabilitiesValue: plugins.Capabilities{
			Name: "wikipedia",
			Version: "0.1.0",
			SourceNamespace: "wikipedia",
			CanonicalKindsEmitted: []string{"person", "city"},
			CanonicalEdgeTypesEmitted: []string{"is_about", "linked_in"},
		},
		FetchValue: &plugins.FetchResult{
			Entity: &store.Entity{
				ID: "wikipedia:martin-wallace",
				Kind: "wikipedia",
				Data: map[string]any{"title": "Martin Wallace"},
			},
			Provenance: []store.ProvenanceEntry{
				{Source: "wikipedia:Martin_Wallace", OK: true},
			},
			CanonicalEdges: []*store.Edge{
				{Type: "is_about", From: "wikipedia:martin-wallace", To: "person:martin-wallace"},
				{Type: "linked_in", From: "wikipedia:martin-wallace", To: "city:bristol"},
			},
		},
	})
	guard := config.NewCanonicalGuard(nil, nil)
	logger := silentLoggerForTest()
	h := NewHandlerWithRegistry(logger, st, registry,
		WithCanonicalGuard(guard))

	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Martin_Wallace",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "ingest body=%s", rec.Body.String())

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	require.Equal(t, http.StatusOK, rec.Code, "cv-status body=%s", rec.Body.String())
	got := decodeCVStatus(t, rec)

	// Two kind drops: person + city. Both attributed to the
	// fixture's plugin name.
	require.Len(t, got.Drift.KindsEmittedNotEnabled, 2)
	for _, row := range got.Drift.KindsEmittedNotEnabled {
		assert.Equal(t, int64(1), row.WouldMaterializeCount)
		assert.NotEmpty(t, row.Plugin, "plugin name attributed on drop counter")
	}
	kinds := []string{
		got.Drift.KindsEmittedNotEnabled[0].Kind,
		got.Drift.KindsEmittedNotEnabled[1].Kind,
	}
	assert.ElementsMatch(t, []string{"city", "person"}, kinds)

	// Two edge-type drops: is_about + linked_in (neither edge
	// type is in the operator's canonical_edge_types config).
	require.Len(t, got.Drift.EdgeTypesEmittedNotEnabled, 2)
	edgeTypes := make([]string, 0, len(got.Drift.EdgeTypesEmittedNotEnabled))
	for _, row := range got.Drift.EdgeTypesEmittedNotEnabled {
		edgeTypes = append(edgeTypes, row.EdgeType)
		assert.Equal(t, int64(1), row.WouldMaterializeCount)
	}
	assert.ElementsMatch(t, []string{"is_about", "linked_in"}, edgeTypes)
}

// reindex_hint is the static guidance string per ADR-0013 §3.
// Pin the exact value so a future change deliberately updates
// the test rather than drifting silently.
func TestCVStatus_ReindexHintIsStatic(t *testing.T) {
	t.Parallel()
	h, _ := newCVStatusAPI(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/cv-status", nil))
	got := decodeCVStatus(t, rec)
	assert.Equal(t,
		"POST /v1/reindex to materialize stubs after enabling kinds/edges in config",
		got.ReindexHint)
}
