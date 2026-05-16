// Package actions implements the workflow engine's action
// runners per ADR-0024 §"Output surface — action vocabulary".
// After Phase 3's decision pipeline records a Fired=true
// Decision, the engine dispatches each workflow.Action to
// the corresponding runner here.
//
// **Phase 4.A scope.** This PR ships the runner dispatch
// substrate + the task_append primitive. The other three
// primitives (add_comment, plugin_dispatch, add_gap) are
// declared as stub-reject runners — they surface a clear
// "not yet implemented" error so operators get an
// actionable signal between 4.A and 4.B/C merges (mystery
// silent-no-op is worse than visible failure). 4.B/C
// replace the stubs with real implementations.
//
// **Action runner contract.** Each ActionRunner.Run takes
// the workflow + the recorded Decision + the activation
// values (entity / edge / bindings) and reports a slice of
// ActionResult — one per action attempt — naming the
// action type + any error. The engine logs these + (in
// Phase 5) routes errors to the per-workflow err-task
// pattern.

package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// ErrActionNotImplemented is returned by stub runners
// (add_comment, plugin_dispatch, add_gap in Phase 4.A) so
// the engine + log surfaces "this action type is reserved
// for a later phase" cleanly. Replaced as 4.B / 4.C lands.
var ErrActionNotImplemented = errors.New("actions: action type not yet implemented in this phase")

// ErrActionAuthorBug is the typed error for workflow-author
// mistakes (e.g. an action references a gap not in the
// workflow's addable_gaps set). Engine surfaces this as a
// permanent error on the action — not retried, not
// re-evaluated.
var ErrActionAuthorBug = errors.New("actions: workflow-author error")

// Decision is the subset of engine.Decision the runners
// need. Kept narrow + local so the actions package doesn't
// import the engine (avoiding the import cycle: engine
// imports actions to dispatch).
type Decision struct {
	Workflow string
	EntityID string
	Subject  string
	At       time.Time

	// DedupKey is the rendered per-pattern dedup key from
	// workflow.Dedup.Key per ADR-0024 §"Per-pattern
	// de-duplication". TaskWriter stamps it on the task's
	// frontmatter so a future "look up task by dedup key"
	// surface (or operator inspection) can match this
	// fire's identity. Empty when the workflow has no
	// dedup.key or the render failed.
	DedupKey string
}

// Activation carries the per-fire CEL-evaluation context
// (entity / edge / bindings) plus the engine's pre-rendered
// values for each action's CEL template fields. Same Entity /
// Edge / Bindings shape as decision.Activation; re-declared
// locally to avoid a cross-package import in either direction.
type Activation struct {
	Entity   map[string]any
	Edge     map[string]any
	Bindings map[string]any

	// RenderedTemplates carries the engine's pre-rendered
	// values for action-level CEL templates (per ADR-0024
	// §"Workflow" — action `target` / `content` / `entity`
	// fields). Indexed by action position (0-based) → field
	// name → rendered string.
	//
	// The map is nil when no template renderer is wired
	// (legacy / test paths) — action runners fall back to the
	// raw action.<Field> string verbatim. The map is non-nil
	// in production: engines wire a renderer at register
	// time, and a non-nil map whose entry lacks an expected
	// field logs a Warn (drift signal) before falling back,
	// so an engine that forgets to populate a templated field
	// surfaces the gap at execute time instead of silently
	// running with unrendered CEL source.
	RenderedTemplates map[int]map[string]string
}

// ActionResult names one action attempt's outcome. One
// entry per workflow.Actions element. Type identifies the
// primitive (task_append / add_comment / plugin_dispatch /
// add_gap); Err is nil on success.
type ActionResult struct {
	// ActionIdx is the action's position in
	// workflow.Actions (0-based) so the engine can
	// correlate result to the source action without
	// re-walking the list.
	ActionIdx int

	// Type is the action primitive name ("task_append",
	// "add_comment", "plugin_dispatch", "add_gap").
	Type string

	// Err is nil on a successful action run; non-nil
	// names the failure cause. ErrActionNotImplemented
	// for stub runners; ErrActionAuthorBug wraps
	// workflow-author mistakes (e.g. add_gap targeting a
	// gap outside addable_gaps); other errors wrap the
	// underlying primitive's failure (vault write,
	// plugin dispatch timeout, etc.).
	Err error
}

// Runner is the public dispatch surface. The engine holds
// one Runner instance; per-fire it calls Run with the
// workflow + decision + activation and gets back a per-
// action result slice.
type Runner interface {
	// Run executes each action in workflow.Actions in
	// declaration order. Returns a slice of ActionResult
	// (one per action) so the caller can surface
	// per-action errors. A nil-slice return is acceptable
	// when workflow.Actions is empty (defensive — the
	// parser already enforces non-empty).
	Run(ctx context.Context, workflow *parser.Workflow, decision Decision, activation Activation) []ActionResult
}

// TaskWriter is the vault-backed task surface the
// task_append runner depends on. Production wires a
// vault.Writer-backed implementation; tests use a
// fakeTaskWriter that records calls.
//
// AppendTaskSection finds-or-creates the canonical task
// file at `tasks/<workflow>-<subject>.md`, appends the
// given content line to the named section, and applies
// the ifAlreadyPresent policy (skip / replace / append-
// anyway) on duplicate content lines. dedupKey, when
// non-empty, is stamped into the task's frontmatter on
// first create so cross-fire identity stays inspectable
// per ADR-0024 §"Per-pattern de-duplication".
type TaskWriter interface {
	AppendTaskSection(
		ctx context.Context,
		workflow string,
		subject string,
		dedupKey string,
		section string,
		content string,
		ifAlreadyPresent string,
	) error
}

// Options configures a Runner. Each writer field is
// optional; nil writers produce clear configuration-error
// results on the matching action type so the engine doesn't
// silently drop them.
type Options struct {
	// TaskWriter backs task_append. Production wires a
	// FileTaskWriter rooted at the vault path.
	TaskWriter TaskWriter

	// CommentWriter backs add_comment. Production wires a
	// stub (Phase 4.B) → vault-backed impl (Phase 4.B.2).
	CommentWriter CommentWriter

	// GapWriter backs add_gap. Same Phase 4.B stub → 4.B.2
	// vault-backed shape as CommentWriter.
	GapWriter GapWriter

	// PluginDispatcher backs plugin_dispatch. Production wires
	// a stub (Phase 4.C) → registry-backed impl (Phase 4.C.2).
	PluginDispatcher PluginDispatcher

	// Logger receives drift-warning lines when an action
	// runner falls back to a raw template field because the
	// engine's pre-rendered Activation.RenderedTemplates map
	// is missing the expected key. Nil → discarding handler
	// (test-friendly default; production wires the daemon's
	// slog).
	Logger *slog.Logger
}

// New constructs a Runner with the given options. The
// returned Runner dispatches per-action by primitive:
//   - task_append → taskAppendRunner backed by
//     opts.TaskWriter.
//   - add_comment → addCommentRunner backed by
//     opts.CommentWriter.
//   - add_gap → addGapRunner backed by opts.GapWriter.
//   - plugin_dispatch → pluginDispatchRunner backed by
//     opts.PluginDispatcher.
func New(opts Options) Runner {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &dispatcher{
		taskWriter:       opts.TaskWriter,
		commentWriter:    opts.CommentWriter,
		gapWriter:        opts.GapWriter,
		pluginDispatcher: opts.PluginDispatcher,
		logger:           logger,
	}
}

// dispatcher routes per-action work by primitive. Holds the
// per-primitive writer dependencies; per-action runners are
// methods that close over the dispatcher's fields.
type dispatcher struct {
	taskWriter       TaskWriter
	commentWriter    CommentWriter
	gapWriter        GapWriter
	pluginDispatcher PluginDispatcher
	logger           *slog.Logger
}

// rendered returns the engine's pre-rendered value for
// (actionIdx, field) from act.RenderedTemplates, or the raw
// fallback. When the map is non-nil but lacks the expected
// (idx, field) entry, logs a Warn before falling back —
// surfacing engine drift (a templated field the engine forgot
// to render). When the map is nil entirely, falls back
// silently (legacy / test path).
func (d *dispatcher) rendered(act Activation, idx int, field, raw string) string {
	if act.RenderedTemplates == nil {
		return raw
	}
	if fields, ok := act.RenderedTemplates[idx]; ok {
		if v, ok := fields[field]; ok {
			return v
		}
	}
	d.logger.Warn(
		"workflow action: rendered-template missing; engine drift — falling back to raw field",
		"action_idx", idx,
		"field", field,
	)
	return raw
}

func (d *dispatcher) Run(ctx context.Context, wf *parser.Workflow, dec Decision, act Activation) []ActionResult {
	if len(wf.Actions) == 0 {
		return nil
	}
	out := make([]ActionResult, len(wf.Actions))
	for i, action := range wf.Actions {
		out[i] = d.runOne(ctx, i, wf, action, dec, act)
	}
	return out
}

// runOne executes a single action by inspecting which
// primitive variant is set on the Action. Per-primitive
// failures land in the returned ActionResult; the
// dispatcher continues to the next action regardless of
// per-action failures (the engine policy is "best effort
// across actions" — one failing action doesn't block the
// others).
func (d *dispatcher) runOne(ctx context.Context, idx int, wf *parser.Workflow, a parser.Action, dec Decision, act Activation) ActionResult {
	switch {
	case a.TaskAppend != nil:
		return d.runTaskAppend(ctx, idx, wf, a.TaskAppend, dec, act)
	case a.AddComment != nil:
		return d.runAddComment(ctx, idx, wf, a.AddComment, dec, act)
	case a.AddGap != nil:
		return d.runAddGap(ctx, idx, wf, a.AddGap, dec, act)
	case a.PluginDispatch != nil:
		return d.runPluginDispatch(ctx, idx, wf, a.PluginDispatch, dec, act)
	default:
		return ActionResult{
			ActionIdx: idx, Type: "unknown",
			Err: fmt.Errorf("actions[%d]: no primitive set (workflow parser should have rejected)", idx),
		}
	}
}

// NopRunner is a runner that records every Run invocation
// but performs no work. Useful for tests that exercise the
// engine wiring without caring about the action
// side-effects, and for dev binaries running without
// vault wiring.
type NopRunner struct{}

// Run on NopRunner reports an "ok stub" result for each
// action. Engine logs the run; no side effects.
func (NopRunner) Run(_ context.Context, wf *parser.Workflow, _ Decision, _ Activation) []ActionResult {
	if len(wf.Actions) == 0 {
		return nil
	}
	out := make([]ActionResult, len(wf.Actions))
	for i, a := range wf.Actions {
		t := "unknown"
		switch {
		case a.TaskAppend != nil:
			t = "task_append"
		case a.AddComment != nil:
			t = "add_comment"
		case a.PluginDispatch != nil:
			t = "plugin_dispatch"
		case a.AddGap != nil:
			t = "add_gap"
		}
		out[i] = ActionResult{ActionIdx: i, Type: t}
	}
	return out
}
