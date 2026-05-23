// Package clock provides operator-timezone-aware time helpers per
// yaad-index. yaad-index is a single-operator system; UTC is
// best-practice for multi-operator setups but creates per-line
// cognitive load when the operator works in a single non-UTC zone
// and has to mentally translate every value. The operator
// configures `timezone:` in the yaad-index config YAML; main.go
// resolves it via time.LoadLocation at boot and calls SetLocation
// here so every subsystem can pick up the operator-chosen zone
// without threading the location through its own constructor.
//
// PR-A scope (this file + main.go wiring) is plumbing-only: no
// existing call sites change yet. Subsequent PRs ( PR-B/C/D)
// switch the various .UTC() / .In(time.UTC) call sites over to
// clock.Now() / clock.In() / value formatting helpers as scope
// allows.
//
// Default location is UTC — preserves legacy behavior for
// operators who don't set `timezone:` in config.

package clock

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// loc is the operator-configured location. atomic.Pointer keeps
// the read path lock-free so high-frequency callers (logging,
// per-fetch provenance stamps) don't contend on a mutex. The zero
// value (nil pointer) is interpreted as UTC by Location() — see
// the Location godoc for why we materialize that on read instead
// of pre-seeding the pointer.
var loc atomic.Pointer[time.Location]

// SetLocation pins the operator-configured location. Called once
// at server startup from main.go after loading + validating the
// config; tests may call it directly to exercise non-UTC paths.
// Idempotent — re-calling overwrites; race-free across
// concurrent readers because of atomic.Pointer.
//
// Passing nil is equivalent to "reset to UTC": the next Location()
// call returns time.UTC. This isn't expected in production wiring
// but is useful for test cleanup.
func SetLocation(l *time.Location) {
	loc.Store(l)
}

// Location returns the operator-configured location, defaulting to
// time.UTC when SetLocation hasn't been called (or was called with
// nil). Always returns a non-nil *time.Location so callers can
// always pass it to .In() without a nil check.
//
// We DON'T pre-seed the atomic with time.UTC because that would
// require an init() function, and an init that runs before main's
// SetLocation is observable as a brief "wrong default" window in
// tests that run main with --timezone America/Los_Angeles (the init's
// UTC value would be the visible state until main reaches its
// SetLocation call). Materializing UTC on read is a one-extra-
// branch cost on the read path; the read path is hot but the
// branch is one CPU cycle and entirely predictable.
func Location() *time.Location {
	if l := loc.Load(); l != nil {
		return l
	}
	return time.UTC
}

// Now returns time.Now() in the operator-configured location.
// Equivalent to `time.Now().In(clock.Location())` — exposed as a
// shorthand because the call sites are dense.
func Now() time.Time {
	return time.Now().In(Location())
}

// In returns t in the operator-configured location. Equivalent to
// `t.In(clock.Location())`. Useful for converting an existing
// time.Time (e.g. a parsed-from-string timestamp) to operator-TZ
// before formatting.
func In(t time.Time) time.Time {
	return t.In(Location())
}

// DayLocation returns the location used to compute day-anchor
// boundaries per ADR-0025 § Timezone. Resolution chain:
//
//  1. Operator-configured location (when SetLocation pinned one) —
//     same source the wider clock package uses for log + provenance
//     timestamps.
//  2. Host system TZ (`time.Local`) as fallback when the operator
//     didn't set `timezone:` in config.
//
// Distinct from Location(): the display-side fallback is UTC
// (preserves legacy behavior for log lines etc); ADR-0025 picks
// host-local for day-resolution because operators reading
// `day:today` expect "today in my wall clock," not "today in UTC."
// Single-operator system, so the host TZ is a reliable proxy for
// "operator's wall clock."
//
// Day-anchor consumers (the cut 2 shape-scan + workflow `today`
// template helper + canonical-ID resolver) should call this rather
// than Location() to honor the ADR-0025 fallback. Display-side
// timestamp consumers (log lines, provenance) keep using Location().
//
// Always returns a non-nil *time.Location.
func DayLocation() *time.Location {
	if l := loc.Load(); l != nil {
		return l
	}
	return time.Local
}

// LogTimeAttr is the slog ReplaceAttr that rewrites the built-in
// `time` attribute to the operator-configured location (per yaad-
// index PR-C). Pass it on slog.HandlerOptions.ReplaceAttr so
// every log line carries the operator-TZ timestamp instead of
// slog's default Local-or-UTC.
//
//	logger := slog.New(slog.NewJSONHandler(os.Stderr,
//	 &slog.HandlerOptions{Level: levelVar, ReplaceAttr: clock.LogTimeAttr}))
//
// Idempotent across multiple handlers in the same process; each
// reads Location() at log-write time, so a SetLocation called
// AFTER handler construction still propagates immediately.
func LogTimeAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Time(slog.TimeKey, a.Value.Time().In(Location()))
	}
	return a
}
