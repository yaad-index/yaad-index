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
	// → edge with the resolved id, options → defer via
	// ResolutionDeferred sentinel.
	//
	// Return-tuple semantics:
	//
	//   - (entityID, nil, nil) on single-match resolution.
	//   - ("", options, nil) on disambiguation; len(options)
	//     ≥ 2 (a single-match plugin doesn't return
	//     options — it returns the entity).
	//   - ("", nil, err) on transport / unresolvable failures
	//     (plugin returns failed state, no plugin matches
	//     the name, etc.).
	//
	// timeout cap is the same shape as IngestURL.
	IngestByName(ctx context.Context, pluginName, name string, timeout time.Duration) (entityID string, options map[string]plugins.DisambiguationOption, err error)
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
func (s *syncIngester) IngestByName(ctx context.Context, pluginName, name string, timeout time.Duration) (string, map[string]plugins.DisambiguationOption, error) {
	if s.registry == nil {
		return "", nil, fmt.Errorf("ingest_sync: registry not wired")
	}
	if pluginName == "" {
		return "", nil, fmt.Errorf("ingest_sync: pluginName is required")
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
		return snap.entityID, nil, nil
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

// trackerHandle returns the inner *ingestTracker. Defined in
// the api package so NewHandlerWithRegistry can extract the
// shared tracker from a WithSyncIngester-supplied SyncIngester
// without exporting the tracker type.
func (s *syncIngester) trackerHandle() *ingestTracker { return s.tracker }
