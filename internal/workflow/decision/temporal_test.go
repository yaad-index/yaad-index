package decision

import (
	"context"
	"testing"

	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evalString compiles + evaluates an expression with the given
// activation and returns the string result. Helper for the
// temporal-CEL tests.
func evalString(t *testing.T, ev *Evaluator, expr string, act Activation) string {
	t.Helper()
	prog, err := ev.Compile(expr, "string")
	require.NoError(t, err, "compile %q", expr)
	got, _, err := prog.EvalString(context.Background(), act)
	require.NoError(t, err, "eval %q", expr)
	return got
}

// evalInt compiles + evaluates an int-returning expression via
// the dyn surface (decision.go has no dedicated EvalInt — int
// return values flow through EvalDyn as native int64).
func evalInt(t *testing.T, ev *Evaluator, expr string, act Activation) int64 {
	t.Helper()
	got := evalDyn(t, ev, expr, act)
	v, ok := got.(int64)
	require.True(t, ok, "expected int64 from %q, got %T", expr, got)
	return v
}

// evalDyn compiles + evaluates with `dyn` return type — used for
// list-returning helpers.
func evalDyn(t *testing.T, ev *Evaluator, expr string, act Activation) any {
	t.Helper()
	prog, err := ev.Compile(expr, "dyn")
	require.NoError(t, err, "compile %q", expr)
	got, _, err := prog.EvalDyn(context.Background(), act)
	require.NoError(t, err, "eval %q", expr)
	return got
}

// TestAddDays_HappyPath: forward + backward + zero + cross-month
// + cross-year arithmetic.
func TestAddDays_HappyPath(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr string
		want string
	}{
		{`add_days("day:2026-11-11", 7)`, "day:2026-11-18"},
		{`add_days("day:2026-11-11", 0)`, "day:2026-11-11"},
		{`add_days("day:2026-11-11", -1)`, "day:2026-11-10"},
		{`add_days("day:2026-11-30", 1)`, "day:2026-12-01"},
		{`add_days("day:2026-12-31", 1)`, "day:2027-01-01"},
		{`add_days("day:2026-01-01", -1)`, "day:2025-12-31"},
		{`add_days("day:2028-02-28", 1)`, "day:2028-02-29"},
		{`add_days("day:2028-03-01", -1)`, "day:2028-02-29"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, evalString(t, ev, tc.expr, Activation{}),
			"expr=%s", tc.expr)
	}
}

// TestDaysBetween_Signed: positive when b is after a, negative
// otherwise. Includes the symmetric-zero case.
func TestDaysBetween_Signed(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr string
		want int64
	}{
		{`days_between("day:2026-11-11", "day:2026-11-18")`, 7},
		{`days_between("day:2026-11-18", "day:2026-11-11")`, -7},
		{`days_between("day:2026-11-11", "day:2026-11-11")`, 0},
		{`days_between("day:2025-12-31", "day:2026-01-01")`, 1},
		{`days_between("day:2028-02-28", "day:2028-03-01")`, 2},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, evalInt(t, ev, tc.expr, Activation{}),
			"expr=%s", tc.expr)
	}
}

// TestWeekOf_ISOSemantics: the load-bearing cross-year case from
// ADR-0027 §2a. Late-Dec dates may belong to ISO week 01 of the
// NEXT calendar year.
func TestWeekOf_ISOSemantics(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr string
		want string
	}{
		{`week_of("day:2025-12-29")`, "2026-W01"}, // ADR-quoted case
		{`week_of("day:2026-01-01")`, "2026-W01"},
		{`week_of("day:2026-01-04")`, "2026-W01"},
		{`week_of("day:2026-01-05")`, "2026-W02"},
		{`week_of("day:2026-11-11")`, "2026-W46"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, evalString(t, ev, tc.expr, Activation{}),
			"expr=%s", tc.expr)
	}
}

// TestMonthOf_AndYearOf: straightforward calendar extraction.
func TestMonthOf_AndYearOf(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	assert.Equal(t, "2026-11", evalString(t, ev, `month_of("day:2026-11-11")`, Activation{}))
	assert.Equal(t, "2028-02", evalString(t, ev, `month_of("day:2028-02-29")`, Activation{}))
	assert.Equal(t, "2026", evalString(t, ev, `year_of("day:2026-11-11")`, Activation{}))
	assert.Equal(t, "2025", evalString(t, ev, `year_of("day:2025-12-31")`, Activation{}))
}

// TestDaysInWeek_ISOCrossYearCases: ADR-quoted edge case.
// Both 2025-W53 and 2026-W01 contain late-Dec-2025 / early-Jan-2026
// days; need correct ISO mapping in both directions.
func TestDaysInWeek_ISOCrossYearCases(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	// 2026-W01: per Go's ISOWeek logic, Dec 29 2025 falls in 2026-W01.
	// Monday of 2026-W01 is Dec 29 2025.
	w1 := evalDyn(t, ev, `days_in_week("2026-W01")`, Activation{})
	w1List, ok := w1.([]ref.Val)
	require.True(t, ok, "got %T", w1)
	require.Len(t, w1List, 7)
	assert.Equal(t, "day:2025-12-29", w1List[0].Value())
	assert.Equal(t, "day:2026-01-04", w1List[6].Value())

	// 2026-W53: 2026 starts Thursday (years starting Thursday or
	// leap years starting Wednesday have 53 ISO weeks). The 53rd
	// ISO week of 2026 contains Dec 28 2026 - Jan 3 2027.
	w53 := evalDyn(t, ev, `days_in_week("2026-W53")`, Activation{})
	w53List, ok := w53.([]ref.Val)
	require.True(t, ok)
	require.Len(t, w53List, 7)
	assert.Equal(t, "day:2026-12-28", w53List[0].Value())
	assert.Equal(t, "day:2027-01-03", w53List[6].Value())
}

// TestDaysInMonth_LeapYearAware: Feb has 28 normally, 29 on leap
// years; other months land 30 or 31 per calendar.
func TestDaysInMonth_LeapYearAware(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr  string
		count int
	}{
		{`days_in_month("2026-02")`, 28},
		{`days_in_month("2028-02")`, 29}, // leap
		{`days_in_month("2026-04")`, 30},
		{`days_in_month("2026-07")`, 31},
		{`days_in_month("2026-12")`, 31},
	}
	for _, tc := range cases {
		got := evalDyn(t, ev, tc.expr, Activation{})
		list, ok := got.([]ref.Val)
		require.True(t, ok, "%s: %T", tc.expr, got)
		assert.Equal(t, tc.count, len(list), "expr=%s", tc.expr)
	}

	// Feb 29 included in 2028 specifically.
	got2028 := evalDyn(t, ev, `days_in_month("2028-02")`, Activation{}).([]ref.Val)
	assert.Equal(t, "day:2028-02-29", got2028[28].Value())
}

// TestDaysInYear_LeapVsCommon: 366 in leap years, 365 otherwise.
func TestDaysInYear_LeapVsCommon(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr  string
		count int
	}{
		{`days_in_year("2026")`, 365},
		{`days_in_year("2028")`, 366}, // leap
		{`days_in_year("2025")`, 365},
		{`days_in_year("2000")`, 366}, // century leap
		{`days_in_year("1900")`, 365}, // century non-leap
	}
	for _, tc := range cases {
		got := evalDyn(t, ev, tc.expr, Activation{})
		list, ok := got.([]ref.Val)
		require.True(t, ok)
		assert.Equal(t, tc.count, len(list), "expr=%s", tc.expr)
	}
}

// TestCurrentPeriodHelpers_ReturnActivationValues pins the
// per-fire caching contract for ADR-0027 cut 2: the this_week /
// this_month / this_year CEL helpers return the Activation's
// pre-computed values verbatim, same pattern as today() in cut 1.
func TestCurrentPeriodHelpers_ReturnActivationValues(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr string
		want string
	}{
		{"this_week()", "2026-W21"},
		{"this_month()", "2026-05"},
		{"this_year()", "2026"},
	}
	act := Activation{
		ThisWeek:  "2026-W21",
		ThisMonth: "2026-05",
		ThisYear:  "2026",
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, evalString(t, ev, tc.expr, act),
			"expr=%s", tc.expr)
	}
}

// TestCurrentPeriodHelpers_PerFireConsistency pins that multiple
// callsites in one fire see the SAME period id even after a
// week / month / year boundary crossing — the activation pre-
// populates and freezes the snapshot.
func TestCurrentPeriodHelpers_PerFireConsistency(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	got := evalString(t, ev,
		`this_week() + "|" + this_week() + "|" + this_week()`,
		Activation{ThisWeek: "2026-W21"})
	assert.Equal(t, "2026-W21|2026-W21|2026-W21", got)
}

// TestPopulateDayHelpers_ExtendsToCurrentPeriod pins that the
// cut 1 PopulateDayHelpers now populates the cut 2 period fields
// too from the same clock snapshot.
func TestPopulateDayHelpers_ExtendsToCurrentPeriod(t *testing.T) {
	t.Parallel()
	a := Activation{}
	a.PopulateDayHelpers()

	require.NotEmpty(t, a.ThisWeek, "this_week id populated")
	require.NotEmpty(t, a.ThisMonth, "this_month id populated")
	require.NotEmpty(t, a.ThisYear, "this_year id populated")

	// Shape checks: ThisWeek matches YYYY-Www, ThisMonth matches
	// YYYY-MM, ThisYear matches YYYY. Length-pinning is enough —
	// the per-helper tests above pin the values themselves.
	assert.Len(t, a.ThisWeek, 8, "YYYY-Www = 8 chars")
	assert.Len(t, a.ThisMonth, 7, "YYYY-MM = 7 chars")
	assert.Len(t, a.ThisYear, 4, "YYYY = 4 chars")

	// Mutual consistency: a.Today's calendar year matches
	// a.ThisYear (so a fire never sees Today on the new year
	// but ThisYear still on the old year mid-stamp).
	assert.Equal(t, a.ThisYear, a.Today[4:8],
		"today + this_year derived from the same clock snapshot")
}

// TestComposability_WeeklyDigest pins the worked-example shape from
// ADR-0027 §2a: chaining days_in_week + this_week + map should
// compose cleanly without type-coercion or extension friction.
// (Doesn't graph-walk — that's cut 3. But pins that the period
// helpers plug into the standard CEL list operations.)
func TestComposability_WeeklyDigest(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	got := evalDyn(t, ev,
		`days_in_week("2026-W21").map(d, d + "/x")`,
		Activation{})
	list, ok := got.([]ref.Val)
	require.True(t, ok, "%T", got)
	require.Len(t, list, 7)
	assert.Equal(t, "day:2026-05-18/x", list[0].Value(),
		"map composes — period helpers return list<string> that flows through CEL macros")
}
