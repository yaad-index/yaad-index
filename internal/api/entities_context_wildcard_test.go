// Tests for the yaad-index wildcard-parity contract on
// /v1/entities/{id}/context: parseEdgeTypesFilter accepts the same
// `*` / `all` sentinels as parseWithEdges does on the main entity-GET,
// returning nil filter so the underlying edge-fetcher returns every
// type. The end-to-end handler test exercises the proof-of-ungated
// assertion the cold-reviewer flagged: the wildcard surfaces edge types that are
// NOT in the operator's `canonical_edge_types:` active config — read
// is ungated; gating happens at write time only.

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
)

func TestParseEdgeTypesFilter_WildcardSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in string
		want []string
	}{
		{"empty", "", nil},
		{"single asterisk", "*", nil},
		{"single all", "all", nil},
		{"asterisk first in mixed", "*,is_about", nil},
		{"asterisk last in mixed", "is_about,*", nil},
		{"all in mixed", "is_about, all, designed_by", nil},
		{"non-wildcard list preserved", "is_about,designed_by", []string{"is_about", "designed_by"}},
		{"non-wildcard list with whitespace + empties", " is_about , , designed_by ", []string{"is_about", "designed_by"}},
		{"sentinels are case-sensitive — uppercase NOT a wildcard", "ALL", []string{"ALL"}},
		{"sentinels are case-sensitive — uppercase asterisk-equivalent", "ASTERISK", []string{"ASTERISK"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseEdgeTypesFilter(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEntitiesContext_WildcardSurfacesUngatedEdgeTypes pins the cold-reviewer's
// strengthening from the spec: the fixture carries one edge
// type that IS in `canonical_edge_types:` active config AND one
// that is NOT — `?edge_types=*` returns BOTH. The unconfigured-
// edge assertion is the load-bearing proof of ungated read
// behavior; a buggy gated impl that cross-references
// canonical_edge_types at read time would surface only the
// configured edge and fail this test silently otherwise.
func TestEntitiesContext_WildcardSurfacesUngatedEdgeTypes(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// Operator config: canonical_edge_types whitelist contains ONLY
	// `designed_by`. Anything else (`obscure_unconfigured_edge_type`)
	// is structurally not in the active config — the daemon's
	// guard.AllowEdgeType returns false on read time would gate it
	// out. The wildcard contract says read is ungated; this fixture
	// exercises that invariant.
	guard := config.NewCanonicalGuard(
		[]string{"boardgame", "person"},
		[]string{"designed_by"},
	)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithCanonicalGuard(guard),
	)

	seedEntity(t, st, "boardgame:fixture", "boardgame")
	seedEntity(t, st, "person:configured-target", "person")
	seedEntity(t, st, "person:ungated-target", "person")
	seedEdge(t, st, "designed_by", "boardgame:fixture", "person:configured-target")
	// The unconfigured edge type — would drop at write-time guard
	// in the ingest path; we direct-seed via the store to bypass
	// that gate so the read-time assertion has something to find.
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "obscure_unconfigured_edge_type",
		From: "boardgame:fixture",
		To: "person:ungated-target",
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/v1/entities/boardgame:fixture/context?depth=1&edge_types=*", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := decodeContextResponse(t, rec.Body.Bytes())
	require.Len(t, got.Neighbors, 2,
		"wildcard must surface both edges — the configured AND the unconfigured (read is ungated)")

	edgeTypes := map[string]string{}
	for _, n := range got.Neighbors {
		edgeTypes[n.Edge.Type] = n.Entity.ID
	}
	assert.Equal(t, "person:configured-target", edgeTypes["designed_by"],
		"configured edge type surfaces")
	assert.Equal(t, "person:ungated-target", edgeTypes["obscure_unconfigured_edge_type"],
		"UNCONFIGURED edge type ALSO surfaces — proof of read-time-ungated wildcard")
}

// TestEntitiesContext_AllSentinelEquivalent: the alternate sentinel
// `all` produces the same wildcard semantic as `*` — both collapse
// to nil filter (no edge-type narrowing).
func TestEntitiesContext_AllSentinelEquivalent(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	seedEntity(t, st, "boardgame:fixture", "boardgame")
	seedEntity(t, st, "person:designer", "person")
	seedEntity(t, st, "person:author", "person")
	seedEdge(t, st, "designed_by", "boardgame:fixture", "person:designer")
	seedEdge(t, st, "authored_by", "boardgame:fixture", "person:author")

	for _, sentinel := range []string{"*", "all"} {
		t.Run("edge_types="+sentinel, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet,
				"/v1/entities/boardgame:fixture/context?depth=1&edge_types="+sentinel, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var got contextResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Len(t, got.Neighbors, 2,
				"wildcard surfaces both edge types regardless of sentinel spelling")
		})
	}
}
