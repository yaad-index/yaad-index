package actions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestClaimEntity_SetsClaimFlag (#169): the claim_entity
// action's only observable effect is ActionResult.Claim =
// true. The engine reads this after the per-workflow chain
// runs.
func TestClaimEntity_SetsClaimFlag(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("classify",
		parser.Action{ClaimEntity: &parser.ClaimEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "claim_entity", results[0].Type)
	assert.NoError(t, results[0].Err)
	assert.True(t, results[0].Claim,
		"claim_entity sets ActionResult.Claim per #169")
}

// TestClaimEntity_NonClaimActionsLeaveFlagFalse: every
// non-claim action returns Claim=false (the zero value).
// Guards against accidental setting from other primitives.
func TestClaimEntity_NonClaimActionsLeaveFlagFalse(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})
	wf := wfWithActions("classify",
		parser.Action{TaskAppend: &parser.TaskAppendAction{
			Section: "candidates",
			Content: "x",
		}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 1)
	assert.False(t, results[0].Claim,
		"task_append leaves Claim=false")
}

// TestClaimEntity_AlongsideOtherActionsInWorkflow: a workflow
// with task_append + claim_entity in declaration order —
// both run (intra-workflow chain isn't halted by Claim), and
// only the claim_entity result carries Claim=true.
//
// The engine's stop-after-claim logic gates further WORKFLOWS,
// not further actions within the same workflow.
func TestClaimEntity_AlongsideOtherActionsInWorkflow(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})
	wf := wfWithActions("classify-and-claim",
		parser.Action{TaskAppend: &parser.TaskAppendAction{
			Section: "candidates",
			Content: "task line",
		}},
		parser.Action{ClaimEntity: &parser.ClaimEntityAction{}},
	)
	results := r.Run(context.Background(), wf,
		Decision{Workflow: "classify-and-claim", EntityID: "gmail:msg-1"},
		Activation{})
	require.Len(t, results, 2)
	assert.Equal(t, "task_append", results[0].Type)
	assert.False(t, results[0].Claim, "task_append result has Claim=false")
	assert.Equal(t, "claim_entity", results[1].Type)
	assert.True(t, results[1].Claim, "claim_entity result has Claim=true")
	// Task writer was still called — actions inside a single
	// workflow all run regardless of subsequent claim.
	assert.Len(t, w.snapshot(), 1, "task_append ran before the claim")
}
