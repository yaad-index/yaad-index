package github

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrRecentDaysInvalid surfaces when the operator's
// `YAAD_GITHUB_RECENT_DAYS` env value isn't a positive integer.
// The bulk-fetch handler converts this to an `_error` control
// packet at startup so the daemon-side NDJSON consumer logs the
// cause before any upstream call.
var ErrRecentDaysInvalid = errors.New("github: YAAD_GITHUB_RECENT_DAYS must be a positive integer")

// ParseRecentDays returns the rolling-window day count the
// closed-item sweep uses per ADR-0026 §4 (2026-05-21 amendment).
// Empty / whitespace input returns DefaultRecentDays so operators
// who haven't set the env var get the documented default.
//
// Validation: the value must parse as an integer >= 1. Negative
// values, zero, and non-integer shapes all return
// ErrRecentDaysInvalid — closed items can't have surfaced from
// the future, and a zero-day window makes the closed search a
// no-op that the operator would not intentionally configure.
func ParseRecentDays(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultRecentDays, nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%w: got %q", ErrRecentDaysInvalid, raw)
	}
	if n < 1 {
		return 0, fmt.Errorf("%w: got %d", ErrRecentDaysInvalid, n)
	}
	return n, nil
}

// RecentSinceQueryFormat is the date layout the GitHub Search
// API expects for the `updated:>=` filter. ISO-8601 date with
// no time component matches Search's coarsest precision and
// keeps the query stable across timezone boundaries (the API
// interprets bare dates as UTC midnight).
const RecentSinceQueryFormat = "2006-01-02"

// FormatRecentSince renders the start-of-window date for the
// closed search query. Given a reference instant + days, it
// returns the ISO date `days` days back in UTC — suitable for
// splicing into the `updated:>=<date>` GitHub Search filter.
func FormatRecentSince(now time.Time, days int) string {
	return now.UTC().AddDate(0, 0, -days).Format(RecentSinceQueryFormat)
}
