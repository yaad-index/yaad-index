// Package engine wires the workflow-registry + event-bus +
// decision-evaluator into the runtime substrate that drives
// workflows per ADR-0024. Each registered workflow gets its own
// decision.Evaluator (one Evaluator per workflow per the
// per-workflow-bindings design) + a set of bus subscriptions
// keyed by Trigger.Type. Incoming events route to matching
// workflows; the engine evaluates context bindings + the
// condition predicate + records the resulting Decision.
//
// **Phase 3.B scope.** This package ships the orchestration
// substrate up to and including decision recording. Action
// execution (task_append / add_note / plugin_dispatch /
// add_gap) is Phase 4. The manual-trigger HTTP / CLI entry
// points are Phase 3.C. The decision-recording surface (in-
// memory ring + slog Info) gives operators visibility into
// what fires + lays groundwork for Phase 4's action runners.

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
	"github.com/yaad-index/yaad-index/internal/workflow/template"
)

// EntityResolver is the entity-fetching surface the engine
// needs at event time. Returns the entity's flattened
// data-map (the same shape decision.GraphLookup uses) keyed
// by the canonical id. Production wires a *store.Store-backed
// adapter; tests substitute fakes.
//
// Returns decision.ErrEntityNotFound when no entity matches
// id so the engine can surface a missing-reference note
// rather than an err-task.
type EntityResolver interface {
	Resolve(ctx context.Context, id string) (map[string]any, error)
}

// IngestRouter is the URL-shape input resolution surface the
// engine uses for Dispatch per ADR-0024 §"workflow.trigger(input)
// input semantics" — when the manual-trigger input is a URL
// rather than a canonical entity id, the engine routes
// through the daemon's plugin pipeline before attaching the
// workflow.
//
// Production wires an adapter around api.SyncIngester so the
// HTTP /v1/ingest path + workflow URL routing share one
// tracker (job-map dedup, cache-TTL gate, persistence). Tests
// substitute in-memory fakes that return canned id-or-error
// pairs.
//
// IngestURL returns the canonical entity id after the ingest
// pipeline reaches a terminal state. timeout caps the wait;
// a tracker still in progress when timeout fires surfaces as
// context.DeadlineExceeded.
type IngestRouter interface {
	IngestURL(ctx context.Context, url string, timeout time.Duration) (entityID string, err error)
}

// Decision captures one workflow's evaluation against one
// incoming event. Engine records these in a bounded in-memory
// ring buffer for operator inspection + Phase 4 action-runner
// consumption. The fields are publish-snapshot — callers don't
// mutate.
type Decision struct {
	// Workflow is the workflow.Name that produced this
	// decision.
	Workflow string

	// EntityID is the triggering entity's canonical id.
	// Empty for manual triggers without an entity target
	// (Phase 3.C).
	EntityID string

	// Subject is the rendered subject string (workflow's
	// `subject` template evaluated against the activation).
	// Used by Phase 4 to derive task paths.
	Subject string

	// Fired reports whether the condition predicate evaluated
	// true. Fired=false means the trigger matched but the
	// predicate rejected — no action runs. The engine still
	// records this for visibility.
	Fired bool

	// MissingRefs is the union of context-binding +
	// condition-eval missing-reference notes per ADR-0024
	// §"Missing-reference handling". Phase 4 surfaces these
	// as notes on the resulting task.
	MissingRefs []decision.MissingRef

	// Err, when non-nil, names a non-MissingRef failure
	// during evaluation (cel-go runtime error, store-side
	// resolve failure, etc.). Phase 5 routes these to the
	// per-workflow err-task pattern.
	Err error

	// At is the wall-clock instant this decision was
	// recorded. Phase 5 ordering / debounce reads this.
	At time.Time

	// DedupKey is the rendered per-pattern dedup key from
	// workflow.Dedup.Key per ADR-0024 §"Per-pattern
	// de-duplication". Stamped on the resulting task's
	// frontmatter so cross-fire identity stays
	// inspectable. Empty when the workflow's Dedup.Key
	// rendered to empty / failed to render — the engine
	// proceeds with action dispatch but doesn't stamp.
	DedupKey string

	// DedupPolicyApplied names the per-pattern dedup policy
	// the engine applied for this fire — one of "update"
	// (default; action runner dispatched normally),
	// "skip" (action runner SKIPPED because the dedup key
	// was already seen), or "replace" (Phase 5.A: not yet
	// implemented; logged + dispatched as update). Empty
	// when no dedup applied (Fired=false or err-path).
	DedupPolicyApplied string

	// SuppressedByCycle is true when the structural
	// cycle-detection backstop tripped on this fire per
	// #147 / ADR-0024 §"Self-loop detection": the
	// triggering event's workflow chain already names this
	// workflow, so firing it again would close a loop. The
	// evaluation pipeline short-circuits before condition
	// eval — Fired stays false, MissingRefs / Subject /
	// DedupKey stay empty, action runner doesn't run. The
	// first cycle-tripped fire of a workflow chain also
	// writes an err-task entry naming the offending chain;
	// subsequent cycle suppressions on the same chain skip
	// the err-task to avoid spam.
	SuppressedByCycle bool

	// CycleChain is the workflow-name list the engine saw on
	// the triggering event when SuppressedByCycle is true.
	// Logged + persisted on the err-task so operators can
	// trace W1→W2→W3→W1 loops by name. Empty when not
	// suppressed.
	CycleChain []string

	// Claimed reports whether this workflow's action chain
	// fired a `claim_entity` action per #169. The engine
	// reads the bool to halt the per-event chain (no further
	// pass-1 workflows fire, no pass-2 catch-all fires); the
	// recorded Decision carries it for post-event
	// observability so operators can trace which workflow
	// took ownership of a given event without re-deriving
	// from the action-result log.
	Claimed bool
}

// Options configures an Engine.
type Options struct {
	// Bus is the daemon-internal pub-sub substrate the engine
	// subscribes against. Required.
	Bus eventbus.Bus

	// Resolver fetches entities by id. Required for any
	// event-driven workflow (resolver-nil makes every
	// trigger's entity-resolve fail with missing-reference;
	// useful for tests where the entity body doesn't matter).
	Resolver EntityResolver

	// Runner executes the workflow's action list when
	// Fired=true. Nil → actions.NopRunner (records the
	// action types without running side effects). The
	// production-wired Runner (Phase 4) routes per-action
	// primitives to their implementations.
	Runner actions.Runner

	// IngestRouter resolves URL-shape Dispatch inputs to a
	// canonical entity id via the daemon's plugin pipeline
	// per ADR-0024 §"workflow.trigger(input) input
	// semantics". Nil → URL-shape Dispatch inputs return
	// ErrURLInputNotSupported (preserves legacy entity-id +
	// empty-input paths for tests / dev binaries without a
	// plugin pipeline).
	IngestRouter IngestRouter

	// IngestTimeout caps the per-call wait on IngestRouter.
	// Zero → DefaultIngestTimeout. Maps to the tracker's
	// long-poll budget; a URL still being ingested when the
	// budget fires surfaces as context.DeadlineExceeded.
	IngestTimeout time.Duration

	// Logger receives the per-decision Info line + any
	// engine-internal warnings. Nil → discarding handler.
	Logger *slog.Logger

	// DecisionRingSize caps the in-memory decision buffer
	// (most-recent-N retained for Decisions() snapshot).
	// Zero → DefaultDecisionRingSize.
	DecisionRingSize int

}

// DefaultIngestTimeout caps Engine.Dispatch's wait on the
// configured IngestRouter when Options.IngestTimeout is unset.
// Sized to cover ADR-0022's 30s plugin-dispatch budget plus
// modest tracker overhead. Production main.go can override
// per its operator-config.
const DefaultIngestTimeout = 60 * time.Second

// DefaultDecisionRingSize bounds the engine's in-memory
// decision history when DecisionRingSize is unset. Sized to
// cover ~30min of workflow firing on a small operator-side
// setup; future v1.x can revisit if operators ship larger
// flows.
const DefaultDecisionRingSize = 1024

// Engine orchestrates workflow registration + event routing +
// decision recording. Construct via New; populate via
// Reconcile(workflows); use Decisions() to inspect the
// recorded history.
type Engine struct {
	bus           eventbus.Bus
	resolver      EntityResolver
	runner        actions.Runner
	errTaskWriter actions.ErrTaskWriter
	ingestRouter  IngestRouter
	ingestTimeout time.Duration
	logger        *slog.Logger
	ringSize      int

	mu        sync.RWMutex
	workflows map[string]*registeredWorkflow

	decMu     sync.Mutex
	decisions []Decision

	// cycleMu guards cycleLogged — the set of workflow
	// chains the engine has already err-tasked for cycle
	// detection per #147. A chain "fingerprint" (joined
	// workflow names + the closing workflow) trips once and
	// stays in the set until a process restart so the same
	// loop doesn't spam the err-task on every re-fire.
	cycleMu     sync.Mutex
	cycleLogged map[string]struct{}

	// dedupMu guards dedupSeen — the set of namespaced
	// dedup keys this engine has already produced a fire
	// against. Used by policy=skip to short-circuit
	// repeated fires of the same (workflow, dedup-key)
	// pair. policy=update + policy=replace also stamp here
	// so a switch from update→skip on a hot-reloaded
	// workflow doesn't lose track. Restart-bounded: a
	// daemon restart resets the set; the next fire treats
	// every key as fresh.
	//
	// Keyed by a (workflow-name, rendered-key) struct so
	// arbitrary characters in either side can't collide
	// across distinct pairs (a string-join scheme with `|`
	// or similar separators is injective only when neither
	// side contains the separator; struct-key avoids the
	// constraint).
	dedupMu   sync.Mutex
	dedupSeen map[dedupID]struct{}

	// Per #169: in-memory FIFO event queue + single worker
	// goroutine. The engine subscribes ONCE per topic (not
	// per workflow) and each handler enqueues a queuedEvent;
	// the worker drains the queue + runs the two-pass
	// evaluation. Bounded buffered chan: at capacity the
	// engine-level handler blocks the bus publisher; the
	// expected steady-state is far below capacity (workflow
	// fires lag bus emits by milliseconds).
	//
	// **Lifecycle invariant.** The queue is created in New
	// + NEVER closed. Senders (`enqueueEvent`, `WaitForIdle`)
	// gate their send on `shutdownCh` via select; the worker
	// drains the queue + exits when `shutdownCh` closes. This
	// avoids the send-to-closed-channel panic that occurs
	// when Shutdown races with an in-flight enqueue.
	queue        chan queuedEvent
	workerWG     sync.WaitGroup
	topicSubs    []eventbus.Subscription
	shutdownCh   chan struct{}
	startedOnce  sync.Once
	shutdownOnce sync.Once
}

// queuedEvent carries the bus event + dispatch context into
// the engine's FIFO queue. The worker dequeues + runs the
// two-pass evaluation per #169.
//
// When `barrier` is non-nil the event is a synchronization
// marker (no real event payload): the worker closes the
// barrier channel after processing every event ahead of it
// in the queue. WaitForIdle uses this to let tests assert
// post-event state without polling.
type queuedEvent struct {
	ctx     context.Context
	event   eventbus.Event
	barrier chan struct{}
}

// queueCapacity is the buffered chan size for the engine's
// event queue. Sized generously — the worker should drain at
// near-zero latency vs the bus emit rate; capacity exists to
// absorb bursts (multiple bus emits inside a single
// PendingEvents.Drain, for example) without backpressuring
// the publisher.
const queueCapacity = 1024

// dedupID names the engine-internal dedup-set key — the
// (workflow, rendered-key) pair under policy-suppression
// tracking.
type dedupID struct {
	workflow string
	key      string
}

// backstopKey names the engine-internal runaway-fire
// registeredWorkflow holds the per-workflow runtime state:
// the parsed Workflow + its compiled programs. Per #169 the
// engine no longer subscribes per-workflow; bus topics are
// subscribed once at engine level + the worker matches each
// event against the registered set.
type registeredWorkflow struct {
	workflow     *parser.Workflow
	evaluator    *decision.Evaluator
	condition    *decision.Program // nil when workflow.Condition is empty (always-fire)
	subject      *template.Template
	dedupKey     *template.Template // compiled dedup.Key template; nil iff workflow.Dedup.Key empty
	contextBinds []compiledBinding
	// actionTemplates is the per-action map of compiled
	// template fields. Indexed by action position →
	// fieldName ("target" / "content" / "entity") →
	// compiled template. Built once at registration time;
	// each event-fire renders the templates against the
	// activation and ships the rendered values to the
	// action runner via actions.Activation.RenderedTemplates.
	actionTemplates []map[string]*template.Template
	// filename is the parser-stamped source filename (e.g.
	// "01-classify-linkedin.md"). The worker sorts pass-1 +
	// pass-2 workflows by this field for deterministic
	// execution order per #169 — operators use `01-`, `02-`
	// prefixes to control priority within a kind.
	filename string
	// catchAll is the parser-stamped catch_all flag mirrored
	// here for fast partitioning into pass-1 (regular,
	// catchAll=false) vs pass-2 (fallback, catchAll=true).
	catchAll bool
}

// compiledBinding ties a workflow's context entry to its
// compiled .via program. Evaluated in order before the
// condition predicate fires.
type compiledBinding struct {
	name    string
	program *decision.Program
}

// New constructs an engine with the given options. Returns
// an error if Bus is nil.
func New(opts Options) (*Engine, error) {
	if opts.Bus == nil {
		return nil, errors.New("engine: Bus is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ring := opts.DecisionRingSize
	if ring <= 0 {
		ring = DefaultDecisionRingSize
	}
	runner := opts.Runner
	if runner == nil {
		runner = actions.NopRunner{}
	}
	ingestTimeout := opts.IngestTimeout
	if ingestTimeout <= 0 {
		ingestTimeout = DefaultIngestTimeout
	}
	e := &Engine{
		bus:           opts.Bus,
		resolver:      opts.Resolver,
		runner:        runner,
		errTaskWriter: actions.ErrTaskWriterFor(runner),
		ingestRouter:  opts.IngestRouter,
		ingestTimeout: ingestTimeout,
		logger:        logger,
		ringSize:      ring,
		workflows:     make(map[string]*registeredWorkflow),
		dedupSeen:     make(map[dedupID]struct{}),
		cycleLogged:   make(map[string]struct{}),
		queue:         make(chan queuedEvent, queueCapacity),
		shutdownCh:    make(chan struct{}),
	}
	// Auto-start the worker + subscribe to bus topics so
	// production main.go doesn't need an explicit Start +
	// existing tests construct the engine the same way they
	// did pre-#169. Tests that need to assert post-event
	// state call WaitForIdle to barrier through the queue.
	e.Start()
	return e, nil
}

// Start subscribes the engine to bus topics (one subscription
// per topic — NOT per-workflow per #169) + spawns the single
// worker goroutine that drains the event queue. Idempotent:
// subsequent calls are no-ops; the engine wires its bus
// subscriptions exactly once over its lifetime.
//
// Callers (typically main.go right after engine.New) must
// invoke Start before the first Reconcile so the queue is
// live when events arrive. Shutdown must be called before
// process exit to drain in-flight events.
func (e *Engine) Start() {
	e.startedOnce.Do(func() {
		e.topicSubs = []eventbus.Subscription{
			e.bus.Subscribe(eventbus.TopicEntityEdgeAdded, e.enqueueEvent),
			e.bus.Subscribe(eventbus.TopicEntityCreated, e.enqueueEvent),
			e.bus.Subscribe(eventbus.TopicFillCompleted, e.enqueueEvent),
		}
		e.workerWG.Add(1)
		go e.workerLoop()
	})
}

// Shutdown stops the engine's worker. Unsubscribes from the
// bus first so no new events enqueue, signals the worker
// via shutdownCh, then waits for the worker to finish its
// current event + exit. Safe to call concurrently from
// multiple goroutines (sync.Once guards the body).
//
// **Drain semantics.** The worker exits as soon as
// shutdownCh fires — any events currently buffered in the
// queue are NOT processed. For in-memory v1 this matches
// the upstream-emitter shape (publishers re-emit on
// restart). Callers expecting a full-drain shutdown need
// to ensure no new events enqueue before Shutdown returns.
//
// Workflow re-registration via Reconcile after Shutdown is
// undefined; callers should treat the engine as terminal
// after Shutdown returns.
func (e *Engine) Shutdown() {
	e.shutdownOnce.Do(func() {
		for _, s := range e.topicSubs {
			s.Unsubscribe()
		}
		e.topicSubs = nil
		close(e.shutdownCh)
		e.workerWG.Wait()
	})
}

// enqueueEvent is the engine-level bus handler — subscribed
// once per topic by Start. Each bus emit lands a queuedEvent
// in the FIFO queue; the worker processes events in arrival
// order per #169. The send is gated on shutdownCh so a
// concurrent Shutdown can never panic this caller — the
// queue is NEVER closed (Shutdown signals via shutdownCh
// only). When shutdown fires the event is dropped.
func (e *Engine) enqueueEvent(ctx context.Context, ev eventbus.Event) {
	if ev == nil {
		return
	}
	select {
	case <-e.shutdownCh:
		// Engine is shutting down; drop the event.
		return
	case e.queue <- queuedEvent{ctx: ctx, event: ev}:
	}
}

// workerLoop drains the event queue + processes each event
// with the two-pass evaluation per #169. Exits when
// shutdownCh closes. Barrier events (qe.barrier non-nil)
// skip processEvent and close the barrier channel so
// WaitForIdle can return.
//
// Select chooses pseudo-randomly between shutdownCh +
// queue when both are ready — buffered events may or may
// not be processed during shutdown. Matches the
// "drain current event, then exit" shape documented on
// Shutdown; upstream emitters re-fire on restart so
// dropped buffered events are not load-bearing for v1.
func (e *Engine) workerLoop() {
	defer e.workerWG.Done()
	for {
		select {
		case <-e.shutdownCh:
			return
		case qe := <-e.queue:
			if qe.barrier != nil {
				close(qe.barrier)
				continue
			}
			e.processEvent(qe)
		}
	}
}

// WaitForIdle blocks until the worker has processed every
// event currently in the queue. Implements the barrier-event
// pattern: enqueue a marker, wait for the worker to reach
// it. Tests use this to barrier through async dispatch
// before asserting post-event state.
//
// Returns immediately if the engine is already shut down
// (the worker has exited and won't process new events).
// The barrier-send is gated on shutdownCh so a concurrent
// Shutdown can never panic this caller.
func (e *Engine) WaitForIdle() {
	barrier := make(chan struct{})
	select {
	case <-e.shutdownCh:
		return
	case e.queue <- queuedEvent{barrier: barrier}:
	}
	select {
	case <-barrier:
	case <-e.shutdownCh:
	}
}

// Reconcile diffs the desired workflow set against the
// engine's current registrations and brings them into sync:
//   - workflows present in `want` but not registered → register
//     (compile programs + subscribe to bus).
//   - workflows registered but not in `want` → unregister
//     (unsubscribe + release).
//   - workflows in both with same identity (Name) but
//     potentially-changed shape → re-register (drop old + add
//     new) so a hot-reload edit picks up the new compile.
//
// Per-workflow compile failures log a WARN line + skip the
// registration (the workflow stays out of the active set
// until the operator fixes it). Other errors propagate.
//
// Callers (typically main.go's Loader-tick wrapper) call
// Reconcile on each registry refresh.
func (e *Engine) Reconcile(workflows []*parser.Workflow) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	want := make(map[string]*parser.Workflow, len(workflows))
	for _, wf := range workflows {
		want[wf.Name] = wf
	}

	// Unregister workflows no longer present (or about to be
	// re-registered with new shape).
	for name, reg := range e.workflows {
		if _, kept := want[name]; !kept {
			e.unregisterLocked(name, reg)
			continue
		}
		// Always treat re-Reconcile as a fresh registration
		// for simplicity — the cost is bounded (compile is
		// cheap + cached at the Evaluator level for repeated
		// expressions). This handles mtime-bumped re-loads
		// without diff-tracking the workflow's internal
		// shape.
		e.unregisterLocked(name, reg)
	}

	// Register everything in want (the loop above cleared
	// any prior entries for the same Name).
	for name, wf := range want {
		if err := e.registerLocked(wf); err != nil {
			e.logger.Warn("workflow engine: registration failed; skipping",
				"workflow", name, "err", err)
		}
	}
	return nil
}

// registerLocked compiles the workflow's programs + sets up
// bus subscriptions. Called with e.mu held.
func (e *Engine) registerLocked(wf *parser.Workflow) error {
	bindings := make([]string, 0, len(wf.Context))
	for _, b := range wf.Context {
		bindings = append(bindings, b.Name)
	}
	ev, err := decision.NewEvaluator(decision.Options{
		Lookup:   &resolverGraphLookup{resolver: e.resolver},
		Bindings: bindings,
	})
	if err != nil {
		return fmt.Errorf("decision evaluator: %w", err)
	}

	reg := &registeredWorkflow{
		workflow:  wf,
		evaluator: ev,
		filename:  wf.Filename,
		catchAll:  wf.CatchAll,
	}

	if wf.Condition != "" {
		prog, err := ev.Compile(wf.Condition, "bool")
		if err != nil {
			return fmt.Errorf("compile condition: %w", err)
		}
		reg.condition = prog
	}
	if wf.Subject != "" {
		tpl, err := template.Compile(wf.Subject, ev)
		if err != nil {
			return fmt.Errorf("compile subject: %w", err)
		}
		reg.subject = tpl
	}
	if wf.Dedup.Key != "" {
		tpl, err := template.Compile(wf.Dedup.Key, ev)
		if err != nil {
			return fmt.Errorf("compile dedup.key: %w", err)
		}
		reg.dedupKey = tpl
	}
	for _, cb := range wf.Context {
		prog, err := ev.Compile(cb.Via, "dyn")
		if err != nil {
			return fmt.Errorf("compile context.%s.via: %w", cb.Name, err)
		}
		reg.contextBinds = append(reg.contextBinds, compiledBinding{
			name:    cb.Name,
			program: prog,
		})
	}

	// Compile per-action template fields once at register
	// time. Each entry in actionTemplates is the rendered-
	// field map for the action at the same index; fields not
	// applicable to the action's primitive (e.g. "target" on
	// a task_append) stay absent.
	reg.actionTemplates = make([]map[string]*template.Template, len(wf.Actions))
	for i, a := range wf.Actions {
		tpls, err := compileActionTemplates(ev, a)
		if err != nil {
			return fmt.Errorf("compile actions[%d]: %w", i, err)
		}
		reg.actionTemplates[i] = tpls
	}

	// Per #169 the engine subscribes to bus topics ONCE at
	// engine level (in Start). registerLocked no longer wires
	// per-workflow subscriptions; the worker matches each
	// dequeued event against the registered set instead. The
	// trigger.type still gates which TopicX events the worker
	// considers for this workflow — see workerLoop +
	// matchesWorkflow.
	switch wf.Trigger.Type {
	case parser.TriggerTypeEdgeCreated,
		parser.TriggerTypeEntityCreated,
		parser.TriggerTypeFillCompleted,
		parser.TriggerTypeManual:
		// Recognized trigger types — registration proceeds.
	default:
		return fmt.Errorf("unsupported trigger.type %q", wf.Trigger.Type)
	}

	// Per #169 the engine is the authoritative gate on
	// catch-all uniqueness per (trigger.type, kind). The
	// loader rejects collisions at parse time as the
	// early-warn surface; this registerLocked check is the
	// defense-in-depth — a future programmatic registration
	// path (operator-CLI, hot-reload race) can't slip in a
	// second catch-all on the same slot.
	if wf.CatchAll {
		if prior := e.findCatchAllConflict(wf); prior != "" {
			return fmt.Errorf("catch_all collision: workflow %q already occupies the (%s, %q) catch_all slot",
				prior, wf.Trigger.Type, catchAllRegistryKey(wf))
		}
	}

	e.workflows[wf.Name] = reg
	e.logger.Info("workflow registered",
		"workflow", wf.Name,
		"trigger", wf.Trigger.Type,
		"bindings", len(reg.contextBinds))
	return nil
}

// unregisterLocked removes the workflow from the engine's
// registered set. Per #169 bus subscriptions live at engine
// level; nothing to unsubscribe per-workflow. Called with
// e.mu held.
func (e *Engine) unregisterLocked(name string, _ *registeredWorkflow) {
	delete(e.workflows, name)
	e.logger.Info("workflow unregistered", "workflow", name)
}

// processEvent is the per-event two-pass evaluator per #169.
// Called from the worker on each dequeued queuedEvent;
// enumerates matching workflows, partitions into pass-1
// (regular) + pass-2 (catch_all), sorts each by filename,
// and iterates with claim-stop chain semantics.
//
// Pass-1: every regular workflow whose trigger matches the
// event fires in filename order. The first claim_entity
// action that lands halts the pass + suppresses pass-2.
//
// Pass-2: only runs if pass-1 ended without a claim. Within
// pass-2 kind-specific catch-alls win over the global
// wildcard (`*` = trigger.match kind/target_kind empty);
// the wildcard fires only when no kind-specific catch-all
// matched the event's entity kind. Same claim-stop chain
// applies in pass-2.
func (e *Engine) processEvent(qe queuedEvent) {
	regs := e.snapshotRegistered()

	pass1, pass2 := e.partitionMatching(qe, regs)
	sortByFilename(pass1)
	if e.runChain(qe, pass1) {
		return
	}
	sortByFilename(pass2)
	pass2KindSpecific, pass2Wildcard := partitionCatchAllSpecificity(pass2)
	if len(pass2KindSpecific) > 0 {
		_ = e.runChain(qe, pass2KindSpecific)
		return
	}
	if len(pass2Wildcard) > 0 {
		_ = e.runChain(qe, pass2Wildcard)
	}
}

// snapshotRegistered returns a stable slice of the engine's
// currently-registered workflows. Taken under e.mu.RLock so
// concurrent event-processing snapshots don't serialize
// against each other (single-worker model means this is
// mostly theoretical today, but tightens the contract).
// Reconcile + register/unregister take the write lock.
func (e *Engine) snapshotRegistered() []*registeredWorkflow {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*registeredWorkflow, 0, len(e.workflows))
	for _, r := range e.workflows {
		out = append(out, r)
	}
	return out
}

// partitionMatching filters the registered set down to
// workflows whose trigger matches the queuedEvent, then
// splits them into pass-1 (regular) + pass-2 (catch_all)
// per #169. Type-switches on the event shape internally.
func (e *Engine) partitionMatching(qe queuedEvent, regs []*registeredWorkflow) (pass1, pass2 []*registeredWorkflow) {
	for _, r := range regs {
		if !e.matchesEvent(qe, r) {
			continue
		}
		if r.catchAll {
			pass2 = append(pass2, r)
		} else {
			pass1 = append(pass1, r)
		}
	}
	return pass1, pass2
}

// matchesEvent reports whether the workflow's trigger.type
// + trigger.match filters match the queuedEvent. Mirrors the
// per-type filtering that the pre-#169 makeXHandler functions
// did inline. TargetKind resolution may need the entity
// resolver — failures land as engine-error records and
// return false (the workflow can't fire without the kind).
func (e *Engine) matchesEvent(qe queuedEvent, reg *registeredWorkflow) bool {
	m := reg.workflow.Trigger.Match
	switch ev := qe.event.(type) {
	case eventbus.EntityEdgeAddedEvent:
		if reg.workflow.Trigger.Type != parser.TriggerTypeEdgeCreated {
			return false
		}
		if m.EdgeType != "" && ev.EdgeType != m.EdgeType {
			return false
		}
		if m.TargetKind != "" {
			got, err := e.resolveEntity(qe.ctx, ev.ToID)
			if err != nil {
				e.recordEngineError(reg, ev.ToID, fmt.Errorf("target_kind probe: %w", err))
				return false
			}
			if kindOf(got) != m.TargetKind {
				return false
			}
		}
		return true
	case eventbus.EntityCreatedEvent:
		if reg.workflow.Trigger.Type != parser.TriggerTypeEntityCreated {
			return false
		}
		if m.Kind != "" && ev.Kind != m.Kind {
			return false
		}
		return true
	case eventbus.FillCompletedEvent:
		if reg.workflow.Trigger.Type != parser.TriggerTypeFillCompleted {
			return false
		}
		if m.Gap != "" && ev.Gap != m.Gap {
			return false
		}
		if m.Source != "" && string(ev.SourceTag) != m.Source {
			return false
		}
		return true
	default:
		return false
	}
}

// runChain iterates the workflows in order, evaluating each
// against the queuedEvent. Returns true on the first
// claim_entity action — the worker halts the per-event
// dispatch on a true return per #169.
func (e *Engine) runChain(qe queuedEvent, regs []*registeredWorkflow) bool {
	for _, r := range regs {
		if e.runWorkflowAgainstEvent(qe, r) {
			return true
		}
	}
	return false
}

// runWorkflowAgainstEvent shapes the event into the
// activation (entity / edge / chain) the existing
// evaluateAndRecord path expects, then evaluates. Returns
// whether claim_entity fired during this workflow's action
// dispatch (the engine reads the flag to halt the per-event
// chain per #169).
func (e *Engine) runWorkflowAgainstEvent(qe queuedEvent, reg *registeredWorkflow) bool {
	switch ev := qe.event.(type) {
	case eventbus.EntityEdgeAddedEvent:
		return e.evaluateEdgeEvent(qe.ctx, reg, ev)
	case eventbus.EntityCreatedEvent:
		return e.evaluateAndRecord(qe.ctx, reg, ev.ID, nil, ev.Chain)
	case eventbus.FillCompletedEvent:
		return e.evaluateAndRecord(qe.ctx, reg, ev.EntityID, nil, ev.Chain)
	default:
		return false
	}
}

// evaluateEdgeEvent runs the per-edge activation prep
// (edgeMap with from/to titles + timestamp) the old
// makeEdgeHandler did inline, then evaluates against the
// workflow. The TargetKind filter check has already
// happened in matchesEvent — here we only resolve the
// to-entity for title (cheap dictionary read) since the
// kind has been validated upstream.
func (e *Engine) evaluateEdgeEvent(ctx context.Context, reg *registeredWorkflow, edge eventbus.EntityEdgeAddedEvent) bool {
	edgeMap := map[string]any{
		"type":      edge.EdgeType,
		"from":      edge.FromID,
		"to":        edge.ToID,
		"timestamp": edge.At,
	}
	toEntity, _ := e.resolveEntity(ctx, edge.ToID)
	fromEntity, _ := e.resolveEntity(ctx, edge.FromID)
	if title := titleOf(fromEntity); title != "" {
		edgeMap["from_title"] = title
	}
	if title := titleOf(toEntity); title != "" {
		edgeMap["to_title"] = title
	}
	return e.evaluateAndRecord(ctx, reg, edge.ToID, edgeMap, edge.Chain)
}

// sortByFilename orders workflows by their parser-stamped
// Filename in ascending lexicographic order — operators use
// `01-`, `02-` prefixes to pin priority within a kind per
// #169. Stable so equal-filename workflows preserve their
// snapshot order (defensive — Reconcile rejects name
// duplicates upstream).
func sortByFilename(regs []*registeredWorkflow) {
	sort.SliceStable(regs, func(i, j int) bool {
		return regs[i].filename < regs[j].filename
	})
}

// partitionCatchAllSpecificity splits a pass-2 catch-all set
// into kind-specific (trigger.match has a kind/target_kind
// filter) and wildcard (no filter — the global `*` slot)
// halves. Per #169 the wildcard fires only when no
// kind-specific catch-all matched.
func partitionCatchAllSpecificity(regs []*registeredWorkflow) (kindSpecific, wildcard []*registeredWorkflow) {
	for _, r := range regs {
		if isCatchAllWildcard(r) {
			wildcard = append(wildcard, r)
		} else {
			kindSpecific = append(kindSpecific, r)
		}
	}
	return kindSpecific, wildcard
}

// findCatchAllConflict returns the name of an
// already-registered catch_all workflow that occupies the
// same (trigger.type, kind) slot as `wf`. Returns "" when
// no conflict exists. Called from registerLocked with e.mu
// held.
func (e *Engine) findCatchAllConflict(wf *parser.Workflow) string {
	key := catchAllRegistryKey(wf)
	for name, reg := range e.workflows {
		if name == wf.Name {
			continue
		}
		if !reg.catchAll {
			continue
		}
		if reg.workflow.Trigger.Type != wf.Trigger.Type {
			continue
		}
		if catchAllRegistryKey(reg.workflow) != key {
			continue
		}
		return name
	}
	return ""
}

// catchAllRegistryKey returns the kind-slot key the
// catch-all workflow occupies. Mirrors the loader-side
// catchAllKindKey so engine + loader agree on what counts
// as a collision.
func catchAllRegistryKey(wf *parser.Workflow) string {
	switch wf.Trigger.Type {
	case parser.TriggerTypeEdgeCreated:
		return wf.Trigger.Match.TargetKind
	case parser.TriggerTypeEntityCreated:
		return wf.Trigger.Match.Kind
	default:
		return ""
	}
}

// isCatchAllWildcard reports whether the catch_all
// workflow's trigger declares no kind filter — the global
// `*` slot in pass-2's specificity hierarchy. A catch_all
// without any kind/target_kind filter is the operator's
// last-resort floor; with a filter it's kind-specific.
func isCatchAllWildcard(reg *registeredWorkflow) bool {
	m := reg.workflow.Trigger.Match
	switch reg.workflow.Trigger.Type {
	case parser.TriggerTypeEdgeCreated:
		return m.TargetKind == ""
	case parser.TriggerTypeEntityCreated:
		return m.Kind == ""
	case parser.TriggerTypeFillCompleted:
		// fill events have no kind in the event payload;
		// every fill catch-all is the wildcard slot for
		// its trigger.type by construction. Loader-side
		// uniqueness still enforces one-per-trigger-type.
		return true
	default:
		return true
	}
}

// titleOf reads the "title" field from a resolved entity
// map. Returns "" when the entity is nil, has no title
// field, or the title isn't a string. Predicates that need
// the title check has(edge.to_title) before reading; the
// empty return lets the engine omit the field cleanly.
func titleOf(entity map[string]any) string {
	if entity == nil {
		return ""
	}
	v, ok := entity["title"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// evaluateAndRecord is the per-event evaluation core:
// resolve entity → evaluate context bindings → evaluate
// condition predicate → render subject → record Decision.
// Each step's failure mode is folded into the recorded
// Decision (Err or MissingRefs) so the engine never blocks
// on a single workflow's misbehavior.
//
// Returns true when a claim_entity action fired during this
// workflow's dispatch — the per-#169 signal that halts the
// worker's per-event chain. False on cycle-suppress, condition
// false, dedup-skip, or any action chain that didn't include
// claim_entity.
func (e *Engine) evaluateAndRecord(ctx context.Context, reg *registeredWorkflow, entityID string, edge map[string]any, chain []string) bool {
	dec := Decision{
		Workflow: reg.workflow.Name,
		EntityID: entityID,
		At:       time.Now().UTC(),
	}

	// #147 structural cycle detection per ADR-0024 §"Self-loop
	// detection": when the triggering event's workflow chain
	// already names this workflow, firing it again would close
	// a loop. Suppress + record an err-task on the first
	// suppression of this chain shape.
	if e.applyCycleCheck(ctx, &dec, chain) {
		return false
	}

	// Append this workflow to the chain for downstream events
	// the action runner publishes (writers read the chain from
	// ctx via eventbus.WorkflowChainFromContext + attach it to
	// the events they emit). This is what propagates the chain
	// across the W1 → W2 → W3 → W1 detection axis.
	childChain := make([]string, 0, len(chain)+1)
	childChain = append(childChain, chain...)
	childChain = append(childChain, reg.workflow.Name)
	ctx = eventbus.WithWorkflowChain(ctx, childChain)

	entity, err := e.resolveEntity(ctx, entityID)
	if err != nil {
		dec.MissingRefs = append(dec.MissingRefs, decision.MissingRef{ID: entityID})
		e.recordDecision(dec)
		return false
	}

	act := decision.Activation{
		Entity:   entity,
		Edge:     edge,
		Bindings: make(map[string]any, len(reg.contextBinds)),
	}

	// Evaluate context bindings in order. Each binding's
	// missing-references roll up into the Decision so the
	// engine can surface them on the resulting task.
	for _, cb := range reg.contextBinds {
		val, bres, err := cb.program.EvalDyn(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("context.%s.via: %w", cb.name, err)
			e.recordDecision(dec)
			return false
		}
		dec.MissingRefs = append(dec.MissingRefs, bres.MissingRefs...)
		act.Bindings[cb.name] = val
	}

	// Evaluate the condition predicate. Empty condition →
	// always fire (per ADR-0024 §"Workflow" — workflows that
	// omit condition default to fire-on-trigger-match).
	fired := true
	if reg.condition != nil {
		got, cres, err := reg.condition.EvalBool(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("condition: %w", err)
			e.recordDecision(dec)
			return false
		}
		dec.MissingRefs = append(dec.MissingRefs, cres.MissingRefs...)
		fired = got
	}
	dec.Fired = fired
	dec.MissingRefs = dedupMissingRefs(dec.MissingRefs)

	// Render subject regardless of fired — Phase 4 needs it
	// for task naming when Fired=true, and tests inspect it
	// when Fired=false to verify the eval pipeline ran.
	if reg.subject != nil {
		sub, sres, err := reg.subject.Render(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("subject: %w", err)
			e.recordDecision(dec)
			return false
		}
		dec.MissingRefs = append(dec.MissingRefs, sres.MissingRefs...)
		dec.MissingRefs = dedupMissingRefs(dec.MissingRefs)
		dec.Subject = sub
	}

	// Phase 5.A: workflow-level dedup. Render the key,
	// stamp it on the Decision, apply the policy to decide
	// whether to dispatch actions. Done BEFORE
	// recordDecision so the recorded entry carries the
	// dedup attribution.
	dispatch := true
	if dec.Fired {
		dec.DedupKey, dec.DedupPolicyApplied, dispatch = e.applyDedupPolicy(ctx, reg, act)
	}

	// Dispatch actions to the configured Runner when the
	// workflow fired AND the dedup policy didn't short-
	// circuit. Failures from individual actions log at
	// WARN; Phase 5's err-task pattern absorbs the per-
	// failure surface (5.B+).
	//
	// runActions returns true when any action set the #169
	// Claim flag — stamp Claimed on the Decision BEFORE
	// recordDecision so observers see the post-action
	// state, then propagate the bool up to the worker so
	// the per-event chain halts.
	claimed := false
	if dec.Fired && e.runner != nil && dispatch {
		claimed = e.runActions(ctx, reg, dec, entity, edge, act)
	}
	dec.Claimed = claimed
	e.recordDecision(dec)
	return claimed
}

// applyCycleCheck implements the #147 structural cycle
// detection that replaces the prior per-(workflow, entity)
// rate-limit backstop. The triggering event's workflow chain
// names every workflow whose firing produced the event; when
// the current workflow is already in that chain, firing it
// again would close a loop. Suppress + record a Decision
// with SuppressedByCycle=true + write a single err-task entry
// the first time the engine sees this particular chain-shape
// loop (the chain fingerprint is unique per loop closure, so
// the same loop won't err-task-spam on re-fire).
//
// Returns true when the fire is suppressed (caller must
// abort the evaluation pipeline).
func (e *Engine) applyCycleCheck(ctx context.Context, dec *Decision, chain []string) bool {
	for _, w := range chain {
		if w == dec.Workflow {
			dec.SuppressedByCycle = true
			dec.CycleChain = append([]string(nil), chain...)
			e.recordDecision(*dec)
			e.maybeWriteCycleErrTask(ctx, dec, chain)
			return true
		}
	}
	return false
}

// maybeWriteCycleErrTask writes a single err-task entry per
// (closing-workflow, chain-fingerprint) so a re-firing loop
// doesn't append per-fire to the workflow's err-task.
func (e *Engine) maybeWriteCycleErrTask(ctx context.Context, dec *Decision, chain []string) {
	if e.errTaskWriter == nil {
		return
	}
	fingerprint := dec.Workflow + "|" + strings.Join(chain, ">")
	e.cycleMu.Lock()
	if _, already := e.cycleLogged[fingerprint]; already {
		e.cycleMu.Unlock()
		return
	}
	e.cycleLogged[fingerprint] = struct{}{}
	e.cycleMu.Unlock()
	msg := fmt.Sprintf("cycle suppressed: workflow %q already in event chain %v on entity %q; further fires on the same chain stay suppressed",
		dec.Workflow, chain, dec.EntityID)
	if err := e.errTaskWriter.AppendErrTask(ctx, dec.Workflow, dec.At, dec.EntityID, msg); err != nil {
		e.logger.Warn("workflow err-task append failed (cycle)",
			"workflow", dec.Workflow, "err", err.Error())
	}
}

// applyDedupPolicy renders the workflow's dedup key against
// the activation, looks up the (workflow, key) pair in the
// engine's seen-set, and decides whether the action runner
// should dispatch this fire per workflow.Dedup.Policy. Returns
// the rendered key (or "" on render failure / no key),
// the policy actually applied (one of "update" / "skip" /
// "replace"), and a bool indicating whether the action
// runner should dispatch.
//
// Per ADR-0024 §"Per-pattern de-duplication":
//   - update (default): always dispatch; the task_append +
//     line-level dedup do the actual write coalescing.
//   - skip: dispatch only on the FIRST fire (key not yet
//     seen). Subsequent fires with the same key short-
//     circuit cleanly.
//   - replace: Phase 5.A first cut — logs a notice + falls
//     through to update behavior. Real replace semantics
//     (close existing task + create new) deferred; tracked
//     as a Phase 5 carry-over.
//
// The seen-set is in-memory + restart-bounded — a daemon
// restart resets it, and the next fire treats every key as
// fresh. Acceptable for v1; future iteration can swap in a
// store-backed lookup if persistence is needed.
func (e *Engine) applyDedupPolicy(ctx context.Context, reg *registeredWorkflow, act decision.Activation) (key, policy string, dispatch bool) {
	policy = reg.workflow.Dedup.Policy
	if policy == "" {
		policy = parser.DedupPolicyUpdate
	}
	if reg.dedupKey == nil {
		return "", policy, true
	}
	rendered, _, err := reg.dedupKey.Render(ctx, act)
	if err != nil {
		e.logger.Warn("workflow dedup: key render failed; dispatching with empty key",
			"workflow", reg.workflow.Name, "err", err.Error())
		return "", policy, true
	}
	if strings.TrimSpace(rendered) == "" {
		return "", policy, true
	}
	id := dedupID{workflow: reg.workflow.Name, key: rendered}

	e.dedupMu.Lock()
	_, seen := e.dedupSeen[id]
	switch policy {
	case parser.DedupPolicySkip:
		if seen {
			e.dedupMu.Unlock()
			return rendered, parser.DedupPolicySkip, false
		}
		e.dedupSeen[id] = struct{}{}
	case parser.DedupPolicyReplace:
		// Phase 5.A first cut: real replace-semantics (close
		// old + create new) is a Phase 5 carry-over. For
		// now log + treat as update so the dispatch shape
		// stays predictable + the operator sees the
		// "policy=replace, treated as update" surface.
		if seen {
			e.logger.Warn("workflow dedup: policy=replace not yet implemented; falling through to update behavior",
				"workflow", reg.workflow.Name, "dedup_key", rendered)
		}
		e.dedupSeen[id] = struct{}{}
	default:
		// update (and any unrecognized policy — parser-level
		// validation already rejects out-of-vocab values).
		e.dedupSeen[id] = struct{}{}
	}
	e.dedupMu.Unlock()
	return rendered, policy, true
}

// runActions dispatches the workflow's action list to the
// configured Runner. Renders the per-action CEL template
// fields against the activation up front (so a render error
// on any field aborts the whole action list with a clear
// engine-side log line) and ships the rendered values into
// the runner via actions.Activation.RenderedTemplates. Shared
// between evaluateAndRecord (event-bus path) and
// runEvaluation (Dispatch target-less path) so both surface
// the same per-action logs + stay in sync when Phase 5 adds
// err-task routing.
// runActions returns true when any action in the workflow's
// dispatch set the #169 Claim flag (the claim_entity
// primitive). The worker reads this signal to halt the
// per-event chain — no further pass-1 workflows fire, no
// pass-2 catch_all fires. False on every other outcome,
// including action errors (errors don't claim by design;
// the err-task pattern handles the failure surface
// separately).
func (e *Engine) runActions(ctx context.Context, reg *registeredWorkflow, dec Decision, entity, edge map[string]any, act decision.Activation) bool {
	rendered, err := e.renderActionTemplates(ctx, reg, act)
	if err != nil {
		e.logger.Warn("workflow action templates render failed; skipping action dispatch",
			"workflow", dec.Workflow,
			"err", err.Error())
		return false
	}
	missingRefIDs := make([]string, 0, len(dec.MissingRefs))
	for _, mr := range dec.MissingRefs {
		missingRefIDs = append(missingRefIDs, mr.ID)
	}
	results := e.runner.Run(ctx, reg.workflow,
		actions.Decision{
			Workflow:    dec.Workflow,
			EntityID:    dec.EntityID,
			Subject:     dec.Subject,
			At:          dec.At,
			DedupKey:    dec.DedupKey,
			MissingRefs: missingRefIDs,
		},
		actions.Activation{
			Entity:            entity,
			Edge:              edge,
			Bindings:          act.Bindings,
			RenderedTemplates: rendered,
		})
	claimed := false
	for _, r := range results {
		if r.Claim {
			claimed = true
		}
		if r.Err != nil {
			e.logger.Warn("workflow action failed",
				"workflow", dec.Workflow,
				"action_idx", r.ActionIdx,
				"type", r.Type,
				"err", r.Err.Error())
			// Phase 5.B: per-action failure → err-task
			// append. Includes ErrActionAuthorBug (workflow-
			// author errors) + production runtime failures
			// (vault write, plugin timeout, etc.) — both are
			// "this workflow can't complete its fire," which
			// is the err-task pattern's scope per ADR-0024.
			if e.errTaskWriter != nil {
				msg := fmt.Sprintf("action[%d] %s: %s", r.ActionIdx, r.Type, r.Err.Error())
				if err := e.errTaskWriter.AppendErrTask(
					ctx, dec.Workflow, dec.At, dec.EntityID, msg,
				); err != nil {
					e.logger.Warn("workflow err-task append failed",
						"workflow", dec.Workflow, "err", err.Error())
				}
			}
			continue
		}
		e.logger.Info("workflow action executed",
			"workflow", dec.Workflow,
			"action_idx", r.ActionIdx,
			"type", r.Type)
	}
	return claimed
}

// compileActionTemplates picks out the CEL-templated fields
// of a single action by primitive and compiles each via
// template.Compile against the workflow's evaluator. Returns
// a fieldName → compiled-template map keyed by the action's
// field shape ("target" / "content" / "entity"). Skips empty
// fields — an action with no Target leaves the "target"
// entry unset so the runner's rendered-or-fallback helper
// hits its empty path cleanly. plugin_dispatch's args
// templating is deferred to Phase 4.C.
func compileActionTemplates(ev *decision.Evaluator, a parser.Action) (map[string]*template.Template, error) {
	tpls := make(map[string]*template.Template)
	switch {
	case a.TaskAppend != nil:
		if a.TaskAppend.Content != "" {
			tpl, err := template.Compile(a.TaskAppend.Content, ev)
			if err != nil {
				return nil, fmt.Errorf("task_append.content: %w", err)
			}
			tpls["content"] = tpl
		}
	case a.AddNote != nil:
		if a.AddNote.Target != "" {
			tpl, err := template.Compile(a.AddNote.Target, ev)
			if err != nil {
				return nil, fmt.Errorf("add_note.target: %w", err)
			}
			tpls["target"] = tpl
		}
		if a.AddNote.Content != "" {
			tpl, err := template.Compile(a.AddNote.Content, ev)
			if err != nil {
				return nil, fmt.Errorf("add_note.content: %w", err)
			}
			tpls["content"] = tpl
		}
	case a.AddGap != nil:
		if a.AddGap.Entity != "" {
			tpl, err := template.Compile(a.AddGap.Entity, ev)
			if err != nil {
				return nil, fmt.Errorf("add_gap.entity: %w", err)
			}
			tpls["entity"] = tpl
		}
	case a.PluginDispatch != nil:
		// Phase 4.C — plugin_dispatch.args templating not yet
		// wired. Returning an empty map keeps the runner-side
		// drift-warn quiet for this action (no templated
		// fields → no rendered keys expected).
	case a.SetProperty != nil:
		if a.SetProperty.Entity != "" {
			tpl, err := template.Compile(a.SetProperty.Entity, ev)
			if err != nil {
				return nil, fmt.Errorf("set_property.entity: %w", err)
			}
			tpls["entity"] = tpl
		}
		// Each field's value is a CEL template; key the
		// rendered output under `field:<name>` so the runner
		// retrieves it the same way it does single-field
		// primitives. set_property's per-field map can grow
		// arbitrarily; namespacing under `field:` avoids
		// collision with the literal "entity" key.
		for name, expr := range a.SetProperty.Fields {
			tpl, err := template.Compile(expr, ev)
			if err != nil {
				return nil, fmt.Errorf("set_property.fields[%q]: %w", name, err)
			}
			tpls["field:"+name] = tpl
		}
	case a.AddCanonicalEdge != nil:
		if a.AddCanonicalEdge.Source != "" {
			tpl, err := template.Compile(a.AddCanonicalEdge.Source, ev)
			if err != nil {
				return nil, fmt.Errorf("add_canonical_edge.source: %w", err)
			}
			tpls["source"] = tpl
		}
		if a.AddCanonicalEdge.TargetName != "" {
			tpl, err := template.Compile(a.AddCanonicalEdge.TargetName, ev)
			if err != nil {
				return nil, fmt.Errorf("add_canonical_edge.target.name: %w", err)
			}
			tpls["target.name"] = tpl
		}
		// Per-entry data values are CEL templates; key the
		// rendered output under `data:<name>` to mirror
		// set_property's `field:<name>` namespacing convention.
		for name, expr := range a.AddCanonicalEdge.Data {
			tpl, err := template.Compile(expr, ev)
			if err != nil {
				return nil, fmt.Errorf("add_canonical_edge.data[%q]: %w", name, err)
			}
			tpls["data:"+name] = tpl
		}
	case a.ArchiveEntity != nil:
		// archive_entity.entity is the target id (defaults to
		// `entity.id` at runner time when empty); reason is an
		// optional audit string. Both are CEL templates per #150.
		if a.ArchiveEntity.Entity != "" {
			tpl, err := template.Compile(a.ArchiveEntity.Entity, ev)
			if err != nil {
				return nil, fmt.Errorf("archive_entity.entity: %w", err)
			}
			tpls["entity"] = tpl
		}
		if a.ArchiveEntity.Reason != "" {
			tpl, err := template.Compile(a.ArchiveEntity.Reason, ev)
			if err != nil {
				return nil, fmt.Errorf("archive_entity.reason: %w", err)
			}
			tpls["reason"] = tpl
		}
	}
	return tpls, nil
}

// renderActionTemplates evaluates every compiled per-action
// template against the activation and builds the map shipped
// into actions.Activation.RenderedTemplates. A render error
// on any field aborts with the wrapped error — the runner
// won't run with partially-rendered state.
func (e *Engine) renderActionTemplates(ctx context.Context, reg *registeredWorkflow, act decision.Activation) (map[int]map[string]string, error) {
	if len(reg.actionTemplates) == 0 {
		return nil, nil
	}
	out := make(map[int]map[string]string, len(reg.actionTemplates))
	for i, tpls := range reg.actionTemplates {
		if len(tpls) == 0 {
			out[i] = map[string]string{}
			continue
		}
		fields := make(map[string]string, len(tpls))
		for name, tpl := range tpls {
			val, _, err := tpl.Render(ctx, act)
			if err != nil {
				return nil, fmt.Errorf("actions[%d].%s: %w", i, name, err)
			}
			fields[name] = val
		}
		out[i] = fields
	}
	return out, nil
}

// resolveEntity wraps the configured EntityResolver +
// translates the not-found case into a clean caller-side
// signal. Returns ErrEntityNotFound from decision.* when
// the resolver reports the id missing.
func (e *Engine) resolveEntity(ctx context.Context, id string) (map[string]any, error) {
	if e.resolver == nil {
		return nil, decision.ErrEntityNotFound
	}
	return e.resolver.Resolve(ctx, id)
}

// recordEngineError records a Decision with Err set for
// engine-side failures (resolver errors, etc.). Used by
// trigger handlers that can't even reach the predicate.
func (e *Engine) recordEngineError(reg *registeredWorkflow, entityID string, err error) {
	e.recordDecision(Decision{
		Workflow: reg.workflow.Name,
		EntityID: entityID,
		Err:      err,
		At:       time.Now().UTC(),
	})
}

// recordDecision appends to the ring buffer + logs INFO.
// The ring is bounded; once full, oldest entries get
// evicted.
func (e *Engine) recordDecision(d Decision) {
	e.decMu.Lock()
	if len(e.decisions) >= e.ringSize {
		copy(e.decisions, e.decisions[1:])
		e.decisions = e.decisions[:len(e.decisions)-1]
	}
	e.decisions = append(e.decisions, d)
	e.decMu.Unlock()

	attrs := []any{
		"workflow", d.Workflow,
		"entity_id", d.EntityID,
		"fired", d.Fired,
		"missing_refs", len(d.MissingRefs),
	}
	if d.Subject != "" {
		attrs = append(attrs, "subject", d.Subject)
	}
	if d.Err != nil {
		attrs = append(attrs, "err", d.Err.Error())
		e.logger.Warn("workflow decision: errored", attrs...)
		// Phase 5.B: systemic failure → err-task append.
		// ADR-0024 §"Runtime errors — the err-task pattern":
		// one err task per workflow accumulates failure
		// details; MissingRefs are handled separately by
		// the missing-ref note path (Phase 5.C).
		if e.errTaskWriter != nil {
			if err := e.errTaskWriter.AppendErrTask(
				context.Background(),
				d.Workflow, d.At, d.EntityID, d.Err.Error(),
			); err != nil {
				e.logger.Warn("workflow err-task append failed",
					"workflow", d.Workflow, "err", err.Error())
			}
		}
		return
	}
	e.logger.Info("workflow decision", attrs...)
}

// Decisions returns a snapshot of the engine's recorded
// decision history (most-recent-N up to DecisionRingSize).
// Safe for concurrent calls; freshly allocated so callers
// can mutate without affecting subsequent snapshots.
func (e *Engine) Decisions() []Decision {
	e.decMu.Lock()
	defer e.decMu.Unlock()
	out := make([]Decision, len(e.decisions))
	copy(out, e.decisions)
	return out
}

// ErrUnknownWorkflow is returned by Dispatch when no
// workflow with the requested name is registered. The HTTP
// + CLI manual-trigger surfaces translate this to a 404
// so the caller can re-list valid names.
var ErrUnknownWorkflow = errors.New("engine: workflow not registered")

// ErrEmptyInputNotAllowed is returned by Dispatch when an
// empty input is passed for an event-driven workflow. Per
// ADR-0024, empty input is reserved for target-less manual
// workflows (e.g. daily-summary).
var ErrEmptyInputNotAllowed = errors.New("engine: workflow requires a non-empty input")

// ErrURLInputNotSupported is returned by Dispatch when the
// input is URL-shape but no IngestRouter is wired. Production
// main.go wires one when the daemon's plugin pipeline is
// available; tests / dev binaries without that wiring keep
// URL-shape input rejecting cleanly.
var ErrURLInputNotSupported = errors.New("engine: URL-shape input requires an IngestRouter; none wired")

// Dispatch is the manual-trigger entry point per ADR-0024
// §"workflow.trigger(input) input semantics". Disambiguates
// input by syntactic shape:
//
//   - Empty string — target-less manual fire (allowed only
//     for trigger.type=manual). Activation.Entity is the
//     empty map; predicates that access entity fields see
//     has()==false.
//   - URL — routes through the configured IngestRouter
//     (plugin pipeline) per ADR-0024 §"workflow.trigger(input)
//     input semantics". On success the returned canonical
//     entity id becomes the workflow's target. On routing
//     failure (no plugin handles, malformed URL,
//     disambiguation required) Dispatch returns a typed
//     error to the caller before any workflow run starts —
//     no Decision is recorded.
//   - Anything else — treated as an entity ID
//     (`<kind>:<slug>` per ADR-0017). Resolver lookup
//     populates Activation.Entity; an unresolved id
//     produces a Decision with MissingRefs rather than a
//     hard Dispatch error.
//
// URL detection: a `://` substring in the input is the
// canonical mark (`https://...`, `http://...`,
// `bgg://...`). Plugin shorthand (`<plugin>: <pattern>`
// without `://`) is intentionally NOT URL-shape — operators
// who want plugin-shorthand input use the plugin's URL
// pattern that includes `://`, or pre-resolve to entity-id.
// This narrows the v1 disambiguation surface; future
// iterations can widen to plugin-shorthand if the use case
// surfaces.
//
// The resulting Decision is appended to the engine's ring
// buffer + returned to the caller. HTTP / CLI handlers
// serialize the Decision directly back to the invoker.
func (e *Engine) Dispatch(ctx context.Context, name, input string) (Decision, error) {
	e.mu.Lock()
	reg, ok := e.workflows[name]
	e.mu.Unlock()
	if !ok {
		return Decision{}, fmt.Errorf("%w: %q", ErrUnknownWorkflow, name)
	}

	if input == "" {
		if reg.workflow.Trigger.Type != parser.TriggerTypeManual {
			return Decision{}, fmt.Errorf("%w: workflow %q is event-driven (trigger=%s)",
				ErrEmptyInputNotAllowed, name, reg.workflow.Trigger.Type)
		}
		// Target-less Dispatch: synthesize an empty entity
		// activation. evaluateAndRecord handles the empty-id
		// case (resolveEntity returns ErrEntityNotFound,
		// which surfaces as a MissingRef — but we want
		// target-less to NOT produce a MissingRef). Inline
		// the empty-target path here:
		e.runEvaluation(ctx, reg, "", map[string]any{}, nil)
		return e.findRecentDecision(name, ""), nil
	}

	target := input
	if strings.Contains(input, "://") {
		// URL-shape input — route through the IngestRouter
		// to get the canonical entity id. Per ADR-0024, the
		// trigger call itself fails synchronously on routing
		// errors; no Decision is recorded.
		if e.ingestRouter == nil {
			return Decision{}, fmt.Errorf("%w: %s", ErrURLInputNotSupported, input)
		}
		id, err := e.ingestRouter.IngestURL(ctx, input, e.ingestTimeout)
		if err != nil {
			return Decision{}, fmt.Errorf("dispatch %s: ingest %q: %w", name, input, err)
		}
		target = id
	}

	// Manual dispatch starts a fresh chain — no upstream
	// workflow firing produced this invocation.
	e.evaluateAndRecord(ctx, reg, target, nil, nil)
	return e.findRecentDecision(name, target), nil
}

// findRecentDecision returns the most-recent recorded
// Decision matching (workflow, entityID). The evaluation
// path just appended it; the reverse-scan finds it without a
// deep struct comparison. A zero-value Decision (rather than
// the inserted one) is the defensive fallback for races where
// the ring buffer evicted the entry between insert + read.
func (e *Engine) findRecentDecision(name, entityID string) Decision {
	e.decMu.Lock()
	defer e.decMu.Unlock()
	for i := len(e.decisions) - 1; i >= 0; i-- {
		d := e.decisions[i]
		if d.Workflow == name && d.EntityID == entityID {
			return d
		}
	}
	return Decision{Workflow: name, EntityID: entityID, At: time.Now().UTC()}
}

// runEvaluation is the inner pipeline used by both
// evaluateAndRecord (event-bus path, which pre-resolves the
// entity) and Dispatch's target-less path (synthesizes an
// empty entity). Skips the resolve step on the assumption
// the caller has already populated entity.
func (e *Engine) runEvaluation(ctx context.Context, reg *registeredWorkflow, entityID string, entity, edge map[string]any) {
	dec := Decision{
		Workflow: reg.workflow.Name,
		EntityID: entityID,
		At:       time.Now().UTC(),
	}

	// #147 cycle check — same shape as evaluateAndRecord;
	// short-circuits before any evaluation runs. runEvaluation
	// is the no-upstream-chain shape (Dispatch path) so the
	// chain is always empty here; the check is defensive in
	// case a future caller threads a chain in.
	if e.applyCycleCheck(ctx, &dec, nil) {
		return
	}

	act := decision.Activation{
		Entity:   entity,
		Edge:     edge,
		Bindings: make(map[string]any, len(reg.contextBinds)),
	}
	for _, cb := range reg.contextBinds {
		val, bres, err := cb.program.EvalDyn(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("context.%s.via: %w", cb.name, err)
			e.recordDecision(dec)
			return
		}
		dec.MissingRefs = append(dec.MissingRefs, bres.MissingRefs...)
		act.Bindings[cb.name] = val
	}
	fired := true
	if reg.condition != nil {
		got, cres, err := reg.condition.EvalBool(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("condition: %w", err)
			e.recordDecision(dec)
			return
		}
		dec.MissingRefs = append(dec.MissingRefs, cres.MissingRefs...)
		fired = got
	}
	dec.Fired = fired
	dec.MissingRefs = dedupMissingRefs(dec.MissingRefs)
	if reg.subject != nil {
		sub, sres, err := reg.subject.Render(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("subject: %w", err)
			e.recordDecision(dec)
			return
		}
		dec.MissingRefs = append(dec.MissingRefs, sres.MissingRefs...)
		dec.MissingRefs = dedupMissingRefs(dec.MissingRefs)
		dec.Subject = sub
	}
	// Phase 5.A dedup attribution — see evaluateAndRecord
	// for the matching shape.
	dispatch := true
	if dec.Fired {
		dec.DedupKey, dec.DedupPolicyApplied, dispatch = e.applyDedupPolicy(ctx, reg, act)
	}
	e.recordDecision(dec)

	// Dispatch actions to the configured Runner when the
	// workflow fired AND the dedup policy didn't short-
	// circuit. shared runActions helper is also called by
	// evaluateAndRecord for event-bus-driven decisions.
	if dec.Fired && e.runner != nil && dispatch {
		e.runActions(ctx, reg, dec, entity, edge, act)
	}
}

// Registered returns the sorted-by-name list of currently-
// registered workflow names. Useful for tests + the future
// workflow.list MCP/HTTP surface.
func (e *Engine) Registered() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.workflows))
	for name := range e.workflows {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// WorkflowSummary is the per-workflow metadata returned by
// List() for the workflow.list HTTP / MCP / CLI surface per
// ADR-0024 §"Agent surface". Keeps the wire shape narrow:
// what an agent or operator needs to identify + classify a
// workflow without re-parsing the source file.
type WorkflowSummary struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Status      string `json:"status"`
	TriggerType string `json:"trigger_type"`
	DedupPolicy string `json:"dedup_policy,omitempty"`
}

// List returns a snapshot of every currently-registered
// workflow with its metadata for the workflow.list surface.
// Sorted by name for deterministic output. Safe for
// concurrent calls; the returned slice is freshly
// allocated so callers can mutate without affecting
// subsequent snapshots.
func (e *Engine) List() []WorkflowSummary {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]WorkflowSummary, 0, len(e.workflows))
	for _, reg := range e.workflows {
		out = append(out, WorkflowSummary{
			Name:        reg.workflow.Name,
			Version:     reg.workflow.Version,
			Status:      reg.workflow.Status,
			TriggerType: reg.workflow.Trigger.Type,
			DedupPolicy: reg.workflow.Dedup.Policy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Discover returns the sorted-by-name list of workflows
// whose condition predicate evaluates true against the
// given entity per ADR-0024 §"workflow.discover(entity_id)
// performance note". Walks every registered workflow + runs
// the condition; workflows without a condition (always-fire)
// are included unconditionally. A predicate that fails to
// evaluate (e.g. references a binding that depends on a
// graph.get miss) is treated as non-matching — Discover is a
// best-effort surface for operator inspection, not a fire
// commitment.
//
// Cost is O(W × T) where W is registered count + T is per-
// condition eval cost; per the ADR the W is single-digit in
// v1, so this is fine. Future iteration could memoize by
// workflow's trigger-match shape for cheaper filtering
// before evaluating the condition.
func (e *Engine) Discover(ctx context.Context, entityID string) ([]string, error) {
	if e.resolver == nil {
		return nil, fmt.Errorf("engine: Discover requires a configured Resolver")
	}
	entity, err := e.resolver.Resolve(ctx, entityID)
	if err != nil {
		if errors.Is(err, decision.ErrEntityNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrEntityNotFoundForDiscover, entityID)
		}
		return nil, fmt.Errorf("resolve entity %q: %w", entityID, err)
	}

	e.mu.Lock()
	regs := make([]*registeredWorkflow, 0, len(e.workflows))
	for _, reg := range e.workflows {
		regs = append(regs, reg)
	}
	e.mu.Unlock()

	var matches []string
	for _, reg := range regs {
		act := decision.Activation{Entity: entity}
		// Pre-evaluate context bindings so the condition has
		// the same activation shape as a real fire.
		ok := true
		for _, cb := range reg.contextBinds {
			val, _, err := cb.program.EvalDyn(ctx, act)
			if err != nil {
				ok = false
				break
			}
			if act.Bindings == nil {
				act.Bindings = make(map[string]any, len(reg.contextBinds))
			}
			act.Bindings[cb.name] = val
		}
		if !ok {
			continue
		}
		if reg.condition == nil {
			matches = append(matches, reg.workflow.Name)
			continue
		}
		fired, _, err := reg.condition.EvalBool(ctx, act)
		if err != nil {
			continue
		}
		if fired {
			matches = append(matches, reg.workflow.Name)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// ErrEntityNotFoundForDiscover is returned by Discover when
// the entity-id has no store row. Distinct from
// ErrUnknownWorkflow + ErrEmptyInputNotAllowed so the HTTP
// handler can translate to 404 cleanly.
var ErrEntityNotFoundForDiscover = errors.New("engine: entity not found for discover")

// AutoArchiveOnDoneFor reports whether the named workflow
// has auto_archive_on_done enabled per ADR-0024 §"Task"
// close lifecycle. Default true when the workflow's
// AutoArchiveOnDone is nil (operator omitted the field) or
// when the workflow isn't registered (defensive — caller
// can resolve tasks for workflows that were unloaded since
// the task was written; the default-true keeps the close
// path predictable).
func (e *Engine) AutoArchiveOnDoneFor(workflowName string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	reg, ok := e.workflows[workflowName]
	if !ok || reg.workflow.AutoArchiveOnDone == nil {
		return true
	}
	return *reg.workflow.AutoArchiveOnDone
}

// resolverGraphLookup adapts an EntityResolver to
// decision.GraphLookup so the decision package's CEL
// graph.get(id) function can dispatch through the engine's
// resolver without the decision package depending on the
// engine package.
type resolverGraphLookup struct {
	resolver EntityResolver
}

func (r *resolverGraphLookup) Get(ctx context.Context, id string) (map[string]any, error) {
	if r.resolver == nil {
		return nil, decision.ErrEntityNotFound
	}
	return r.resolver.Resolve(ctx, id)
}

// kindOf reads the "kind" field from a resolved entity map.
// Returns "" when the entity has no kind field — the engine
// treats an empty kind as "no match" against any non-empty
// TargetKind filter.
func kindOf(entity map[string]any) string {
	if entity == nil {
		return ""
	}
	v, ok := entity["kind"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// dedupMissingRefs collapses duplicate ids in a MissingRefs
// slice + sorts by id. Used after merging multiple Result
// slices (context-binding + condition + subject) so the
// final decision's MissingRefs is deterministic.
func dedupMissingRefs(refs []decision.MissingRef) []decision.MissingRef {
	if len(refs) <= 1 {
		return refs
	}
	seen := make(map[string]struct{}, len(refs))
	out := make([]decision.MissingRef, 0, len(refs))
	for _, r := range refs {
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
