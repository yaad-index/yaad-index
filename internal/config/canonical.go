package config

// CanonicalGuard is the runtime check the ingest + fill paths consult
// before persisting a canonical-shape entity or edge. Construct via
// NewCanonicalGuard from a loaded Config; reuse across requests
// (read-only, safe for concurrent use).
//
// Source-shape entities (kinds owned by a registered plugin via its
// --init capabilities) bypass the guard entirely — they're always
// allowed regardless of operator config. The guard only gates the
// canonical / cross-source layer (`person`, `city`, `country`, edge
// types like `is_about`, `same_as`, …) per ADR-0008.
type CanonicalGuard struct {
	kinds map[string]struct{}
	edges map[string]struct{}
}

// NewCanonicalGuard builds a guard from the operator's enabled sets.
// Nil and empty slices are observationally equivalent — both mean
// "no canonical layer enabled." Both produce a guard whose
// AllowKind / AllowEdgeType return false for every input.
func NewCanonicalGuard(kinds, edgeTypes []string) *CanonicalGuard {
	g := &CanonicalGuard{
		kinds: make(map[string]struct{}, len(kinds)),
		edges: make(map[string]struct{}, len(edgeTypes)),
	}
	for _, k := range kinds {
		if k == "" {
			continue
		}
		g.kinds[k] = struct{}{}
	}
	for _, t := range edgeTypes {
		if t == "" {
			continue
		}
		g.edges[t] = struct{}{}
	}
	return g
}

// AllowKind reports whether kind is in the operator's enabled
// canonical-kinds set. Used by the ingest + fill paths to gate
// canonical-shape entity persistence.
func (g *CanonicalGuard) AllowKind(kind string) bool {
	if g == nil {
		return false
	}
	_, ok := g.kinds[kind]
	return ok
}

// AllowEdgeType reports whether edgeType is in the operator's
// enabled canonical-edge-types set.
func (g *CanonicalGuard) AllowEdgeType(edgeType string) bool {
	if g == nil {
		return false
	}
	_, ok := g.edges[edgeType]
	return ok
}

// EnabledKinds returns a copy of the enabled canonical-kinds set as
// a slice (unsorted). Used by the startup discoverability warning
// for plugin-declared canonical emissions; tests can assert exact
// contents via ElementsMatch.
func (g *CanonicalGuard) EnabledKinds() []string {
	if g == nil || len(g.kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.kinds))
	for k := range g.kinds {
		out = append(out, k)
	}
	return out
}
