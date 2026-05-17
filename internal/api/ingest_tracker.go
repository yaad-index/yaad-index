package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/attachments"
	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// ingestState names the lifecycle phases of a /v1/ingest attempt.
// pending → terminal (complete | needs_fill | failed). The tracker
// emits exactly one transition per attempt by closing
// ingestRecord.transition.
type ingestState int

const (
	ingestStatePending ingestState = iota
	ingestStateComplete
	ingestStateNeedsFill
	ingestStateDisambiguation
	ingestStateNotFound
	ingestStateFailed
)

// ingestRecord is the in-flight bookkeeping for a single /v1/ingest
// attempt. Long-pollers wait on transition; once it closes, the
// terminal-state fields are stable for read.
//
// Field discipline: only the simulator goroutine writes to the state /
// gaps / clean_content / fill-token / failure fields, and only inside
// the tracker's mutex; the goroutine closes transition exactly once,
// after the writes. Readers acquire the mutex to take a snapshot.
//
// Per yaad-index (the operator's "extend tracker" decision over a
// separate jobs package): records may be SHARED across concurrent
// /v1/ingest calls when their invocationKey collides. The first
// arrival creates the record + spawns the simulator goroutine
// (the runner). Later arrivals receive the SAME record pointer
// (subscribers); they wait on the runner's transition channel and
// read the runner's terminal-state fields when it closes. This
// preserves disambiguation / failure fidelity for free — every
// subscriber sees the exact options / failureMessage the runner
// surfaced, not a re-derivation through the store.
type ingestRecord struct {
	state ingestState
	entityID string
	transition chan struct{}

	// invocationKey is the dedup key Per the prior design, populated for plugin-
	// path attempts only. Shape: `<plugin>:<rawURL>` (URL-shape).
	// When non-empty, the tracker's byInvocationKey index points at
	// this record while it's in-flight; clearing on terminal-state
	// set lets a subsequent ingest with the same key spawn fresh.
	// Fixture-path attempts leave this empty — fixtures dispatch
	// per-call by design.
	invocationKey string

	// Populated on ingestStateNeedsFill. gaps is a {field → description}
	// map per ADR-0002 universal-state amendment — keys are the data
	// field names the agent must fill, values are short descriptions
	// the AI uses to produce the right value. Per ADR-0008 / a prior PR
	// the entity ID is the durable callback handle for fill — there
	// is no separate fill_token field on this record.
	cleanContent string
	cleanContentTruncated bool
	gaps map[string]string

	// Populated on ingestStateDisambiguation. The plugin returned
	// multiple candidate entities, keyed by the plugin's canonical id;
	// no entity is persisted (the caller re-invokes ingest via the
	// plugin's shorthand input shape — `<plugin>: <id>` — to fetch
	// the chosen candidate).
	options map[string]plugins.DisambiguationOption

	// Populated on ingestStateFailed.
	failureCode string
	failureMessage string
}

// ingestSimulation drives a per-attempt state transition. Two flavours:
//
// - Fixture path: delay > 0, transitionTo is the terminal state, and
// entity / provenance fields are persisted on a successful
// transition. Used today only for the URL-fixture sentinels
// (brass-birmingham / queued-test / needs-fill-test) that drive
// long-poll tests without a network.
//
// - Plugin path: plugin != nil. The simulator calls plugin.Fetch
// with rawURL, then dispatches to complete or needs_fill based on
// the FetchResult.Gaps; on error transitions to failed.
//
// Exactly one path is set per ingestAttempt; the simulator selects
// via `plugin != nil`.
type ingestSimulation struct {
	delay time.Duration
	transitionTo ingestState

	// for ingestStateNeedsFill (fixture path): gaps map keys are the
	// data field names the agent must fill; values are short
	// descriptions per ADR-0002 universal-state amendment.
	cleanContent string
	cleanContentTruncated bool
	gaps map[string]string

	// for ingestStateFailed (fixture path):
	failureCode string
	failureMessage string

	// for the plugin path:
	plugin plugins.Plugin
	rawURL string
}

// ingestAttempt bundles everything beginAttempt needs.
//
// For fixture attempts, entity + provenance + simulation drive a
// declarative transition. For plugin attempts, simulation.plugin
// performs the work; entity / provenance fields are nil (the plugin
// returns the canonical values from Fetch).
//
// plannedEntityID is the id surfaced on the queued (timeout) response
// — for fixtures it's the known entity.ID; for plugins it's empty
// (the id is data-derived per ADR-0002 lines 138–145).
type ingestAttempt struct {
	plannedEntityID string
	entity *store.Entity
	provenance store.ProvenanceEntry
	simulation ingestSimulation

	// forceRefetch + ttlExpired flow into the auto-commit message
	// (per yaad-index the source issue) so a `re-ingest:` commit can carry
	// the cause: `[force_refetch=true]` or `[ttl_expired]`. Default
	// false on both → the plain `ingest:` / `re-ingest:` shape (a
	// re-ingest with no flagged cause is a normal cache-bypass refresh).
	forceRefetch bool
	ttlExpired bool
}

// ingestTracker is the in-memory state map for in-flight /v1/ingest
// attempts. Records are keyed by entity id; a new beginAttempt
// overwrites the existing record for that id (re-ingest). Long-pollers
// hold the record reference returned by their own beginAttempt call,
// so concurrent re-ingests don't cross-wire each other's wait calls.
//
// The tracker is safe for concurrent use. A single-conn store + a
// short-lived simulator goroutine per attempt keeps the contention
// surface small.
type ingestTracker struct {
	mu sync.Mutex
	records map[string]*ingestRecord
	// byInvocationKey indexes the in-flight runner record for each
	// plugin invocation key . The first ingest with a
	// given key inserts here + spawns the simulator goroutine; later
	// ingests with the same key receive the same record pointer
	// (subscriber path) and wait on its transition channel. Cleared
	// on terminal state so a subsequent ingest of the same URL can
	// spawn fresh (the runner's persisted state survives in vault +
	// DB; idempotency is about COLLAPSING CONCURRENT spawns, not
	// preventing post-completion re-fetches).
	byInvocationKey map[string]*ingestRecord
	store store.Store
	vaultWriter *vault.Writer
	vaultReader *vault.Reader
	guard *config.CanonicalGuard
	logger *slog.Logger
	// globalCacheTTLSeconds is the operator's
	// `cache_ttl_seconds` config in seconds (per yaad-index).
	// Used as the global-level input to resolveCacheTTL at ingest
	// time. Sentinel rules: 0 = no opinion, positive = N seconds,
	// negative = infinite. Lookup-side reads TTL from vault
	// frontmatter directly; the global config only participates at
	// ingest-resolution time.
	globalCacheTTLSeconds int
	// attachmentsDispatcher resolves plugin-emitted FetchResult.
	// Attachments to vault filenames per ADR-0014. nil = "no
	// dispatcher wired" — plugins that emit attachments see them
	// silently dropped (debug log). Tests typically pass nil; the
	// production main.go wires a Dispatcher rooted at the
	// operator-configured plugin_staging_dir.
	attachmentsDispatcher *attachments.Dispatcher
	// writeLocks is the per-artifact daemon write-lock manager (per
	// yaad-index #23). persistEnvelope acquires the entity-ID lock
	// before any vault.Writer call; conflict on acquire means a
	// cross-surface writer (operator-fill, archive, etc.) is in
	// flight against the same entity and the ingest fails with a
	// fetch_failed envelope. The dedup-by-invocationKey path above
	// already collapses concurrent ingests of the same URL; this
	// lock catches cross-surface conflicts the dedup can't see.
	writeLocks *writelocks.Manager
	// bus is the daemon-internal pub-sub substrate per ADR-0024
	// Phase 2. The tracker publishes entity.created on fresh-
	// ingest (gated on cache-hit detection — a pre-upsert
	// GetEntity probe distinguishes the new-entity path from the
	// re-fetch path per the ADR's "Cache-hit re-fetch semantics"
	// note) and entity.edge_added on each canonical-edge create
	// + thin-row materialization. nil bus skips all emissions —
	// tests + dev deployments without a bus stay unaffected.
	bus eventbus.Bus
}

// entityIsNew reports whether the given id has no row in the
// store yet, so the caller can decide whether to emit
// entity.created post-upsert. Per ADR-0024's cache-hit re-fetch
// semantics, entity.created fires only on the first-time-seen
// path — re-fetch of a known entity does NOT re-publish.
//
// Probe failures (anything other than ErrNotFound) return false:
// without proof of new-ness, the emit-as-create path is suppressed
// and the caller relies on the existing TopicEntityEdgeAdded
// emissions for change detection. The probe is best-effort.
func (t *ingestTracker) entityIsNew(ctx context.Context, id string) bool {
	_, err := t.store.GetEntity(ctx, id)
	return errors.Is(err, store.ErrNotFound)
}

// publishEntityCreated emits entity.created with SourceAgent
// (ingest path) when bus is configured. Centralizes the
// time-now + payload construction so the four call sites are
// one-liners.
func (t *ingestTracker) publishEntityCreated(ctx context.Context, id, kind string) {
	if t.bus == nil {
		return
	}
	t.bus.Publish(ctx, eventbus.EntityCreatedEvent{
		ID:        id,
		Kind:      kind,
		SourceTag: eventbus.SourceAgent,
		At:        time.Now().UTC(),
	})
}

// publishEntityEdgeAdded emits entity.edge_added with SourceAgent
// (ingest path) when bus is configured. Mirrors
// publishEntityCreated's shape for the per-edge call sites.
func (t *ingestTracker) publishEntityEdgeAdded(ctx context.Context, e *store.Edge) {
	if t.bus == nil || e == nil {
		return
	}
	t.bus.Publish(ctx, eventbus.EntityEdgeAddedEvent{
		FromID:    e.From,
		ToID:      e.To,
		EdgeType:  e.Type,
		SourceTag: eventbus.SourceAgent,
		At:        time.Now().UTC(),
	})
}

// newIngestTracker constructs the in-flight tracker. writer + reader
// are both either non-nil (vault wiring active per a prior PR) or both nil
// (DB-only fallback for tests + dev binaries without a configured
// vault). Mixing nil/non-nil is a programming error and panics — the
// read-merge-write contract requires both halves.
//
// guard is the canonical-kinds / canonical-edge-types validator (per
// ADR-0008 + a prior PR). A nil guard is allowed and treated as "no
// canonical layer enabled" — every canonical-shape entity / edge that
// a plugin emits gets dropped with a debug log. Production main.go
// constructs a guard from the keys of cfg.CanonicalKinds (the
// registry map per ADR-0013 §1) plus cfg.CanonicalEdgeTypes; tests
// typically pass nil unless they exercise the canonical path.
func newIngestTracker(logger *slog.Logger, st store.Store, writer *vault.Writer, reader *vault.Reader, guard *config.CanonicalGuard, globalCacheTTLSeconds int, dispatcher *attachments.Dispatcher, writeLocks *writelocks.Manager, bus eventbus.Bus) *ingestTracker {
	if (writer == nil) != (reader == nil) {
		panic("newIngestTracker: vault writer and reader must both be set or both be nil")
	}
	if writeLocks == nil {
		writeLocks = writelocks.New()
	}
	return &ingestTracker{
		records:               make(map[string]*ingestRecord),
		byInvocationKey:       make(map[string]*ingestRecord),
		globalCacheTTLSeconds: globalCacheTTLSeconds,
		store:                 st,
		vaultWriter:           writer,
		vaultReader:           reader,
		guard:                 guard,
		logger:                logger,
		attachmentsDispatcher: dispatcher,
		writeLocks:            writeLocks,
		bus:                   bus,
	}
}

// beginAttempt creates a fresh pending record and kicks off the
// simulator goroutine. Returns the record so the caller can wait on
// its transition. The simulator is responsible for persistence on
// successful transitions — observers just read the store back, so
// concurrent waiters can't double-persist.
//
// For plugin attempts the entity id is unknown until Fetch returns;
// the record's entityID starts as plannedEntityID (often empty for
// plugin paths) and is filled by the simulator post-fetch.
func (t *ingestTracker) beginAttempt(att ingestAttempt) *ingestRecord {
	plannedID := att.plannedEntityID
	if plannedID == "" && att.entity != nil {
		plannedID = att.entity.ID
	}

	// dedup: plugin-path attempts derive an invocationKey
	// `<plugin>:<rawURL>`. Concurrent ingests with the same key
	// collapse to a single runner record + a single simulator
	// goroutine; subscribers receive the runner's pointer and
	// wait on its transition channel. Fixture-path attempts skip
	// this — they're test fixtures dispatching per-call by design.
	invocationKey := ""
	if att.simulation.plugin != nil && att.simulation.rawURL != "" {
		invocationKey = att.simulation.plugin.Name() + ":" + att.simulation.rawURL
	}

	t.mu.Lock()
	if invocationKey != "" {
		if existing, ok := t.byInvocationKey[invocationKey]; ok {
			// Subscriber path. Return the runner's record;
			// caller's wait() will see the same transition
			// channel and the same terminal-state fields.
			t.mu.Unlock()
			return existing
		}
	}

	rec := &ingestRecord{
		state: ingestStatePending,
		entityID: plannedID,
		transition: make(chan struct{}),
		invocationKey: invocationKey,
	}

	mapKey := plannedID
	if mapKey == "" {
		mapKey = att.simulation.rawURL
	}
	t.records[mapKey] = rec
	if invocationKey != "" {
		t.byInvocationKey[invocationKey] = rec
	}
	t.mu.Unlock()

	go t.runSimulation(rec, att)
	return rec
}

// clearInvocationKeyLocked removes the rec's byInvocationKey entry
// when set. MUST be called with t.mu held (it's a map mutation under
// the same mutex that guards reads in beginAttempt's dedup check).
// No-op when invocationKey is empty (fixture-path records, or
// post-clearing).
func (t *ingestTracker) clearInvocationKeyLocked(rec *ingestRecord) {
	if rec.invocationKey == "" {
		return
	}
	// Defensive: only clear if WE are still the runner. A pathological
	// double-set on the same record (shouldn't happen — state set is
	// the only call site) would otherwise drop a different runner's
	// entry.
	if t.byInvocationKey[rec.invocationKey] == rec {
		delete(t.byInvocationKey, rec.invocationKey)
	}
}

// runSimulation sleeps for the simulated extraction duration, then
// performs any persistence required by the terminal state and signals
// waiters.
//
// Persistence happens here (not in the handler) so concurrent
// long-pollers waking up on the same record can't each call UpsertEntity
// + AppendProvenance + IssueFillToken — the work happens exactly once
// per attempt.
func (t *ingestTracker) runSimulation(rec *ingestRecord, att ingestAttempt) {
	// Detached context so a long-poll caller's cancellation doesn't
	// abort the simulator's persistence. The simulator is short-lived
	// and bounded by the simulation delay (fixture path) or the
	// plugin's own subprocess timeout (plugin path).
	ctx := context.Background()

	if att.simulation.plugin != nil {
		t.runPluginSimulation(ctx, rec, att)
		return
	}

	if att.simulation.delay > 0 {
		time.Sleep(att.simulation.delay)
	}

	switch att.simulation.transitionTo {
	case ingestStateComplete, ingestStateNeedsFill:
		// Vault first (per ADR-0008): a vault-write failure aborts
		// the request and the DB stays untouched. Only after the
		// markdown file is durably renamed into place does the DB
		// receive its derived rows.
		// Fixture path has no plugin / per-fetch TTL inputs — the
		// global config is the only level that can express an
		// opinion. resolveCacheExpires collapses to a stamp from
		// fetched_at + global TTL (or nil / Never per the sentinel).
		fetchedAt := time.Time{}
		if att.provenance.FetchedAt != nil {
			fetchedAt = *att.provenance.FetchedAt
		}
		fixtureExpires := resolveCacheExpires(nil, 0, t.globalCacheTTLSeconds, fetchedAt)
		if err := t.writeIngestVaultFile(ctx, att.entity, "fixture",
			gapKeys(att.simulation.gaps), att.simulation.cleanContent,
			nil, // fixture path — no notations
			nil, // fixture path — no aliases
			att.provenance,
			att.forceRefetch, att.ttlExpired,
			fixtureExpires,
			nil, // fixture path — no attachment manifest
			nil, // fixture path — no canonical edges
		); err != nil {
			t.logger.Error("ingest simulator: vault write failed",
				"err", err, "id", att.entity.ID)
			t.markFailed(rec, "internal_error", "failed to write vault file")
			return
		}
		// Cache-hit-aware emit: probe before upsert so we know
		// whether this fixture run is producing a fresh entity.
		// ADR-0024 Phase 2 — entity.created fires only on the
		// first-time-seen path.
		fixtureWasNew := t.bus != nil && t.entityIsNew(ctx, att.entity.ID)
		if err := t.store.UpsertEntity(ctx, att.entity); err != nil {
			t.logger.Error("ingest simulator: UpsertEntity failed",
				"err", err, "id", att.entity.ID)
			t.markFailed(rec, "internal_error", "failed to persist entity")
			return
		}
		if fixtureWasNew {
			t.publishEntityCreated(ctx, att.entity.ID, att.entity.Kind)
		}
		if err := t.store.AppendProvenance(ctx, att.entity.ID,
			[]store.ProvenanceEntry{att.provenance},
		); err != nil {
			t.logger.Error("ingest simulator: AppendProvenance failed",
				"err", err, "id", att.entity.ID)
			t.markFailed(rec, "internal_error", "failed to record provenance")
			return
		}
		// Per ADR-0013 §4 / yaad-index: a fresh fetch rolls the
		// entity into a new fetch-cycle. Clear any prior gap-call-done
		// flag so the next ingest cache-hit will re-issue the
		// needs_fill payload. Best-effort: a clear failure logs but
		// doesn't fail ingest — the worst case is a stale flag that
		// suppresses one needs_fill payload, easily recoverable via
		// another force_refetch.
		//
		// ErrNotFound here is anomalous (the row was just inserted
		// by UpsertEntity above) — log it the same as any other
		// failure rather than silently swallowing per the cold-reviewer's a prior PR
		// review.
		if err := t.store.ClearGapCallDone(ctx, att.entity.ID); err != nil {
			t.logger.Warn("ingest simulator: ClearGapCallDone (best-effort)",
				"err", err, "id", att.entity.ID)
		}
	}

	// needs_fill no longer issues a fill token (per ADR-0008 + PR
	//): the entity ID itself is the durable callback handle.
	// The simulator just records the gap set; the agent calls
	// POST /v1/entities/{id}/fill with the same id as the durable
	// callback.

	t.mu.Lock()
	rec.state = att.simulation.transitionTo
	rec.cleanContent = att.simulation.cleanContent
	rec.cleanContentTruncated = att.simulation.cleanContentTruncated
	rec.gaps = att.simulation.gaps
	rec.failureCode = att.simulation.failureCode
	rec.failureMessage = att.simulation.failureMessage
	close(rec.transition)
	t.mu.Unlock()
}

// markFailed transitions the record to the failed state with a
// canonical error envelope mapping. Used when the simulator can't
// satisfy the requested terminal state (e.g. UpsertEntity errored,
// or plugin.Fetch errored).
func (t *ingestTracker) markFailed(rec *ingestRecord, code, message string) {
	t.mu.Lock()
	rec.state = ingestStateFailed
	rec.failureCode = code
	rec.failureMessage = message
	t.clearInvocationKeyLocked(rec)
	close(rec.transition)
	t.mu.Unlock()
}

// gapKeys returns the set of gap field names from a {field → desc}
// map, for callers (FillToken issuance) that only need the keys.
// Sorted output keeps the slice stable across calls — the slice is
// otherwise meaningless to compare.
func gapKeys(gaps map[string]string) []string {
	out := make([]string, 0, len(gaps))
	for k := range gaps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// markNotFound transitions the record to the not_found terminal — the
// plugin Fetch'd but produced no candidates (no Entity, no Gaps, no
// Options). Distinct from markFailed because the handler emits 404
// not 500: the request was well-formed and the plugin worked, the
// upstream just had nothing to return.
func (t *ingestTracker) markNotFound(rec *ingestRecord, message string) {
	t.mu.Lock()
	rec.state = ingestStateNotFound
	rec.failureMessage = message
	t.clearInvocationKeyLocked(rec)
	close(rec.transition)
	t.mu.Unlock()
}

// runPluginSimulation is the plugin-driven counterpart of
// runSimulation. It calls plugin.Fetch, persists the returned entity
// + provenance on success, and dispatches to ingestStateNeedsFill
// (when FetchResult.Gaps is non-empty, after issuing a fill token)
// or ingestStateComplete. Plugin errors transition the record to
// ingestStateFailed with `fetch_failed` + the plugin name.
func (t *ingestTracker) runPluginSimulation(ctx context.Context, rec *ingestRecord, att ingestAttempt) {
	// ADR-0023 N-envelope wiring Per the prior design,. Plugin.Stream yields zero
	// or more source envelopes; the tracker writes each as it arrives
	// (write-as-you-go), with the FIRST envelope determining the
	// API-surface state {disambiguation, notFound, needsFill,
	// complete} per option-A in the room consensus. Subsequent
	// envelopes are persisted silently with an INFO breadcrumb;
	// command-shape callers (gmail: !fetch) get proper job-id surface
	// in the future in-memory job system, not here.
	var (
		envelopeIndex int
		firstHandled bool
	)
	streamErr := att.simulation.plugin.Stream(ctx, att.simulation.rawURL,
		func(env *plugins.FetchResult) error {
			envelopeIndex++
			if env == nil {
				return fmt.Errorf("plugin %s: Stream delivered nil envelope at index %d",
					att.simulation.plugin.Name(), envelopeIndex)
			}
			if !firstHandled {
				firstHandled = true
				return t.handleFirstEnvelope(ctx, rec, att, env)
			}
			return t.persistSubsequentEnvelope(ctx, att, env, envelopeIndex)
		},
		nil, // onControl: nil → subprocess logs `_error`/`_summary` per a prior PR's behavior
	)

	if streamErr != nil {
		// Plugin invocation failed. If the first envelope was
		// already handled (write-as-you-go committed), DON'T
		// overwrite the tracker state — the operator sees the
		// in-flight envelopes + a stream-failure log. If no
		// envelope was handled yet, mark fetch_failed so the
		// API surface returns the canonical error shape.
		t.logger.Warn("ingest simulator: plugin.Stream failed",
			"err", streamErr, "plugin", att.simulation.plugin.Name(),
			"url", att.simulation.rawURL,
			"envelopes_committed", envelopeIndex,
		)
		if !firstHandled {
			t.markFailed(rec, "fetch_failed",
				att.simulation.plugin.Name()+": "+streamErr.Error())
		}
		return
	}

	if !firstHandled {
		// Stream completed cleanly with zero source emissions
		// (silent exit, all-control-packets-only, etc). Maps to
		// 404 not_found per ADR-0006's "all-empty" rule, same as
		// legacy single-envelope all-empty.
		t.logger.Info("ingest simulator: plugin emitted zero envelopes",
			"plugin", att.simulation.plugin.Name(), "url", att.simulation.rawURL)
		t.markNotFound(rec,
			att.simulation.plugin.Name()+": no result for "+att.simulation.rawURL)
	}
}

// handleFirstEnvelope runs the single-envelope state machine on the
// first envelope of a Plugin.Stream invocation: discriminates
// disambiguation / not-found / invalid / needsFill / complete and
// sets the tracker.record state accordingly. Source emissions are
// also persisted (vault write + DB upsert + provenance + notations
// + canonical edges) before the state transition; disambiguation /
// not-found / invalid don't persist anything.
//
// Returns a non-nil error only when the persist path fails (vault
// write / DB upsert errors). The tracker.record is already marked
// failed by persistEnvelope's logging path; the returned error
// signals Stream to abort so subsequent envelopes don't try to
// persist on top of a half-written state.
func (t *ingestTracker) handleFirstEnvelope(ctx context.Context, rec *ingestRecord, att ingestAttempt, result *plugins.FetchResult) error {
	// Disambiguation: plugin emitted Options. No persist; state
	// transitions to ingestStateDisambiguation.
	if len(result.Options) > 0 {
		opts := make(map[string]plugins.DisambiguationOption, len(result.Options))
		for id, o := range result.Options {
			opts[id] = o
		}
		t.mu.Lock()
		rec.state = ingestStateDisambiguation
		rec.options = opts
		t.clearInvocationKeyLocked(rec)
		close(rec.transition)
		t.mu.Unlock()
		return nil
	}

	// All-empty: maps to 404 not_found per ADR-0006.
	if result.Entity == nil && len(result.Gaps) == 0 {
		t.logger.Info("ingest simulator: plugin returned no entity/options/gaps",
			"plugin", att.simulation.plugin.Name(), "url", att.simulation.rawURL)
		t.markNotFound(rec,
			att.simulation.plugin.Name()+": no result for "+att.simulation.rawURL)
		return nil
	}

	if result.Entity == nil || result.Entity.ID == "" || result.Entity.Kind == "" {
		t.logger.Error("ingest simulator: plugin returned an unusable FetchResult",
			"plugin", att.simulation.plugin.Name(), "url", att.simulation.rawURL)
		t.markFailed(rec, "internal_error",
			att.simulation.plugin.Name()+": returned an entity without id/kind")
		return nil
	}

	if err := t.persistEnvelope(ctx, att, result); err != nil {
		t.markFailed(rec, "internal_error", err.Error())
		return err
	}

	if len(result.Gaps) > 0 {
		gapsCopy := make(map[string]string, len(result.Gaps))
		for k, v := range result.Gaps {
			gapsCopy[k] = v
		}
		t.mu.Lock()
		rec.entityID = result.Entity.ID
		rec.state = ingestStateNeedsFill
		rec.cleanContent = result.RawContent
		rec.cleanContentTruncated = result.RawContentTruncated
		rec.gaps = gapsCopy
		t.clearInvocationKeyLocked(rec)
		close(rec.transition)
		t.mu.Unlock()
		return nil
	}
	t.mu.Lock()
	rec.entityID = result.Entity.ID
	rec.state = ingestStateComplete
	t.clearInvocationKeyLocked(rec)
	close(rec.transition)
	t.mu.Unlock()
	return nil
}

// persistSubsequentEnvelope is the reduced per-envelope path for
// envelopes 2..N: vault + DB + attachments + canonical edges, no
// state-machine transition. Per option-A the tracker.record state
// is fixed by envelope 1; envelopes 2..N persist silently with an
// INFO breadcrumb.
//
// A persist failure on envelope K mid-stream surfaces through
// Stream's return error; envelope K is NOT committed but envelopes
// 1..K-1 stay on disk per write-as-you-go.
func (t *ingestTracker) persistSubsequentEnvelope(ctx context.Context, att ingestAttempt, result *plugins.FetchResult, envelopeIndex int) error {
	// Disambiguation / not-found shapes from a non-first envelope
	// are protocol violations under option-A — the API state was
	// already fixed by envelope 1. Log + skip.
	if len(result.Options) > 0 {
		t.logger.Warn("ingest simulator: subsequent envelope carries Options; ignored under first-envelope-state contract",
			"plugin", att.simulation.plugin.Name(),
			"envelope_index", envelopeIndex,
		)
		return nil
	}
	if result.Entity == nil || result.Entity.ID == "" || result.Entity.Kind == "" {
		t.logger.Warn("ingest simulator: subsequent envelope without usable entity; skipped",
			"plugin", att.simulation.plugin.Name(),
			"envelope_index", envelopeIndex,
		)
		return nil
	}
	if err := t.persistEnvelope(ctx, att, result); err != nil {
		return err
	}
	t.logger.Info("ingest simulator: subsequent envelope persisted (write-as-you-go)",
		"plugin", att.simulation.plugin.Name(),
		"envelope_index", envelopeIndex,
		"id", result.Entity.ID,
	)
	return nil
}

// persistEnvelope is the per-envelope persist path shared between
// handleFirstEnvelope (entity-shape branch) and
// persistSubsequentEnvelope: synthesize provenance, dispatch
// attachments, vault write, DB upsert, AppendProvenance,
// ClearGapCallDone, ReplaceNotations, materialize canonical thin
// rows, persist canonical edges. Any failure surfaces as a non-nil
// error (the caller decides whether to mark the tracker record
// failed or just abort the stream).
func (t *ingestTracker) persistEnvelope(ctx context.Context, att ingestAttempt, result *plugins.FetchResult) error {
	// Per-artifact write-lock (per yaad-index #23 + ADR-0024).
	// Block-on-conflict against any cross-surface writer holding
	// the same entity ID (operator-fill, archive, delete, etc.).
	// The dedup-by-invocationKey path in beginAttempt already
	// collapses concurrent ingests of the same URL into a single
	// runner; this lock catches the cross-surface case the dedup
	// can't see. Holder names the plugin + the rawURL so a 409
	// from a concurrent HTTP handler reads as
	// "ingest of <plugin>:<url> in flight."
	holder := "ingest:" + att.simulation.plugin.Name() + ":" + att.simulation.rawURL
	release, lockErr := t.writeLocks.Acquire(result.Entity.ID, holder)
	if lockErr != nil {
		if ce, ok := writelocks.AsConflict(lockErr); ok {
			t.logger.Warn("ingest persistEnvelope: write conflict",
				"entity_id", result.Entity.ID,
				"current_holder", ce.Holder,
				"acquired_at", ce.AcquiredAt,
			)
			return fmt.Errorf("write conflict: entity %q locked by %q", result.Entity.ID, ce.Holder)
		}
		return fmt.Errorf("acquire write lock: %w", lockErr)
	}
	defer release()

	provenance := result.Provenance
	if len(provenance) == 0 {
		// Synthesize when the plugin omits provenance (the cold-reviewer's PR
		// contract): every entity must surface at least one
		// provenance entry naming its plugin.
		now := clock.Now()
		provenance = []store.ProvenanceEntry{{
			Source: att.simulation.plugin.Name(),
			FetchedAt: &now,
			OK: true,
		}}
	}

	// Per-envelope attachments dispatch (ADR-0014 + ADR-0023).
	// Streamed plugins commit per-envelope: copy/stage runs
	// immediately after each envelope's vault write succeeds, so a
	// crash mid-stream leaves committed-envelope attachments in
	// their final vault location.
	var attachmentManifest []vault.Attachment
	if len(result.Attachments) > 0 && t.attachmentsDispatcher != nil && t.vaultWriter != nil {
		attachmentManifest = t.dispatchAttachments(ctx, result, provenance, att.forceRefetch)
	}

	fetchedAt := time.Time{}
	if provenance[0].FetchedAt != nil {
		fetchedAt = *provenance[0].FetchedAt
	}
	resolvedExpires := resolveCacheExpires(
		result.CacheTTLSeconds,
		att.simulation.plugin.Capabilities().CacheTTLSeconds,
		t.globalCacheTTLSeconds,
		fetchedAt,
	)
	if err := t.writeIngestVaultFile(ctx, result.Entity, att.simulation.plugin.Name(),
		gapKeys(result.Gaps), result.RawContent, result.Notations, result.Aliases, provenance[0],
		att.forceRefetch, att.ttlExpired,
		resolvedExpires, attachmentManifest, result.CanonicalEdges,
	); err != nil {
		t.logger.Error("ingest simulator: vault write failed (plugin path)",
			"err", err, "id", result.Entity.ID)
		return fmt.Errorf("failed to write vault file: %w", err)
	}

	// Cache-hit-aware emit: probe before upsert so a re-fetch
	// of a known entity doesn't re-publish entity.created. ADR-0024
	// Phase 2 — re-fetch surfaces as entity.edge_added on any new
	// edge, never as entity.created.
	pluginWasNew := t.bus != nil && t.entityIsNew(ctx, result.Entity.ID)
	if err := t.store.UpsertEntity(ctx, result.Entity); err != nil {
		t.logger.Error("ingest simulator: UpsertEntity (plugin path) failed",
			"err", err, "id", result.Entity.ID)
		return fmt.Errorf("failed to persist entity: %w", err)
	}
	if pluginWasNew {
		t.publishEntityCreated(ctx, result.Entity.ID, result.Entity.Kind)
	}
	if err := t.store.AppendProvenance(ctx, result.Entity.ID, provenance); err != nil {
		t.logger.Error("ingest simulator: AppendProvenance (plugin path) failed",
			"err", err, "id", result.Entity.ID)
		return fmt.Errorf("failed to record provenance: %w", err)
	}
	if err := t.store.ClearGapCallDone(ctx, result.Entity.ID); err != nil {
		t.logger.Warn("ingest simulator: ClearGapCallDone (plugin path; best-effort)",
			"err", err, "id", result.Entity.ID)
	}

	if len(result.Notations) > 0 {
		entries := make([]store.Notation, len(result.Notations))
		for i, n := range result.Notations {
			entries[i] = store.Notation{
				Notation: n,
				EntityID: result.Entity.ID,
				Kind: store.NotationKindURL,
			}
		}
		if err := t.store.ReplaceNotations(ctx, result.Entity.ID, entries); err != nil {
			t.logger.Error("ingest simulator: ReplaceNotations failed",
				"err", err, "id", result.Entity.ID,
				"notations", result.Notations)
			return fmt.Errorf("failed to register notation cache entries: %w", err)
		}
	}

	if len(result.CanonicalEdges) > 0 {
		t.materializeThinLabelRowsFromEdges(ctx, result.CanonicalEdges, att.simulation.plugin.Name())
	}
	t.persistCanonicalEdges(ctx, result.CanonicalEdges, att.simulation.plugin.Name())
	return nil
}

// writeIngestVaultFile is the read-merge-write half of the
// ADR-0008 ingest contract: read any existing vault file for this
// entity, append the new provenance entry to its accumulated list,
// and write the merged shape back atomically via vault.Writer.
//
// When the tracker has no vault wiring (writer/reader both nil — the
// pre-a prior PR fallback for tests + dev binaries without vault.path),
// this is a no-op. Callers (the simulator paths) treat the no-op as
// success: the DB-only flow that predates a prior PR still works.
//
// **Concurrency note.** Read-merge-write is NOT atomic across
// concurrent ingests of the same URL. Two simulators that both read
// the prior file, append their respective provenance entries, and
// race on os.Rename will produce a vault file containing only the
// last writer's added entry — the loser's provenance is lost. This
// is acceptable for personal-scale single-operator usage where
// concurrent ingests of the exact same URL are rare. When that
// assumption breaks, the v1+ closes are: a per-entity-id mutex on
// the tracker (cheap, in-process), a filesystem-level advisory lock
// (heavier but cross-process), or a single-writer goroutine fed by a
// channel (simplest concurrency model). Picking among them is a
// follow-up when concurrent re-ingest becomes load-bearing — flagged
// in a prior PR's body and not gated by this PR.
func (t *ingestTracker) writeIngestVaultFile(ctx context.Context, e *store.Entity, plugin string, gaps []string, cleanContent string, notations []string, aliases []string, newProv store.ProvenanceEntry, forceRefetch, ttlExpired bool, cacheExpires *vault.CacheExpires, attachmentManifest []vault.Attachment, canonicalEdges []*store.Edge) error {
	if t.vaultWriter == nil {
		return nil
	}

	// Read existing vault file (if any) to inherit accumulated state.
	// vault.IsNotExist signals the new-entity path — fine, just skip
	// the merge.
	var existing *vault.Entity
	got, err := t.vaultReader.ReadByID(e.Kind, e.ID)
	switch {
	case err == nil:
		existing = got
	case vault.IsNotExist(err):
		// new entity; existing stays nil
	default:
		return fmt.Errorf("read existing vault file for %s: %w", e.ID, err)
	}

	// ADR-0015 marker-pair body merge. Plugin-emitted body content
	// is wrapped in `<!-- yaad:plugin start/end -->` markers so
	// operator hand-edits outside the markers survive across re-
	// ingest. Plugins emit plain markdown; the daemon owns the
	// markers end-to-end.
	mergedBody, mergeReason, mergeErr := mergePluginBodyForVault(existing, cleanContent)
	if mergeErr != nil {
		// ErrPluginEmittedMarker bubbles up — caller marks the
		// ingest as failed with a clean error so the plugin
		// author sees their bug. Per ADR-0015 §4 fail-fast.
		return fmt.Errorf("plugin body merge for %s: %w", e.ID, mergeErr)
	}
	if mergeReason != "" && mergeReason != "clean" {
		// Malformed prior markers — daemon fell back to wholesale
		// replace. Log WARN so operators investigating a missing
		// hand-edit can find the cause; per ADR-0015 §4 this
		// should never happen since the daemon owns marker
		// placement.
		t.logger.Warn("plugin body merge: malformed prior markers; falling back to wholesale-replace",
			"id", e.ID, "reason", mergeReason)
	}

	merged := buildVaultEntity(e, plugin, gaps, mergedBody, notations, aliases, newProv, existing, cacheExpires, attachmentManifest, canonicalEdges)
	commitMsg := ingestCommitMessage(e.ID, existing != nil, forceRefetch, ttlExpired)
	return t.vaultWriter.WriteWithCommit(ctx, merged, commitMsg, "agent:"+plugin)
}

// mergePluginBodyForVault is the ingest-tracker-side wrapper around
// vault.MergePluginBody (ADR-0015 §3). Handles the empty-plugin-
// emission case the lower-level helper deliberately doesn't:
//
// - cleanContent == "" + existing != nil → preserve existing body
// verbatim. A plugin re-ingesting without body content (yaad-bgg
// legacy, fixture entities) MUST NOT clobber whatever the
// previous run + operator hand-edits left there.
// - cleanContent == "" + existing == nil → empty body, no markers.
// - cleanContent != "" → vault.MergePluginBody handles it (first-
// write wrap, re-ingest splice, malformed fallback).
//
// Returns (mergedBody, mergeReason, error). mergeReason mirrors
// PluginBodyMerge.PriorMarkers — "" on first-write or empty-emission,
// "clean" on happy re-ingest, malformed-name on fallback paths
// (caller logs WARN). Error is ErrPluginEmittedMarker (or wraps it)
// when the plugin emitted the reserved marker substring.
func mergePluginBodyForVault(existing *vault.Entity, cleanContent string) (string, string, error) {
	if cleanContent == "" {
		if existing != nil {
			return existing.CleanContent, "", nil
		}
		return "", "", nil
	}
	existingBody := ""
	if existing != nil {
		existingBody = existing.CleanContent
	}
	result, err := vault.MergePluginBody(existingBody, cleanContent)
	if err != nil {
		return "", "", err
	}
	return result.Body, result.PriorMarkers, nil
}

// buildVaultEntity assembles the vault.Entity that gets written for
// an ingest attempt. Provenance accumulates: existing entries (read
// from the prior vault file) come first, followed by the new entry.
// Gaps reflect the most recent attempt's gap set; data + summary are
// taken from the existing file when the new attempt's data is empty
// so a partial re-ingest doesn't clobber agent-filled state.
//
// canonicalEdges is the plugin's resolved canonical-label edge set
// (`<source-id> -[<type>]-> <kind>:<slug>`). When non-nil, it
// REPLACES any prior edges on the existing vault file — plugin
// emission is canonical for source-shape edges . When
// nil (e.g. fixture path or a plugin emission with no edges), the
// prior vault file's edges are preserved unchanged so a non-edge
// plugin's re-ingest doesn't wipe an edge set written by an
// earlier plugin run. The full plugin-emitted set lands in vault
// without operator-config gating; the gate is applied at DB-write
// time in persistCanonicalEdges so that a later config change can
// resurface previously-gated edges via reindex.
func buildVaultEntity(e *store.Entity, plugin string, gaps []string, cleanContent string, notations []string, aliases []string, newProv store.ProvenanceEntry, existing *vault.Entity, cacheExpires *vault.CacheExpires, attachmentManifest []vault.Attachment, canonicalEdges []*store.Edge) *vault.Entity {
	provenance := []vault.ProvenanceEntry{toVaultProvenance(newProv)}
	if existing != nil {
		// existing first, new entry appended.
		acc := make([]vault.ProvenanceEntry, 0, len(existing.Provenance)+1)
		acc = append(acc, existing.Provenance...)
		acc = append(acc, provenance[0])
		provenance = acc
	}

	merged := &vault.Entity{
		ID: e.ID,
		Kind: e.Kind,
		Plugin: plugin,
		Data: e.Data,
		Provenance: provenance,
		Gaps: gaps,
		CleanContent: cleanContent,
		// Notations is the cache-key list per yaad-index the source issue
		// a prior PR. Plugin emits the canonical set on FetchResult; we
		// land it on every vault write so reindex can reconstitute
		// the entity_notations DB table from the vault. Replace
		// wholesale (no merging with existing) — the plugin's view
		// is canonical for cache keys.
		Notations: notations,
		// Aliases (per yaad-index the source issue a prior PR). Plugin-emitted
		// list rides through to vault.Entity.Aliases; the Marshal
		// step (synthesizeAliases) merges in ADR-0011's title-
		// synthesized alias and dedupes. Same wholesale-replace
		// shape as Notations — plugin's view is canonical.
		Aliases: aliases,
	}
	if existing != nil {
		// Preserve agent-filled state across re-ingest. The vault
		// frontmatter is the canonical source for these fields; the
		// new ingest attempt only replaces them when it has its own
		// values to write (which today it never does — plugin output
		// doesn't include summary/tags/notes).
		merged.Summary = existing.Summary
		merged.Tags = existing.Tags
		merged.Notes = existing.Notes
		// Edges: prior vault edges survive when this ingest emits
		// none. When the plugin DID emit canonical edges, the block
		// below replaces them wholesale (plugin is canonical for
		// source-shape edges Per the prior design,).
		merged.Edges = existing.Edges
	}

	// Plugin-emitted canonical-label edges land on vault.Entity.Edges
	// so reindex can reconstitute the DB edge graph from the vault
	// alone (Per the prior design, + ADR-0008's source-of-truth invariant). Replace
	// wholesale when the plugin emitted any — the plugin's view is
	// canonical, same shape as Notations + Aliases.
	if canonicalEdges != nil {
		out := make([]vault.Edge, 0, len(canonicalEdges))
		for _, ce := range canonicalEdges {
			if ce == nil || ce.Type == "" || ce.To == "" {
				continue
			}
			out = append(out, vault.Edge{Type: ce.Type, To: ce.To})
		}
		merged.Edges = out
	}

	// Per yaad-index: stamp the resolved absolute-date cache
	// expiry into vault frontmatter (replaces's duration-
	// based `cache_ttl_seconds:`). nil cacheExpires leaves the
	// field absent (no opinion at any resolution level → cache
	// forever, preserves legacy contract).
	//
	// Per the operator's 2026-05-06 clarification (still in force after
	// the format change): re-resolve on every ingest. Operator-
	// edited `cache_expires:` in vault frontmatter does NOT
	// persist across re-ingests; this stamp always reflects the
	// live resolution against the current entry/plugin/global
	// inputs and overwrites any prior value.
	merged.CacheExpires = cacheExpires

	// ADR-0018 step 6 §Attachments: thread the dispatcher's manifest
	// onto the vault entity so the frontmatter `attachments:` list
	// reflects what's on disk. When the dispatcher emitted a manifest
	// (entity has plugin attachments this fetch), use that. When the
	// dispatcher emitted nothing AND we have an existing entity,
	// preserve its manifest — UGC silence per ADR-0014 §4 (a fetch
	// that doesn't list attachments must not orphan existing on-disk
	// files, which include their manifest entries).
	switch {
	case len(attachmentManifest) > 0:
		merged.Attachments = attachmentManifest
	case existing != nil && len(existing.Attachments) > 0:
		merged.Attachments = existing.Attachments
	}
	return merged
}

// toVaultProvenance converts a store-shaped provenance entry to the
// vault-shaped one. Field-for-field copy; the two types are
// intentionally separate so the vault package has no store import.
func toVaultProvenance(p store.ProvenanceEntry) vault.ProvenanceEntry {
	return vault.ProvenanceEntry{
		Source: p.Source,
		FetchedAt: p.FetchedAt,
		FilledAt: p.FilledAt,
		OK: p.OK,
		Error: p.Error,
		ErrorMessage: p.ErrorMessage,
	}
}

// SourceTypeKind is re-exported from internal/canonical so the
// existing ingest-tracker call sites keep their unqualified
// reference. The constant moved out of this file so the reindex
// package (which can't import internal/api without a cycle) can
// share it.
const SourceTypeKind = canonical.SourceTypeKind

// materializeThinLabelRowsFromEdges ensures every canonical-edge
// target endpoint has an entity row at ingest time, so
// CreateEdge's FK constraint is satisfied. Per ADR-0021 (a)-path:
// the row exists for FK + reachability surfacing in
// list_entities / search_local / needs_fill, but the operator-
// facing vault file is deferred until first operator-fill (
// §5).
//
// Existing rows are preserved as-is (GetEntity-then-skip): a row
// previously populated by an operator-fill must NOT be wiped to
// thin on a subsequent ingest re-emitting the same canonical
// reference. UpsertEntity's ON CONFLICT DO UPDATE would clobber
// Data; we explicitly check-then-insert instead.
//
// `source-type` targets bypass the AllowKind canonical-kinds gate
// (system-reserved per ADR-0021) and are auto-materialized
// identically. All other target kinds are gated: an edge to a
// kind not in the operator's canonical_kinds drops at the row-
// creation step (with the same drop-counter semantics as
// persistCanonicalEntities). The subsequent persistCanonicalEdges
// CreateEdge call then surfaces ErrMissingEntity for those edges
// and they drop with a debug log — same shape as the legacy
// canonical-stub-filtered-out path.
func (t *ingestTracker) materializeThinLabelRowsFromEdges(ctx context.Context, edges []*store.Edge, plugin string) {
	seen := make(map[string]struct{}, len(edges))
	for _, e := range edges {
		if e == nil || e.To == "" {
			continue
		}
		if _, ok := seen[e.To]; ok {
			continue
		}
		seen[e.To] = struct{}{}

		kind, _, ok := splitCanonicalLabelID(e.To)
		if !ok {
			t.logger.Debug("ingest: thin-row materialize skipping malformed edge target",
				"plugin", plugin, "to", e.To)
			continue
		}
		if kind != SourceTypeKind && !t.guard.AllowKind(kind) {
			t.logger.Debug("ingest: canonical edge target kind not in operator config — thin row skipped, edge will drop at FK",
				"plugin", plugin, "to", e.To, "kind", kind)
			if err := t.store.IncDroppedCanonicalKind(ctx, plugin, kind); err != nil {
				t.logger.Warn("ingest: IncDroppedCanonicalKind on edge target (best-effort)",
					"err", err, "plugin", plugin, "kind", kind)
			}
			continue
		}

		// Skip if the row already exists — preserves prior data
		// (operator-fill values, populated Data from a legacy
		// canonical stub, etc.).
		if _, err := t.store.GetEntity(ctx, e.To); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			t.logger.Error("ingest: GetEntity probe before thin-row materialize failed",
				"err", err, "id", e.To, "plugin", plugin)
			continue
		}

		thin := &store.Entity{ID: e.To, Kind: kind}
		if err := t.store.UpsertEntity(ctx, thin); err != nil {
			t.logger.Error("ingest: UpsertEntity for thin label row failed",
				"err", err, "id", e.To, "kind", kind, "plugin", plugin)
			continue
		}
		// ADR-0024 Phase 2 — thin canonical-label row materialized
		// for the first time on this ingest. The skip-if-exists
		// probe above guarantees we only reach here on the create
		// path, so the emit is unconditional (no extra was-new
		// gate needed).
		t.publishEntityCreated(ctx, e.To, kind)
		// Provenance: stamp who referenced this label and when.
		// Mirrors persistCanonicalEntities' provenance shape so
		// /v1/entities surfaces look uniform across legacy stubs
		// and ADR-0021 thin labels.
		now := clock.Now()
		prov := []store.ProvenanceEntry{{
			Source: "canonical-label:" + plugin,
			FetchedAt: &now,
			OK: true,
		}}
		if err := t.store.AppendProvenance(ctx, e.To, prov); err != nil {
			t.logger.Error("ingest: AppendProvenance for thin label row failed",
				"err", err, "id", e.To, "plugin", plugin)
		}
	}
}

// persistCanonicalEdges filters the plugin-emitted canonical edges
// through the operator's canonical-edge-types guard and persists the
// survivors via CreateEdge. ErrMissingEntity from CreateEdge means
// the canonical endpoint was filtered out by canonical-kinds — the
// edge is structurally orphaned, so dropping it is correct (the
// debug log makes the chain visible to operators investigating).
func (t *ingestTracker) persistCanonicalEdges(ctx context.Context, candidates []*store.Edge, plugin string) {
	for _, e := range candidates {
		if e == nil || e.Type == "" || e.From == "" || e.To == "" {
			continue
		}
		if !t.guard.AllowEdgeType(e.Type) {
			t.logger.Debug("ingest: canonical edge dropped — edge type not in operator config",
				"plugin", plugin, "type", e.Type, "from", e.From, "to", e.To)
			// Bump the per-(plugin, edge_type) drop counter (per
			// ADR-0013 §3 / yaad-index a prior PR). Counterpart of
			// the kind drop above; surfaces on /v1/cv-status as
			// `drift.edge_types_emitted_not_enabled[]`. Best-effort.
			//
			// The line below the next `if` (where CreateEdge
			// returns ErrMissingEntity because the endpoint kind
			// was already dropped) does NOT bump the counter —
			// that drop was already accounted for at the kind-
			// drop site, so re-counting here would inflate.
			if err := t.store.IncDroppedCanonicalEdge(ctx, plugin, e.Type); err != nil {
				t.logger.Warn("ingest: IncDroppedCanonicalEdge (best-effort)",
					"err", err, "plugin", plugin, "edge_type", e.Type)
			}
			continue
		}
		if err := t.store.CreateEdge(ctx, e); err != nil {
			if errors.Is(err, store.ErrMissingEntity) {
				t.logger.Debug("ingest: canonical edge dropped — endpoint filtered out by canonical-kinds",
					"plugin", plugin, "type", e.Type, "from", e.From, "to", e.To)
				continue
			}
			t.logger.Error("ingest: CreateEdge for canonical edge failed",
				"err", err, "type", e.Type, "from", e.From, "to", e.To, "plugin", plugin)
			continue
		}
		// ADR-0024 Phase 2 — one entity.edge_added per landed
		// canonical edge. Drops (missing endpoint, guard-filtered)
		// don't emit; the edge never reached the graph.
		t.publishEntityEdgeAdded(ctx, e)
	}
}

// dispatchAttachments resolves the plugin-emitted attachments to
// vault filenames per ADR-0014 and stamps the resulting (role, uri)
// pairs onto provenance[0].FetchAttachments. Mutates provenance in
// place (passed by slice header — caller's view of the same backing
// array changes too).
//
// Per-attachment failures inside Dispatch log at WARN and the
// offending attachment is dropped from the result. A whole-input
// dispatch error (validation of vault-root / kind / local-id)
// likewise logs and is non-fatal — the entity write proceeds without
// fetch_attachments stamped, which is degraded but coherent.
//
// The previous attachments set is read from the freshest existing
// provenance row (the last element of existing.Provenance after a
// vault read). When no existing entity is found (new ingest), the
// previous set is empty and every attachment is fetched fresh.
func (t *ingestTracker) dispatchAttachments(ctx context.Context, result *plugins.FetchResult, provenance []store.ProvenanceEntry, forceRefetch bool) []vault.Attachment {
	if len(provenance) == 0 {
		return nil
	}

	localID, err := localIDFromEntityID(result.Entity.ID, result.Entity.Kind)
	if err != nil {
		t.logger.Warn("attachments: skipping dispatch — local-id derivation failed",
			"id", result.Entity.ID, "kind", result.Entity.Kind, "err", err)
		return nil
	}

	// Read existing vault entity to find the freshest provenance
	// row's fetch_attachments. New-entity path returns nil + ok via
	// vault.IsNotExist; treat as no-prior-attachments. Read errors
	// log + degrade to no-prior — overfetching is correct when we
	// can't see the prior state.
	var prevAtts []attachments.PreviousAttachment
	existing, readErr := t.vaultReader.ReadByID(result.Entity.Kind, result.Entity.ID)
	switch {
	case readErr == nil:
		if len(existing.Provenance) > 0 {
			last := existing.Provenance[len(existing.Provenance)-1]
			prevAtts = make([]attachments.PreviousAttachment, len(last.FetchAttachments))
			for i, p := range last.FetchAttachments {
				prevAtts[i] = attachments.PreviousAttachment{Role: p.Role, URI: p.URI}
			}
		}
	case vault.IsNotExist(readErr):
		// new entity — no prior attachments
	default:
		t.logger.Warn("attachments: vault read for prior-attachments failed; treating as no-prior",
			"id", result.Entity.ID, "err", readErr)
	}

	in := attachments.DispatchInput{
		Attachments: make([]attachments.Attachment, len(result.Attachments)),
		Previous: prevAtts,
		ForceRefetch: forceRefetch,
		VaultRoot: t.vaultWriter.Root(),
		Kind: result.Entity.Kind,
		LocalID: localID,
	}
	for i, a := range result.Attachments {
		in.Attachments[i] = attachments.Attachment{
			Role: a.Role,
			URI: a.URI,
			Extension: a.Extension,
		}
	}

	res, err := t.attachmentsDispatcher.Dispatch(ctx, in)
	if err != nil {
		t.logger.Warn("attachments: dispatch failed; entity proceeds without fetch_attachments",
			"id", result.Entity.ID, "err", err)
		return nil
	}

	if len(res.FetchAttachments) == 0 {
		return nil
	}
	stamped := make([]store.FetchAttachmentRef, len(res.FetchAttachments))
	for i, p := range res.FetchAttachments {
		stamped[i] = store.FetchAttachmentRef{Role: p.Role, URI: p.URI}
	}
	provenance[0].FetchAttachments = stamped

	// ADR-0018 step 6 §Attachments: bridge dispatcher manifest into
	// vault frontmatter so the entity's `attachments:` list reflects
	// what's on disk. The caller threads this onto vault.Entity
	// before write so the manifest is the contract surface for the
	// HTTP read endpoint + the cascade.
	if len(res.Manifest) == 0 {
		return nil
	}
	manifest := make([]vault.Attachment, len(res.Manifest))
	for i, m := range res.Manifest {
		manifest[i] = vault.Attachment{
			Name: m.Name,
			Kind: m.Kind,
			Path: m.Path,
			Bytes: m.Bytes,
		}
	}
	return manifest
}

// localIDFromEntityID extracts the part of an entity ID after the
// `<kind>:` namespace prefix. Used by the attachments dispatcher to
// build vault filenames. Errors when the ID doesn't begin with
// `<kind>:` (defensive — caller should pre-check entity.ID and
// entity.Kind for consistency, but we surface the mismatch loudly
// rather than silently building a malformed path).
func localIDFromEntityID(entityID, kind string) (string, error) {
	prefix := kind + ":"
	if !strings.HasPrefix(entityID, prefix) {
		return "", fmt.Errorf("entity id %q does not begin with kind prefix %q", entityID, prefix)
	}
	return strings.TrimPrefix(entityID, prefix), nil
}

// errIngestTimeout is returned by wait when timeout elapses before the
// record transitions. Long-pollers map it to the canonical 202 queued
// response shape.
var errIngestTimeout = errors.New("ingest long-poll timed out")

// wait blocks until the record transitions out of pending or the
// timeout / context elapses. Returns a snapshot whose terminal-state
// fields are safe to read without further locking.
//
// Behaviour:
// - record already terminal → snapshot returned immediately
// - transition fires within window → snapshot returned
// - timeout fires first → returns (nil, errIngestTimeout)
// - context cancelled → returns (nil, ctx.Err())
//
// timeout == 0 means "no timeout" — used for the synchronous fast-path
// where the handler knows the simulation is already terminal (e.g.
// re-entry on a complete record).
func (t *ingestTracker) wait(ctx context.Context, rec *ingestRecord, timeout time.Duration) (*ingestRecord, error) {
	t.mu.Lock()
	if rec.state != ingestStatePending {
		snap := *rec
		t.mu.Unlock()
		return &snap, nil
	}
	transition := rec.transition
	t.mu.Unlock()

	var timerC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	select {
	case <-transition:
		t.mu.Lock()
		snap := *rec
		t.mu.Unlock()
		return &snap, nil
	case <-timerC:
		return nil, errIngestTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
