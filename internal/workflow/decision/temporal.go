// CEL temporal helpers per ADR-0027 cut 2 (#231): date arithmetic
// + period helpers. All operate on string ids (day:YYYY-MM-DD /
// YYYY-Www / YYYY-MM / YYYY) — no custom CEL types, no DB access,
// no clock reads from within the helpers (the this_* current-period
// helpers serve their value from Activation cached at fire-start).
//
// ISO 8601 week semantics throughout: Monday-start, week containing
// Jan 4 is week 01. Go's time.Time.ISOWeek() implements the same
// rule so cross-year cases (Dec 29 2025 → ISO week 01 of 2026)
// resolve correctly.

package decision

import (
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

const (
	dayDateFormat   = "2006-01-02"          // bare date used after stripping the "day:" prefix
	dayIDFormat     = "day:" + dayDateFormat // day:YYYY-MM-DD canonical id
	monthIDFormat   = "2006-01"             // YYYY-MM
	yearIDFormat    = "2006"                // YYYY
	weekIDSeparator = "-W"                  // ISO week ids use YYYY-Www
)

// parseDayID strips the "day:" prefix and parses the date in UTC.
// Returns a UTC midnight time for the named day.
func parseDayID(id string) (time.Time, error) {
	if len(id) < 4 || id[:4] != "day:" {
		return time.Time{}, fmt.Errorf("expected day:YYYY-MM-DD id, got %q", id)
	}
	return time.ParseInLocation(dayDateFormat, id[4:], time.UTC)
}

// formatDay reformats a UTC time back to the canonical day:YYYY-MM-DD.
func formatDay(t time.Time) string {
	return t.UTC().Format(dayIDFormat)
}

// formatWeek formats an ISO (year, week) pair as YYYY-Www with
// zero-padded two-digit week.
func formatWeek(isoYear, isoWeek int) string {
	return fmt.Sprintf("%04d%s%02d", isoYear, weekIDSeparator, isoWeek)
}

// parseWeekID parses a YYYY-Www id into (isoYear, isoWeek).
// Strict on shape — exactly four digits, then "-W", then two digits.
func parseWeekID(id string) (isoYear, isoWeek int, err error) {
	if len(id) != 8 || id[4:6] != weekIDSeparator {
		return 0, 0, fmt.Errorf("expected YYYY-Www id, got %q", id)
	}
	if _, err := fmt.Sscanf(id, "%04d-W%02d", &isoYear, &isoWeek); err != nil {
		return 0, 0, fmt.Errorf("parse week id %q: %w", id, err)
	}
	if isoWeek < 1 || isoWeek > 53 {
		return 0, 0, fmt.Errorf("week id %q: week out of range", id)
	}
	return isoYear, isoWeek, nil
}

// parseMonthID parses YYYY-MM into a UTC midnight time at the first
// of the month.
func parseMonthID(id string) (time.Time, error) {
	return time.ParseInLocation(monthIDFormat, id, time.UTC)
}

// parseYearID parses YYYY into a UTC midnight time at Jan 1.
func parseYearID(id string) (time.Time, error) {
	return time.ParseInLocation(yearIDFormat, id, time.UTC)
}

// mondayOfISOWeek returns the UTC midnight time at the Monday of
// the requested ISO (year, week). Jan 4 is guaranteed to fall in
// week 01 of its ISO-year; back-walking to that week's Monday and
// adding (isoWeek-1)*7 days lands the target Monday.
func mondayOfISOWeek(isoYear, isoWeek int) time.Time {
	jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, time.UTC)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7 // Sunday: Go returns 0, ISO uses 7
	}
	week1Monday := jan4.AddDate(0, 0, -(wd - 1))
	return week1Monday.AddDate(0, 0, (isoWeek-1)*7)
}

// addDaysBinding implements add_days(day_id, n). String + int → string.
func addDaysBinding(args ...ref.Val) ref.Val {
	id, ok := args[0].Value().(string)
	if !ok {
		return types.NewErr("add_days: arg 0: expected string, got %T", args[0].Value())
	}
	n, ok := args[1].Value().(int64)
	if !ok {
		return types.NewErr("add_days: arg 1: expected int, got %T", args[1].Value())
	}
	t, err := parseDayID(id)
	if err != nil {
		return types.NewErr("add_days: %v", err)
	}
	return types.String(formatDay(t.AddDate(0, 0, int(n))))
}

// daysBetweenBinding implements days_between(a, b). Signed —
// positive when b is in the future relative to a.
func daysBetweenBinding(args ...ref.Val) ref.Val {
	a, ok := args[0].Value().(string)
	if !ok {
		return types.NewErr("days_between: arg 0: expected string, got %T", args[0].Value())
	}
	b, ok := args[1].Value().(string)
	if !ok {
		return types.NewErr("days_between: arg 1: expected string, got %T", args[1].Value())
	}
	ta, err := parseDayID(a)
	if err != nil {
		return types.NewErr("days_between: arg 0: %v", err)
	}
	tb, err := parseDayID(b)
	if err != nil {
		return types.NewErr("days_between: arg 1: %v", err)
	}
	// UTC midnight-to-midnight, exactly 24h per day, no DST surprises.
	diff := tb.Sub(ta) / (24 * time.Hour)
	return types.Int(int64(diff))
}

// daysInWeekBinding implements days_in_week("YYYY-Www"). Returns
// 7 day-ids starting at the ISO week's Monday.
func daysInWeekBinding(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("days_in_week: expected string, got %T", arg.Value())
	}
	isoYear, isoWeek, err := parseWeekID(id)
	if err != nil {
		return types.NewErr("days_in_week: %v", err)
	}
	monday := mondayOfISOWeek(isoYear, isoWeek)
	out := make([]ref.Val, 7)
	for i := 0; i < 7; i++ {
		out[i] = types.String(formatDay(monday.AddDate(0, 0, i)))
	}
	return types.DefaultTypeAdapter.NativeToValue(out)
}

// daysInMonthBinding implements days_in_month("YYYY-MM"). Returns
// 28-31 day-ids depending on the month. Go's time package handles
// leap years natively (Feb 29 exists in leap years, not otherwise).
func daysInMonthBinding(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("days_in_month: expected string, got %T", arg.Value())
	}
	first, err := parseMonthID(id)
	if err != nil {
		return types.NewErr("days_in_month: %v", err)
	}
	// Last day = first of next month - 1 day. Go's AddDate normalizes
	// out-of-range months automatically.
	nextMonth := first.AddDate(0, 1, 0)
	count := int(nextMonth.Sub(first) / (24 * time.Hour))
	out := make([]ref.Val, count)
	for i := 0; i < count; i++ {
		out[i] = types.String(formatDay(first.AddDate(0, 0, i)))
	}
	return types.DefaultTypeAdapter.NativeToValue(out)
}

// daysInYearBinding implements days_in_year("YYYY"). Returns 365 or
// 366 day-ids; Feb 29 included on leap years.
func daysInYearBinding(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("days_in_year: expected string, got %T", arg.Value())
	}
	first, err := parseYearID(id)
	if err != nil {
		return types.NewErr("days_in_year: %v", err)
	}
	nextYear := first.AddDate(1, 0, 0)
	count := int(nextYear.Sub(first) / (24 * time.Hour))
	out := make([]ref.Val, count)
	for i := 0; i < count; i++ {
		out[i] = types.String(formatDay(first.AddDate(0, 0, i)))
	}
	return types.DefaultTypeAdapter.NativeToValue(out)
}

// weekOfBinding implements week_of("day:YYYY-MM-DD") → "YYYY-Www".
// The ISO-week-year is NOT always the calendar year (Dec 29 2025
// returns "2026-W01" per ISO 8601).
func weekOfBinding(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("week_of: expected string, got %T", arg.Value())
	}
	t, err := parseDayID(id)
	if err != nil {
		return types.NewErr("week_of: %v", err)
	}
	y, w := t.ISOWeek()
	return types.String(formatWeek(y, w))
}

// monthOfBinding implements month_of("day:YYYY-MM-DD") → "YYYY-MM".
func monthOfBinding(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("month_of: expected string, got %T", arg.Value())
	}
	t, err := parseDayID(id)
	if err != nil {
		return types.NewErr("month_of: %v", err)
	}
	return types.String(t.Format(monthIDFormat))
}

// yearOfBinding implements year_of("day:YYYY-MM-DD") → "YYYY".
func yearOfBinding(arg ref.Val) ref.Val {
	id, ok := arg.Value().(string)
	if !ok {
		return types.NewErr("year_of: expected string, got %T", arg.Value())
	}
	t, err := parseDayID(id)
	if err != nil {
		return types.NewErr("year_of: %v", err)
	}
	return types.String(t.Format(yearIDFormat))
}

// temporalFunctions returns the cel.Function options for the 8
// helpers that don't depend on activation state. The 3 current-
// period helpers (this_week / this_month / this_year) bind through
// the Evaluator's per-eval state — registered separately alongside
// today() in buildEnv.
func temporalFunctions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("add_days",
			cel.Overload("add_days_string_int",
				[]*cel.Type{cel.StringType, cel.IntType},
				cel.StringType,
				cel.FunctionBinding(addDaysBinding),
			),
		),
		cel.Function("days_between",
			cel.Overload("days_between_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.IntType,
				cel.FunctionBinding(daysBetweenBinding),
			),
		),
		cel.Function("days_in_week",
			cel.Overload("days_in_week_string",
				[]*cel.Type{cel.StringType},
				cel.ListType(cel.StringType),
				cel.UnaryBinding(daysInWeekBinding),
			),
		),
		cel.Function("days_in_month",
			cel.Overload("days_in_month_string",
				[]*cel.Type{cel.StringType},
				cel.ListType(cel.StringType),
				cel.UnaryBinding(daysInMonthBinding),
			),
		),
		cel.Function("days_in_year",
			cel.Overload("days_in_year_string",
				[]*cel.Type{cel.StringType},
				cel.ListType(cel.StringType),
				cel.UnaryBinding(daysInYearBinding),
			),
		),
		cel.Function("week_of",
			cel.Overload("week_of_string",
				[]*cel.Type{cel.StringType},
				cel.StringType,
				cel.UnaryBinding(weekOfBinding),
			),
		),
		cel.Function("month_of",
			cel.Overload("month_of_string",
				[]*cel.Type{cel.StringType},
				cel.StringType,
				cel.UnaryBinding(monthOfBinding),
			),
		),
		cel.Function("year_of",
			cel.Overload("year_of_string",
				[]*cel.Type{cel.StringType},
				cel.StringType,
				cel.UnaryBinding(yearOfBinding),
			),
		),
	}
}
