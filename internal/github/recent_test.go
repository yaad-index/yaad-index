package github

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRecentDays_Default(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "   ", "\t\n"} {
		got, err := ParseRecentDays(raw)
		require.NoError(t, err, "raw=%q", raw)
		assert.Equal(t, DefaultRecentDays, got)
	}
}

func TestParseRecentDays_HappyPath(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"1":   1,
		"7":   7,
		"30":  30,
		"365": 365,
		" 14": 14,
	}
	for raw, want := range cases {
		got, err := ParseRecentDays(raw)
		require.NoError(t, err, "raw=%q", raw)
		assert.Equal(t, want, got, "raw=%q", raw)
	}
}

func TestParseRecentDays_Invalid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"0",
		"-1",
		"-7",
		"abc",
		"7.5",
		"7 days",
		"infinity",
	}
	for _, raw := range cases {
		_, err := ParseRecentDays(raw)
		require.Error(t, err, "raw=%q", raw)
		assert.True(t, errors.Is(err, ErrRecentDaysInvalid), "raw=%q err=%v", raw, err)
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
	// Caller's clock in a non-UTC tz still produces UTC-anchored
	// dates — the function calls .UTC() before the subtraction.
	tz := time.FixedZone("UTC-8", -8*3600)
	local := time.Date(2026, 5, 22, 1, 0, 0, 0, tz) // = 2026-05-22T09:00Z
	assert.Equal(t, "2026-05-15", FormatRecentSince(local, 7))
}
