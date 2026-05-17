// Package decision implements the workflow engine's predicate +
// template evaluation layer per ADR-0024 §"Decision logic is
// agent-free in v1". Workflows declare CEL expressions for their
// `condition`, `subject`, `context.via`, and per-action template
// fields; this package compiles + caches the programs and
// evaluates them against an Activation (entity + edge + named
// context bindings) produced by the engine layer.
//
// CEL (Common Expression Language, Google's purpose-built
// predicate language with a mature Go implementation in
// `cel-go`) was chosen by the ADR — see §"Decision logic" for
// the reasoning. The cel-go dependency is wrapped behind this
// package's surface so a future evaluator swap doesn't leak
// into every workflow-engine consumer.
//
// **Phase 3.A scope.** This package ships the evaluator
// substrate: Compile + Evaluate, the standard environment
// (entity, edge, graph.get), the missing-reference path, and
// pluggable graph-lookup interface. Phase 3.B wires the
// evaluator into the engine + event-bus subscriber layer; 3.C
// adds the manual-trigger HTTP / CLI entry points.

package decision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
)

// GraphLookup is the entity-resolution interface the CEL
// `graph.get(id)` function dispatches through. Production wires
// a store-backed implementation in Phase 3.B; tests substitute
// in-memory fakes.
//
// Get returns the entity as a dynamic map (the same shape CEL
// evaluates against the `entity` variable). Returns
// ErrEntityNotFound when no entity is registered under id; any
// other error halts evaluation with a wrapped EvalError.
type GraphLookup interface {
	Get(ctx context.Context, id string) (map[string]any, error)
}

// ErrEntityNotFound is the sentinel GraphLookup.Get returns
// when an id doesn't resolve. The evaluator translates this
// into a MissingRef on the result rather than a fatal error —
// per ADR-0024 §"Missing-reference handling", a workflow whose
// graph.get(id) misses still surfaces a task with the note
// attached.
var ErrEntityNotFound = errors.New("decision: entity not found")

// MissingRef captures one graph.get(id) miss during evaluation.
// The engine layer (Phase 3.B) attaches these as notes to any
// resulting task. Order follows the order of graph.get() calls
// the program made.
type MissingRef struct {
	// ID is the id that failed to resolve.
	ID string
}

// Activation is the evaluation context: the triggering entity,
// optional triggering edge, and any named context bindings the
// engine pre-evaluated (per the workflow's `context` stanza).
type Activation struct {
	// Entity is the triggering entity's frontmatter / data map.
	// Required; an empty map is acceptable for manual triggers
	// with no entity (the CEL `entity` variable is bound to the
	// empty map and predicates that access fields will see
	// has() == false).
	Entity map[string]any

	// Edge is the triggering edge's wire shape. Nil for triggers
	// that don't carry an edge (entity_created / fill_completed /
	// manual). When set, the fields per ADR-0024 §"Decision
	// logic" are: `type`, `from`, `to`, `from_title`,
	// `to_title`, `timestamp`.
	Edge map[string]any

	// Bindings is the named-binding map produced by evaluating
	// the workflow's `context` stanza. Each entry's value is
	// the result of the binding's `via` expression. Bindings
	// are visible to the predicate + downstream templates by
	// their declared name.
	Bindings map[string]any
}

// Result is the outcome of one evaluation pass.
type Result struct {
	// MissingRefs is the set of graph.get() calls whose ids
	// didn't resolve during this evaluation. The engine
	// attaches the corresponding notes to any task spawned.
	// The evaluator de-duplicates by id within one Eval pass
	// (so a predicate that calls graph.get(same id) twice
	// produces one MissingRef, not two) and sorts the
	// result by id for deterministic output. PR-79 review
	// fold-in.
	MissingRefs []MissingRef
}

// EvalError wraps a non-missing-ref failure during evaluation —
// programming-error shapes (type mismatch, division by zero,
// nil deref through CEL's safe traversal). The engine logs
// these + surfaces an err-task per ADR-0024 §"Runtime errors —
// the err-task pattern."
type EvalError struct {
	// Expression is the original CEL source.
	Expression string
	// Cause is the underlying cel-go error.
	Cause error
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("decision: eval %q: %v", e.Expression, e.Cause)
}

func (e *EvalError) Unwrap() error { return e.Cause }

// Evaluator is the cached CEL program compiler. One instance
// per workflow (per-workflow Bindings declaration). Compile
// is safe for concurrent use; Eval calls SERIALIZE on
// evalMu so the per-eval refTracker + ctx don't bleed across
// concurrent evaluations of the same Evaluator.
//
// Workflow throughput is bounded by the per-workflow event
// rate (low in v1). Cross-workflow concurrency is unaffected:
// each workflow gets its own Evaluator, and unrelated
// workflows evaluate independently.
type Evaluator struct {
	env    *cel.Env
	lookup GraphLookup

	cacheMu sync.RWMutex
	cache   map[cacheKey]cel.Program

	evalMu         sync.Mutex
	currentTracker *refTracker
	currentCtx     context.Context
}

type cacheKey struct {
	expr     string
	expectAs string // "bool" | "string" | "dyn"
}

// Options configures an Evaluator. Lookup is the entity
// resolver for graph.get; nil makes every graph.get(id) record
// a MissingRef (useful for tests + dev). Bindings names the
// CEL variables made visible from the workflow's `context`
// stanza — they must be declared at env-build time so CEL's
// Check pass accepts predicate / template references to them.
type Options struct {
	Lookup   GraphLookup
	Bindings []string
}

// NewEvaluator constructs an evaluator with the standard
// environment + the workflow-declared context bindings + the
// given graph-lookup implementation. lookup may be nil — in
// that case `graph.get(id)` always reports missing-reference.
// Useful for unit tests that don't need an entity-resolution
// backend, and for engine paths where the store isn't wired
// (dev mode).
//
// The workflow engine (Phase 3.B) constructs one Evaluator
// per workflow with the workflow's Context[].Name set in
// Bindings.
func NewEvaluator(opts Options) (*Evaluator, error) {
	e := &Evaluator{
		lookup: opts.Lookup,
		cache:  make(map[cacheKey]cel.Program),
	}
	env, err := e.buildEnv(opts.Bindings)
	if err != nil {
		return nil, fmt.Errorf("decision: build cel env: %w", err)
	}
	e.env = env
	return e, nil
}

// buildEnv constructs the CEL environment with the variables +
// functions ADR-0024 §"Decision logic" prescribes:
//   - entity: dynamic map of the triggering entity.
//   - edge: dynamic map of the triggering edge (or empty/null
//     when the trigger doesn't carry one).
//   - graph.get(id) → dyn: canonical-id entity lookup; missing
//     id surfaces as MissingRef in the Result, not as a
//     terminal error.
//   - <binding>: one dyn variable per workflow-declared
//     context binding (per-Evaluator scope).
//
// The graph.get UnaryBinding is fixed at env-build time per
// cel-go's recommended pattern; per-eval state (refTracker,
// ctx) flows through the evaluator's `evalMu`-guarded
// currentTracker / currentCtx fields. Eval calls within a
// single Evaluator are SERIALIZED via evalMu — workflow
// throughput is bounded by per-workflow event rate (low in
// v1); cross-workflow concurrency is unaffected since each
// workflow gets its own Evaluator.
func (e *Evaluator) buildEnv(bindings []string) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("entity", cel.DynType),
		cel.Variable("edge", cel.DynType),
		cel.Function("graph.get",
			cel.Overload("graph_get_string",
				[]*cel.Type{cel.StringType},
				cel.DynType,
				cel.UnaryBinding(e.graphGet),
			),
		),
		ext.Strings(),
		regexCaptureFunction(),
	}
	seen := make(map[string]struct{}, len(bindings))
	for _, name := range bindings {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if name == "entity" || name == "edge" {
			return nil, fmt.Errorf("decision: binding name %q collides with reserved CEL variable", name)
		}
		opts = append(opts, cel.Variable(name, cel.DynType))
	}
	return cel.NewEnv(opts...)
}

// graphGet is the env-time binding for graph.get(id). It
// reads the per-eval ctx + tracker from the evaluator's
// evalMu-guarded fields populated by eval() before invoking
// cel-go's Eval. Concurrent Eval calls on the same Evaluator
// are serialized by evalMu so the per-eval state is unique
// for the duration of the call.
func (e *Evaluator) graphGet(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("graph.get: argument must be string, got %T", arg.Value())
	}
	tracker := e.currentTracker
	if tracker == nil {
		// Defensive: a bare program.Eval invocation without
		// an active tracker shouldn't happen via this
		// package's API but might if a caller bypasses
		// eval(). Returning null + recording nothing is the
		// safest shape.
		return types.NullValue
	}
	if e.lookup == nil {
		tracker.record(id)
		return types.NullValue
	}
	got, err := e.lookup.Get(e.currentCtx, id)
	if err != nil {
		if errors.Is(err, ErrEntityNotFound) {
			tracker.record(id)
			return types.NullValue
		}
		return types.NewErr("graph.get(%q): %v", id, err)
	}
	return types.DefaultTypeAdapter.NativeToValue(got)
}

// Compile returns a compiled program for expr that can be
// evaluated against any activation. Cached by (expr, returnAs)
// so repeated Compile calls for the same shape are free.
//
// returnAs controls the post-eval type coercion + cache
// keying:
//   - "bool" — predicates; non-bool results return EvalError.
//   - "string" — subject + content templates; CEL stringifies
//     other types via fmt.Sprint-style coercion at eval time.
//   - "dyn" — context.via expressions; the raw ref.Val is
//     returned (typically a map for graph.get() results).
//
// The function returns a Program handle, not the cel.Program
// directly, so the package's missing-ref / EvalError contract
// stays in front of cel-go.
func (e *Evaluator) Compile(expr string, returnAs string) (*Program, error) {
	switch returnAs {
	case "bool", "string", "dyn":
		// recognized
	default:
		return nil, fmt.Errorf("decision: Compile: returnAs %q must be one of {bool, string, dyn}", returnAs)
	}
	key := cacheKey{expr: expr, expectAs: returnAs}

	e.cacheMu.RLock()
	if prog, ok := e.cache[key]; ok {
		e.cacheMu.RUnlock()
		return &Program{evaluator: e, expr: expr, returnAs: returnAs, program: prog}, nil
	}
	e.cacheMu.RUnlock()

	ast, issues := e.env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("decision: parse %q: %w", expr, issues.Err())
	}
	checked, issues := e.env.Check(ast)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("decision: check %q: %w", expr, issues.Err())
	}
	if err := validateLiteralRegexCaptures(checked, expr); err != nil {
		return nil, err
	}
	prog, err := e.env.Program(checked, cel.EvalOptions(cel.OptOptimize))
	if err != nil {
		return nil, fmt.Errorf("decision: program %q: %w", expr, err)
	}

	e.cacheMu.Lock()
	e.cache[key] = prog
	e.cacheMu.Unlock()
	return &Program{evaluator: e, expr: expr, returnAs: returnAs, program: prog}, nil
}

// Program is a compiled CEL expression ready for evaluation.
// Construct via Evaluator.Compile.
type Program struct {
	evaluator *Evaluator
	expr      string
	returnAs  string
	program   cel.Program
}

// EvalBool evaluates the program as a predicate and returns
// the bool result. Used for `condition` expressions. Non-bool
// program results return EvalError.
func (p *Program) EvalBool(ctx context.Context, act Activation) (bool, Result, error) {
	if p.returnAs != "bool" {
		return false, Result{}, fmt.Errorf("decision: EvalBool on program compiled with returnAs=%q", p.returnAs)
	}
	v, res, err := p.eval(ctx, act)
	if err != nil {
		return false, res, err
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, res, &EvalError{
			Expression: p.expr,
			Cause:      fmt.Errorf("predicate returned %T, want bool", v.Value()),
		}
	}
	return b, res, nil
}

// EvalString evaluates the program as a template and returns
// the string result. Used for `subject` + per-action content
// templates. CEL native string-returning expressions
// (entity.slug, entity.name + "@" + entity.year, etc.) just
// work; other types are stringified via Go's default
// representation.
func (p *Program) EvalString(ctx context.Context, act Activation) (string, Result, error) {
	if p.returnAs != "string" {
		return "", Result{}, fmt.Errorf("decision: EvalString on program compiled with returnAs=%q", p.returnAs)
	}
	v, res, err := p.eval(ctx, act)
	if err != nil {
		return "", res, err
	}
	switch x := v.Value().(type) {
	case string:
		return x, res, nil
	default:
		return fmt.Sprint(x), res, nil
	}
}

// EvalDyn evaluates the program and returns the raw value.
// Used for `context.via` expressions where the bound value is
// then exposed as a named CEL variable in subsequent
// predicate / template eval passes.
func (p *Program) EvalDyn(ctx context.Context, act Activation) (any, Result, error) {
	if p.returnAs != "dyn" {
		return nil, Result{}, fmt.Errorf("decision: EvalDyn on program compiled with returnAs=%q", p.returnAs)
	}
	v, res, err := p.eval(ctx, act)
	if err != nil {
		return nil, res, err
	}
	return v.Value(), res, nil
}

// eval is the shared evaluation core. Acquires evalMu so the
// per-Evaluator graphGet closure can read currentTracker +
// currentCtx without racing; builds the activation map +
// invokes cel-go; harvests the tracker for the result.
func (p *Program) eval(ctx context.Context, act Activation) (ref.Val, Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e := p.evaluator
	e.evalMu.Lock()
	defer e.evalMu.Unlock()

	tracker := &refTracker{}
	e.currentTracker = tracker
	e.currentCtx = ctx
	defer func() {
		e.currentTracker = nil
		e.currentCtx = nil
	}()

	inputs := map[string]any{
		"entity": act.Entity,
		"edge":   act.Edge,
	}
	if inputs["entity"] == nil {
		inputs["entity"] = map[string]any{}
	}
	if inputs["edge"] == nil {
		inputs["edge"] = map[string]any{}
	}
	for name, val := range act.Bindings {
		inputs[name] = val
	}

	out, _, err := p.program.Eval(inputs)
	if err != nil {
		return nil, tracker.result(), &EvalError{Expression: p.expr, Cause: err}
	}
	// cel-go surfaces an evaluation error via the ref.Val
	// being a types.Err; check for that shape and translate.
	if errVal, ok := out.(*types.Err); ok {
		return nil, tracker.result(), &EvalError{Expression: p.expr, Cause: fmt.Errorf("%s", errVal.String())}
	}
	return out, tracker.result(), nil
}

// refTracker captures graph.get(id) misses during an eval
// pass so the engine layer can surface them as task notes.
type refTracker struct {
	mu      sync.Mutex
	misses  []MissingRef
	missSet map[string]struct{}
}

func (t *refTracker) record(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.missSet == nil {
		t.missSet = make(map[string]struct{})
	}
	if _, dup := t.missSet[id]; dup {
		return
	}
	t.missSet[id] = struct{}{}
	t.misses = append(t.misses, MissingRef{ID: id})
}

func (t *refTracker) result() Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.misses) == 0 {
		return Result{}
	}
	out := make([]MissingRef, len(t.misses))
	copy(out, t.misses)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return Result{MissingRefs: out}
}

