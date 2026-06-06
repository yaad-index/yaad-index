// archive_when predicate evaluator per ADR-0030. Pure function over
// a small read-only entity view; no engine wiring lives here. The
// engine (Cut 3) builds the view from post-action entity state and
// calls EvaluateArchiveWhen; on true the engine invokes the same
// archive code path the existing /v1/entities/{id}/archive surface
// uses.

package decision

import (
	"reflect"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// EntityView is the read-only entity surface the archive_when
// evaluator inspects. Holds only what the predicate vocabulary
// cares about so the evaluator is decoupled from the full store /
// vault entity types — tests can build a view inline without
// spinning up the storage stack. The producer (engine-side
// post-action hook in Cut 3) builds this from the entity's current
// state.
type EntityView struct {
	// HasUnfilledGaps is true iff at least one gap on the entity
	// is still unfilled-and-undeferred. The predicate primitive
	// AllGapsResolved evaluates true when this is false.
	HasUnfilledGaps bool

	// OutgoingEdgeTypes is the set of edge types present on the
	// entity (canonical names — `is_about`, `is_actionable_for`,
	// …). Order is not meaningful; membership is. The HasEdges
	// primitive checks that every listed type appears here.
	OutgoingEdgeTypes []string

	// Data is the entity's frontmatter `data` map. The FieldEquals
	// primitive looks up keys here. Nil/empty data with a
	// FieldEquals primitive that names any field evaluates to
	// false (the field is missing → not equal).
	Data map[string]any
}

// EvaluateArchiveWhen returns true iff the predicate matches the
// entity view per ADR-0030 §2. Nil predicate returns false — a
// workflow without archive_when does not opt into archive. Empty
// predicate (no populated primitive) also returns false defensively,
// even though the parser's Validate gate rejects that shape at
// parse time; this keeps the evaluator robust when called from
// inline-constructed predicates in tests or future call paths.
//
// Sibling primitives AND together implicitly — declaring
// AllGapsResolved=true and FieldEquals together means both must
// hold. AnyOf is OR, AllOf is AND.
func EvaluateArchiveWhen(p *parser.ArchiveWhen, v EntityView) bool {
	if p == nil {
		return false
	}
	if !evaluateAllGapsResolved(p, v) {
		return false
	}
	if !evaluateHasEdges(p, v) {
		return false
	}
	if !evaluateFieldEquals(p, v) {
		return false
	}
	if !evaluateAnyOf(p, v) {
		return false
	}
	if !evaluateAllOf(p, v) {
		return false
	}
	// At least one primitive must have populated for the predicate
	// to evaluate true; the no-primitive case returns false even
	// when every gate trivially passed (vacuous truth would archive
	// every entity which is the opposite of the opt-in design).
	return archiveWhenHasAnyPrimitive(p)
}

func evaluateAllGapsResolved(p *parser.ArchiveWhen, v EntityView) bool {
	if !p.AllGapsResolved {
		return true
	}
	return !v.HasUnfilledGaps
}

func evaluateHasEdges(p *parser.ArchiveWhen, v EntityView) bool {
	if len(p.HasEdges) == 0 {
		return true
	}
	present := make(map[string]struct{}, len(v.OutgoingEdgeTypes))
	for _, t := range v.OutgoingEdgeTypes {
		present[t] = struct{}{}
	}
	for _, required := range p.HasEdges {
		if _, ok := present[required]; !ok {
			return false
		}
	}
	return true
}

func evaluateFieldEquals(p *parser.ArchiveWhen, v EntityView) bool {
	if len(p.FieldEquals) == 0 {
		return true
	}
	for field, want := range p.FieldEquals {
		got, ok := v.Data[field]
		if !ok {
			return false
		}
		if !fieldValueEqual(got, want) {
			return false
		}
	}
	return true
}

// fieldValueEqual reports whether an entity data value equals a
// configured field_equals value. Numeric values are compared by value:
// entity data round-trips through the store's JSON column and decodes
// numbers as float64, while the workflow config parses the same literal
// from YAML as int — so a plain reflect.DeepEqual(float64(5), int(5))
// would be false and field_equals would silently never match a numeric
// field. Non-numeric values (strings, bools, nested) fall back to
// reflect.DeepEqual.
func fieldValueEqual(got, want any) bool {
	if gf, ok := numericValue(got); ok {
		if wf, ok := numericValue(want); ok {
			return gf == wf
		}
		return false
	}
	return reflect.DeepEqual(got, want)
}

// numericValue widens the int / float kinds the YAML + JSON decoders
// produce to float64 for cross-type value comparison. Non-numeric
// values return ok=false.
func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	default:
		return 0, false
	}
}

func evaluateAnyOf(p *parser.ArchiveWhen, v EntityView) bool {
	if len(p.AnyOf) == 0 {
		return true
	}
	for i := range p.AnyOf {
		child := p.AnyOf[i]
		if EvaluateArchiveWhen(&child, v) {
			return true
		}
	}
	return false
}

func evaluateAllOf(p *parser.ArchiveWhen, v EntityView) bool {
	if len(p.AllOf) == 0 {
		return true
	}
	for i := range p.AllOf {
		child := p.AllOf[i]
		if !EvaluateArchiveWhen(&child, v) {
			return false
		}
	}
	return true
}

// archiveWhenHasAnyPrimitive mirrors the parser's same-named guard.
// Duplicated rather than re-exported to keep the evaluator free of
// any new parser-side exports beyond the ArchiveWhen type itself —
// the predicate's "what counts as populated" definition is small
// and stable enough that duplication is the cleaner trade-off than
// growing the parser's public surface.
func archiveWhenHasAnyPrimitive(p *parser.ArchiveWhen) bool {
	switch {
	case p.AllGapsResolved:
		return true
	case len(p.HasEdges) > 0:
		return true
	case len(p.FieldEquals) > 0:
		return true
	case len(p.AnyOf) > 0:
		return true
	case len(p.AllOf) > 0:
		return true
	}
	return false
}
