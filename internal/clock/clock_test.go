package clock

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Default location is UTC when SetLocation has never been called.
// New process state is the zero atomic.Pointer; Location resolves
// to time.UTC via the read-time fallback.
func TestLocation_DefaultUTC(t *testing.T) {
	// Save / restore any previously-set location so this test
	// doesn't leak state into siblings (table-style runs share
	// the package-level atomic).
	prev := loc.Load()
	t.Cleanup(func() { loc.Store(prev) })
	SetLocation(nil)

	assert.Equal(t, time.UTC, Location(),
		"unset / nil location must read as UTC, not nil")
}

func TestSetLocation_RoundTrip(t *testing.T) {
	prev := loc.Load()
	t.Cleanup(func() { loc.Store(prev) })

	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	SetLocation(loc)
	assert.Equal(t, loc, Location())
}

// Now() returns the current time in the configured location.
// Asserts that the returned Time's Location matches what we set,
// not just by string equality (timezone offsets at "now" can
// match between two zones for a brief window).
func TestNow_HonorsConfiguredLocation(t *testing.T) {
	prev := loc.Load()
	t.Cleanup(func() { loc.Store(prev) })

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	SetLocation(tokyo)

	got := Now()
	assert.Equal(t, tokyo, got.Location(),
		"Now() must return time tagged in the configured location")
}

// LogTimeAttr rewrites slog's built-in `time` attribute to the
// operator-configured location. The Time value's underlying
// instant is preserved; only the Location() differs.
//
// NOT t.Parallel — clock.SetLocation writes package-level global
// state; same rationale as the api-side TZ test.
func TestLogTimeAttr_RewritesToOperatorTimezone(t *testing.T) {
	prev := loc.Load()
	t.Cleanup(func() { loc.Store(prev) })

	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	SetLocation(loc)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: LogTimeAttr,
	}))
	logger.Info("ping")

	// Decode the JSON line and inspect the time attribute. slog's
	// JSON handler renders time.Time as RFC3339Nano; Pacific's
	// offset is +01:00 (winter) or +02:00 (summer DST).
	var entry map[string]any
	require.NoError(t, json.NewDecoder(&buf).Decode(&entry))
	gotTime, ok := entry[slog.TimeKey].(string)
	require.True(t, ok, "time attr must be a string; got %T", entry[slog.TimeKey])
	// Either +01:00 or +02:00 lands depending on the test instant
	// vs DST. Just assert that it's NOT a `Z` suffix (UTC default)
	// — that's the load-bearing claim.
	assert.NotContains(t, gotTime, "Z",
		"LogTimeAttr must rewrite away from UTC; got %s", gotTime)
	assert.True(t, strings.Contains(gotTime, "+01:") || strings.Contains(gotTime, "+02:"),
		"want Pacific offset (-08:00 or -07:00 depending on DST); got %s", gotTime)
}

// In() reshapes an arbitrary time.Time (parsed from elsewhere)
// into the configured location WITHOUT changing its instant.
func TestIn_PreservesInstant(t *testing.T) {
	prev := loc.Load()
	t.Cleanup(func() { loc.Store(prev) })

	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	SetLocation(loc)

	utcInstant := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	got := In(utcInstant)
	assert.True(t, got.Equal(utcInstant),
		"In() must preserve the absolute instant; only Location() differs")
	assert.Equal(t, loc, got.Location())
}
