package edgewrite

import "context"

// Mode discriminates the caller's auto-resolve behavior per
// #304 Cut C2. Default Interactive — auto-mode is opt-in via
// `context.WithValue(ctx, modeKey, Auto)`.
type Mode int

const (
	// Interactive (zero value) preserves the daemon's
	// pre-#304 edge-write behavior: edges land as-supplied,
	// disambiguation surfaces inline to the caller, no
	// resolution-task is created. Every HTTP entry point
	// (manual POST /v1/edges, /v1/ingest, fills, UGC
	// frontmatter, etc.) leaves the context default.
	Interactive Mode = iota

	// Auto enables resolver-aware edge-write per Cut C2 /
	// Cut C3. Workflow-spawned edges set this mode at the
	// action-dispatch root because no human is in the loop
	// to disambiguate inline; the daemon either resolves
	// single-match names inline (Cut C2) or defers to a
	// resolution-task (Cut C3).
	Auto
)

// String returns a stable rendering for logs + WARN messages.
func (m Mode) String() string {
	switch m {
	case Auto:
		return "auto"
	default:
		return "interactive"
	}
}

// modeContextKey is the unexported context key the mode
// helpers stamp + read. Unexported so callers can't collide
// from outside the package — every mode change goes through
// WithMode.
type modeContextKey struct{}

// WithMode returns a context derived from parent with the
// caller-mode value stamped per Cut C2. Callers that need to
// flip auto-mode set this at the dispatch root (workflow
// engine before action-runner invocation); everything else
// inherits the parent's mode (Interactive by default).
func WithMode(parent context.Context, m Mode) context.Context {
	return context.WithValue(parent, modeContextKey{}, m)
}

// ModeFromContext reads the caller-mode stamped on ctx via
// WithMode. Returns Interactive when no value has been set —
// the safe default that preserves pre-#304 behavior across
// every entry point that hasn't opted in.
func ModeFromContext(ctx context.Context) Mode {
	if ctx == nil {
		return Interactive
	}
	v, _ := ctx.Value(modeContextKey{}).(Mode)
	return v
}
