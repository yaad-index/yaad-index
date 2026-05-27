package edgewrite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedEntity(t *testing.T, st store.Store, id, kind string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   id,
		Kind: kind,
		Data: map[string]any{"title": id},
		Provenance: []store.ProvenanceEntry{
			{Source: "seed", FetchedAt: &now, OK: true},
		},
	}))
}

// TestNew_RejectsMultiResolver pins the Cut C1 cardinality
// upgrade: a canonical kind with 2+ plugins claiming resolver
// responsibility rejects at config-load. Cut A let this through
// as WARN; Cut C1 makes it ERROR because Cut C2's routing must
// pick a single plugin.
func TestNew_RejectsMultiResolver(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	_, err := New(st, map[string][]string{
		"boardgame": {"yaad-bgg", "yaad-wikipedia"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-resolver")
	assert.Contains(t, err.Error(), "boardgame")
	// Conflict plugins sorted lexicographically so CI logs are
	// diff-able across runs regardless of map iteration order.
	assert.Contains(t, err.Error(), "yaad-bgg, yaad-wikipedia")
}

func TestNew_MultiConflictsSortedDeterministically(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	_, err := New(st, map[string][]string{
		"person":    {"plugin-b", "plugin-a"},
		"boardgame": {"plugin-d", "plugin-c"},
	})
	require.Error(t, err)
	// Kinds also sorted in the error message so a 2-conflict
	// error reads the same on every run.
	msg := err.Error()
	bgPos := indexOrFail(t, msg, "boardgame")
	prPos := indexOrFail(t, msg, "person")
	assert.Less(t, bgPos, prPos, "kind names sorted ASC in the error")
	assert.Contains(t, msg, "plugin-c, plugin-d")
	assert.Contains(t, msg, "plugin-a, plugin-b")
}

func TestNew_SingleResolverPerKindAccepted(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	svc, err := New(st, map[string][]string{
		"boardgame": {"yaad-bgg"},
		"person":    {"yaad-wikipedia"},
	})
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Equal(t, "yaad-bgg", svc.ResolverFor("boardgame"))
	assert.Equal(t, "yaad-wikipedia", svc.ResolverFor("person"))
	assert.Equal(t, "", svc.ResolverFor("city"), "unmapped kind → empty plugin name")
}

func TestNew_EmptyResolversAccepted(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	svc, err := New(st, nil)
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Equal(t, "", svc.ResolverFor("anything"))
}

func TestNew_EmptyPluginEntriesDropped(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	// Slice with only empty strings → kind has zero non-empty
	// claimants → treated as no resolver (not an error).
	svc, err := New(st, map[string][]string{
		"boardgame": {""},
	})
	require.NoError(t, err)
	assert.Equal(t, "", svc.ResolverFor("boardgame"),
		"empty plugin names dropped, leaving no resolver")
}

func TestNew_NilStoreRejected(t *testing.T) {
	t.Parallel()

	_, err := New(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store")
}

// TestCreateEdge_PassthroughEquivalence pins the Cut C1 contract:
// service.CreateEdge yields the same store state as a direct
// store.CreateEdge call. Cut C2 introduces semantic divergence;
// C1 must not.
func TestCreateEdge_PassthroughEquivalence(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	seedEntity(t, st, "book:lotr", "book")
	seedEntity(t, st, "person:tolkien", "person")

	svc, err := New(st, nil)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, svc.CreateEdge(ctx, &store.Edge{
		Type: "authored_by",
		From: "book:lotr",
		To:   "person:tolkien",
		Metadata: map[string]any{
			"role": "primary",
		},
	}))

	got, err := st.GetEdgesFor(ctx, "book:lotr", nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "authored_by", got[0].Type)
	assert.Equal(t, "person:tolkien", got[0].To)
	assert.Equal(t, "primary", got[0].Metadata["role"])
}

func TestCreateEdge_PropagatesMissingEntityError(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	svc, err := New(st, nil)
	require.NoError(t, err)

	err = svc.CreateEdge(context.Background(), &store.Edge{
		Type: "authored_by",
		From: "book:nope",
		To:   "person:nope",
	})
	require.Error(t, err,
		"passthrough must surface store-level FK errors so existing handlers' 422 mapping still fires")
}

// indexOrFail returns strings.Index(s, sub) but fails the test
// when the substring isn't found.
func indexOrFail(t *testing.T, s, sub string) int {
	t.Helper()
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	t.Fatalf("substring %q not found in %q", sub, s)
	return -1
}
