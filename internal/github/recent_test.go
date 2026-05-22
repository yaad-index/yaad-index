package github

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveRecentDays_DefaultsOnZero(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultRecentDays, ResolveRecentDays(0))
}

func TestResolveRecentDays_DefaultsOnNegative(t *testing.T) {
	t.Parallel()
	// Defensive — JSON Schema upstream enforces minimum:1 so a
	// negative value can't reach the plugin via the validated
	// config channel, but the helper still substitutes the
	// default rather than producing a negative-window query.
	assert.Equal(t, DefaultRecentDays, ResolveRecentDays(-7))
}

func TestResolveRecentDays_PassesThroughPositive(t *testing.T) {
	t.Parallel()
	cases := []int{1, 7, 30, 365}
	for _, n := range cases {
		assert.Equal(t, n, ResolveRecentDays(n))
	}
}

func TestFormatRecentSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 22, 18, 30, 0, 0, time.UTC)
	cases := map[int]string{
		1:  "2026-05-21",
		7:  "2026-05-15",
		14: "2026-05-08",
		30: "2026-04-22",
	}
	for days, want := range cases {
		assert.Equal(t, want, FormatRecentSince(now, days), "days=%d", days)
	}
}

func TestFormatRecentSince_UTCNormalisation(t *testing.T) {
	t.Parallel()
	tz := time.FixedZone("UTC-8", -8*3600)
	local := time.Date(2026, 5, 22, 1, 0, 0, 0, tz) // = 2026-05-22T09:00Z
	assert.Equal(t, "2026-05-15", FormatRecentSince(local, 7))
}
