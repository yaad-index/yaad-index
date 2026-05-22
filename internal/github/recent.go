package github

import "time"

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

// ResolveRecentDays returns the effective N-day rolling window
// for the closed-item sweep per ADR-0026 §4 (2026-05-21
// amendment). When the operator's structured `config:` block
// sets `recent_days`, JSON Schema validation upstream has
// already enforced `>= 1`; this helper just substitutes the
// DefaultRecentDays fallback when the caller passes 0
// (zero-value from a missing operator config).
func ResolveRecentDays(n int) int {
	if n <= 0 {
		return DefaultRecentDays
	}
	return n
}
