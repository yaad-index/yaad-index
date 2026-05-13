package plugins

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// fakePlugin is a tiny in-test Plugin used to exercise the registry's
// dispatch order. Distinct from the production `fixture` package
// (which lives in internal/plugins/fixture) so the registry tests
// don't import their own subdirectory.
type fakePlugin struct {
	name string
	matchPred func(string) bool
}

func (p *fakePlugin) Name() string { return p.name }
func (p *fakePlugin) Match(raw string) bool { return p.matchPred(raw) }
func (p *fakePlugin) Fetch(_ context.Context, _ string) (*FetchResult, error) {
	return nil, errors.New("fakePlugin.Fetch never called in registry tests")
}

func (p *fakePlugin) Stream(_ context.Context, _ string, _ EnvelopeFunc, _ ControlFunc) error {
	return errors.New("fakePlugin.Stream never called in registry tests")
}
func (p *fakePlugin) Capabilities() Capabilities { return Capabilities{} }
func (p *fakePlugin) Search(_ context.Context, _ string, _ int) ([]SearchCandidate, error) {
	return nil, ErrSearchNotSupported
}

// compile-time interface assertion (mirrors what we do for the real
// implementations in subprocess + fixture).
var _ Plugin = (*fakePlugin)(nil)

func TestRegistry_LookupReturnsFirstMatch(t *testing.T) {
	t.Parallel()

	a := &fakePlugin{name: "a", matchPred: func(s string) bool { return s == "a-url" }}
	b := &fakePlugin{name: "b", matchPred: func(s string) bool { return s == "b-url" }}
	r := NewRegistry()
	r.Register(a)
	r.Register(b)

	got, ok := r.Lookup("a-url")
	require.True(t, ok, "Lookup(a-url) should match")
	assert.Equal(t, "a", got.Name())

	got, ok = r.Lookup("b-url")
	require.True(t, ok, "Lookup(b-url) should match")
	assert.Equal(t, "b", got.Name())
}

func TestRegistry_LookupReturnsFalseWhenNoMatch(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakePlugin{name: "x", matchPred: func(s string) bool { return s == "specific" }})

	got, ok := r.Lookup("anything-else")
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestRegistry_FirstMatchWinsOnAmbiguity(t *testing.T) {
	t.Parallel()

	matchAll := func(string) bool { return true }
	r := NewRegistry()
	r.Register(&fakePlugin{name: "first", matchPred: matchAll})
	r.Register(&fakePlugin{name: "second", matchPred: matchAll})

	got, ok := r.Lookup("anything")
	require.True(t, ok)
	assert.Equal(t, "first", got.Name(), "ambiguous match: want first (registration order)")
}

func TestRegistry_PluginsListReflectsRegistrationOrder(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakePlugin{name: "alpha"})
	r.Register(&fakePlugin{name: "beta"})
	r.Register(&fakePlugin{name: "gamma"})

	got := r.Plugins()
	require.Len(t, got, 3)
	gotNames := []string{got[0].Name(), got[1].Name(), got[2].Name()}
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, gotNames)
}

func TestRegistry_EmptyLookupReturnsFalse(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_, ok := r.Lookup("anything")
	assert.False(t, ok)
}

// silence "imported and not used" if store ever drops out of the file
// without affecting the test interface.
var _ = store.Entity{}
