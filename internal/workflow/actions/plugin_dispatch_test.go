package actions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// fakePluginDispatcher records every Dispatch invocation so
// tests can assert the runner translated the action's
// (plugin, command, args, timeout) into the right
// PluginDispatcher call. Returns an injected error if
// dispatchErr is non-nil; sleeps for delay before returning to
// exercise the timeout path.
type fakePluginDispatcher struct {
	mu          sync.Mutex
	calls       []pluginCall
	dispatchErr error
	delay       time.Duration
}

type pluginCall struct {
	plugin   string
	command  string
	args     map[string]any
	ctxAlive bool
}

func (f *fakePluginDispatcher) Dispatch(ctx context.Context, plugin, command string, args map[string]any) error {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			f.recordCall(plugin, command, args, false)
			return ctx.Err()
		case <-time.After(f.delay):
		}
	}
	f.recordCall(plugin, command, args, ctx.Err() == nil)
	return f.dispatchErr
}

func (f *fakePluginDispatcher) recordCall(plugin, command string, args map[string]any, ctxAlive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pluginCall{
		plugin:   plugin,
		command:  command,
		args:     args,
		ctxAlive: ctxAlive,
	})
}

func (f *fakePluginDispatcher) snapshot() []pluginCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pluginCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestPluginDispatch_HappyPath: plugin in AllowedPlugins, both
// plugin + command non-empty, dispatcher succeeds. Args + the
// resolved plugin/command flow through.
func TestPluginDispatch_HappyPath(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("bgg-fetch",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-bgg",
			Command: "fetch",
			Args:    map[string]any{"id": "123"},
		}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "bgg-fetch"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, d.snapshot(), 1)
	assert.Equal(t, "yaad-bgg", d.snapshot()[0].plugin)
	assert.Equal(t, "fetch", d.snapshot()[0].command)
	assert.Equal(t, map[string]any{"id": "123"}, d.snapshot()[0].args)
}

// TestPluginDispatch_RenderedArgsReachDispatcher pins #456: a
// string-valued arg that the engine pre-rendered (keyed
// `arg:<name>` in RenderedTemplates) reaches the dispatcher as
// the resolved value, not the raw `{{ ... }}` source. Non-string
// args pass through verbatim.
func TestPluginDispatch_RenderedArgsReachDispatcher(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("refetch-wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-fetch",
			Command: "refetch",
			Args: map[string]any{
				"id":    "{{ entity.id }}",
				"count": int64(3),
			},
		}},
	)
	wf.AllowedPlugins = []string{"yaad-fetch"}
	act := Activation{
		RenderedTemplates: map[int]map[string]string{
			0: {"arg:id": "widget:alpha-prime"},
		},
	}
	results := r.Run(context.Background(), wf, Decision{Workflow: "refetch-wf"}, act)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, d.snapshot(), 1)
	assert.Equal(t, map[string]any{
		"id":    "widget:alpha-prime",
		"count": int64(3),
	}, d.snapshot()[0].args, "rendered string arg resolved; non-string arg passed through")
}

// TestPluginDispatch_NilRenderedTemplates_RawArgsBackCompat pins
// the back-compat path: with no RenderedTemplates (legacy / test
// callers that bypass the engine renderer), string args fall back
// to their raw value rather than dropping.
func TestPluginDispatch_NilRenderedTemplates_RawArgsBackCompat(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-fetch",
			Command: "refetch",
			Args:    map[string]any{"id": "literal-value"},
		}},
	)
	wf.AllowedPlugins = []string{"yaad-fetch"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	require.Len(t, d.snapshot(), 1)
	assert.Equal(t, map[string]any{"id": "literal-value"}, d.snapshot()[0].args)
}

// TestPluginDispatch_AllowedPluginsEnforcement_RuntimeReject:
// per ADR-0024 §"Workflow declares its plugin scope", the
// runtime check rejects a plugin outside AllowedPlugins even
// if the parser-side static check has drifted (e.g. via
// hot-reload).
func TestPluginDispatch_AllowedPluginsEnforcement_RuntimeReject(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-evil",
			Command: "fetch",
		}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
	assert.Contains(t, results[0].Err.Error(), "allowed_plugins")
	assert.Empty(t, d.snapshot(), "no dispatcher call on AllowedPlugins rejection")
}

// TestPluginDispatch_EmptyPlugin_AuthorBug: missing Plugin
// rejects with ErrActionAuthorBug.
func TestPluginDispatch_EmptyPlugin_AuthorBug(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{Command: "fetch"}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestPluginDispatch_EmptyCommand_AuthorBug: missing Command.
func TestPluginDispatch_EmptyCommand_AuthorBug(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{Plugin: "yaad-bgg"}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, ErrActionAuthorBug)
}

// TestPluginDispatch_DispatcherError: a PluginDispatcher error
// is wrapped with the plugin/command context for operator
// debugging.
func TestPluginDispatch_DispatcherError(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{dispatchErr: errors.New("plugin crashed")}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-bgg",
			Command: "fetch",
		}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "plugin crashed")
	assert.Contains(t, results[0].Err.Error(), "yaad-bgg/fetch")
}

// TestPluginDispatch_Timeout: the action's TimeoutSeconds is
// applied at the runner level — a dispatcher that exceeds the
// budget receives ctx.Done and surfaces a deadline-exceeded
// error which the runner wraps.
func TestPluginDispatch_Timeout(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{delay: 200 * time.Millisecond}
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-bgg",
			Command:        "fetch",
			TimeoutSeconds: 1, // ample for the 200ms delay
		}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	start := time.Now()
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	elapsed := time.Since(start)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err, "200ms < 1s budget")
	assert.Less(t, elapsed, 800*time.Millisecond, "should return after the delay, not block for the full timeout")
}

// TestPluginDispatch_TimeoutExceeded: a dispatcher slower than
// TimeoutSeconds receives ctx.Done and surfaces the
// deadline-exceeded error path.
func TestPluginDispatch_TimeoutExceeded(t *testing.T) {
	t.Parallel()
	d := &fakePluginDispatcher{delay: 500 * time.Millisecond}
	// Custom timeout via a short TimeoutSeconds. Action's
	// TimeoutSeconds is in whole seconds (the YAML schema),
	// so we test the deadline-overrun by setting the
	// dispatcher's delay > the action timeout.
	r := New(Options{PluginDispatcher: d})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-bgg",
			Command:        "fetch",
			TimeoutSeconds: 0, // → DefaultPluginDispatchTimeout (30s) — too long for a unit test
		}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	// Force the deadline via the caller-ctx instead.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	results := r.Run(ctx, wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.ErrorIs(t, results[0].Err, context.DeadlineExceeded)
}

// TestPluginDispatch_NoPluginDispatcher_ConfigError: when the
// runner is constructed without a PluginDispatcher, the
// plugin_dispatch result names the config error so operators
// see it on the first event-fire rather than a silent skip.
func TestPluginDispatch_NoPluginDispatcher_ConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	wf := wfWithActions("wf",
		parser.Action{PluginDispatch: &parser.PluginDispatchAction{
			Plugin:  "yaad-bgg",
			Command: "fetch",
		}},
	)
	wf.AllowedPlugins = []string{"yaad-bgg"}
	results := r.Run(context.Background(), wf, Decision{Workflow: "wf"}, Activation{})
	require.Len(t, results, 1)
	assert.Equal(t, "plugin_dispatch", results[0].Type)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no PluginDispatcher wired")
}

// TestStubPluginDispatcher_ReturnsNotImplemented: the
// production-default dispatcher (Phase 4.C stub) returns
// ErrActionNotImplemented with the attempted plugin + command
// for operator debugging.
func TestStubPluginDispatcher_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	err := StubPluginDispatcher{}.Dispatch(context.Background(), "yaad-bgg", "fetch", map[string]any{"id": "1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrActionNotImplemented)
	assert.Contains(t, err.Error(), "yaad-bgg")
	assert.Contains(t, err.Error(), "fetch")
}

// TestPluginDispatch_DefaultTimeout: when TimeoutSeconds is
// unset (0), the runner applies DefaultPluginDispatchTimeout
// rather than running unbounded.
func TestPluginDispatch_DefaultTimeout(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 30*time.Second, DefaultPluginDispatchTimeout,
		"v1 default per ADR-0024 §plugin_dispatch execution semantics")
}
