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

// fakeGapWriter records every AddGap call.
type fakeGapWriter struct {
	mu       sync.Mutex
	calls    []gapCall
	writeErr error
}

type gapCall struct {
	workflow string
	entityID string
	gap      string
	inj      GapInjection
}

func (f *fakeGapWriter) AddGap(_ context.Context, workflow, entityID, gap string, inj GapInjection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, gapCall{
		workflow: workflow,
		entityID: entityID,
		gap:      gap,
		inj:      inj,
	})
	return f.writeErr
}

func (f *fakeGapWriter) snapshot() []gapCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gapCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestAddGap_HappyPath: gap declared in addable_gaps,
// target defaults to dec.EntityID.
func TestAddGap_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "is_interesting_to_me"}},
	)
	wf.AddableGaps = []string{"is_interesting_to_me", "owned_status"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	assert.Equal(t, "email:m1", w.snapshot()[0].entityID)
	assert.Equal(t, "is_interesting_to_me", w.snapshot()[0].gap)
}

// TestAddGap_VocabularyEnforcement_RuntimeReject: per
// ADR-0024 §"Constraints on add_gap", the gap MUST appear
// in the workflow's addable_gaps. The runtime check
// catches the case where the static parser-side check has
// drifted (e.g. via hot-reload).
func TestAddGap_VocabularyEnforcement_RuntimeReject(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "not_in_vocab"}},
	)
	wf.AddableGaps = []string{"is_interesting_to_me"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "classify", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Contains(t, results[0].Err.Error(), "addable_gaps vocabulary")
	assert.Empty(t, w.snapshot(), "no writer call on vocabulary rejection")
}

// TestAddGap_EmptyGap_AuthorBug: empty gap name.
func TestAddGap_EmptyGap_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddGap: &parser.AddGapAction{}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddGap_ExplicitEntity: action.entity overrides the
// triggering entity_id.
func TestAddGap_ExplicitEntity(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddGap: &parser.AddGapAction{Entity: "email:other", Gap: "g"}},
	)
	wf.AddableGaps = []string{"g"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "email:trigger"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "email:other", w.snapshot()[0].entityID)
}

// TestAddGap_NoTarget_AuthorBug: empty entity + empty
// decision.entity_id → no target → author bug.
func TestAddGap_NoTarget_AuthorBug(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "g"}},
	)
	wf.AddableGaps = []string{"g"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestAddGap_WriterError: GapWriter errors wrap through.
func TestAddGap_WriterError(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{writeErr: errors.New("vault unavailable")}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "g"}},
	)
	wf.AddableGaps = []string{"g"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "vault unavailable")
}

// TestStubGapWriter_ReturnsNotImplemented: the stub
// GapWriter (test/dev default) returns
// ErrActionNotImplemented with the workflow + entity + gap.
func TestStubGapWriter_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubGapWriter{}.AddGap(context.Background(), "wf", "email:m1", "is_interesting_to_me", GapInjection{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
	assert.Contains(t, err.Error(), "wf")
	assert.Contains(t, err.Error(), "email:m1")
	assert.Contains(t, err.Error(), "is_interesting_to_me")
}

// TestAddGap_DataSchemaPassedThrough: an AddGapAction carrying
// data_schema (#117) flows the map through to the GapWriter so
// the vault impl persists it on the GapStateEntry.
func TestAddGap_DataSchemaPassedThrough(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	schema := map[string]string{
		"role":   "the role title in the hiring alert",
		"salary": "salary range if mentioned, else omit",
	}
	wf := wfWithActions("linkedin-classify",
		parser.Action{AddGap: &parser.AddGapAction{
			Gap:        "hiring_alert_for",
			DataSchema: schema,
		}},
	)
	wf.AddableGaps = []string{"hiring_alert_for"}
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "linkedin-classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, schema, calls[0].inj.DataSchema,
		"data_schema flows to GapWriter verbatim")
}

// TestAddGap_DataSchemaNilFlowsAsNil: an AddGapAction without
// data_schema (the legacy shape) passes nil to the GapWriter so
// vault impls can branch cleanly on "schema absent".
func TestAddGap_DataSchemaNilFlowsAsNil(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("wf",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "g"}},
	)
	wf.AddableGaps = []string{"g"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf", EntityID: "e1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	calls := w.snapshot()
	require.Len(t, calls, 1)
	assert.Nil(t, calls[0].inj.DataSchema)
}

// TestAddGap_WorkflowAttribution: the workflow name from the
// recorded Decision flows through to the GapWriter as the
// first arg, mirroring the add_note attribution pattern.
func TestAddGap_WorkflowAttribution(t *testing.T) {
	t.Parallel()
	w := &fakeGapWriter{}
	r := New(Options{GapWriter: w})
	wf := wfWithActions("classify",
		parser.Action{AddGap: &parser.AddGapAction{Gap: "is_interesting_to_me"}},
	)
	wf.AddableGaps = []string{"is_interesting_to_me"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "classify", EntityID: "email:m1"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, w.snapshot(), 1)
	assert.Equal(t, "classify", w.snapshot()[0].workflow)
}
