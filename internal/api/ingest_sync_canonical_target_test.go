// Tests for resolveCanonicalTarget per #304 Cut C2 — pins
// the source-shape → canonical-kind hop the IngestByName
// happy path runs after the tracker returns. Pre-Cut-C2
// snap.entityID was the plugin source row id; workflows
// declaring an edge target of `<targetKind>:` need the
// canonical-kind id one hop downstream of the source row.

package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

func newSyncIngesterForCanonicalTarget(t *testing.T) (*syncIngester, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return &syncIngester{tracker: &ingestTracker{store: st}}, st
}

// TestResolveCanonicalTarget_IdentityShape pins the
// canonical-shape-already-id branch: when the tracker returned
// `<targetKind>:<slug>` (identity-resolver plugins that mint
// canonical ids directly), the helper returns it as-is without
// touching the store.
func TestResolveCanonicalTarget_IdentityShape(t *testing.T) {
	t.Parallel()
	s, _ := newSyncIngesterForCanonicalTarget(t)
	got, err := s.resolveCanonicalTarget(context.Background(), "boardgame:brass-birmingham", "boardgame")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got)
}

// TestResolveCanonicalTarget_SourceShapeHopsViaCanonicalEdge
// pins the universal-source-plugin path: the tracker returns
// `yaad-bgg:thing-12345` (the source row) and the boardgame
// target lives one canonical edge away. resolveCanonicalTarget
// walks the outgoing edges and surfaces the first
// `<targetKind>:` target.
func TestResolveCanonicalTarget_SourceShapeHopsViaCanonicalEdge(t *testing.T) {
	t.Parallel()
	s, st := newSyncIngesterForCanonicalTarget(t)
	seedEntity(t, st, "yaad-bgg:thing-12345", "boardgame-source")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "is_about",
		From: "yaad-bgg:thing-12345",
		To:   "boardgame:brass-birmingham",
	}))

	got, err := s.resolveCanonicalTarget(context.Background(), "yaad-bgg:thing-12345", "boardgame")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got)
}

// TestResolveCanonicalTarget_NoCanonicalTargetErrors pins the
// hard contract: a workflow that asked for `boardgame:` cannot
// be fulfilled by a plugin that produced no boardgame target.
// The helper returns an error rather than silently misrouting
// the workflow edge to the source-row id.
func TestResolveCanonicalTarget_NoCanonicalTargetErrors(t *testing.T) {
	t.Parallel()
	s, st := newSyncIngesterForCanonicalTarget(t)
	seedEntity(t, st, "yaad-bgg:thing-12345", "boardgame-source")
	seedEntity(t, st, "person:martin-wallace", "person")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by",
		From: "yaad-bgg:thing-12345",
		To:   "person:martin-wallace",
	}))

	_, err := s.resolveCanonicalTarget(context.Background(), "yaad-bgg:thing-12345", "boardgame")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no canonical boardgame target")
}

// TestResolveCanonicalTarget_NoEdgesErrors pins the same
// hard-fail when the source row exists but emits no outgoing
// edges (e.g. a fresh ingest that didn't extract any canonical
// links).
func TestResolveCanonicalTarget_NoEdgesErrors(t *testing.T) {
	t.Parallel()
	s, st := newSyncIngesterForCanonicalTarget(t)
	seedEntity(t, st, "yaad-bgg:thing-99999", "boardgame-source")

	_, err := s.resolveCanonicalTarget(context.Background(), "yaad-bgg:thing-99999", "boardgame")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no canonical boardgame target")
}

// TestResolveCanonicalTarget_PicksFirstMatchingKind pins the
// "one canonical edge per (source, kind)" contract: a source
// row carrying edges to multiple kinds returns the first
// matching the requested kind, ignoring others. Plugin-side
// duplicates are out of scope for this helper.
func TestResolveCanonicalTarget_PicksFirstMatchingKind(t *testing.T) {
	t.Parallel()
	s, st := newSyncIngesterForCanonicalTarget(t)
	seedEntity(t, st, "yaad-bgg:thing-12345", "boardgame-source")
	seedEntity(t, st, "person:martin-wallace", "person")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "designed_by",
		From: "yaad-bgg:thing-12345",
		To:   "person:martin-wallace",
	}))
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "is_about",
		From: "yaad-bgg:thing-12345",
		To:   "boardgame:brass-birmingham",
	}))

	got, err := s.resolveCanonicalTarget(context.Background(), "yaad-bgg:thing-12345", "boardgame")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got)
}
