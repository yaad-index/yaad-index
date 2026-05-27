// Package edgewrite centralizes every edge-write entry point in
// the daemon through a single auto-resolver-aware service per
// #304 Cut C.
//
// In Cut C1 the service is a thin passthrough — every caller's
// `store.CreateEdge(ctx, edge)` becomes `service.CreateEdge(ctx,
// edge)` with no behavior change. The semantic differentiation
// (caller-mode reading, resolver-plugin invocation, structured-
// task creation on ambiguity) lands in Cut C2 + C3.
//
// Why centralize first: the seven pre-existing call sites
// (canonical/dayrefs, workflow add_canonical_edge,
// workflow file_task_writer, api/edges manual POST,
// api/canonical_edges fill, api/ingest_tracker, reindex
// re-derive) would otherwise each grow their own caller-mode
// branching in Cut C2. Threading the routing through one place
// keeps the new behavior isolated to a single source file and
// avoids future drift between paths.
//
// Cardinality enforcement (per Cut C1): the resolver-ownership
// map computed at config-load per #304 Cut A is rejected if
// any canonical kind has more than one plugin claiming
// resolver responsibility. Cut A allowed multi-resolver maps
// at config-load (WARN-only); Cut C1 upgrades that to ERROR
// because Cut C2's routing layer must pick a single plugin —
// ambiguous routing is fail-fast at startup, not at first
// edge-write.
package edgewrite

import (
	"context"
	"fmt"
	"sort"

	"github.com/yaad-index/yaad-index/internal/store"
)

// EdgeWriter is the narrow interface every edge-writing call site
// holds — just the single CreateEdge method on the centralized
// service. Decoupling lets callers depend on the operation
// without pulling in the full Service struct (and its resolver
// map). Production wires `*Service`; tests wire in-memory
// implementations against the same shape.
type EdgeWriter interface {
	CreateEdge(ctx context.Context, e *store.Edge) error
}

// Service is the centralized edge-write entry point. Construct
// via New; safe for concurrent use (each method delegates to the
// underlying store, which is itself concurrency-safe).
type Service struct {
	store store.Store

	// resolvers is the kind → plugin-name ownership map per
	// #304 Cut A. Validated at construction time (≤1 resolver
	// per kind); Cut C2 reads it to route auto-mode edges to
	// the resolver plugin. Cut C1 doesn't consume it for
	// routing, but holds it for symmetry + so future cuts
	// don't re-validate.
	resolvers map[string]string
}

// New constructs a Service backed by the given store + the
// canonical-kind resolver ownership map. Returns a config-load
// error when the map carries any kind with more than one plugin
// — Cut C2's routing layer assumes exactly one resolver per
// kind, and "let's pick the first one" is the kind of silent
// behavior that surfaces as a routing mystery later.
//
// resolvers shape:
//
//   - nil / empty → no plugin opted into resolution; every kind
//     falls through to legacy edge-write. Returned Service still
//     routes through Cut C1's passthrough (the structural cut
//     stays valid even when no resolvers are wired).
//   - map[kind][]plugin with at most one plugin per kind →
//     converted to the flat map[kind]plugin form held on the
//     Service.
//   - map[kind][]plugin with any kind carrying >1 plugin →
//     returns a wrapped error listing every conflict kind plus
//     the conflicting plugin names, deterministically sorted so
//     CI logs across runs are diff-able.
func New(st store.Store, resolvers map[string][]string) (*Service, error) {
	if st == nil {
		return nil, fmt.Errorf("edgewrite.New: store is required")
	}
	flat, err := flattenResolvers(resolvers)
	if err != nil {
		return nil, err
	}
	return &Service{store: st, resolvers: flat}, nil
}

// CreateEdge routes through the centralized service. In Cut C1
// this is a thin passthrough to store.CreateEdge — semantic
// routing lands in Cut C2 + C3. Callers MUST go through the
// service rather than calling store.CreateEdge directly so the
// future-cut routing surfaces uniformly across every edge-write
// entry point.
func (s *Service) CreateEdge(ctx context.Context, e *store.Edge) error {
	return s.store.CreateEdge(ctx, e)
}

// ResolverFor returns the plugin name that resolves the given
// canonical kind, or empty when no plugin opted in. Exposed for
// Cut C2's routing layer; Cut C1 leaves the method present but
// unused at call sites.
func (s *Service) ResolverFor(kind string) string {
	return s.resolvers[kind]
}

// flattenResolvers validates the kind → []plugin shape Cut A
// builds and returns the kind → plugin flat map Cut C requires.
// Multi-resolver kinds reject with a deterministic error.
func flattenResolvers(resolvers map[string][]string) (map[string]string, error) {
	if len(resolvers) == 0 {
		return nil, nil
	}
	type conflict struct {
		kind    string
		plugins []string
	}
	var conflicts []conflict
	flat := make(map[string]string, len(resolvers))
	for kind, plugins := range resolvers {
		nonEmpty := make([]string, 0, len(plugins))
		for _, p := range plugins {
			if p == "" {
				continue
			}
			nonEmpty = append(nonEmpty, p)
		}
		if len(nonEmpty) == 0 {
			continue
		}
		if len(nonEmpty) > 1 {
			sorted := append([]string{}, nonEmpty...)
			sort.Strings(sorted)
			conflicts = append(conflicts, conflict{kind: kind, plugins: sorted})
			continue
		}
		flat[kind] = nonEmpty[0]
	}
	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			return conflicts[i].kind < conflicts[j].kind
		})
		msgs := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			msgs = append(msgs, fmt.Sprintf("%s: [%s]", c.kind, joinStrings(c.plugins, ", ")))
		}
		return nil, fmt.Errorf("edgewrite.New: multi-resolver kinds rejected per #304 Cut C1 cardinality enforcement (≤1 resolver per kind) — %s", joinStrings(msgs, "; "))
	}
	return flat, nil
}

// joinStrings is strings.Join with a tiny shim that avoids the
// import for the only call site.
func joinStrings(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := len(sep) * (len(parts) - 1)
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	b = append(b, parts[0]...)
	for _, p := range parts[1:] {
		b = append(b, sep...)
		b = append(b, p...)
	}
	return string(b)
}
