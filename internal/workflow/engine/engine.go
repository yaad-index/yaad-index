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
// execution (task_append / add_comment / plugin_dispatch /
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
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
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

	// Logger receives the per-decision Info line + any
	// engine-internal warnings. Nil → discarding handler.
	Logger *slog.Logger

	// DecisionRingSize caps the in-memory decision buffer
	// (most-recent-N retained for Decisions() snapshot).
	// Zero → DefaultDecisionRingSize.
	DecisionRingSize int
}

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
	bus      eventbus.Bus
	resolver EntityResolver
	logger   *slog.Logger
	ringSize int

	mu        sync.Mutex
	workflows map[string]*registeredWorkflow

	decMu     sync.Mutex
	decisions []Decision
}

// registeredWorkflow holds the per-workflow runtime state:
// the parsed Workflow, its compiled programs, and its bus
// subscriptions. Released atomically on Unregister.
type registeredWorkflow struct {
	workflow      *parser.Workflow
	evaluator     *decision.Evaluator
	condition     *decision.Program // nil when workflow.Condition is empty (always-fire)
	subject       *decision.Program
	contextBinds  []compiledBinding
	subscriptions []eventbus.Subscription
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
	return &Engine{
		bus:       opts.Bus,
		resolver:  opts.Resolver,
		logger:    logger,
		ringSize:  ring,
		workflows: make(map[string]*registeredWorkflow),
	}, nil
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
	}

	if wf.Condition != "" {
		prog, err := ev.Compile(wf.Condition, "bool")
		if err != nil {
			return fmt.Errorf("compile condition: %w", err)
		}
		reg.condition = prog
	}
	if wf.Subject != "" {
		prog, err := ev.Compile(wf.Subject, "string")
		if err != nil {
			return fmt.Errorf("compile subject: %w", err)
		}
		reg.subject = prog
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

	// Subscribe to bus topics per Trigger.Type. Manual
	// triggers (Phase 3.C entry-points) skip the bus subscribe.
	switch wf.Trigger.Type {
	case parser.TriggerTypeEdgeCreated:
		sub := e.bus.Subscribe(eventbus.TopicEntityEdgeAdded, e.makeEdgeHandler(reg))
		reg.subscriptions = append(reg.subscriptions, sub)
	case parser.TriggerTypeEntityCreated:
		sub := e.bus.Subscribe(eventbus.TopicEntityCreated, e.makeEntityHandler(reg))
		reg.subscriptions = append(reg.subscriptions, sub)
	case parser.TriggerTypeFillCompleted:
		sub := e.bus.Subscribe(eventbus.TopicFillCompleted, e.makeFillHandler(reg))
		reg.subscriptions = append(reg.subscriptions, sub)
	case parser.TriggerTypeManual:
		// No bus subscribe; Phase 3.C entry-points invoke
		// the engine's Dispatch path directly.
	default:
		return fmt.Errorf("unsupported trigger.type %q", wf.Trigger.Type)
	}

	e.workflows[wf.Name] = reg
	e.logger.Info("workflow registered",
		"workflow", wf.Name,
		"trigger", wf.Trigger.Type,
		"bindings", len(reg.contextBinds))
	return nil
}

// unregisterLocked tears down a workflow's subscriptions +
// removes it from the map. Called with e.mu held.
func (e *Engine) unregisterLocked(name string, reg *registeredWorkflow) {
	for _, s := range reg.subscriptions {
		s.Unsubscribe()
	}
	delete(e.workflows, name)
	e.logger.Info("workflow unregistered", "workflow", name)
}

// makeEdgeHandler returns a bus handler that routes an
// entity.edge_added event to the workflow's predicate-eval
// pipeline when the event's edge type + target kind match
// the trigger's match filter.
func (e *Engine) makeEdgeHandler(reg *registeredWorkflow) eventbus.Handler {
	return func(ctx context.Context, ev eventbus.Event) {
		edge, ok := ev.(eventbus.EntityEdgeAddedEvent)
		if !ok {
			return
		}
		m := reg.workflow.Trigger.Match
		if m.EdgeType != "" && edge.EdgeType != m.EdgeType {
			return
		}
		// TargetKind filter requires resolving the edge.ToID
		// to its kind. For Phase 3.B we resolve it via the
		// resolver — if resolution fails, the predicate
		// can't run anyway, so a missing-reference shape
		// makes more sense than a silent skip.
		entityID := edge.ToID
		var toEntity, fromEntity map[string]any
		toResolved := false
		if m.TargetKind != "" {
			got, err := e.resolveEntity(ctx, entityID)
			if err != nil {
				e.recordEngineError(reg, entityID, fmt.Errorf("target_kind probe: %w", err))
				return
			}
			if kindOf(got) != m.TargetKind {
				return
			}
			toEntity = got
			toResolved = true
		}
		// Build the full edge field set per ADR-0024 §"Decision
		// logic": type / from / to / from_title / to_title /
		// timestamp. Title fields are resolved through the
		// EntityResolver; on missing/empty the title field is
		// omitted so predicates can use has() to guard.
		// Timestamp is the event's At (publisher-stamped on
		// emit, per eventbus contract).
		edgeMap := map[string]any{
			"type":      edge.EdgeType,
			"from":      edge.FromID,
			"to":        edge.ToID,
			"timestamp": edge.At,
		}
		// Resolve from/to titles. Skip the to-resolve if we
		// already did it above for the target_kind filter.
		if !toResolved {
			toEntity, _ = e.resolveEntity(ctx, edge.ToID)
		}
		fromEntity, _ = e.resolveEntity(ctx, edge.FromID)
		if title := titleOf(fromEntity); title != "" {
			edgeMap["from_title"] = title
		}
		if title := titleOf(toEntity); title != "" {
			edgeMap["to_title"] = title
		}
		e.evaluateAndRecord(ctx, reg, entityID, edgeMap)
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

// makeEntityHandler returns a bus handler for
// entity.created events. Kind filter applies (operator-
// declared Match.Kind).
func (e *Engine) makeEntityHandler(reg *registeredWorkflow) eventbus.Handler {
	return func(ctx context.Context, ev eventbus.Event) {
		created, ok := ev.(eventbus.EntityCreatedEvent)
		if !ok {
			return
		}
		m := reg.workflow.Trigger.Match
		if m.Kind != "" && created.Kind != m.Kind {
			return
		}
		e.evaluateAndRecord(ctx, reg, created.ID, nil)
	}
}

// makeFillHandler returns a bus handler for fill.completed
// events. Filters by Gap + Source. The Source filter
// implements ADR-0024 §"Self-loop detection" — a workflow
// X listening to fill.completed with Source: workflow:X
// would loop, so workflows commonly filter to
// Source: operator (the answered-by-human shape).
func (e *Engine) makeFillHandler(reg *registeredWorkflow) eventbus.Handler {
	return func(ctx context.Context, ev eventbus.Event) {
		fill, ok := ev.(eventbus.FillCompletedEvent)
		if !ok {
			return
		}
		m := reg.workflow.Trigger.Match
		if m.Gap != "" && fill.Gap != m.Gap {
			return
		}
		if m.Source != "" && string(fill.SourceTag) != m.Source {
			return
		}
		e.evaluateAndRecord(ctx, reg, fill.EntityID, nil)
	}
}

// evaluateAndRecord is the per-event evaluation core:
// resolve entity → evaluate context bindings → evaluate
// condition predicate → render subject → record Decision.
// Each step's failure mode is folded into the recorded
// Decision (Err or MissingRefs) so the engine never blocks
// on a single workflow's misbehavior.
func (e *Engine) evaluateAndRecord(ctx context.Context, reg *registeredWorkflow, entityID string, edge map[string]any) {
	dec := Decision{
		Workflow: reg.workflow.Name,
		EntityID: entityID,
		At:       time.Now().UTC(),
	}

	entity, err := e.resolveEntity(ctx, entityID)
	if err != nil {
		dec.MissingRefs = append(dec.MissingRefs, decision.MissingRef{ID: entityID})
		e.recordDecision(dec)
		return
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
			return
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
			return
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
		sub, sres, err := reg.subject.EvalString(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("subject: %w", err)
			e.recordDecision(dec)
			return
		}
		dec.MissingRefs = append(dec.MissingRefs, sres.MissingRefs...)
		dec.MissingRefs = dedupMissingRefs(dec.MissingRefs)
		dec.Subject = sub
	}

	e.recordDecision(dec)
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

// Dispatch is the manual-trigger entry point per ADR-0024
// §"workflow.trigger(input) input semantics". Disambiguates
// input by syntactic shape:
//
//   - Empty string — target-less manual fire (allowed only
//     for trigger.type=manual). Activation.Entity is the
//     empty map; predicates that access entity fields see
//     has()==false.
//   - Anything else — treated as an entity ID
//     (`<kind>:<slug>` per ADR-0017). Resolver lookup
//     populates Activation.Entity; an unresolved id
//     produces a Decision with MissingRefs rather than a
//     hard Dispatch error.
//
// URL → ingest-or-lookup route is a Phase 3.C+ follow-up
// (requires coupling to the ingest tracker). The current
// v1 cut handles entity-ID + empty, covering the
// already-resolved-entity + daily-summary shapes the ADR
// names as primary.
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
	} else {
		e.evaluateAndRecord(ctx, reg, input, nil)
	}

	// Return the most-recent decision matching (name,
	// entityID). evaluateAndRecord just appended it; the
	// reverse-scan finds it without a deep struct comparison.
	e.decMu.Lock()
	defer e.decMu.Unlock()
	for i := len(e.decisions) - 1; i >= 0; i-- {
		d := e.decisions[i]
		if d.Workflow == name && d.EntityID == input {
			return d, nil
		}
	}
	return Decision{Workflow: name, EntityID: input, At: time.Now().UTC()}, nil
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
		sub, sres, err := reg.subject.EvalString(ctx, act)
		if err != nil {
			dec.Err = fmt.Errorf("subject: %w", err)
			e.recordDecision(dec)
			return
		}
		dec.MissingRefs = append(dec.MissingRefs, sres.MissingRefs...)
		dec.MissingRefs = dedupMissingRefs(dec.MissingRefs)
		dec.Subject = sub
	}
	e.recordDecision(dec)
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
