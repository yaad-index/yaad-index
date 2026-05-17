package actions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

type fakePropertyWriter struct {
	mu       sync.Mutex
	calls    []propertyCall
	writeErr error
}

type propertyCall struct {
	workflow string
	entityID string
	fields   map[string]any
}

func (f *fakePropertyWriter) SetProperties(_ context.Context, workflow, entityID string, fields map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make(map[string]any, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	f.calls = append(f.calls, propertyCall{workflow: workflow, entityID: entityID, fields: copied})
	return f.writeErr
}

func (f *fakePropertyWriter) snapshot() []propertyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]propertyCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// busRecorder collects every published event for assertion.
type busRecorder struct {
	mu     sync.Mutex
	events []eventbus.Event
}

func (b *busRecorder) record() func(context.Context, eventbus.Event) {
	return func(_ context.Context, e eventbus.Event) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.events = append(b.events, e)
	}
}

func (b *busRecorder) snapshot() []eventbus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]eventbus.Event, len(b.events))
	copy(out, b.events)
	return out
}

// TestSetProperty_HappyPath: a set_property action with
// static field values lands on the writer with target
// defaulting to dec.EntityID.
func TestSetProperty_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("classify",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Fields: map[string]string{
				"repo": "'example-org/my-project'",
				"type": "'note'",
			},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1"},
		Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	got := w.snapshot()[0]
	assert.Equal(t, "email:m1", got.entityID)
	assert.Equal(t, "classify", got.workflow)
	// Runner falls back to raw expressions when no engine
	// pre-render was supplied; this happy-path verifies the
	// fields map flowed through key-by-key (CEL-quoted-string
	// shape stays verbatim).
	assert.Equal(t, "'example-org/my-project'", got.fields["repo"])
	assert.Equal(t, "'note'", got.fields["type"])
}

// TestSetProperty_ExplicitEntity: action.entity wins over
// dec.EntityID.
func TestSetProperty_ExplicitEntity(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Entity: "'pr:456'",
			Fields: map[string]string{"x": "'y'"},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "email:m1"},
		Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "'pr:456'", w.snapshot()[0].entityID,
		"raw fallback used when engine has no pre-render — verifies the resolution order")
}

// TestSetProperty_NoTarget_AuthorBug: no action.entity + no
// dec.EntityID → ErrActionAuthorBug.
func TestSetProperty_NoTarget_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Fields: map[string]string{"x": "'y'"},
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Empty(t, w.snapshot())
}

// TestSetProperty_EmptyFields_AuthorBug: empty fields map
// rejected as an author bug (parser validate also rejects
// this, but the runner double-checks for robustness against
// callers that bypass validate).
func TestSetProperty_EmptyFields_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "e1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestSetProperty_WriterError: PropertyWriter errors surface
// with the underlying cause wrapped.
func TestSetProperty_WriterError(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{writeErr: errors.New("vault unavailable")}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Fields: map[string]string{"x": "'y'"},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "e1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault unavailable")
}

// TestSetProperty_NoPropertyWriter: dispatcher without a
// PropertyWriter wired surfaces a clear engine-misconfig error
// rather than silently dropping the action.
func TestSetProperty_NoPropertyWriter(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Fields: map[string]string{"x": "'y'"},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "e1"},
		Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no PropertyWriter wired")
}

// TestSetProperty_PublishesFillCompletedPerField: one
// fill.completed event per landed field, deterministic order
// (sorted ascending field names), Source=workflow:<name>.
func TestSetProperty_PublishesFillCompletedPerField(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	bus := eventbus.NewMemoryBus()
	rec := &busRecorder{}
	sub := bus.Subscribe(eventbus.TopicFillCompleted, rec.record())
	defer sub.Unsubscribe()

	r := New(Options{PropertyWriter: w, Bus: bus})
	when := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	wf := wfWithActions("classify",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Fields: map[string]string{
				"type": "'note'",
				"repo": "'example-org/my-project'",
				"ref":  "'#42'",
			},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "email:m1", At: when},
		Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)

	got := rec.snapshot()
	require.Len(t, got, 3, "one event per field")
	// Sorted field names → deterministic delivery order.
	assert.Equal(t, "ref", got[0].(eventbus.FillCompletedEvent).Gap)
	assert.Equal(t, "repo", got[1].(eventbus.FillCompletedEvent).Gap)
	assert.Equal(t, "type", got[2].(eventbus.FillCompletedEvent).Gap)
	for i, ev := range got {
		fc := ev.(eventbus.FillCompletedEvent)
		assert.Equal(t, "email:m1", fc.EntityID, "events[%d].entity_id", i)
		assert.Equal(t, eventbus.WorkflowSource("classify"), fc.SourceTag, "events[%d].source", i)
		assert.Equal(t, when, fc.At, "events[%d].at must match Decision.At for the self-loop backstop", i)
	}
}

// TestSetProperty_NilBus_SkipsEmission: when no bus is wired
// the runner still writes successfully — emission is
// optional, error path is decoupled.
func TestSetProperty_NilBus_SkipsEmission(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Fields: map[string]string{"x": "'y'"},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "e1"},
		Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1, "write still happens")
}

// TestSetProperty_UsesRenderedTemplates: when the engine
// supplies RenderedTemplates, the runner picks rendered values
// per `entity` + `field:<name>` keys (versus falling back to
// the raw expressions).
func TestSetProperty_UsesRenderedTemplates(t *testing.T) {
	t.Parallel()
	w := &fakePropertyWriter{}
	r := New(Options{PropertyWriter: w})
	wf := wfWithActions("wf",
		parser.Action{SetProperty: &parser.SetPropertyAction{
			Entity: "entity.id",
			Fields: map[string]string{
				"repo": "entity.data.repo",
				"ref":  "entity.data.ref",
			},
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "wf", EntityID: "fallback-not-used"},
		Activation{
			RenderedTemplates: map[int]map[string]string{
				0: {
					"entity":     "email:m1",
					"field:repo": "example-org/my-project",
					"field:ref":  "#42",
				},
			},
		})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	got := w.snapshot()[0]
	assert.Equal(t, "email:m1", got.entityID,
		"rendered entity wins over decision.entity_id fallback")
	assert.Equal(t, "example-org/my-project", got.fields["repo"])
	assert.Equal(t, "#42", got.fields["ref"])
}

// TestStubPropertyWriter_ReturnsNotImplemented: the production-
// default writer surfaces ErrActionNotImplemented with workflow
// + entity + field-count for operator debugging.
func TestStubPropertyWriter_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubPropertyWriter{}.SetProperties(
		context.Background(), "wf", "email:m1",
		map[string]any{"x": "y"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
	assert.Contains(t, err.Error(), "wf")
	assert.Contains(t, err.Error(), "email:m1")
}
