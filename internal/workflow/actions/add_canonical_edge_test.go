package actions

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

type fakeEdgeWriter struct {
	mu       sync.Mutex
	calls    []edgeCall
	writeErr error
}

type edgeCall struct {
	workflow   string
	sourceID   string
	edgeType   string
	targetKind string
	targetName string
	data       map[string]string
}

func (f *fakeEdgeWriter) AddCanonicalEdge(
	_ context.Context,
	workflow, sourceID, edgeType, targetKind, targetName string,
	data map[string]string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, edgeCall{
		workflow:   workflow,
		sourceID:   sourceID,
		edgeType:   edgeType,
		targetKind: targetKind,
		targetName: targetName,
		data:       data,
	})
	return f.writeErr
}

func (f *fakeEdgeWriter) snapshot() []edgeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]edgeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestAddCanonicalEdge_HappyPath: literal source + literal target,
// no data — the writer receives the resolved tuple verbatim.
func TestAddCanonicalEdge_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "github-repository",
			TargetName: "acme/widget",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "classify", calls[0].workflow)
	assert.Equal(t, "email:m1", calls[0].sourceID, "defaults to dec.EntityID")
	assert.Equal(t, "is_about", calls[0].edgeType)
	assert.Equal(t, "github-repository", calls[0].targetKind)
	assert.Equal(t, "acme/widget", calls[0].targetName)
	assert.Nil(t, calls[0].data)
}

// TestAddCanonicalEdge_DataPassesThrough: action.Data flows to
// the writer (the dataview-paragraph payload).
func TestAddCanonicalEdge_DataPassesThrough(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "github-repository",
			TargetName: "acme/widget",
			Data: map[string]string{
				"reference": "42",
				"type":      "review",
			},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "42", calls[0].data["reference"])
	assert.Equal(t, "review", calls[0].data["type"])
}

// TestAddCanonicalEdge_DataValueRenderedEmptyDrops: a CEL
// expression that renders to "" drops the key — workflows
// commonly extract optional fields and skip when absent.
func TestAddCanonicalEdge_DataValueRenderedEmptyDrops(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "github-repository",
			TargetName: "acme/widget",
			Data: map[string]string{
				"reference": "42",
				"salary":    "", // CEL produced no value
			},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "42", calls[0].data["reference"])
	_, hasSalary := calls[0].data["salary"]
	assert.False(t, hasSalary, "empty-render keys drop from the data map")
}

// TestAddCanonicalEdge_ExplicitSource: action.Source overrides
// the triggering entity_id.
func TestAddCanonicalEdge_ExplicitSource(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			Source:     "boardgame:other",
			EdgeType:   "is_about",
			TargetKind: "person",
			TargetName: "Uwe",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:trigger"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "boardgame:other", w.snapshot()[0].sourceID)
}

// TestAddCanonicalEdge_NoSource_AuthorBug: empty Source +
// empty Decision.EntityID → no source → author bug.
func TestAddCanonicalEdge_NoSource_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "person",
			TargetName: "Uwe",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot())
}

// TestAddCanonicalEdge_EmptyEdgeType_AuthorBug: defense-in-
// depth, the runner catches an empty edge_type even though the
// parser already rejects it.
func TestAddCanonicalEdge_EmptyEdgeType_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			TargetKind: "person",
			TargetName: "Uwe",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddCanonicalEdge_EmptyTargetKind_AuthorBug: same
// defense-in-depth check on target.kind.
func TestAddCanonicalEdge_EmptyTargetKind_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetName: "Uwe",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddCanonicalEdge_EmptyRenderedTargetName_AuthorBug: a
// CEL-rendered target name that comes back empty is an
// author bug — the workflow promised a derivable name and
// the data didn't carry it. The runner stops with
// ErrActionAuthorBug rather than create an edge to a
// degenerate slug.
func TestAddCanonicalEdge_EmptyRenderedTargetName_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "person",
			TargetName: "", // empty after parser trim
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	// Empty target.name surfaces as either the early empty-string
	// guard OR the rendered-empty guard; both share the author-
	// bug shape so the test asserts on the wrap.
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddCanonicalEdge_NoEdgeWriter: nil EdgeWriter on Options
// produces a configuration-error result.
func TestAddCanonicalEdge_NoEdgeWriter(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "person",
			TargetName: "Uwe",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no EdgeWriter wired")
}

// TestAddCanonicalEdge_WriterError: EdgeWriter errors wrap
// through to the ActionResult.
func TestAddCanonicalEdge_WriterError(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{writeErr: errors.New("store unreachable")}
	r := New(Options{EdgeWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType:   "is_about",
			TargetKind: "person",
			TargetName: "Uwe",
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "store unreachable")
}

// TestStubEdgeWriter_ReturnsNotImplemented: the stub
// EdgeWriter (test/dev default) returns ErrActionNotImplemented
// with workflow + source + target context.
func TestStubEdgeWriter_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubEdgeWriter{}.AddCanonicalEdge(
		context.Background(), "wf", "email:m1", "is_about", "person", "Uwe", nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
	assert.Contains(t, err.Error(), "wf")
	assert.Contains(t, err.Error(), "email:m1")
	assert.Contains(t, err.Error(), "is_about")
	assert.Contains(t, err.Error(), "person:Uwe")
}
