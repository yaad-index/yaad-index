package decision

import (
	"context"
	"testing"

	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGraphWalker is the in-memory GraphWalker used to drive the
// CEL graph-walk tests. Each method records the (id, edgeType,
// limit) tuple it was called with so tests can assert SQL-side
// filter forwarding without needing a real store.
type fakeGraphWalker struct {
	fakeGraph // satisfies GraphLookup
	calls     []walkCall
	inEdges   []WalkEdge
	outEdges  []WalkEdge
	inN       []map[string]any
	outN      []map[string]any
	inTotal   int
	outTotal  int
}

type walkCall struct {
	fn       string
	id       string
	edgeType string
	limit    int
}

func (f *fakeGraphWalker) InEdges(_ context.Context, toID, edgeType string, limit int) ([]WalkEdge, int, error) {
	f.calls = append(f.calls, walkCall{fn: "in_edges", id: toID, edgeType: edgeType, limit: limit})
	total := f.inTotal
	if total == 0 {
		total = len(f.inEdges)
	}
	return f.inEdges, total, nil
}
func (f *fakeGraphWalker) OutEdges(_ context.Context, fromID, edgeType string, limit int) ([]WalkEdge, int, error) {
	f.calls = append(f.calls, walkCall{fn: "out_edges", id: fromID, edgeType: edgeType, limit: limit})
	total := f.outTotal
	if total == 0 {
		total = len(f.outEdges)
	}
	return f.outEdges, total, nil
}
func (f *fakeGraphWalker) InNeighbors(_ context.Context, toID, edgeType string, limit int) ([]map[string]any, int, error) {
	f.calls = append(f.calls, walkCall{fn: "in_neighbors", id: toID, edgeType: edgeType, limit: limit})
	total := f.inTotal
	if total == 0 {
		total = len(f.inN)
	}
	return f.inN, total, nil
}
func (f *fakeGraphWalker) OutNeighbors(_ context.Context, fromID, edgeType string, limit int) ([]map[string]any, int, error) {
	f.calls = append(f.calls, walkCall{fn: "out_neighbors", id: fromID, edgeType: edgeType, limit: limit})
	total := f.outTotal
	if total == 0 {
		total = len(f.outN)
	}
	return f.outN, total, nil
}

// TestGraphWalk_NilWalker_EmptyStruct: no walker wired → every
// helper returns the empty {items, truncated, total} struct. Lets
// unit tests of unrelated workflow logic skip the walker stub.
func TestGraphWalk_NilWalker_EmptyStruct(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	got := evalDyn(t, ev, `graph.in_edges("any:id")`, Activation{})
	m, ok := got.(map[string]any)
	require.True(t, ok, "%T", got)
	items, ok := m["items"].([]ref.Val)
	require.True(t, ok)
	assert.Empty(t, items)
	assert.Equal(t, false, m["truncated"])
	assert.Equal(t, 0, m["total"])
}

// TestGraphWalk_InEdges_BothArities pins both the unfiltered and
// filtered overloads dispatch through the walker with the right
// edge-type arg.
func TestGraphWalk_InEdges_BothArities(t *testing.T) {
	t.Parallel()
	walker := &fakeGraphWalker{
		inEdges: []WalkEdge{
			{From: "task:t1", To: "day:2026-11-11", Type: "due_on"},
		},
	}
	ev, err := NewEvaluator(Options{Walker: walker})
	require.NoError(t, err)

	// Unfiltered overload — no edge_type forwarded.
	_ = evalDyn(t, ev, `graph.in_edges("day:2026-11-11")`, Activation{})
	require.Len(t, walker.calls, 1)
	assert.Equal(t, "in_edges", walker.calls[0].fn)
	assert.Equal(t, "day:2026-11-11", walker.calls[0].id)
	assert.Equal(t, "", walker.calls[0].edgeType, "unfiltered arity → empty filter")
	assert.Equal(t, DefaultGraphWalkCap, walker.calls[0].limit)

	// Filtered overload — edge_type forwarded.
	_ = evalDyn(t, ev, `graph.in_edges("day:2026-11-11", "due_on")`, Activation{})
	require.Len(t, walker.calls, 2)
	assert.Equal(t, "due_on", walker.calls[1].edgeType)
}

// TestGraphWalk_OutEdges_ReturnsWalkEdgeShape pins the CEL edge
// shape: each item is a map with from / to / type / metadata
// keys (matches store.Edge.Metadata via the lowercase rename).
func TestGraphWalk_OutEdges_ReturnsWalkEdgeShape(t *testing.T) {
	t.Parallel()
	walker := &fakeGraphWalker{
		outEdges: []WalkEdge{
			{
				From:     "task:t1",
				To:       "day:2026-11-11",
				Type:     "due_on",
				Metadata: map[string]any{"set_by": "operator"},
			},
		},
	}
	ev, err := NewEvaluator(Options{Walker: walker})
	require.NoError(t, err)

	got := evalDyn(t, ev,
		`graph.out_edges("task:t1").items[0]`,
		Activation{})
	m, ok := got.(map[string]any)
	require.True(t, ok, "%T", got)
	assert.Equal(t, "task:t1", m["from"])
	assert.Equal(t, "day:2026-11-11", m["to"])
	assert.Equal(t, "due_on", m["type"])
	mdata, ok := m["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "operator", mdata["set_by"])
}

// TestGraphWalk_Neighbors_ReturnsEntities: in_neighbors /
// out_neighbors return Entity-shaped maps directly (no edge
// wrapping). Verifies the convenience-helper path produces the
// shape callers want for `[[{{ n.id }}]]` rendering.
func TestGraphWalk_Neighbors_ReturnsEntities(t *testing.T) {
	t.Parallel()
	walker := &fakeGraphWalker{
		inN: []map[string]any{
			{"id": "task:t1", "kind": "task"},
			{"id": "task:t2", "kind": "task"},
		},
	}
	ev, err := NewEvaluator(Options{Walker: walker})
	require.NoError(t, err)

	got := evalDyn(t, ev,
		`graph.in_neighbors("day:2026-11-11", "due_on").items.map(n, n.id)`,
		Activation{})
	list, ok := got.([]ref.Val)
	require.True(t, ok, "%T", got)
	require.Len(t, list, 2)
	assert.Equal(t, "task:t1", list[0].Value())
	assert.Equal(t, "task:t2", list[1].Value())
}

// TestGraphWalk_TruncationStruct: when total > limit, the result
// struct carries truncated:true + accurate total. Workflows that
// need full results must check the flag.
func TestGraphWalk_TruncationStruct(t *testing.T) {
	t.Parallel()
	// Simulate 1500 inbound edges; cap holds 1000.
	edges := make([]WalkEdge, 1000)
	for i := range edges {
		edges[i] = WalkEdge{From: "task:t", To: "day:X", Type: "due_on"}
	}
	walker := &fakeGraphWalker{inEdges: edges, inTotal: 1500}
	ev, err := NewEvaluator(Options{Walker: walker})
	require.NoError(t, err)

	got := evalDyn(t, ev, `graph.in_edges("day:X")`, Activation{})
	m, ok := got.(map[string]any)
	require.True(t, ok)
	items, ok := m["items"].([]ref.Val)
	require.True(t, ok)
	assert.Len(t, items, 1000, "items capped at limit")
	assert.Equal(t, true, m["truncated"], "truncated=true when total > items")
	assert.Equal(t, 1500, m["total"], "total reflects pre-cap count")
}

// TestGraphWalk_NoTruncation_WhenWithinCap: total ≤ items →
// truncated:false. Operators distinguish "got everything" from
// "got a sample."
func TestGraphWalk_NoTruncation_WhenWithinCap(t *testing.T) {
	t.Parallel()
	walker := &fakeGraphWalker{
		inEdges: []WalkEdge{
			{From: "task:a", To: "day:X", Type: "due_on"},
			{From: "task:b", To: "day:X", Type: "due_on"},
		},
	}
	ev, err := NewEvaluator(Options{Walker: walker})
	require.NoError(t, err)

	got := evalDyn(t, ev, `graph.in_edges("day:X")`, Activation{})
	m := got.(map[string]any)
	assert.Equal(t, false, m["truncated"])
	assert.Equal(t, 2, m["total"])
}

// TestGraphWalk_OperatorCapOverride: Options.GraphWalkCap
// overrides DefaultGraphWalkCap and propagates to the walker
// call's limit arg.
func TestGraphWalk_OperatorCapOverride(t *testing.T) {
	t.Parallel()
	walker := &fakeGraphWalker{}
	ev, err := NewEvaluator(Options{Walker: walker, GraphWalkCap: 50})
	require.NoError(t, err)

	_ = evalDyn(t, ev, `graph.out_edges("task:t1")`, Activation{})
	require.Len(t, walker.calls, 1)
	assert.Equal(t, 50, walker.calls[0].limit, "operator cap propagated")
}

// TestGraphWalk_PostFilter_UsesItemsMap: workflows can run
// CEL-side .filter() on .items for ad-hoc narrowing that the
// SQL filter doesn't cover.
func TestGraphWalk_PostFilter_UsesItemsMap(t *testing.T) {
	t.Parallel()
	walker := &fakeGraphWalker{
		outEdges: []WalkEdge{
			{From: "task:t1", To: "day:2026-11-11", Type: "due_on"},
			{From: "task:t1", To: "day:2026-11-15", Type: "due_on"},
			{From: "task:t1", To: "person:alice", Type: "assigned_to"},
		},
	}
	ev, err := NewEvaluator(Options{Walker: walker})
	require.NoError(t, err)

	got := evalDyn(t, ev,
		`graph.out_edges("task:t1").items.filter(e, e.type == "due_on").map(e, e.to)`,
		Activation{})
	list, ok := got.([]ref.Val)
	require.True(t, ok)
	require.Len(t, list, 2)
	assert.Equal(t, "day:2026-11-11", list[0].Value())
	assert.Equal(t, "day:2026-11-15", list[1].Value())
}

// TestGraphWalk_JoinAndFlatten_ExtensionsWired pins that
// ext.Strings() (for .join()) and ext.Lists() (for .flatten())
// are wired in buildEnv so ADR-0027 §4's worked-example idioms
// parse cleanly without per-test extension setup.
func TestGraphWalk_JoinAndFlatten_ExtensionsWired(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	// .join() on list<string>
	got := evalString(t, ev, `["a", "b", "c"].join(",")`, Activation{})
	assert.Equal(t, "a,b,c", got)

	// .flatten() on list<list<string>>
	flat := evalDyn(t, ev, `[["a"], ["b", "c"]].flatten()`, Activation{})
	list, ok := flat.([]ref.Val)
	require.True(t, ok, "%T", flat)
	require.Len(t, list, 3)
	assert.Equal(t, "a", list[0].Value())
	assert.Equal(t, "c", list[2].Value())
}
