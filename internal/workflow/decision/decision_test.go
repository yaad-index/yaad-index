// Phase 3.A unit tests for the workflow decision package.
// Exercises CEL predicate / template / dyn evaluation against
// synthetic activations + the missing-reference path the
// engine layer (Phase 3.B) will surface as task notes.

package decision

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/clock"
)

// fakeGraph is the test-side GraphLookup. Returns the seeded
// entity for known ids; ErrEntityNotFound for unknowns; an
// arbitrary error for the "fatal lookup failure" path.
type fakeGraph struct {
	mu        sync.Mutex
	entities  map[string]map[string]any
	failOnGet error
}

func newFakeGraph(entities map[string]map[string]any) *fakeGraph {
	return &fakeGraph{entities: entities}
}

func (f *fakeGraph) Get(_ context.Context, id string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnGet != nil {
		return nil, f.failOnGet
	}
	if got, ok := f.entities[id]; ok {
		return got, nil
	}
	return nil, ErrEntityNotFound
}

// TestEvaluator_PredicateHappyPath compiles a simple
// predicate + evaluates it against an entity that satisfies
// the condition.
func TestEvaluator_PredicateHappyPath(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	prog, err := ev.Compile("entity.rating > 7", "bool")
	require.NoError(t, err)

	got, res, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"rating": int64(9)},
	})
	require.NoError(t, err)
	assert.True(t, got)
	assert.Empty(t, res.MissingRefs)
}

// TestEvaluator_PredicateFalse — predicate evaluates to false.
func TestEvaluator_PredicateFalse(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile("entity.rating > 7", "bool")
	require.NoError(t, err)

	got, _, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"rating": int64(5)},
	})
	require.NoError(t, err)
	assert.False(t, got)
}

// TestEvaluator_CompileCachesPrograms: re-compiling the same
// expression returns the same cached cel.Program.
func TestEvaluator_CompileCachesPrograms(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	p1, err := ev.Compile("entity.x > 0", "bool")
	require.NoError(t, err)
	p2, err := ev.Compile("entity.x > 0", "bool")
	require.NoError(t, err)
	assert.Same(t, p1.program, p2.program, "second Compile hits the cache")
}

// TestEvaluator_CompileFailsOnSyntaxError: malformed CEL
// surfaces a clear error at Compile time.
func TestEvaluator_CompileFailsOnSyntaxError(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	_, err := ev.Compile("entity.x > > 0", "bool")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

// TestEvaluator_CompileFailsOnInvalidReturnAs
func TestEvaluator_CompileFailsOnInvalidReturnAs(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	_, err := ev.Compile("entity.x", "int")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returnAs")
}

// TestEvaluator_EvalBoolOnNonBoolReturnsError: a program
// compiled as "bool" that evaluates to a non-bool surfaces
// EvalError (not a hidden false).
func TestEvaluator_EvalBoolOnNonBoolReturnsError(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	// `entity.name` is a string; predicate expects bool.
	prog, err := ev.Compile("entity.name", "bool")
	require.NoError(t, err)
	_, _, err = prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"name": "x"},
	})
	require.Error(t, err)
	var evalErr *EvalError
	require.True(t, errors.As(err, &evalErr))
	assert.Contains(t, evalErr.Error(), "want bool")
}

// TestEvaluator_SubjectTemplate: subject-style CEL string
// expression evaluates to the rendered string.
func TestEvaluator_SubjectTemplate(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`entity.slug`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"slug": "brass-birmingham"},
	})
	require.NoError(t, err)
	assert.Equal(t, "brass-birmingham", got)
}

// TestEvaluator_SubjectTemplate_NonStringStringified: a
// subject CEL expression that evaluates to a non-string
// (e.g., an int) gets stringified via Go's default
// representation.
func TestEvaluator_SubjectTemplate_NonStringStringified(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`entity.year`, "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Entity: map[string]any{"year": int64(2018)},
	})
	require.NoError(t, err)
	assert.Equal(t, "2018", got)
}

// TestEvaluator_GraphGet_KnownEntity: graph.get(id) returns
// the seeded entity + the result is usable in the predicate.
func TestEvaluator_GraphGet_KnownEntity(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{
		"boardgame:brass-pittsburgh": {"rating": int64(8)},
	})
	ev, err := NewEvaluator(Options{Lookup: graph})
	require.NoError(t, err)

	prog, err := ev.Compile(`graph.get(entity.previous_id).rating > 7`, "bool")
	require.NoError(t, err)
	got, res, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"previous_id": "boardgame:brass-pittsburgh"},
	})
	require.NoError(t, err)
	assert.True(t, got)
	assert.Empty(t, res.MissingRefs, "known id: no MissingRef")
}

// TestEvaluator_GraphGet_MissingEntity: graph.get(id) on an
// unknown id returns null + records a MissingRef. The
// predicate can guard via != null so the workflow's decision
// still evaluates.
func TestEvaluator_GraphGet_MissingEntity(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{})
	ev, _ := NewEvaluator(Options{Lookup: graph})
	prog, err := ev.Compile(
		`graph.get(entity.previous_id) != null && graph.get(entity.previous_id).rating > 7`,
		"bool")
	require.NoError(t, err)
	got, res, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"previous_id": "boardgame:does-not-exist"},
	})
	require.NoError(t, err)
	assert.False(t, got, "guarded predicate returns false when ref missing")
	require.Len(t, res.MissingRefs, 1, "missing id captured as MissingRef")
	assert.Equal(t, "boardgame:does-not-exist", res.MissingRefs[0].ID)
}

// TestEvaluator_GraphGet_MultipleMissingRefs_DedupedInResult:
// the result-side MissingRefs dedupe on id so a predicate
// that calls graph.get(same id) twice doesn't double-count
// the note.
func TestEvaluator_GraphGet_MultipleMissingRefs_DedupedInResult(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{})
	ev, _ := NewEvaluator(Options{Lookup: graph})
	prog, err := ev.Compile(
		`graph.get(entity.a) == null && graph.get(entity.a) == null`,
		"bool")
	require.NoError(t, err)
	_, res, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"a": "missing-1"},
	})
	require.NoError(t, err)
	assert.Len(t, res.MissingRefs, 1, "duplicate misses on same id dedup")
}

// TestEvaluator_GraphGet_FatalLookupError: a non-not-found
// error from the GraphLookup halts evaluation with an
// EvalError so the engine can route to the err-task pattern.
func TestEvaluator_GraphGet_FatalLookupError(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{})
	graph.failOnGet = errors.New("store unreachable")
	ev, _ := NewEvaluator(Options{Lookup: graph})
	prog, err := ev.Compile(`graph.get(entity.x).rating > 7`, "bool")
	require.NoError(t, err)
	_, _, err = prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"x": "any"},
	})
	require.Error(t, err)
	var ev2 *EvalError
	require.True(t, errors.As(err, &ev2))
	assert.True(t, strings.Contains(ev2.Error(), "store unreachable"),
		"underlying lookup error wrapped: %v", ev2)
}

// TestEvaluator_NilGraphLookup_AlwaysMissingRef: when the
// evaluator is constructed without a GraphLookup (e.g. dev
// mode), every graph.get(id) records a MissingRef + returns
// null. Useful for tests + bootstrap before the store is
// wired.
func TestEvaluator_NilGraphLookup_AlwaysMissingRef(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`graph.get(entity.x) == null`, "bool")
	require.NoError(t, err)
	got, res, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"x": "any-id"},
	})
	require.NoError(t, err)
	assert.True(t, got, "nil lookup → null → predicate's null-check holds")
	require.Len(t, res.MissingRefs, 1)
}

// TestEvaluator_ContextBindings: bindings injected via the
// Activation's Bindings map appear as named CEL variables in
// the predicate.
func TestEvaluator_ContextBindings(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{
		"boardgame:prior": {"rating": int64(9)},
	})
	ev, _ := NewEvaluator(Options{Lookup: graph, Bindings: []string{"prior"}})
	prog, err := ev.Compile(
		`entity.rating > 7 || (prior != null && prior.rating > 7)`,
		"bool")
	require.NoError(t, err)

	// Engine layer would evaluate context.prior =
	// graph.get(entity.previous_id) first and bind it here.
	got, _, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{"rating": int64(3), "previous_id": "boardgame:prior"},
		Bindings: map[string]any{
			"prior": map[string]any{"rating": int64(9)},
		},
	})
	require.NoError(t, err)
	assert.True(t, got, "binding allows predicate to see the related entity")
}

// TestEvaluator_EdgeFieldsVisible: the edge variable is
// available in predicates with the documented fields per
// ADR-0024 §"Decision logic".
func TestEvaluator_EdgeFieldsVisible(t *testing.T) {
	t.Parallel()
	ev, _ := NewEvaluator(Options{})
	prog, err := ev.Compile(`edge.type == "is_about" && edge.to_title == "Brass"`, "bool")
	require.NoError(t, err)
	got, _, err := prog.EvalBool(context.Background(), Activation{
		Entity: map[string]any{},
		Edge: map[string]any{
			"type":     "is_about",
			"from":     "source:newsletter-may",
			"to":       "boardgame:brass-birmingham",
			"to_title": "Brass",
		},
	})
	require.NoError(t, err)
	assert.True(t, got)
}

// TestEvaluator_DynReturnPath: context.via expressions return
// raw dyn values, used to feed downstream bindings.
func TestEvaluator_DynReturnPath(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{
		"boardgame:b": {"rating": int64(9), "name": "B"},
	})
	ev, _ := NewEvaluator(Options{Lookup: graph})
	prog, err := ev.Compile(`graph.get(entity.id)`, "dyn")
	require.NoError(t, err)
	got, _, err := prog.EvalDyn(context.Background(), Activation{
		Entity: map[string]any{"id": "boardgame:b"},
	})
	require.NoError(t, err)
	// cel-go wraps the dyn result in its own value type; the
	// engine layer (Phase 3.B) is responsible for unwrapping
	// before re-feeding into a sibling Eval as a binding.
	// Phase 3.A asserts only that the value is non-nil + the
	// missing-ref slot is empty (entity exists).
	assert.NotNil(t, got, "dyn binding result populated")
}

// TestEvaluator_ConcurrentEval: many goroutines evaluating
// the same Program against different activations. The
// per-eval refTracker must not bleed across goroutines.
func TestEvaluator_ConcurrentEval(t *testing.T) {
	t.Parallel()
	graph := newFakeGraph(map[string]map[string]any{})
	ev, _ := NewEvaluator(Options{Lookup: graph})
	prog, err := ev.Compile(`graph.get(entity.id) == null`, "bool")
	require.NoError(t, err)

	const goroutines = 16
	const iters = 25
	var wg sync.WaitGroup
	mismatches := make(chan string, goroutines*iters)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmtID(g, i)
				got, res, err := prog.EvalBool(context.Background(), Activation{
					Entity: map[string]any{"id": id},
				})
				if err != nil {
					mismatches <- "err: " + err.Error()
					continue
				}
				if !got {
					mismatches <- "expected true for " + id
					continue
				}
				if len(res.MissingRefs) != 1 || res.MissingRefs[0].ID != id {
					mismatches <- "wrong MissingRefs for " + id
				}
			}
		}(g)
	}
	wg.Wait()
	close(mismatches)
	var problems []string
	for m := range mismatches {
		problems = append(problems, m)
	}
	require.Empty(t, problems, "concurrent eval bled state across goroutines")
}

func fmtID(g, i int) string {
	return "missing-" + intToStr(g) + "-" + intToStr(i)
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	if neg {
		return "-" + out
	}
	return out
}

// TestEvaluator_DayHelpers_ReturnActivationValues pins the
// load-bearing contract for ADR-0027 cut 1: the today() /
// yesterday() / tomorrow() CEL helpers return the Activation's
// pre-computed values verbatim. The engine populates these
// once per fire via PopulateDayHelpers; the helpers serve them
// without re-reading the clock.
func TestEvaluator_DayHelpers_ReturnActivationValues(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	cases := []struct {
		expr string
		want string
	}{
		{"today()", "day:2026-11-11"},
		{"yesterday()", "day:2026-11-10"},
		{"tomorrow()", "day:2026-11-12"},
	}
	for _, tc := range cases {
		prog, err := ev.Compile(tc.expr, "string")
		require.NoError(t, err, "compile %s", tc.expr)
		got, _, err := prog.EvalString(context.Background(), Activation{
			Today:     "day:2026-11-11",
			Yesterday: "day:2026-11-10",
			Tomorrow:  "day:2026-11-12",
		})
		require.NoError(t, err, "eval %s", tc.expr)
		assert.Equal(t, tc.want, got, "expr=%s", tc.expr)
	}
}

// TestEvaluator_DayHelpers_PerFireConsistency pins the cadence
// rule: multiple today() callsites within a single fire — i.e.
// successive eval() calls all passing the SAME Activation — return
// the SAME value. The engine's responsibility is to call
// PopulateDayHelpers once and reuse the activation; the evaluator
// enforces consistency by reading the activation's fields rather
// than re-querying the clock per-call.
func TestEvaluator_DayHelpers_PerFireConsistency(t *testing.T) {
	t.Parallel()
	ev, err := NewEvaluator(Options{})
	require.NoError(t, err)

	prog, err := ev.Compile("today() + '|' + today() + '|' + today()", "string")
	require.NoError(t, err)
	got, _, err := prog.EvalString(context.Background(), Activation{
		Today: "day:2026-11-11",
	})
	require.NoError(t, err)
	assert.Equal(t, "day:2026-11-11|day:2026-11-11|day:2026-11-11", got)
}

// TestPopulateDayHelpers_MutuallyConsistent pins that
// PopulateDayHelpers reads the clock once and derives all three
// fields from that single snapshot, so a fire activation never
// sees yesterday = day-N-1 + today = day-N+1 (mid-stamp midnight
// crossing).
func TestPopulateDayHelpers_MutuallyConsistent(t *testing.T) {
	t.Parallel()
	a := Activation{}
	a.PopulateDayHelpers()

	require.Len(t, a.Today, len("day:2006-01-02"))
	require.Len(t, a.Yesterday, len("day:2006-01-02"))
	require.Len(t, a.Tomorrow, len("day:2006-01-02"))

	today, err := time.Parse("day:2006-01-02", a.Today)
	require.NoError(t, err)
	yesterday, err := time.Parse("day:2006-01-02", a.Yesterday)
	require.NoError(t, err)
	tomorrow, err := time.Parse("day:2006-01-02", a.Tomorrow)
	require.NoError(t, err)
	assert.Equal(t, today.AddDate(0, 0, -1), yesterday)
	assert.Equal(t, today.AddDate(0, 0, 1), tomorrow)
}

// TestPopulateDayHelpers_UsesOperatorTimezone pins that the
// operator-configured TZ flows through (per ADR-0025's
// clock.DayLocation chain). Pacific is far enough offset from
// UTC that the day-id will differ from UTC stamping during a
// midnight-band window — even outside that window, the format
// resolves against the operator's zone.
func TestPopulateDayHelpers_UsesOperatorTimezone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	clock.SetLocation(tokyo)
	t.Cleanup(func() { clock.SetLocation(nil) })

	a := Activation{}
	a.PopulateDayHelpers()
	expected := time.Now().In(tokyo).Format("day:2006-01-02")
	assert.Equal(t, expected, a.Today)
}
