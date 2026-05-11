// Package fixture provides an in-memory plugins.Plugin implementation
// for tests. Unlike subprocess.Plugin, fixture.Plugin doesn't shell out
// — Match runs a closure, Fetch returns a canned FetchResult — so
// tests of the tracker / handler can register a plugin without
// compiling a binary.
//
// Production code never registers a fixture plugin. The package exists
// solely to satisfy the plugins.Plugin interface from inside _test.go
// files where a real subprocess would be wasteful.
package fixture

import (
	"context"
	"errors"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// Plugin is the in-memory test plugin. Construct via New or by
// populating the fields directly.
type Plugin struct {
	NameValue string
	MatchFunc func(rawURL string) bool
	FetchFunc func(ctx context.Context, rawURL string) (*plugins.FetchResult, error)
	FetchError error
	FetchValue *plugins.FetchResult
	CapabilitiesValue plugins.Capabilities

	// StreamFunc, when set, drives Plugin.Stream — replaces the
	// FetchFunc/FetchValue plumbing for tests of N-envelope flows.
	// The fixture invokes it with the registered onEnvelope /
	// onControl callbacks; the script decides what envelopes / control
	// packets / errors to produce.
	//
	// When StreamFunc is nil and FetchValue / FetchFunc / FetchError
	// is set, Stream synthesizes a single-envelope script from the
	// Fetch path so existing fixtures keep working without
	// modification (one envelope → onEnvelope, then return nil).
	StreamFunc func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error
}

// New builds a fixture plugin with the most common shape: name + a
// substring match against rawURL + a constant FetchResult. Tests with
// fancier needs construct *Plugin directly and set MatchFunc /
// FetchFunc explicitly.
func New(name string, urlSubstring string, result *plugins.FetchResult) *Plugin {
	return &Plugin{
		NameValue: name,
		MatchFunc: func(rawURL string) bool {
			return urlSubstring != "" && containsSubstring(rawURL, urlSubstring)
		},
		FetchValue: result,
	}
}

// Name implements plugins.Plugin.
func (p *Plugin) Name() string { return p.NameValue }

// Match implements plugins.Plugin.
func (p *Plugin) Match(rawURL string) bool {
	if p.MatchFunc == nil {
		return false
	}
	return p.MatchFunc(rawURL)
}

// Capabilities implements plugins.Plugin. Returns the operator-set
// CapabilitiesValue — zero-value if the test fixture didn't supply
// one (which the kinds handler treats as "no kinds contributed").
func (p *Plugin) Capabilities() plugins.Capabilities {
	return p.CapabilitiesValue
}

// Fetch implements plugins.Plugin. Precedence: explicit FetchFunc →
// FetchError → FetchValue. If none are set, returns an error so tests
// fail loud rather than silently see a nil result.
func (p *Plugin) Fetch(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
	if p.FetchFunc != nil {
		return p.FetchFunc(ctx, rawURL)
	}
	if p.FetchError != nil {
		return nil, p.FetchError
	}
	if p.FetchValue != nil {
		return p.FetchValue, nil
	}
	return nil, errors.New("fixture.Plugin: no FetchFunc / FetchError / FetchValue set")
}

// Stream implements plugins.Plugin. Precedence: explicit StreamFunc →
// synthesized single-envelope script from the Fetch path. The
// synthesis lets existing fixtures (FetchValue / FetchFunc) continue
// to work for callers that switched from Fetch to Stream — one
// envelope is delivered to onEnvelope, then Stream returns nil.
//
// Errors from the Fetch path surface as a non-nil Stream return
// (matching the canonical "plugin invocation failed" surface), and
// onEnvelope is NOT invoked.
func (p *Plugin) Stream(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
	if p.StreamFunc != nil {
		return p.StreamFunc(ctx, rawURL, onEnvelope, onControl)
	}
	res, err := p.Fetch(ctx, rawURL)
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	if onEnvelope != nil {
		return onEnvelope(res)
	}
	return nil
}

// containsSubstring is the cheap substring matcher used by New. Kept
// inline to avoid a strings import for the simplest possible fixture.
func containsSubstring(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Compile-time assertion that *Plugin satisfies plugins.Plugin.
var _ plugins.Plugin = (*Plugin)(nil)
