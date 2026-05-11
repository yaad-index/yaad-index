package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// sourceShapePlugin returns a fixture plugin producing the
// ADR-0021 source-shape (post-subprocess.toFetchResult): Entity
// is keyed by source_namespace, CanonicalEntities is empty
// (canonical labels are not entity rows pre-materialize), and
// CanonicalEdges carries `<kind>:<slug>` targets.
//
// The fixture skips subprocess wiring; the post-translation
// shape is what ingest_tracker sees, so this test setup matches
// what a real BGG/Wikipedia plugin emits after the wire-shape
// catchup lands.
func sourceShapePlugin() plugins.Plugin {
	return &fixture.Plugin{
		NameValue: "bgg",
		MatchFunc: func(rawURL string) bool {
			return rawURL != ""
		},
		CapabilitiesValue: plugins.Capabilities{
			Name: "bgg",
			Version: "0.1.0",
			SourceNamespace: "bgg",
			CanonicalKindsEmitted: []string{"person", "boardgame"},
			CanonicalEdgeTypesEmitted: []string{"is_a", "designed_by", "is_about"},
		},
		FetchValue: &plugins.FetchResult{
			Entity: &store.Entity{
				ID: "bgg:brass-birmingham",
				Kind: "bgg",
				Data: map[string]any{"bgg_id": 224517},
			},
			SourceName: "Brass: Birmingham (2018)",
			Provenance: []store.ProvenanceEntry{
				{Source: "bgg:224517", OK: true},
			},
			// ADR-0021: no CanonicalEntities; thin rows are auto-
			// materialized at ingest from edge targets.
			CanonicalEdges: []*store.Edge{
				{
					Type: "is_a",
					From: "bgg:brass-birmingham",
					To: "source-type:bgg-record",
				},
				{
					Type: "is_about",
					From: "bgg:brass-birmingham",
					To: "boardgame:brass-birmingham",
				},
				{
					Type: "designed_by",
					From: "bgg:brass-birmingham",
					To: "person:martin-wallace",
				},
			},
		},
	}
}

func newAPIWithSourceShapePlugin(t *testing.T, kinds, edgeTypes []string) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	registry := plugins.NewRegistry()
	registry.Register(sourceShapePlugin())

	guard := config.NewCanonicalGuard(kinds, edgeTypes)
	logger := silentLoggerForTest()
	return NewHandlerWithRegistry(logger, st, registry,
		WithCanonicalGuard(guard)), st
}

// TestIngest_SourceShape_ThinRowsMaterializeForEnabledKinds asserts
// the ADR-0021 thin-row materialization path: when a plugin emits
// the new source-shape (CanonicalEntities empty, CanonicalEdges
// populated), the daemon auto-creates a thin entity row for each
// edge target whose kind is in the operator's canonical_kinds.
// The thin row enables persistCanonicalEdges' FK-constrained
// CreateEdge to land the edge.
func TestIngest_SourceShape_ThinRowsMaterializeForEnabledKinds(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithSourceShapePlugin(t,
		[]string{"person", "boardgame"},
		[]string{"is_a", "designed_by", "is_about"},
	)

	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Source entity lands at <source_namespace>:<slug> with
	// Kind=source_namespace.
	src, err := st.GetEntity(context.Background(), "bgg:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "bgg", src.Kind)

	// Thin canonical-label rows materialized for both edge targets
	// in canonical_kinds.
	person, err := st.GetEntity(context.Background(), "person:martin-wallace")
	require.NoError(t, err)
	assert.Equal(t, "person", person.Kind)
	assert.Empty(t, person.Data,
		"thin label row should have empty Data (vault file materializes on first operator-fill)")

	boardgame, err := st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame", boardgame.Kind)

	// Source-type label gets a thin row even though "source-type"
	// is NOT in canonical_kinds (system-reserved per ADR-0021).
	srcType, err := st.GetEntity(context.Background(), "source-type:bgg-record")
	require.NoError(t, err)
	assert.Equal(t, "source-type", srcType.Kind)

	// All three edges land — both endpoints exist for each.
	edges, err := st.GetEdgesFor(context.Background(), "bgg:brass-birmingham", nil)
	require.NoError(t, err)
	assert.Len(t, edges, 3)
}

// TestIngest_SourceShape_ThinRowGatedByCanonicalKinds asserts the
// AllowKind gate fires for source-shape edge targets: an edge to
// `country:<slug>` when `country` is NOT in canonical_kinds drops
// the thin-row creation, and the subsequent CreateEdge surfaces
// ErrMissingEntity (silent drop with debug log). The drop counter
// fires once.
func TestIngest_SourceShape_ThinRowGatedByCanonicalKinds(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithSourceShapePlugin(t,
		// Only person enabled; boardgame deliberately omitted so
		// the boardgame edge target hits the gate.
		[]string{"person"},
		[]string{"is_a", "designed_by", "is_about"},
	)

	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Person row materialized.
	_, err := st.GetEntity(context.Background(), "person:martin-wallace")
	require.NoError(t, err)

	// Boardgame row did NOT materialize (kind not in
	// canonical_kinds).
	_, err = st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"boardgame:brass-birmingham should NOT have materialized: boardgame not in canonical_kinds")

	// is_about edge dropped (target row missing → FK fail → silent
	// drop). designed_by and is_a edges survive.
	edges, err := st.GetEdgesFor(context.Background(), "bgg:brass-birmingham", nil)
	require.NoError(t, err)
	assert.Len(t, edges, 2)
	for _, e := range edges {
		assert.NotEqual(t, "is_about", e.Type,
			"is_about edge should have dropped along with its missing target")
	}
}

// TestIngest_SourceShape_ThinRowPreservesPriorData asserts the
// idempotency property of materializeThinLabelRowsFromEdges: a
// re-ingest emitting the same canonical-label edge target where
// a row already exists with populated Data (e.g. from a prior
// operator-fill) does NOT clobber the Data with empty.
func TestIngest_SourceShape_ThinRowPreservesPriorData(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithSourceShapePlugin(t,
		[]string{"person", "boardgame"},
		[]string{"is_a", "designed_by", "is_about"},
	)

	// First ingest creates the thin row.
	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "first ingest body=%s", rec.Body.String())

	// Simulate operator-fill having populated the canonical-label
	// row's Data via a direct UpsertEntity (the operator-fill
	// auto-materialize wiring lands in a follow-up).
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: "person:martin-wallace",
		Kind: "person",
		Data: map[string]any{"birthdate": "1962-01-01", "country": "uk"},
	}))

	// Second ingest re-emits the same edge target. Data MUST
	// survive — UpsertEntity's ON CONFLICT DO UPDATE would
	// clobber it; our check-then-skip path explicitly guards
	// against that.
	rec = postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517",
		"wait_seconds": 2,
		"force_refetch": true,
	})
	require.Equal(t, http.StatusOK, rec.Code, "re-ingest body=%s", rec.Body.String())

	person, err := st.GetEntity(context.Background(), "person:martin-wallace")
	require.NoError(t, err)
	assert.Equal(t, "1962-01-01", person.Data["birthdate"],
		"prior operator-fill data must survive thin-row re-materialize")
	assert.Equal(t, "uk", person.Data["country"])
}
