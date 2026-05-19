// Tests for the #169 sequential FIFO queue + two-pass
// evaluation + explicit-claim + catch-all semantics. The
// helpers + types here are scoped to this file so the
// existing engine_test scaffolding stays untouched.

package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// twoPassRunner is an actions.Runner that records each
// invocation so two-pass tests can assert order + claim
// stop-chain behavior. Each registered workflow's name is
// pushed onto `fired` when Run is invoked; workflows whose
// name appears in `claimNames` set ActionResult.Claim=true
// so the engine halts the chain at that workflow.
type twoPassRunner struct {
	fired      []string
	claimNames map[string]struct{}
}

func newTwoPassRunner(claimNames ...string) *twoPassRunner {
	set := make(map[string]struct{}, len(claimNames))
	for _, n := range claimNames {
		set[n] = struct{}{}
	}
	return &twoPassRunner{claimNames: set}
}

func (r *twoPassRunner) Run(_ context.Context, wf *parser.Workflow, _ actions.Decision, _ actions.Activation) []actions.ActionResult {
	r.fired = append(r.fired, wf.Name)
	claim := false
	if _, ok := r.claimNames[wf.Name]; ok {
		claim = true
	}
	// One synthetic result per workflow regardless of the
	// workflow's actual action list. The engine reads the
	// Claim flag to decide stop-chain; the action body is
	// otherwise irrelevant in two_pass tests.
	return []actions.ActionResult{{
		ActionIdx: 0,
		Type:      "claim_entity",
		Claim:     claim,
	}}
}

// wfEntityCreated builds an entity_created-triggered
// workflow. Empty kind matches every entity_created event;
// non-empty kind filters to that kind. claimAll wires a
// claim_entity action so the workflow halts the chain when
// recording-runner sees it.
func wfEntityCreated(name, kind string, catchAll bool) *parser.Workflow {
	wf := &parser.Workflow{
		Name:           name,
		Filename:       name + ".md",
		Version:        1,
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEntityCreated,
			Match: parser.TriggerMatch{Kind: kind},
		},
		Subject: "entity.id",
		Actions: []parser.Action{{
			ClaimEntity: &parser.ClaimEntityAction{},
		}},
		CatchAll: catchAll,
	}
	return wf
}

// publishEntity emits an entity.created event + waits for
// the worker to drain so subsequent assertions see the
// finalized run list.
func publishEntity(bus eventbus.Bus, eng *Engine, id, kind string) {
	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID:        id,
		Kind:      kind,
		SourceTag: eventbus.SourceAgent,
		At:        time.Now().UTC(),
	})
	eng.WaitForIdle()
}

// TestTwoPass_Pass1Chain_FirstClaimStopsOthers (#169):
// three regular workflows match the event; filename order
// is `01-` then `02-` then `03-`. The first one's
// claim_entity action halts the chain; workflows 2 + 3
// don't fire.
func TestTwoPass_Pass1Chain_FirstClaimStopsOthers(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner("01-claims")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		{Name: "03-third", Filename: "03-third.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"}, Trigger: parser.Trigger{Type: parser.TriggerTypeEntityCreated},
			Subject: "entity.id", Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}}},
		{Name: "01-claims", Filename: "01-claims.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"}, Trigger: parser.Trigger{Type: parser.TriggerTypeEntityCreated},
			Subject: "entity.id", Actions: []parser.Action{{ClaimEntity: &parser.ClaimEntityAction{}}}},
		{Name: "02-second", Filename: "02-second.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"}, Trigger: parser.Trigger{Type: parser.TriggerTypeEntityCreated},
			Subject: "entity.id", Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'y'"}}}},
	}
	require.NoError(t, eng.Reconcile(wfs))

	publishEntity(bus, eng, "gmail:msg-1", "gmail")

	assert.Equal(t, []string{"01-claims"}, runner.fired,
		"first claim halts the chain — 02-second + 03-third never fire")
}

// TestTwoPass_Pass1_NoClaimFallsThroughToPass2 (#169):
// one regular workflow runs (no claim) + one catch-all
// matches the kind; the catch-all fires after pass-1
// completes without a claim.
func TestTwoPass_Pass1_NoClaimFallsThroughToPass2(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner("catch-gmail")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		{Name: "regular", Filename: "01-regular.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"}, Trigger: parser.Trigger{Type: parser.TriggerTypeEntityCreated},
			Subject: "entity.id", Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}}},
		{Name: "catch-gmail", Filename: "99-catch-gmail.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"}, Trigger: parser.Trigger{Type: parser.TriggerTypeEntityCreated, Match: parser.TriggerMatch{Kind: "gmail"}},
			Subject: "entity.id", CatchAll: true,
			Actions: []parser.Action{{ClaimEntity: &parser.ClaimEntityAction{}}}},
	}
	require.NoError(t, eng.Reconcile(wfs))

	publishEntity(bus, eng, "gmail:msg-1", "gmail")

	assert.Equal(t, []string{"regular", "catch-gmail"}, runner.fired,
		"pass-1 regular runs; no claim → pass-2 catch-all fires")
}

// TestTwoPass_Pass2_KindSpecificOverWildcard (#169):
// pass-2 has both a kind-specific catch-all (kind=gmail) +
// the wildcard catch-all (no kind). Per spec the
// kind-specific wins; wildcard skipped.
func TestTwoPass_Pass2_KindSpecificOverWildcard(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner("catch-gmail")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		wfEntityCreated("catch-gmail", "gmail", true),
		wfEntityCreated("catch-star", "", true),
	}
	require.NoError(t, eng.Reconcile(wfs))

	publishEntity(bus, eng, "gmail:msg-1", "gmail")

	assert.Equal(t, []string{"catch-gmail"}, runner.fired,
		"kind-specific catch-all fires; wildcard skipped")
}

// TestTwoPass_Pass2_WildcardFiresWhenNoKindSpecific (#169):
// pass-2 has only the wildcard catch-all matching the
// event's kind (no kind-specific catch-all registered);
// wildcard fires as the last-resort floor.
func TestTwoPass_Pass2_WildcardFiresWhenNoKindSpecific(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"unregistered:1": {"id": "unregistered:1", "kind": "unregistered"},
	})
	runner := newTwoPassRunner("catch-star")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		wfEntityCreated("catch-star", "", true),
	}
	require.NoError(t, eng.Reconcile(wfs))

	publishEntity(bus, eng, "unregistered:1", "unregistered")

	assert.Equal(t, []string{"catch-star"}, runner.fired,
		"wildcard catch-all fires when no kind-specific catch-all matches")
}

// TestTwoPass_NoClaim_ChainRunsToEnd (#169): no workflow in
// pass-1 or pass-2 sets the claim flag; every matching
// workflow fires + the chain exits cleanly without error.
func TestTwoPass_NoClaim_ChainRunsToEnd(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner() // nobody claims
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		wfEntityCreated("regular-1", "", false),
		wfEntityCreated("catch-all", "gmail", true),
	}
	require.NoError(t, eng.Reconcile(wfs))

	publishEntity(bus, eng, "gmail:msg-1", "gmail")

	assert.Equal(t, []string{"regular-1", "catch-all"}, runner.fired,
		"no claim → both regular + catch-all fire; chain exits cleanly")
}

// TestTwoPass_FilenameOrderDeterminism (#169): three
// workflows registered in REVERSE filename order; the
// worker sorts by filename so they fire `01-`, `02-`, `03-`.
func TestTwoPass_FilenameOrderDeterminism(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner()
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	// Register in reverse so the sort actually does work.
	wfs := []*parser.Workflow{
		wfEntityCreated("c-third", "", false),
		wfEntityCreated("b-second", "", false),
		wfEntityCreated("a-first", "", false),
	}
	// Override Filename so the ordering is explicit.
	wfs[0].Filename = "03-c-third.md"
	wfs[1].Filename = "02-b-second.md"
	wfs[2].Filename = "01-a-first.md"
	require.NoError(t, eng.Reconcile(wfs))

	publishEntity(bus, eng, "gmail:msg-1", "gmail")

	assert.Equal(t, []string{"a-first", "b-second", "c-third"}, runner.fired,
		"workers sort by filename — `01-` fires before `02-` fires before `03-`")
}

// TestTwoPass_SequentialQueueOrdering (#169): two events
// for different entities — the worker processes them in
// enqueue order. With one matching workflow + two events,
// the runner sees the workflow fire twice; assertion
// pivots on the order of EntityIDs the runner observed.
func TestTwoPass_SequentialQueueOrdering(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:first":  {"id": "gmail:first", "kind": "gmail"},
		"gmail:second": {"id": "gmail:second", "kind": "gmail"},
	})
	runner := newSequenceRunner()
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{
		wfEntityCreated("regular", "", false),
	}))

	publishEntity(bus, eng, "gmail:first", "gmail")
	publishEntity(bus, eng, "gmail:second", "gmail")

	assert.Equal(t, []string{"gmail:first", "gmail:second"}, runner.entityIDs(),
		"worker drains in enqueue order — FIFO")
}

// sequenceRunner records the EntityIDs the engine fired
// the workflow against, in arrival order. Single worker
// → single-goroutine writes → atomic.Int32 barrier keeps
// the race detector quiet.
type sequenceRunner struct {
	seen    []string
	barrier atomic.Int32
}

func newSequenceRunner() *sequenceRunner {
	return &sequenceRunner{}
}

func (r *sequenceRunner) Run(_ context.Context, _ *parser.Workflow, dec actions.Decision, _ actions.Activation) []actions.ActionResult {
	r.barrier.Add(1)
	r.seen = append(r.seen, dec.EntityID)
	return nil
}

func (r *sequenceRunner) entityIDs() []string {
	out := make([]string, len(r.seen))
	copy(out, r.seen)
	return out
}
