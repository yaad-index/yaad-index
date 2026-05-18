package actions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// fakePlugin is a minimal plugins.Plugin for the dispatcher
// tests. Capabilities + Fetch are the only methods exercised;
// the rest panic to flag accidental dependencies.
type fakePlugin struct {
	name         string
	commands     []string
	fetchInputs  []string
	fetchErr     error
	fetchDelay   time.Duration
	fetchResult  *plugins.FetchResult
}

func (p *fakePlugin) Name() string { return p.name }

func (p *fakePlugin) Capabilities() plugins.Capabilities {
	return plugins.Capabilities{
		Name:     p.name,
		Commands: p.commands,
	}
}

func (p *fakePlugin) Fetch(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
	if p.fetchDelay > 0 {
		select {
		case <-ctx.Done():
			p.fetchInputs = append(p.fetchInputs, rawURL)
			return nil, ctx.Err()
		case <-time.After(p.fetchDelay):
		}
	}
	p.fetchInputs = append(p.fetchInputs, rawURL)
	if p.fetchErr != nil {
		return nil, p.fetchErr
	}
	return p.fetchResult, nil
}

func (p *fakePlugin) Match(string) bool { panic("not used") }

func (p *fakePlugin) Stream(context.Context, string, plugins.EnvelopeFunc, plugins.ControlFunc) error {
	panic("not used")
}

func (p *fakePlugin) Search(context.Context, string, int) ([]plugins.SearchCandidate, error) {
	panic("not used")
}

// fakePluginRegistry is the in-memory PluginRegistry for the
// dispatcher tests. Holds a plugin set keyed by name.
type fakePluginRegistry struct {
	plugins map[string]plugins.Plugin
}

func newFakePluginRegistry(ps ...*fakePlugin) *fakePluginRegistry {
	m := make(map[string]plugins.Plugin, len(ps))
	for _, p := range ps {
		m[p.name] = p
	}
	return &fakePluginRegistry{plugins: m}
}

func (r *fakePluginRegistry) LookupByName(name string) (plugins.Plugin, bool) {
	p, ok := r.plugins[name]
	return p, ok
}

// TestRegistryPluginDispatcher_HappyPath: a known plugin with
// the named command in its Capabilities.Commands list
// dispatches successfully — the invocation is the canonical
// `<plugin>: !<command>` shape per ADR-0022.
func TestRegistryPluginDispatcher_HappyPath(t *testing.T) {
	t.Parallel()
	p := &fakePlugin{name: "yaad-bgg", commands: []string{"fetch"}}
	reg := newFakePluginRegistry(p)
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	err = d.Dispatch(context.Background(), "yaad-bgg", "fetch", nil)
	require.NoError(t, err)
	require.Len(t, p.fetchInputs, 1)
	assert.Equal(t, "yaad-bgg: !fetch", p.fetchInputs[0])
}

// TestRegistryPluginDispatcher_UnknownPlugin: a plugin name
// not in the registry returns ErrUnknownPlugin. The parser
// load-time check usually catches this; the runtime check is
// the hot-reload safety net.
func TestRegistryPluginDispatcher_UnknownPlugin(t *testing.T) {
	t.Parallel()
	reg := newFakePluginRegistry()
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	err = d.Dispatch(context.Background(), "yaad-absent", "fetch", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownPlugin)
	assert.Contains(t, err.Error(), "yaad-absent")
}

// TestRegistryPluginDispatcher_UnknownCommand: a known plugin
// + command outside its advertised Commands list returns
// ErrUnknownCommand. Per ADR-0022 §4 routing validation —
// the daemon rejects mismatched commands before spawn.
func TestRegistryPluginDispatcher_UnknownCommand(t *testing.T) {
	t.Parallel()
	p := &fakePlugin{name: "yaad-gmail", commands: []string{"fetch"}}
	reg := newFakePluginRegistry(p)
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	err = d.Dispatch(context.Background(), "yaad-gmail", "sync", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownCommand)
	assert.Contains(t, err.Error(), "sync")
	assert.Contains(t, err.Error(), "yaad-gmail")
	assert.Empty(t, p.fetchInputs, "no plugin.Fetch call on command-mismatch")
}

// TestRegistryPluginDispatcher_PluginNoCommands: a plugin with
// an empty Commands list rejects every command dispatch. This
// is the case where a URL-shape-only plugin (yaad-wikipedia,
// yaad-bgg pre-ADR-0022) is named in a workflow that tries
// command-shape against it.
func TestRegistryPluginDispatcher_PluginNoCommands(t *testing.T) {
	t.Parallel()
	p := &fakePlugin{name: "yaad-bgg-old", commands: nil}
	reg := newFakePluginRegistry(p)
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	err = d.Dispatch(context.Background(), "yaad-bgg-old", "fetch", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownCommand)
}

// TestRegistryPluginDispatcher_FetchError: a plugin.Fetch
// failure wraps the underlying error with the invocation
// string for operator debugging.
func TestRegistryPluginDispatcher_FetchError(t *testing.T) {
	t.Parallel()
	p := &fakePlugin{
		name:     "yaad-gmail",
		commands: []string{"fetch"},
		fetchErr: errors.New("plugin subprocess crashed"),
	}
	reg := newFakePluginRegistry(p)
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	err = d.Dispatch(context.Background(), "yaad-gmail", "fetch", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin subprocess crashed")
	assert.Contains(t, err.Error(), "yaad-gmail: !fetch")
}

// TestRegistryPluginDispatcher_ContextTimeout: the dispatcher
// respects ctx cancellation (the runner-side ctx already
// carries the action's TimeoutSeconds budget per PR-85). A
// slow plugin gets the cancelled ctx and returns
// context.DeadlineExceeded.
func TestRegistryPluginDispatcher_ContextTimeout(t *testing.T) {
	t.Parallel()
	p := &fakePlugin{
		name:       "yaad-bgg",
		commands:   []string{"fetch"},
		fetchDelay: 500 * time.Millisecond,
	}
	reg := newFakePluginRegistry(p)
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = d.Dispatch(ctx, "yaad-bgg", "fetch", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestRegistryPluginDispatcher_NilRegistry: constructing with
// a nil registry returns an error so the daemon's main wiring
// surfaces the mis-config at boot rather than at first
// dispatch.
func TestRegistryPluginDispatcher_NilRegistry(t *testing.T) {
	t.Parallel()
	_, err := NewRegistryPluginDispatcher(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry is required")
}

// TestRegistryPluginDispatcher_ArgsAreOpaque: the action's
// Args map is currently dropped by the dispatcher (v1 narrow
// scope; ADR-0022 doesn't specify the structured-args wire
// format yet). This test pins the behavior so a future change
// — threading args into the invocation string or plugin
// stdin — explicitly updates the contract.
func TestRegistryPluginDispatcher_ArgsAreOpaque(t *testing.T) {
	t.Parallel()
	p := &fakePlugin{name: "yaad-bgg", commands: []string{"fetch"}}
	reg := newFakePluginRegistry(p)
	d, err := NewRegistryPluginDispatcher(reg)
	require.NoError(t, err)

	err = d.Dispatch(context.Background(), "yaad-bgg", "fetch", map[string]any{
		"id":   "boardgame:acme-game",
		"hint": "rerun",
	})
	require.NoError(t, err)
	require.Len(t, p.fetchInputs, 1)
	// The args don't appear in the invocation in v1.
	assert.Equal(t, "yaad-bgg: !fetch", p.fetchInputs[0])
}
