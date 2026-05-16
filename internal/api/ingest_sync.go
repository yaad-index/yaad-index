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
	registry *plugins.Registry,
	vaultWriter *vault.Writer,
	vaultReader *vault.Reader,
	canonicalGuard *config.CanonicalGuard,
	cacheTTLSeconds int,
	dispatcher *attachments.Dispatcher,
	writeLocks *writelocks.Manager,
	bus eventbus.Bus,
) SyncIngester {
	return &syncIngester{
		logger:   logger,
		registry: registry,
		tracker:  newIngestTracker(logger, st, vaultWriter, vaultReader, canonicalGuard, cacheTTLSeconds, dispatcher, writeLocks, bus),
	}
}

type syncIngester struct {
	logger   *slog.Logger
	registry *plugins.Registry
	tracker  *ingestTracker
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
	att := ingestAttemptForPlugin(plugin, url)
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

// trackerHandle returns the inner *ingestTracker. Defined in
// the api package so NewHandlerWithRegistry can extract the
// shared tracker from a WithSyncIngester-supplied SyncIngester
// without exporting the tracker type.
func (s *syncIngester) trackerHandle() *ingestTracker { return s.tracker }
