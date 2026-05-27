package actions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// fakeResolutionDeferredEdgeWriter returns a wrapped
// ResolutionDeferred from AddCanonicalEdge so the action
// runner's C3.2 catch site can be exercised in isolation.
// The wrap is `fmt.Errorf("...: %w", deferred)` matching
// VaultEdgeWriter's real return shape (errors.As must still
// unwrap to the sentinel).
type fakeResolutionDeferredEdgeWriter struct {
	deferred *edgewrite.ResolutionDeferred
	calls    int
}

func (f *fakeResolutionDeferredEdgeWriter) AddCanonicalEdge(
	_ context.Context,
	_, _, _, _, _ string,
	_ map[string]string,
) error {
	f.calls++
	return fmt.Errorf("create canonical edge: %w", f.deferred)
}

type fakeResolutionTaskWriter struct {
	mu        sync.Mutex
	calls     []*edgewrite.ResolutionDeferred
	taskID    string
	created   bool
	writeErr  error
}

func (f *fakeResolutionTaskWriter) WriteResolutionTask(
	_ context.Context, d *edgewrite.ResolutionDeferred,
) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, d)
	if f.writeErr != nil {
		return "", false, f.writeErr
	}
	return f.taskID, f.created, nil
}

func (f *fakeResolutionTaskWriter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

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

func newDeferredFixture() *edgewrite.ResolutionDeferred {
	return &edgewrite.ResolutionDeferred{
		From:           "email:m1",
		EdgeType:       "mentions",
		TargetKind:     "boardgame",
		RawTarget:      "Brass",
		ResolverPlugin: "yaad-bgg",
		Options: map[string]plugins.DisambiguationOption{
			"boardgame:brass-birmingham": {Label: "Brass: Birmingham"},
			"boardgame:brass-lancashire": {Label: "Brass: Lancashire"},
		},
	}
}

// TestAddCanonicalEdge_ResolutionDeferredSpawnsTask is the
// load-bearing C3.2 contract: the runner catches the
// sentinel that VaultEdgeWriter wraps from the centralized
// edge-write service, invokes the resolution-task writer,
// and returns Err=nil + Deferred=true so the engine skips
// the err-task append. The fake writer captures the
// deferred payload so we can assert it propagates verbatim.
func TestAddCanonicalEdge_ResolutionDeferredSpawnsTask(t *testing.T) {
	t.Parallel()
	deferred := newDeferredFixture()
	edge := &fakeResolutionDeferredEdgeWriter{deferred: deferred}
	tw := &fakeResolutionTaskWriter{taskID: "task:boardgame-deadbeef", created: true}
	r := New(Options{EdgeWriter: edge, ResolutionTaskWriter: tw})

	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType: "mentions", TargetKind: "boardgame", TargetName: "Brass",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err, "deferred resolves are NOT errors at the engine layer")
	assert.True(t, results[0].Deferred, "deferred bit signals engine to skip err-task")
	assert.Equal(t, "add_canonical_edge", results[0].Type)

	require.Equal(t, 1, tw.callCount(), "resolution-task writer invoked exactly once")
	assert.Equal(t, deferred, tw.calls[0], "sentinel payload propagates verbatim")
}

// TestAddCanonicalEdge_ResolutionDeferredCollapsesOnRetry
// pins the idempotency-handoff contract: a second fire of
// the same workflow over the same (source, edge, kind, raw)
// hits the writer's filesystem idempotency probe; the
// runner still reports Deferred=true (the workflow's
// "paused, awaiting operator pick" state is unchanged).
// `created=false` on the second call is the C3.1
// writer-side guarantee — the action runner doesn't need
// to gate on it.
func TestAddCanonicalEdge_ResolutionDeferredCollapsesOnRetry(t *testing.T) {
	t.Parallel()
	deferred := newDeferredFixture()
	edge := &fakeResolutionDeferredEdgeWriter{deferred: deferred}
	tw := &fakeResolutionTaskWriter{taskID: "task:boardgame-deadbeef", created: true}
	r := New(Options{EdgeWriter: edge, ResolutionTaskWriter: tw})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType: "mentions", TargetKind: "boardgame", TargetName: "Brass",
		}},
	)
	dec := Decision{Workflow: "classify", EntityID: "email:m1"}

	r1 := r.Run(context.Background(), wf, dec, Activation{})
	require.Len(t, r1, 1)
	assert.True(t, r1[0].Deferred)

	// Second fire — the writer flips `created` to false to
	// mirror C3.1's idempotency-probe shape.
	tw.created = false
	r2 := r.Run(context.Background(), wf, dec, Activation{})
	require.Len(t, r2, 1)
	assert.NoError(t, r2[0].Err)
	assert.True(t, r2[0].Deferred, "deferred bit propagates regardless of created bool")
	assert.Equal(t, 2, tw.callCount(), "every workflow fire invokes the writer; idempotency is writer-side")
}

// TestAddCanonicalEdge_ResolutionDeferredWriterErrorBubbles
// pins that a writer-side failure (filesystem error, store
// outage, etc.) DOES surface as an action error so the
// err-task pattern catches it. Only the "writer landed the
// task" path qualifies as a Deferred success.
func TestAddCanonicalEdge_ResolutionDeferredWriterErrorBubbles(t *testing.T) {
	t.Parallel()
	edge := &fakeResolutionDeferredEdgeWriter{deferred: newDeferredFixture()}
	tw := &fakeResolutionTaskWriter{writeErr: errors.New("disk full")}
	r := New(Options{EdgeWriter: edge, ResolutionTaskWriter: tw})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType: "mentions", TargetKind: "boardgame", TargetName: "Brass",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.False(t, results[0].Deferred, "writer failure is NOT a successful deferral")
	assert.Contains(t, results[0].Err.Error(), "spawn resolution-task")
	assert.Contains(t, results[0].Err.Error(), "disk full")
}

// TestAddCanonicalEdge_ResolutionDeferredFallbackWithoutWriter
// pins the legacy / dev-build path: when no
// ResolutionTaskWriter is wired, the sentinel still surfaces
// as an action error so the err-task pattern captures the
// signal — silently dropping ResolutionDeferred would lose
// the workflow's "paused on ambiguity" state entirely.
func TestAddCanonicalEdge_ResolutionDeferredFallbackWithoutWriter(t *testing.T) {
	t.Parallel()
	deferred := newDeferredFixture()
	edge := &fakeResolutionDeferredEdgeWriter{deferred: deferred}
	r := New(Options{EdgeWriter: edge}) // no ResolutionTaskWriter
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType: "mentions", TargetKind: "boardgame", TargetName: "Brass",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.False(t, results[0].Deferred)
	var got *edgewrite.ResolutionDeferred
	require.True(t, errors.As(results[0].Err, &got), "sentinel preserved through fallback")
	assert.Equal(t, deferred, got)
}

// TestAddCanonicalEdge_NonDeferredErrorBubblesNormally pins
// that the C3.2 catch is narrow — it triggers ONLY on
// ResolutionDeferred. A plain edge-write failure still
// goes through the err-task path.
func TestAddCanonicalEdge_NonDeferredErrorBubblesNormally(t *testing.T) {
	t.Parallel()
	w := &fakeEdgeWriter{writeErr: errors.New("store offline")}
	tw := &fakeResolutionTaskWriter{taskID: "task:should-not-fire"}
	r := New(Options{EdgeWriter: w, ResolutionTaskWriter: tw})
	wf := wfWithActions("classify",
		parser.Action{AddCanonicalEdge: &parser.AddCanonicalEdgeAction{
			EdgeType: "mentions", TargetKind: "boardgame", TargetName: "Brass",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.False(t, results[0].Deferred, "plain errors do NOT mark Deferred")
	assert.Equal(t, 0, tw.callCount(), "resolution-task writer NOT invoked for non-deferred errors")
	assert.Contains(t, results[0].Err.Error(), "store offline")
}
