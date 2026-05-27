// SyncIngester is the public synchronous IngestURL surface
// for callers that need entity-id-from-URL resolution without
// going through the HTTP /v1/ingest envelope. The workflow
// engine's manual-trigger URL-shape input path
// (ADR-0024 §"workflow.trigger(input) input semantics") wires
// this so the daemon's existing tracker handles
// ingest-or-lookup uniformly across the HTTP + workflow
// surfaces — same job-map dedup, same cache-TTL gate, same
// persistence flow.
//
// Construction (NewSyncIngester) is exposed so main.go can
// build one tracker + share it between the api handler and
// the workflow engine's IngestRouter adapter. The
// WithSyncIngester HandlerOption tells NewHandlerWithRegistry
// to reuse this tracker for /v1/ingest instead of building a
// fresh one — without that wiring, /v1/ingest and workflow URL
// triggers would coordinate on disjoint job maps + cache state.

package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/attachments"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// SyncIngester resolves a URL to a canonical entity ID via
// the daemon's plugin pipeline, returning synchronously after
// the ingest tracker transitions to a terminal state. Used by
// the workflow engine's URL-shape workflow.trigger() input
// path.
type SyncIngester interface {
	// IngestURL routes url through the existing
	// plugin-dispatch logic (URL-shape regex match OR
	// command-shape `<plugin>: !<command>` per ADR-0022),
	// runs the plugin via the shared ingest tracker, and
	// returns the resolved entity id once the attempt
	// reaches a terminal state. Returns an error when no
	// plugin handles the URL, when the URL is ambiguous
	// (disambiguation required), or when ingest fails
	// outright (plugin error, parse failure, etc.).
	//
	// timeout caps the wait on the tracker; an attempt
	// still in progress when timeout fires returns
	// context.DeadlineExceeded. The tracker continues the
	// simulation in the background — a subsequent
	// IngestURL with the same plugin+url will subscribe to
	// the in-flight record per the tracker's invocation-key
	// dedup.
	IngestURL(ctx context.Context, url string, timeout time.Duration) (entityID string, err error)

	// IngestByName invokes the named resolver plugin's
	// `<plugin>: <name>` shorthand path per #304 Cut C2.
	// Mirror of IngestURL with one critical wire-shape
	// difference: on disambiguation, the plugin's options
	// map bubbles back instead of collapsing into an error
	// string. The centralized edgewrite.Service consumes
	// this on the auto-mode resolution branch — single match
	// → edge with the resolved canonical-kind id, options →
	// defer via ResolutionDeferred sentinel.
	//
	// targetKind is the canonical kind the workflow declared
	// for the edge target. After a successful ingest the
	// returned entityID is guaranteed to be of that kind:
	//
	//   - If the plugin's `result.Entity.ID` already has the
	//     `<targetKind>:` prefix, it's returned directly
	//     (identity-resolver case).
	//   - Otherwise the source row is treated as a hop and
	//     the implementation walks the persisted outgoing
	//     canonical edges, returning the first edge `to`
	//     of `<targetKind>:` shape (source-shape resolver
	//     case — yaad-bgg, yaad-wikipedia, etc.).
	//   - If neither path yields a `<targetKind>:` id the
	//     call errors out — the workflow's declared edge
	//     target shape is a hard contract.
	//
	// Return-tuple semantics:
	//
	//   - (entityID, nil, nil) on single-match resolution;
	//     entityID has `<targetKind>:` prefix.
	//   - ("", options, nil) on disambiguation; len(options)
	//     ≥ 2 (a single-match plugin doesn't return
	//     options — it returns the entity).
	//   - ("", nil, err) on transport / unresolvable failures
	//     (plugin returns failed state, no plugin matches
	//     the name, plugin ingest succeeded but produced no
	//     canonical `<targetKind>` target, etc.).
	//
	// timeout cap is the same shape as IngestURL.
	IngestByName(ctx context.Context, pluginName, targetKind, name string, timeout time.Duration) (entityID string, options map[string]plugins.DisambiguationOption, err error)
}

// NewSyncIngester constructs a tracker-backed SyncIngester
// sharing the daemon's store / vault / bus / writelocks /
// registry / dispatcher state. Returns a concrete value the
// caller can both invoke directly AND pass into
// WithSyncIngester on api.NewHandlerWithRegistry so the
// /v1/ingest HTTP handler shares the same tracker.
func NewSyncIngester(
	logger *slog.Logger,
	st store.Store,
	edgeWriter edgewrite.EdgeWriter,
	registry *plugins.Registry,
	vaultWriter *vault.Writer,
	vaultReader *vault.Reader,
	canonicalGuard *config.CanonicalGuard,
	cacheTTLSeconds int,
	dispatcher *attachments.Dispatcher,
	writeLocks *writelocks.Manager,
	bus eventbus.Bus,
	pluginInstances map[string][]string,
	pluginInstanceConfigs map[string][]config.InstanceEntry,
	canonicalEdgeTypes []string,
	canonicalKinds []string,
) SyncIngester {
	return &syncIngester{
		logger:                logger,
		registry:              registry,
		tracker:               newIngestTracker(logger, st, edgeWriter, vaultWriter, vaultReader, canonicalGuard, cacheTTLSeconds, dispatcher, writeLocks, bus, pluginInstances, canonicalEdgeTypes, canonicalKinds),
		pluginInstanceConfigs: pluginInstanceConfigs,
	}
}

type syncIngester struct {
	logger   *slog.Logger
	registry *plugins.Registry
	tracker  *ingestTracker
	// pluginInstanceConfigs carries the per-plugin instance
	// configs so the workflow-trigger URL-shape ingest path can
	// run the same ADR-0028 §3 pickInstance routing as the
	// /v1/ingest HTTP handler. Without this, multi-instance
	// plugins ingested via workflow trigger would silently fall
	// through to resolveInstanceName's first-instance default —
	// the §3 fail-fast contract is designed to prevent exactly
	// that silent mis-attribution path.
	pluginInstanceConfigs map[string][]config.InstanceEntry
}

// IngestURL implements SyncIngester. Mirrors the dispatch
// branching in handleIngest (ParseInvocation → URL-regex
// match OR by-name lookup) so the workflow URL path takes the
// same routing decisions the HTTP path takes.
func (s *syncIngester) IngestURL(ctx context.Context, url string, timeout time.Duration) (string, error) {
	if s.registry == nil {
		return "", fmt.Errorf("ingest_sync: registry not wired")
	}
	inv := plugins.ParseInvocation(url)
	var plugin plugins.Plugin
	if inv.Shape == plugins.InvocationCommand {
		// Command-shape: by-name lookup. Routing-time
		// command validation (the plugin must advertise the
		// command) lives in the daemon's routing layer for
		// /v1/ingest; for workflow URL triggers we surface
		// "no plugin handles URL" if the plugin's
		// command list rejects.
		if p, ok := s.registry.LookupByName(inv.Plugin); ok {
			plugin = p
		}
	} else if p, matched := s.registry.Lookup(url); matched {
		plugin = p
	}
	if plugin == nil {
		return "", fmt.Errorf("no plugin handles URL %s", url)
	}
	// ADR-0028 §3 Cut 3: workflow-trigger URL ingest must run
	// the same instance routing + fail-fast as /v1/ingest. The
	// §3 fail-fast contract is designed to prevent silent
	// mis-attribution to a first-declared fallback; bypassing
	// pickInstance here would re-open that gap on the workflow
	// surface. Command-shape invocations stay on the fallback
	// (per ADR-0028 §4 — command instance dispatch is Cut 4's
	// domain).
	var instanceName string
	if inv.Shape != plugins.InvocationCommand {
		picked, perr := pickInstance(plugin, s.pluginInstanceConfigs[plugin.Name()], url)
		if perr != nil {
			return "", fmt.Errorf("instance routing for %s: %w", url, perr)
		}
		instanceName = picked
	}
	att := ingestAttemptForPlugin(plugin, url, instanceName)
	rec := s.tracker.beginAttempt(att)
	snap, err := s.tracker.wait(ctx, rec, timeout)
	if err != nil {
		return "", fmt.Errorf("ingest wait for %s: %w", url, err)
	}
	switch snap.state {
	case ingestStateComplete, ingestStateNeedsFill:
		// Both terminal-with-entity states return the
		// resolved id. NeedsFill is fine for the workflow
		// path — the entity is in the store with gaps; the
		// workflow's CEL still evaluates against the
		// available data, and the gaps surface as
		// missing-refs on the resulting task.
		return snap.entityID, nil
	case ingestStateDisambiguation:
		return "", fmt.Errorf("ingest of %s requires disambiguation (multiple candidates)", url)
	case ingestStateFailed:
		return "", fmt.Errorf("ingest of %s failed: %s (%s)", url, snap.failureMessage, snap.failureCode)
	default:
		return "", fmt.Errorf("ingest of %s returned unexpected state %d", url, snap.state)
	}
}

// IngestByName implements SyncIngester per #304 Cut C2. Wraps
// the plugin's `<plugin>: <name>` shorthand input through the
// shared ingest tracker, bubbling Options back on
// disambiguation rather than collapsing into an error string
// (the Cut C2 wire-shape contract).
//
// On successful ingest, snap.entityID is the plugin's
// `result.Entity.ID` — which is the source-row id for
// universal-source plugins (yaad-bgg, yaad-wikipedia, ...)
// rather than the canonical-kind target. resolveCanonicalTarget
// reconciles the source-row id with the requested targetKind by
// walking the persisted outgoing edges.
func (s *syncIngester) IngestByName(ctx context.Context, pluginName, targetKind, name string, timeout time.Duration) (string, map[string]plugins.DisambiguationOption, error) {
	if s.registry == nil {
		return "", nil, fmt.Errorf("ingest_sync: registry not wired")
	}
	if pluginName == "" {
		return "", nil, fmt.Errorf("ingest_sync: pluginName is required")
	}
	if targetKind == "" {
		return "", nil, fmt.Errorf("ingest_sync: targetKind is required")
	}
	if name == "" {
		return "", nil, fmt.Errorf("ingest_sync: name is required")
	}
	plugin, ok := s.registry.LookupByName(pluginName)
	if !ok {
		return "", nil, fmt.Errorf("no plugin named %q in registry", pluginName)
	}
	// Synthesize the URL-shape shorthand input the existing
	// dispatch path consumes — `<plugin>: <name>`. The tracker
	// + plugin take it from there; `ParseInvocation` recognizes
	// the shape and routes to this plugin's pattern matcher
	// (which the plugin's `url_patterns` is expected to accept
	// for the shorthand prefix when it advertises resolver
	// support).
	shorthand := pluginName + ": " + name
	att := ingestAttemptForPlugin(plugin, shorthand, "")
	rec := s.tracker.beginAttempt(att)
	snap, err := s.tracker.wait(ctx, rec, timeout)
	if err != nil {
		return "", nil, fmt.Errorf("ingest wait for %s: %w", shorthand, err)
	}
	switch snap.state {
	case ingestStateComplete, ingestStateNeedsFill:
		canonicalID, err := s.resolveCanonicalTarget(ctx, snap.entityID, targetKind)
		if err != nil {
			return "", nil, fmt.Errorf("ingest of %s: %w", shorthand, err)
		}
		return canonicalID, nil, nil
	case ingestStateDisambiguation:
		// Copy the map so callers can't accidentally mutate
		// the tracker's stored slice. Cheap — disambiguation
		// option counts are small (single-digit on every
		// plugin observed in production).
		out := make(map[string]plugins.DisambiguationOption, len(snap.options))
		for k, v := range snap.options {
			out[k] = v
		}
		return "", out, nil
	case ingestStateFailed:
		return "", nil, fmt.Errorf("ingest of %s failed: %s (%s)", shorthand, snap.failureMessage, snap.failureCode)
	default:
		return "", nil, fmt.Errorf("ingest of %s returned unexpected state %d", shorthand, snap.state)
	}
}

// resolveCanonicalTarget reconciles a tracker-emitted entity id
// with the workflow's declared canonical kind per #304 Cut C2.
// Universal-source plugins materialize a source row as
// `result.Entity.ID` (e.g. `yaad-bgg:thing-12345`) and emit
// canonical-kind edges from that row to the actual target
// (`boardgame:brass-birmingham`); identity-resolver plugins
// emit a canonical-shape row directly. Both shapes funnel
// through this helper so callers never see a source-row id
// leaking into a workflow edge target.
//
// Strategy:
//
//  1. If ingestedID already has the `<targetKind>:` prefix, it
//     IS the canonical target — return it directly. Covers the
//     identity-resolver shape + the future case where a
//     universal-source plugin happens to mint canonical-shape
//     ids.
//  2. Otherwise walk the outgoing edges from ingestedID (no
//     type filter — the canonical-edge type is plugin-defined
//     and ADR-0016 doesn't pin it) and return the first edge
//     `to` whose prefix matches the requested kind. Order is
//     store-defined; the workflow's contract is "one canonical
//     edge per (source, kind)" so multi-match would indicate a
//     plugin-side bug, not a routing-policy choice.
//  3. If neither path yields a `<targetKind>:` id, error out —
//     a workflow that asked for `boardgame:` cannot be
//     fulfilled by a plugin that produced no boardgame target.
func (s *syncIngester) resolveCanonicalTarget(ctx context.Context, ingestedID, targetKind string) (string, error) {
	prefix := targetKind + ":"
	if strings.HasPrefix(ingestedID, prefix) {
		return ingestedID, nil
	}
	edges, err := s.tracker.store.GetEdgesFor(ctx, ingestedID, nil)
	if err != nil {
		return "", fmt.Errorf("lookup canonical edges from %s: %w", ingestedID, err)
	}
	for _, e := range edges {
		if strings.HasPrefix(e.To, prefix) {
			return e.To, nil
		}
	}
	return "", fmt.Errorf("plugin ingest of %s produced no canonical %s target", ingestedID, targetKind)
}

// trackerHandle returns the inner *ingestTracker. Defined in
// the api package so NewHandlerWithRegistry can extract the
// shared tracker from a WithSyncIngester-supplied SyncIngester
// without exporting the tracker type.
func (s *syncIngester) trackerHandle() *ingestTracker { return s.tracker }
