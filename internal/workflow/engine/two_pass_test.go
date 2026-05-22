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

// kindToList wraps a scalar kind into the canonical_kind list
// shape; empty string yields nil so the filter is "no kind
// narrowing" rather than "match the empty-string kind".
func kindToList(kind string) []string {
	if kind == "" {
		return nil
	}
	return []string{kind}
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
			Match: parser.TriggerMatch{Kinds: kindToList(kind)},
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
			AllowedPlugins: []string{"p"}, Trigger: parser.Trigger{Type: parser.TriggerTypeEntityCreated, Match: parser.TriggerMatch{Kinds: []string{"gmail"}}},
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

// TestEngine_ConcurrentShutdown_NoPanic (#169 fold-in): two
// goroutines call Shutdown simultaneously; the second waits
// for the first via sync.Once. Neither panics.
func TestEngine_ConcurrentShutdown_NoPanic(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(nil)
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Logger: quietLogger(),
	})
	require.NoError(t, err)

	done := make(chan struct{}, 2)
	go func() { eng.Shutdown(); done <- struct{}{} }()
	go func() { eng.Shutdown(); done <- struct{}{} }()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("Shutdown call %d hung — concurrent-Shutdown deadlock", i)
		}
	}
}

// TestEngine_ShutdownDuringProcessing_DrainsCurrentEvent
// (#169 fold-in): Shutdown fires while the worker is mid-
// event. The current event finishes; Shutdown returns once
// the worker exits.
func TestEngine_ShutdownDuringProcessing_DrainsCurrentEvent(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	// blockingRunner: the runner blocks until the test signals
	// `proceed`. While it blocks, Shutdown is invoked from the
	// test goroutine; the worker must drain THIS event before
	// the Shutdown returns.
	proceed := make(chan struct{})
	started := make(chan struct{})
	runner := &blockingRunner{started: started, proceed: proceed}
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{
		wfEntityCreated("regular", "", false),
	}))

	bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
		ID:        "gmail:msg-1",
		Kind:      "gmail",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now().UTC(),
	})
	// Wait until the worker has actually started the event.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started processing the event")
	}

	// Kick off Shutdown from a goroutine; verify the runner
	// can still complete.
	shutdownDone := make(chan struct{})
	go func() {
		eng.Shutdown()
		close(shutdownDone)
	}()
	// Give Shutdown a moment to register, then release the
	// blocking runner.
	time.Sleep(20 * time.Millisecond)
	close(proceed)

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown didn't return after worker finished the event")
	}
	assert.Equal(t, int32(1), runner.calls.Load(),
		"worker drained the current event before exit")
}

type blockingRunner struct {
	started chan struct{}
	proceed chan struct{}
	calls   atomic.Int32
}

func (r *blockingRunner) Run(_ context.Context, _ *parser.Workflow, _ actions.Decision, _ actions.Activation) []actions.ActionResult {
	r.calls.Add(1)
	// Signal once (the first call). Subsequent calls don't
	// signal again — tests that check the started channel
	// don't expect more than one signal per fixture.
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.proceed
	return nil
}

// TestEngine_SendToShutdownEngine_NoPanic (#169 fold-in):
// regression cover for the send-to-closed-channel concern.
// Publish events on the bus AFTER Shutdown; the enqueue
// path drops them silently rather than panicking.
func TestEngine_SendToShutdownEngine_NoPanic(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(nil)
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Logger: quietLogger(),
	})
	require.NoError(t, err)
	eng.Shutdown()

	// Multiple publishes after shutdown — must not panic.
	for i := 0; i < 10; i++ {
		assert.NotPanics(t, func() {
			bus.Publish(context.Background(), eventbus.EntityCreatedEvent{
				ID:        "gmail:msg-after-shutdown",
				Kind:      "gmail",
				SourceTag: eventbus.SourceAgent,
				At:        time.Now().UTC(),
			})
		})
	}
	// WaitForIdle on a shutdown engine returns immediately.
	assert.NotPanics(t, func() { eng.WaitForIdle() })
}

// TestEngine_RegisterCatchAllCollision_Rejects (#169
// fold-in): registerLocked is the authoritative
// catch-all-uniqueness gate. Two workflows with
// `catch_all=true` on the same (trigger.type, kind) slot
// can't both register; the second returns an error.
func TestEngine_RegisterCatchAllCollision_Rejects(t *testing.T) {
	t.Parallel()
	eng, _ := newEngineWithBus(t, nil)
	first := wfEntityCreated("first-catch", "gmail", true)
	second := wfEntityCreated("second-catch", "gmail", true)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{first, second}))
	registered := eng.Registered()
	// Only one of the two registered (the loader-style
	// behavior is preserved at the engine-side rejection too:
	// the first one wins, the second's registerLocked errors,
	// reconcile WARNs + skips).
	assert.Len(t, registered, 1,
		"engine-side rejection drops the second catch-all on the same slot")
}

// TestTwoPass_EdgeTrigger_FilenameOrder (#169 fold-in):
// edge_created trigger fires multiple matching workflows in
// filename order with claim-stop chain. Verifies the
// matchesEvent edge-type filter + the worker's per-event
// sort.
func TestTwoPass_EdgeTrigger_FilenameOrder(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1":      {"id": "gmail:msg-1", "kind": "gmail"},
		"email:msg-target": {"id": "email:msg-target", "kind": "email"},
	})
	runner := newTwoPassRunner("a-first")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		{Name: "a-first", Filename: "01-a-first.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"},
			Trigger: parser.Trigger{Type: parser.TriggerTypeEdgeCreated,
				Match: parser.TriggerMatch{EdgeType: "is_about"}},
			Subject: "edge.to",
			Actions: []parser.Action{{ClaimEntity: &parser.ClaimEntityAction{}}}},
		{Name: "b-second", Filename: "02-b-second.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"},
			Trigger: parser.Trigger{Type: parser.TriggerTypeEdgeCreated,
				Match: parser.TriggerMatch{EdgeType: "is_about"}},
			Subject: "edge.to",
			Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}}},
	}
	require.NoError(t, eng.Reconcile(wfs))

	bus.Publish(context.Background(), eventbus.EntityEdgeAddedEvent{
		FromID:    "gmail:msg-1",
		ToID:      "email:msg-target",
		EdgeType:  "is_about",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now().UTC(),
	})
	eng.WaitForIdle()
	assert.Equal(t, []string{"a-first"}, runner.fired,
		"edge_created: first claim halts the chain; b-second skipped")
}

// TestTwoPass_FillTrigger_FilenameOrder (#169 fold-in):
// fill_completed trigger fires multiple matching workflows
// in filename order with claim-stop chain.
func TestTwoPass_FillTrigger_FilenameOrder(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner("first-fill")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	wfs := []*parser.Workflow{
		{Name: "first-fill", Filename: "01-first-fill.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"},
			Trigger: parser.Trigger{Type: parser.TriggerTypeFillCompleted,
				Match: parser.TriggerMatch{Gap: "hiring_alert_for"}},
			Subject: "entity.id",
			Actions: []parser.Action{{ClaimEntity: &parser.ClaimEntityAction{}}}},
		{Name: "second-fill", Filename: "02-second-fill.md", Version: 1, Status: parser.StatusActive,
			AllowedPlugins: []string{"p"},
			Trigger: parser.Trigger{Type: parser.TriggerTypeFillCompleted,
				Match: parser.TriggerMatch{Gap: "hiring_alert_for"}},
			Subject: "entity.id",
			Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'x'"}}}},
	}
	require.NoError(t, eng.Reconcile(wfs))

	bus.Publish(context.Background(), eventbus.FillCompletedEvent{
		EntityID:  "gmail:msg-1",
		Gap:       "hiring_alert_for",
		SourceTag: eventbus.SourceAgent,
		At:        time.Now().UTC(),
	})
	eng.WaitForIdle()
	assert.Equal(t, []string{"first-fill"}, runner.fired,
		"fill_completed: first claim halts; second-fill skipped")
}

// TestEngine_Decision_ClaimedFlagRecorded (#169 fold-in):
// Decision.Claimed reflects the per-event claim state.
// Operators can read the recorded decision and see which
// workflow claimed without re-deriving from the action-result
// log.
func TestEngine_Decision_ClaimedFlagRecorded(t *testing.T) {
	t.Parallel()
	bus := eventbus.NewMemoryBus()
	resolver := newFakeResolver(map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	})
	runner := newTwoPassRunner("claims")
	eng, err := New(Options{
		Bus: bus, Resolver: resolver, Runner: runner, Logger: quietLogger(),
	})
	require.NoError(t, err)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{
		wfEntityCreated("claims", "", false),
	}))

	publishEntity(bus, eng, "gmail:msg-1", "gmail")

	decs := eng.Decisions()
	require.Len(t, decs, 1)
	assert.True(t, decs[0].Claimed,
		"Decision.Claimed records the post-action claim state")
	assert.Equal(t, "claims", decs[0].Workflow)
}
