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
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/yaad-index/yaad-index/internal/slug"
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

// CanonicalEdgeWriter is the auto-resolver-aware extension
// EdgeWriter callers reach for when the target is a raw name
// (e.g. workflow `add_canonical_edge` per #304 Cut C2). Embeds
// EdgeWriter so callers that need both methods don't carry
// two fields; embedded so structural typing keeps tests
// flexible.
//
// The `created` return on CreateCanonicalEdgeByName preserves
// the pre-Cut-C2 entity.created event contract: true iff the
// slugify-fall-through path's thin canonical-label row was
// freshly materialized by this call. The auto-resolve branch
// always returns false — the resolver plugin's ingest
// tracker emits the entity.created event from its own path
// (so the workflow runner doesn't double-emit).
type CanonicalEdgeWriter interface {
	EdgeWriter
	CreateCanonicalEdgeByName(ctx context.Context, fromID, edgeType, targetKind, targetName string, edgeMetadata map[string]any) (targetID string, created bool, err error)
}

// Service is the centralized edge-write entry point. Construct
// via New; safe for concurrent use (each method delegates to the
// underlying store, which is itself concurrency-safe).
type Service struct {
	store store.Store

	// resolvers is the kind → plugin-name ownership map per
	// #304 Cut A. Validated at construction time (≤1 resolver
	// per kind); Cut C2 reads it to route auto-mode edges to
	// the resolver plugin.
	resolvers map[string]string

	// resolver is the plugin-name-resolution surface per
	// #304 Cut C2. nil disables the auto-mode resolution
	// branch (every edge falls through to legacy
	// CreateEdge); main.go wires a shim around
	// api.SyncIngester. Tests that don't exercise auto-mode
	// leave it unset.
	resolver NameResolver
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
// this was a thin passthrough; Cut C2 keeps it as a passthrough
// because the auto-mode resolution path uses the separate
// CreateCanonicalEdgeByName entry point (see Gap 1 in the
// design comment). Callers with already-resolved canonical
// IDs in their edge.To stay on this method; the workflow
// `add_canonical_edge` path that carries a raw name uses
// CreateCanonicalEdgeByName.
func (s *Service) CreateEdge(ctx context.Context, e *store.Edge) error {
	return s.store.CreateEdge(ctx, e)
}

// SetNameResolver wires the plugin-name-resolution surface per
// #304 Cut C2. main.go calls this once at startup, after the
// SyncIngester is constructed, to thread the shim. Subsequent
// CreateCanonicalEdgeByName calls in auto-mode route through
// it. Idempotent — setting nil is a no-op (the previous value
// stays), so accidental re-wiring during boot doesn't blank
// out the resolver mid-flight.
func (s *Service) SetNameResolver(r NameResolver) {
	if r != nil {
		s.resolver = r
	}
}

// CreateCanonicalEdgeByName is the auto-resolver-aware entry
// point per #304 Cut C2. Callers supply the raw target name +
// canonical kind; the Service decides whether to resolve via
// a plugin (auto mode + resolver-for-kind + non-id name) or
// fall through to legacy slugify-then-CreateEdge.
//
// Routing per Cut C2's framing:
//
//   - mode=Interactive OR no resolver-for-kind OR targetName
//     already a `<kind>:<slug>` shape → slugify locally +
//     CreateEdge with the slugged target id. Pre-#304
//     behavior preserved.
//   - mode=Auto + resolver-for-kind + non-id name → invoke
//     NameResolver.ResolveCanonicalEntity. On single-match
//     create the edge with the resolved id and return its
//     entity id to the caller. On ambiguous-options return
//     ResolutionDeferred (no edge created — Cut C3 picks up
//     the sentinel and creates a task).
//
// Returns:
//   - (targetID, created, nil) on success. `created` is true
//     iff the slugify-fall-through path freshly materialized
//     the thin canonical-label row (caller emits
//     entity.created for the new row); false on the
//     auto-resolve branch (the resolver plugin's ingest path
//     emits its own events).
//   - ("", false, *ResolutionDeferred) on deferred-resolution.
//   - ("", false, err) on transport / write failures.
//
// edgeMetadata may be nil. Used by the workflow path's
// AddCanonicalEdge metadata-passthrough; non-workflow callers
// can leave it nil.
func (s *Service) CreateCanonicalEdgeByName(ctx context.Context, fromID, edgeType, targetKind, targetName string, edgeMetadata map[string]any) (string, bool, error) {
	switch {
	case fromID == "":
		return "", false, fmt.Errorf("CreateCanonicalEdgeByName: fromID is required")
	case edgeType == "":
		return "", false, fmt.Errorf("CreateCanonicalEdgeByName: edgeType is required")
	case targetKind == "":
		return "", false, fmt.Errorf("CreateCanonicalEdgeByName: targetKind is required")
	case strings.TrimSpace(targetName) == "":
		return "", false, fmt.Errorf("CreateCanonicalEdgeByName: targetName is required")
	}

	mode := ModeFromContext(ctx)
	resolverPlugin := s.resolvers[targetKind]

	// Strip the canonical-kind prefix when the caller passed
	// the canonical-ID form (e.g. `boardgame:brass-birmingham`
	// rather than the raw name "Brass"). Mirrors the existing
	// workflow runner's prefix-strip pattern (ADR-0027 cut 1)
	// so slug.Slug doesn't mangle the colon into a hyphen.
	stripped := strings.TrimPrefix(targetName, targetKind+":")
	alreadyResolved := stripped != targetName

	if mode == Auto && resolverPlugin != "" && s.resolver != nil && !alreadyResolved {
		entityID, options, err := s.resolver.ResolveCanonicalEntity(ctx, resolverPlugin, targetName)
		if err != nil {
			return "", false, fmt.Errorf("resolve %s via plugin %s: %w", targetName, resolverPlugin, err)
		}
		if len(options) > 0 {
			return "", false, &ResolutionDeferred{
				From:           fromID,
				EdgeType:       edgeType,
				TargetKind:     targetKind,
				RawTarget:      targetName,
				ResolverPlugin: resolverPlugin,
				Options:        options,
			}
		}
		if entityID == "" {
			return "", false, fmt.Errorf("resolve %s via plugin %s: empty entity id without options", targetName, resolverPlugin)
		}
		if err := s.store.CreateEdge(ctx, &store.Edge{
			Type:     edgeType,
			From:     fromID,
			To:       entityID,
			Metadata: edgeMetadata,
		}); err != nil {
			return "", false, fmt.Errorf("create resolved edge %s -[%s]-> %s: %w", fromID, edgeType, entityID, err)
		}
		// Auto-resolve branch returns created=false — the
		// plugin's ingest tracker emits the entity.created
		// event on its own materialization path; the
		// workflow caller doesn't double-emit.
		return entityID, false, nil
	}

	// Legacy slugify-then-CreateEdge path — preserved for
	// every non-auto-mode call site + auto-mode kinds without
	// a registered resolver (operator's "no resolver = legacy
	// pass-through" clarification per the v1 cut framing).
	targetSlug := slug.Slug(stripped)
	if targetSlug == "" {
		return "", false, fmt.Errorf("slugify target name %q produced empty slug", targetName)
	}
	targetID := targetKind + ":" + targetSlug
	// Auto-materialize the thin canonical-label row so the
	// CreateEdge FK holds. Mirrors canonical.EnsureLabelRow's
	// existing shape — the duplication is intentional to
	// keep edgewrite from importing canonical (cycle via the
	// dayrefs DayRefEdgeWriter dedup landed earlier).
	created, err := s.ensureTargetRow(ctx, targetKind, targetID)
	if err != nil {
		return "", false, err
	}
	if err := s.store.CreateEdge(ctx, &store.Edge{
		Type:     edgeType,
		From:     fromID,
		To:       targetID,
		Metadata: edgeMetadata,
	}); err != nil {
		return "", false, fmt.Errorf("create edge %s -[%s]-> %s: %w", fromID, edgeType, targetID, err)
	}
	return targetID, created, nil
}

// ensureTargetRow probes the store for an entity at targetID
// and creates a thin row (Kind only, no Data) when absent —
// the same shape canonical.EnsureLabelRow produces, duplicated
// here to keep edgewrite from importing canonical. Cycle
// landed via the dayrefs DayRefEdgeWriter dedup. Returns
// `created` true iff a row was freshly inserted by this call;
// false on pre-existing-row reuse. Caller propagates the bit
// so workflow-spawned edge writes can emit entity.created on
// thin-row materialization.
func (s *Service) ensureTargetRow(ctx context.Context, kind, targetID string) (bool, error) {
	if _, err := s.store.GetEntity(ctx, targetID); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("probe target row %q: %w", targetID, err)
	}
	if err := s.store.UpsertEntity(ctx, &store.Entity{ID: targetID, Kind: kind}); err != nil {
		return false, fmt.Errorf("upsert thin target row %q: %w", targetID, err)
	}
	return true, nil
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
