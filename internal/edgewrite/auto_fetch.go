// Auto-fetch on canonical-edge writes per #325. Extends the
// operator-config `resolver_plugin` field from its prior
// fill-gate-only behavior (HTTP 422 on unresolved fills, per
// ADR-0008 §"Per-kind resolver_plugin:") to ALSO trigger plugin
// ingest when an edge lands on a canonical that has no plugin
// source connection yet. Single operator-config field activates
// both behaviors.
//
// Trigger (per the #325 issue body):
//
//  1. Edge is created targeting canonical X (any edge type, any
//     caller — fill, operator-fill, workflow add_canonical_edge
//     legacy path, ingest tracker, reindex).
//  2. canonical_kinds[X.kind].resolver_plugin is set in operator
//     config (already plumbed via the resolvers ownership map on
//     Service construction).
//  3. X has no incoming edge from a plugin-source-shape entity
//     yet. Source-shape = entity whose kind is NOT in the
//     operator's canonical_kinds set; plugins emit source rows
//     with non-canonical kinds (e.g. `bgg`, `gmail`, `yaad-bgg`).
//
// On trigger: invoke the resolver plugin's name-ingest path with
// X's slug as the name string (per the issue: "the plugin owns
// slug → upstream-id resolution"). Outcomes:
//
//   - single-match canonical (entityID returned) → plugin's ingest
//     wrote the source-shape row + canonical edge. Done. The
//     canonical's data lands via the existing edge-target
//     enrichment path (dataview append + provenance).
//   - disambiguation (options map non-empty) → spawn a resolution-
//     task via the existing #304 surface, idempotency-keyed on
//     the canonical id.
//   - error / timeout from the resolver shim → spawn an err-task
//     so the operator sees the failure. Canonical stays inert
//     until manual resolution.
//
// Recursion break: when the resolver plugin's ingest creates its
// own `source-shape -> canonical` edge (e.g. `bgg:13 ->
// boardgame:brass`), that edge write routes back through CreateEdge
// and would re-trigger auto-fetch on `boardgame:brass`. Suppression:
// any edge whose FROM kind is NOT in the canonical-kinds set (i.e.,
// source-shape) skips the auto-fetch hook entirely.

package edgewrite

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// autoFetchErrTaskWorkflow names the workflow string the
// FileErrTaskWriter uses to compose the err-task file path for
// #325 dispatch failures. Fixed value so all auto-fetch
// failures land under one directory (one append-only file per
// resolver kind would also be defensible, but a single shared
// file keeps the operator's recovery surface compact).
const autoFetchErrTaskWorkflow = "resolver-auto-fetch"

// MaybeDispatchResolverAutoFetch is the shared resolution-
// attempt path per #325 — the single source of truth for
// "canonical X is not yet resolved → invoke its resolver
// plugin." Called from two surfaces:
//
//   - Service.CreateEdge / CreateCanonicalEdgeByName legacy
//     slugify path: AFTER the edge lands. Best-effort with
//     no callback to the caller; the edge already exists.
//   - api.checkCanonicalTypeResolverPlugins (fill / operator-
//     fill pre-flight gate): BEFORE the gate decides. The
//     gate runs this, then re-probes GetEntity. When the
//     plugin completed sync (single-match), the re-probe
//     succeeds and the fill proceeds. When the plugin
//     deferred (disambiguation) or errored, the corresponding
//     task is spawned and the re-probe still fails — the
//     gate returns 422 so the agent follows the task
//     surface.
//
// fromID + edgeType supply ResolutionDeferred context if the
// plugin returns disambiguation options. For the fill-gate
// case the source entity id + the canonical-edge field name
// fit naturally; the edge-write case passes the just-created
// edge's tuple.
//
// Best-effort posture: any failure logs a WARN; observable
// outcome lives in the store (re-check via GetEntity) and the
// vault tasks/ directory (resolution-task or err-task).
//
// The four short-circuits (no resolver, source-shape from-kind,
// not a canonical-id-shape target, already source-connected)
// keep the common case (no resolver configured for this kind)
// at ~one map lookup of overhead per call.
func (s *Service) MaybeDispatchResolverAutoFetch(ctx context.Context, fromID, edgeType, toID string) {
	if s.resolver == nil {
		return
	}
	if len(s.resolvers) == 0 {
		return
	}
	targetKind, targetSlug, ok := splitCanonicalID(toID)
	if !ok {
		return
	}
	resolverPlugin := s.resolvers[targetKind]
	if resolverPlugin == "" {
		return
	}
	// Recursion break: when the resolver plugin's ingest itself
	// creates a `<plugin-source>:<id> -> <canonical>:<slug>` edge
	// via CreateEdge, the from-kind is source-shape (not in the
	// canonical-kinds set). Skip the auto-fetch hook so the
	// plugin's own edge writes don't re-trigger the plugin.
	if !s.fromKindIsCanonical(ctx, fromID) {
		return
	}
	// Source-connection check: if X has any incoming edge whose
	// source entity is plugin-source-shape, a plugin has already
	// connected to this canonical. Skip.
	if connected, err := s.hasPluginSourceIncomingEdge(ctx, toID); err != nil {
		s.warnLog(ctx, "edgewrite auto-fetch: source-connection check failed (skipping dispatch)",
			"target_id", toID, "err", err)
		return
	} else if connected {
		return
	}

	// Dispatch the resolver plugin's name-ingest path. The
	// canonical's slug doubles as the "name" arg per the #325
	// issue body — the plugin's existing url_patterns matcher
	// recognizes `<plugin>: <slug>` shorthand and resolves
	// against upstream.
	resolved, options, err := s.resolver.ResolveCanonicalEntity(ctx, resolverPlugin, targetKind, targetSlug)
	switch {
	case err != nil:
		s.spawnAutoFetchErrTask(ctx, resolverPlugin, toID, err)
		return
	case len(options) > 0:
		s.spawnAutoFetchResolutionTask(ctx, &ResolutionDeferred{
			From:           fromID,
			EdgeType:       edgeType,
			TargetKind:     targetKind,
			RawTarget:      targetSlug,
			ResolverPlugin: resolverPlugin,
			Options:        options,
		})
		return
	case resolved == "":
		s.spawnAutoFetchErrTask(ctx, resolverPlugin, toID,
			fmt.Errorf("resolver returned empty entity id without disambiguation options"))
		return
	}
	// Single-match success. The plugin's ingest already
	// materialized the source-shape entity + canonical edge.
	// Log when the resolved canonical id differs from the
	// originally-edged target — that's a plugin-side
	// correction (e.g., we held `boardgame:brass`, plugin
	// resolved to `boardgame:brass-birmingham`). The original
	// edge stays as written; updating it is out of scope for
	// #325 (would require a separate edge-rewire hook).
	if resolved != toID {
		s.warnLog(ctx, "edgewrite auto-fetch: resolver returned a canonical id distinct from the edge target",
			"edge_target", toID, "resolved", resolved, "resolver_plugin", resolverPlugin)
	}
}

// splitCanonicalID parses a `<kind>:<slug>` shaped id into its
// component parts. Returns (kind, slug, true) on success;
// (..., false) for ids that don't match the canonical shape
// (no colon, empty kind, empty slug). The shape check is the
// minimum needed for the auto-fetch hook's gate to be
// well-defined; the existing CreateCanonicalEdgeByName path
// already enforces the shape during slugify.
func splitCanonicalID(id string) (kind, slug string, ok bool) {
	colon := strings.IndexByte(id, ':')
	if colon <= 0 || colon == len(id)-1 {
		return "", "", false
	}
	return id[:colon], id[colon+1:], true
}

// fromKindIsCanonical reports whether fromID belongs to a kind
// the operator declared as canonical. When the kind is NOT in
// the canonical set (i.e., it's a source-shape kind emitted by
// a plugin), the auto-fetch hook suppresses dispatch — the
// recursion-break described in the package docstring.
//
// nil canonicalKinds means the Service hasn't been told the
// registry (e.g., test fixtures that don't exercise auto-fetch).
// In that case we fall through to "treat as canonical" so the
// hook still fires; tests that DO exercise auto-fetch wire the
// registry explicitly.
func (s *Service) fromKindIsCanonical(ctx context.Context, fromID string) bool {
	if s.canonicalKinds == nil {
		return true
	}
	kind, _, ok := splitCanonicalID(fromID)
	if !ok {
		return false
	}
	_, isCanonical := s.canonicalKinds[kind]
	return isCanonical
}

// hasPluginSourceIncomingEdge returns true when toID has at least
// one incoming edge whose source entity is plugin-source-shape
// (kind not in the canonical-kinds set). On error reading the
// edges, returns (false, err) — caller logs + skips dispatch
// (best-effort posture: a broken read shouldn't trigger spurious
// auto-fetches).
//
// When the canonical-kinds registry hasn't been wired, the
// check degrades to "any incoming edge counts as source-
// connected" — conservative behavior that won't spam plugin
// ingests in tests that don't exercise the suppression path.
func (s *Service) hasPluginSourceIncomingEdge(ctx context.Context, toID string) (bool, error) {
	edges, err := s.store.GetEdgesTo(ctx, toID, nil)
	if err != nil {
		return false, err
	}
	if len(edges) == 0 {
		return false, nil
	}
	if s.canonicalKinds == nil {
		return true, nil
	}
	for _, e := range edges {
		kind, _, ok := splitCanonicalID(e.From)
		if !ok {
			continue
		}
		if _, isCanonical := s.canonicalKinds[kind]; !isCanonical {
			return true, nil
		}
	}
	return false, nil
}

// spawnAutoFetchResolutionTask routes a #325 disambiguation
// outcome through the centralized resolution-task surface
// (FileTaskWriter.WriteResolutionTask via the
// ResolutionTaskWriter interface). Idempotency-keyed on the
// canonical id via ResolutionTaskKey in the actions package —
// repeated edge writes against the same canonical reuse the
// same task file.
//
// Best-effort: a write failure logs but doesn't propagate. The
// original edge already landed; the operator can re-trigger
// the source workflow if the task didn't materialize.
func (s *Service) spawnAutoFetchResolutionTask(ctx context.Context, d *ResolutionDeferred) {
	if s.resolutionTaskWriter == nil {
		s.warnLog(ctx, "edgewrite auto-fetch: disambiguation but no ResolutionTaskWriter wired",
			"target_kind", d.TargetKind, "raw_target", d.RawTarget,
			"resolver_plugin", d.ResolverPlugin, "options", len(d.Options))
		return
	}
	if _, _, err := s.resolutionTaskWriter.WriteResolutionTask(ctx, d); err != nil {
		s.warnLog(ctx, "edgewrite auto-fetch: WriteResolutionTask failed",
			"target_kind", d.TargetKind, "raw_target", d.RawTarget,
			"resolver_plugin", d.ResolverPlugin, "err", err)
	}
}

// spawnAutoFetchErrTask routes a #325 dispatch failure through
// the centralized err-task surface. Workflow string is a fixed
// `resolver-auto-fetch` value so all auto-fetch failures land
// in one directory regardless of which kind / plugin
// originated; the err-task body carries the resolver plugin
// name + canonical id for triage.
//
// Same best-effort posture as spawnAutoFetchResolutionTask.
// Falls back to a WARN log when the err-task writer isn't
// wired (e.g., tests that don't exercise the err path).
func (s *Service) spawnAutoFetchErrTask(ctx context.Context, resolverPlugin, entityID string, dispatchErr error) {
	if s.errTaskWriter == nil {
		s.warnLog(ctx, "edgewrite auto-fetch: dispatch failed but no ErrTaskWriter wired",
			"entity_id", entityID, "resolver_plugin", resolverPlugin, "err", dispatchErr)
		return
	}
	now := s.now()
	msg := fmt.Sprintf("resolver-plugin auto-fetch dispatch failed for %s via %s: %v",
		entityID, resolverPlugin, dispatchErr)
	if err := s.errTaskWriter.AppendErrTask(ctx, autoFetchErrTaskWorkflow, now, entityID, msg); err != nil {
		s.warnLog(ctx, "edgewrite auto-fetch: AppendErrTask failed",
			"entity_id", entityID, "resolver_plugin", resolverPlugin,
			"dispatch_err", dispatchErr, "err_task_err", err)
	}
}

// warnLog is the Service's logging shim. Tests that don't
// inject a logger get slog.DiscardHandler so the test output
// stays quiet. Production wires the daemon's shared logger via
// SetLogger.
func (s *Service) warnLog(ctx context.Context, msg string, args ...any) {
	logger := s.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger.WarnContext(ctx, msg, args...)
}

// now reads the configured clock or falls back to plain time.
// Now. Lets tests fix the err-task timestamp deterministically
// without touching production clock state.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now().UTC()
}

