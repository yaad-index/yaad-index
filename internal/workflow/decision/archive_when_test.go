package decision

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestEvaluateArchiveWhen_PerPrimitive table-drives each primitive
// in isolation per ADR-0030 §2. Each case sets exactly one primitive
// and varies the EntityView to cross the predicate's truth boundary.
func TestEvaluateArchiveWhen_PerPrimitive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		predicate *parser.ArchiveWhen
		view      EntityView
		want      bool
	}{
		// AllGapsResolved
		{
			name:      "all_gaps_resolved=true on entity with no unfilled gaps → true",
			predicate: &parser.ArchiveWhen{AllGapsResolved: true},
			view:      EntityView{HasUnfilledGaps: false},
			want:      true,
		},
		{
			name:      "all_gaps_resolved=true on entity with unfilled gaps → false",
			predicate: &parser.ArchiveWhen{AllGapsResolved: true},
			view:      EntityView{HasUnfilledGaps: true},
			want:      false,
		},

		// HasEdges
		{
			name:      "has_edges single type present → true",
			predicate: &parser.ArchiveWhen{HasEdges: []string{"is_about"}},
			view:      EntityView{OutgoingEdgeTypes: []string{"is_about"}},
			want:      true,
		},
		{
			name:      "has_edges single type missing → false",
			predicate: &parser.ArchiveWhen{HasEdges: []string{"is_about"}},
			view:      EntityView{OutgoingEdgeTypes: []string{"references"}},
			want:      false,
		},
		{
			name:      "has_edges multiple types all present → true",
			predicate: &parser.ArchiveWhen{HasEdges: []string{"is_about", "is_actionable_for"}},
			view:      EntityView{OutgoingEdgeTypes: []string{"is_about", "is_actionable_for", "designed_by"}},
			want:      true,
		},
		{
			name:      "has_edges multiple types one missing → false (AND semantics)",
			predicate: &parser.ArchiveWhen{HasEdges: []string{"is_about", "is_actionable_for"}},
			view:      EntityView{OutgoingEdgeTypes: []string{"is_about"}},
			want:      false,
		},

		// FieldEquals
		{
			name:      "field_equals single field match → true",
			predicate: &parser.ArchiveWhen{FieldEquals: map[string]any{"is_actionable": "no"}},
			view:      EntityView{Data: map[string]any{"is_actionable": "no", "title": "irrelevant"}},
			want:      true,
		},
		{
			name:      "field_equals single field mismatch → false",
			predicate: &parser.ArchiveWhen{FieldEquals: map[string]any{"is_actionable": "no"}},
			view:      EntityView{Data: map[string]any{"is_actionable": "yes"}},
			want:      false,
		},
		{
			name:      "field_equals field missing → false",
			predicate: &parser.ArchiveWhen{FieldEquals: map[string]any{"is_actionable": "no"}},
			view:      EntityView{Data: map[string]any{"unrelated": "x"}},
			want:      false,
		},
		{
			name:      "field_equals multiple fields all match → true (implicit AND across keys)",
			predicate: &parser.ArchiveWhen{FieldEquals: map[string]any{"state": "closed", "is_actionable": "no"}},
			view:      EntityView{Data: map[string]any{"state": "closed", "is_actionable": "no"}},
			want:      true,
		},
		{
			name:      "field_equals multiple fields one mismatch → false (AND semantics)",
			predicate: &parser.ArchiveWhen{FieldEquals: map[string]any{"state": "closed", "is_actionable": "no"}},
			view:      EntityView{Data: map[string]any{"state": "closed", "is_actionable": "yes"}},
			want:      false,
		},
		{
			name:      "field_equals nil data with required field → false (defensive miss)",
			predicate: &parser.ArchiveWhen{FieldEquals: map[string]any{"any_field": "anyvalue"}},
			view:      EntityView{Data: nil},
			want:      false,
		},

		// Nil + empty edge cases
		{
			name:      "nil predicate → false (workflow opted out)",
			predicate: nil,
			view:      EntityView{HasUnfilledGaps: false},
			want:      false,
		},
		{
			name:      "empty predicate (no primitive populated) → false (defensive)",
			predicate: &parser.ArchiveWhen{},
			view:      EntityView{HasUnfilledGaps: false},
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EvaluateArchiveWhen(tc.predicate, tc.view)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEvaluateArchiveWhen_FieldEquals_NumericCrossType pins value-equal
// numeric comparison for field_equals: entity data round-trips through
// the store's JSON column as float64, while the workflow config parses
// the same literal from YAML as int. A plain reflect.DeepEqual would
// treat float64(5) and int(5) as unequal, so a numeric field_equals
// would silently never match.
func TestEvaluateArchiveWhen_FieldEquals_NumericCrossType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		want  any // field_equals config value
		got   any // entity data value
		match bool
	}{
		{"config int vs data float64 (the bug)", 5, float64(5), true},
		{"config float64 vs data int", float64(5), 5, true},
		{"config int64 vs data float64", int64(5), float64(5), true},
		{"numeric not equal", 5, float64(7), false},
		{"fractional float equal", 2.5, float64(2.5), true},
		{"number vs string not equal", 5, "5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &parser.ArchiveWhen{FieldEquals: map[string]any{"rating": tc.want}}
			v := EntityView{Data: map[string]any{"rating": tc.got}}
			assert.Equal(t, tc.match, EvaluateArchiveWhen(p, v))
		})
	}
}

// TestEvaluateArchiveWhen_SiblingPrimitivesAndImplicitly pins the
// ADR-0030 §2 semantic: declaring multiple primitives at the same
// level AND together (no explicit all_of wrapper required). Both
// must hold for the composite to be true.
func TestEvaluateArchiveWhen_SiblingPrimitivesAndImplicitly(t *testing.T) {
	t.Parallel()
	pred := &parser.ArchiveWhen{
		AllGapsResolved: true,
		FieldEquals:     map[string]any{"is_actionable": "no"},
	}

	t.Run("both branches true → composite true", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: false,
			Data:            map[string]any{"is_actionable": "no"},
		})
		assert.True(t, got)
	})

	t.Run("gaps-branch fails → composite false", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: true,
			Data:            map[string]any{"is_actionable": "no"},
		})
		assert.False(t, got)
	})

	t.Run("field-branch fails → composite false", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: false,
			Data:            map[string]any{"is_actionable": "yes"},
		})
		assert.False(t, got)
	})
}

// TestEvaluateArchiveWhen_AnyOfComposition pins the OR composition
// per ADR-0030 §2. The composite is true iff at least one nested
// predicate is true.
func TestEvaluateArchiveWhen_AnyOfComposition(t *testing.T) {
	t.Parallel()
	pred := &parser.ArchiveWhen{
		AnyOf: []parser.ArchiveWhen{
			{AllGapsResolved: true},
			{FieldEquals: map[string]any{"state": "closed"}},
		},
	}

	t.Run("first branch true → composite true", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{HasUnfilledGaps: false})
		assert.True(t, got)
	})

	t.Run("second branch true → composite true", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: true, // first branch fails
			Data:            map[string]any{"state": "closed"},
		})
		assert.True(t, got)
	})

	t.Run("neither branch true → composite false", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: true,
			Data:            map[string]any{"state": "open"},
		})
		assert.False(t, got)
	})
}

// TestEvaluateArchiveWhen_AllOfComposition pins the explicit AND
// composition per ADR-0030 §2. Equivalent to declaring sibling
// primitives directly; the explicit form is for nested branches.
func TestEvaluateArchiveWhen_AllOfComposition(t *testing.T) {
	t.Parallel()
	pred := &parser.ArchiveWhen{
		AllOf: []parser.ArchiveWhen{
			{AllGapsResolved: true},
			{HasEdges: []string{"is_about"}},
		},
	}

	t.Run("all branches true → composite true", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps:   false,
			OutgoingEdgeTypes: []string{"is_about"},
		})
		assert.True(t, got)
	})

	t.Run("one branch fails → composite false", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps:   false,
			OutgoingEdgeTypes: []string{"references"}, // missing is_about
		})
		assert.False(t, got)
	})
}

// TestEvaluateArchiveWhen_NestedAnyAllOfComposition pins the
// recursive case: any_of holding all_of branches. ADR-0030 §2
// requires the evaluator to walk arbitrary nesting depth.
func TestEvaluateArchiveWhen_NestedAnyAllOfComposition(t *testing.T) {
	t.Parallel()
	pred := &parser.ArchiveWhen{
		AnyOf: []parser.ArchiveWhen{
			{
				AllOf: []parser.ArchiveWhen{
					{AllGapsResolved: true},
					{FieldEquals: map[string]any{"category": "auto"}},
				},
			},
			{
				FieldEquals: map[string]any{"state": "closed"},
			},
		},
	}

	t.Run("nested all_of all true → composite true via first any_of branch", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: false,
			Data:            map[string]any{"category": "auto"},
		})
		assert.True(t, got)
	})

	t.Run("nested all_of partial → composite true via second any_of branch", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: true, // first nested all_of's all_gaps_resolved fails
			Data:            map[string]any{"category": "auto", "state": "closed"},
		})
		assert.True(t, got)
	})

	t.Run("neither any_of branch true → composite false", func(t *testing.T) {
		t.Parallel()
		got := EvaluateArchiveWhen(pred, EntityView{
			HasUnfilledGaps: true,
			Data:            map[string]any{"category": "manual", "state": "open"},
		})
		assert.False(t, got)
	})
}
