// Package eventbus is the daemon-internal pub-sub substrate per
// ADR-0024 §"Internal event bus (v1 core)". Workflow engine
// subscribers (added in later phases — parser, decision pipeline,
// action runners) react to changes inside the index, not just to
// fresh ingest input. Without this substrate workflows could only
// fire on external arrivals and the fill-gap integration
// described in the ADR would collapse.
//
// Phase 2.1 ships ONLY the substrate: event-type structs, a Bus
// interface, an in-memory implementation, and a Source-tag enum
// for the source-attribution carrying through fill events.
// Emission sites (ingest / fill / edge-add paths) and any actual
// subscribers come in Phase 2.2 + later phases.
//
// Scope:
//   - Three event topics: TopicEntityCreated, TopicEntityEdgeAdded,
//     TopicFillCompleted. The set is closed by the ADR; new
//     topics need an ADR amendment.
//   - In-process only; no cross-host distribution. Subscribers
//     register Handler callbacks; the bus delivers each published
//     event to every active subscription whose topic matches.
//   - Synchronous delivery in the publisher's goroutine. Handlers
//     are expected to be fast (≤ tens of µs of work, no I/O).
//     Slow handlers block other subscribers AND the publisher;
//     handlers needing real work spawn their own goroutine.
//
// Out of scope (this package):
//   - Topic wildcards / pattern subscriptions (added only when a
//     consumer needs them — keep the surface minimal).
//   - Self-loop suppression based on the Source tag (Phase 5;
//     this package only carries the tag through).
//   - Persistence / replay (in-process pub-sub by ADR design).

package eventbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Topic names the channel an event is published on. The set is
// closed by ADR-0024 §"Internal event bus (v1 core)"; adding a
// new topic requires an ADR amendment.
type Topic string

const (
	// TopicEntityCreated fires when a new entity is added by any
	// plugin via a fresh ingest. It does NOT fire on cache-hit
	// re-fetches of an already-known entity — those surface as
	// TopicEntityEdgeAdded for any new connection the re-fetch
	// produces (see ADR-0024 §"Cache-hit re-fetch semantics").
	TopicEntityCreated Topic = "entity.created"

	// TopicEntityEdgeAdded fires when a new edge is attached to
	// an entity. The ADR enumerates four production sites:
	// fresh-ingest edge creation, cache-hit re-fetch surfacing a
	// new edge, operator-side manual edge add (POST /v1/edges),
	// and fill-gap operations that produce edges per the ADR's
	// fill-produces-edges semantics.
	TopicEntityEdgeAdded Topic = "entity.edge_added"

	// TopicFillCompleted fires when a gap-fill lands on an
	// entity. Carries the Source tag identifying who initiated
	// the fill — `agent`, `operator`, or `workflow:<name>` for
	// workflow-injected fills (workflow-injected fills come in
	// Phase 4+; the Source vocabulary is laid down here so the
	// shape is stable).
	TopicFillCompleted Topic = "fill.completed"

	// TopicEntityUpdated fires when a plugin's re-fetch surfaces
	// a per-field delta inside `structured.data` on an
	// already-known entity. One event per changed field per the
	// ADR-0024 2026-05-21 amendment — mirrors TopicEntityEdgeAdded
	// per-edge granularity so subscribers fan out via per-field
	// matching instead of navigating a delta list in CEL.
	//
	// Distinct from TopicEntityCreated (first ingest only),
	// TopicEntityEdgeAdded (edge-only changes), and
	// TopicFillCompleted (gap-fill closures — preserves the
	// gap-author-tagged vs upstream-truth boundary per ADR-0008).
	TopicEntityUpdated Topic = "entity.updated"
)

// AllTopics lists the closed topic set so callers (validators,
// tests, future enumerators) don't hard-code the literals.
var AllTopics = []Topic{
	TopicEntityCreated,
	TopicEntityEdgeAdded,
	TopicFillCompleted,
	TopicEntityUpdated,
}

// Source tags WHO initiated the mutation that produced this
// event. The vocabulary is closed at this layer:
//
//   - SourceAgent: an agent (LLM / human-operator-via-MCP) called
//     into the daemon and the resulting mutation chain produced
//     the event. Practically, the auth Principal at the API
//     boundary maps to this category for the common case.
//   - SourceOperator: the operator's own write (currently
//     overlaps with SourceAgent at the auth layer; reserved here
//     so the future operator-direct-write paths can distinguish).
//   - workflow:<name>: a workflow-injected mutation. Built via
//     WorkflowSource(name) so the formatting is canonical.
//
// The Source is opaque to delivery — the bus carries the tag and
// lets subscribers branch on it. The Phase 5 self-loop backstop
// uses the workflow:<name> form to skip re-firing a workflow on
// its own injected fills (ADR-0024 §"Self-loop detection").
type Source string

const (
	SourceAgent    Source = "agent"
	SourceOperator Source = "operator"
)

const workflowSourcePrefix = "workflow:"

// WorkflowSource constructs the canonical Source tag for a
// workflow-injected mutation. The name is expected to be the
// workflow's bare name (without the `workflow:` prefix); empty
// names round-trip to an invalid Source that ValidateSource
// rejects.
func WorkflowSource(name string) Source {
	return Source(workflowSourcePrefix + name)
}

// IsWorkflow reports whether the source tag identifies a
// workflow-injected mutation (i.e. matches the `workflow:<name>`
// shape with a non-empty name).
func (s Source) IsWorkflow() bool {
	return strings.HasPrefix(string(s), workflowSourcePrefix) && len(s) > len(workflowSourcePrefix)
}

// WorkflowName returns the workflow name carried in a
// workflow:<name> Source, or "" when the Source is not of that
// shape.
func (s Source) WorkflowName() string {
	if !s.IsWorkflow() {
		return ""
	}
	return strings.TrimPrefix(string(s), workflowSourcePrefix)
}

// ValidateSource returns nil when s is one of the recognized
// forms: SourceAgent, SourceOperator, or workflow:<non-empty>.
// Any other string is rejected — emission sites should construct
// Source values via the typed constants or WorkflowSource so
// validation is normally a no-op, but the function exists for
// boundary checks (API ingress, tests).
func ValidateSource(s Source) error {
	switch s {
	case SourceAgent, SourceOperator:
		return nil
	}
	if s.IsWorkflow() {
		return nil
	}
	if s == "" {
		return errors.New("eventbus: empty Source")
	}
	return fmt.Errorf("eventbus: unrecognized Source %q (want agent, operator, or workflow:<name>)", string(s))
}

// Event is the common interface every published event satisfies.
// Concrete event types add per-topic fields below; subscribers
// type-switch on the concrete type for fields beyond the
// common metadata.
type Event interface {
	// Topic identifies the channel this event was published on.
	// Subscribers match on Topic to scope delivery; the value
	// is one of the constants declared above.
	Topic() Topic

	// Source carries the SourceAgent / SourceOperator /
	// workflow:<name> tag identifying who initiated the
	// mutation that produced the event. Self-loop detection
	// (Phase 5) reads this.
	Source() Source

	// OccurredAt is the wall-clock instant at which the
	// mutation completed (publisher-stamped on emit, not on
	// delivery). Time-based subscribers (rate-limit windows,
	// debounce, dedup-by-recency) read this; clock skew across
	// publishers isn't a concern in the in-process model.
	OccurredAt() time.Time

	// WorkflowChain returns the ordered list of workflow names
	// whose firings produced THIS event (per #147 structural
	// cycle detection). An event that wasn't produced by any
	// workflow chain (fresh ingest, agent fill, operator
	// action) has an empty chain. The engine reads the chain
	// before firing a workflow on the event; the workflow's
	// own name appearing in the chain means firing it would
	// close a cycle, so the engine suppresses + records.
	WorkflowChain() []string
}

// EntityCreatedEvent is published when a fresh ingest produces
// a previously-unknown entity (id new to the store). Re-ingest
// of an already-known entity does NOT republish this event —
// the ADR's cache-hit re-fetch semantics route those through
// EntityEdgeAddedEvent for any new connection.
type EntityCreatedEvent struct {
	// ID is the new entity's canonical id.
	ID string

	// Kind is the entity's kind (e.g. `email`, `boardgame`,
	// `task`). Subscribers commonly filter on this; the engine
	// also uses it for workflow trigger matching.
	Kind string

	// SourceTag is the Source attribution. For ingest the
	// common case is SourceAgent (an agent called POST
	// /v1/ingest); workflow-triggered ingests carry the
	// workflow:<name> form.
	SourceTag Source

	// At is the wall-clock time the entity row was written.
	At time.Time

	// Chain is the workflow-name list per #147 cycle detection.
	// Producers reading WorkflowChainFromContext on publish set
	// this; the engine reads it before firing a workflow and
	// skips when the workflow's own name appears.
	Chain []string
}

func (e EntityCreatedEvent) Topic() Topic           { return TopicEntityCreated }
func (e EntityCreatedEvent) Source() Source         { return e.SourceTag }
func (e EntityCreatedEvent) OccurredAt() time.Time  { return e.At }
func (e EntityCreatedEvent) WorkflowChain() []string { return e.Chain }

// EntityEdgeAddedEvent is published when a new edge is attached
// to an entity. Production sites (per ADR-0024):
//
//   - Fresh ingest producing edges on a new or existing entity.
//   - Cache-hit re-fetch surfacing a new connection on an
//     already-known entity.
//   - POST /v1/edges (operator-side manual add).
//   - Fill operations that result in edges (per ADR's
//     fill-produces-edges semantics; fill.completed AND
//     entity.edge_added both fire in that case).
type EntityEdgeAddedEvent struct {
	// FromID is the edge tail (the entity gaining the
	// connection); ToID is the head.
	FromID string
	ToID   string

	// EdgeType is the canonical edge type name (per ADR-0002).
	// Subscribers filter on this to scope trigger conditions.
	EdgeType string

	// SourceTag is the Source attribution — same vocabulary
	// as EntityCreatedEvent.
	SourceTag Source

	// At is the wall-clock time the edge row was written.
	At time.Time

	// Chain is the workflow-name list per #147 cycle detection.
	Chain []string
}

func (e EntityEdgeAddedEvent) Topic() Topic           { return TopicEntityEdgeAdded }
func (e EntityEdgeAddedEvent) Source() Source         { return e.SourceTag }
func (e EntityEdgeAddedEvent) OccurredAt() time.Time  { return e.At }
func (e EntityEdgeAddedEvent) WorkflowChain() []string { return e.Chain }

// FillCompletedEvent is published when a gap-fill lands on an
// entity. The Source tag is the load-bearing field here: the
// Phase 5 self-loop backstop reads it to skip re-firing
// workflow X on a fill X itself injected.
type FillCompletedEvent struct {
	// EntityID is the entity the fill landed on.
	EntityID string

	// Gap is the gap name (the frontmatter key the fill
	// resolved). Subscribers commonly trigger on a specific
	// gap surfacing rather than any-gap-on-this-entity.
	Gap string

	// SourceTag is the Source attribution — `agent` for
	// LLM-strategy fills, `operator` for operator-strategy
	// fills, `workflow:<name>` for workflow-injected fills.
	SourceTag Source

	// At is the wall-clock time the fill was committed.
	At time.Time

	// Chain is the workflow-name list per #147 cycle detection.
	Chain []string
}

func (e FillCompletedEvent) Topic() Topic           { return TopicFillCompleted }
func (e FillCompletedEvent) Source() Source         { return e.SourceTag }
func (e FillCompletedEvent) OccurredAt() time.Time  { return e.At }
func (e FillCompletedEvent) WorkflowChain() []string { return e.Chain }

// EntityUpdatedEvent is published when an ingest re-fetch
// surfaces a per-field delta on `structured.data` for an
// already-known entity. **One event per changed field** —
// a re-fetch that flips both `data.state` and
// `data.comment_count` emits two EntityUpdatedEvents in
// declaration order, not one event with a delta slice.
// Mirrors EntityEdgeAddedEvent's per-edge granularity so
// subscribers fan out via simple per-Field matching.
//
// Does NOT fire on first ingest (that's EntityCreatedEvent)
// or on gap-fill closures (that's FillCompletedEvent). The
// split keeps gap-author-tagged provenance distinct from
// upstream-truth provenance per ADR-0008.
type EntityUpdatedEvent struct {
	// EntityID is the canonical id of the entity whose data
	// changed. Subscribers commonly filter on Kind below;
	// the engine's entity-kind probe path goes through the
	// entity resolver same as EntityCreatedEvent.
	EntityID string

	// Kind is the entity's canonical kind (e.g. `github-pr`,
	// `boardgame`). Carried inline so the engine's match
	// path doesn't need a separate resolve round-trip — the
	// publisher already has the kind in hand on the re-fetch
	// upsert path.
	Kind string

	// Field is the dotted path of the changed field inside
	// `structured.data` (e.g. `data.state`,
	// `data.comment_count`). Subscribers' `field_changed`
	// match filter pins this to the field the workflow cares
	// about.
	//
	// v1 carries direct map-key changes only — deep / nested
	// map deltas surface as a change on the top-level key
	// (the old vs new are the whole nested map). Workflows
	// needing deep matching navigate the value in CEL.
	Field string

	// Old is the previous value of Field; nil when the field
	// was absent before the re-fetch. Carried as any so
	// subscribers can downcast as needed; the publisher
	// stores the raw decoded value, no coercion.
	Old any

	// New is the post-re-fetch value of Field; nil when the
	// field was dropped (re-fetch surfaced no value for a
	// previously-set key). Carried as any same as Old.
	New any

	// SourceTag is the Source attribution — same vocabulary
	// as EntityCreatedEvent. The ingest path stamps
	// SourceAgent for the common case.
	SourceTag Source

	// At is the wall-clock time the re-fetch upsert was
	// committed.
	At time.Time

	// Chain is the workflow-name list per #147 cycle
	// detection. Producers reading WorkflowChainFromContext
	// on publish set this; the engine reads it before firing
	// a workflow and skips when the workflow's own name
	// appears.
	Chain []string
}

func (e EntityUpdatedEvent) Topic() Topic            { return TopicEntityUpdated }
func (e EntityUpdatedEvent) Source() Source          { return e.SourceTag }
func (e EntityUpdatedEvent) OccurredAt() time.Time   { return e.At }
func (e EntityUpdatedEvent) WorkflowChain() []string { return e.Chain }

// Handler is the per-subscription callback the bus invokes on
// every matching published event. Handlers MUST return quickly
// (≤ tens of µs of pure-Go work, no I/O, no blocking calls);
// slow handlers block other subscribers AND the publisher in
// the synchronous-delivery model. Handlers needing real work
// spawn their own goroutine (and own their own backpressure).
//
// Handlers MUST NOT panic; the bus does not recover. A panicking
// handler propagates to the publisher's goroutine and likely
// crashes the daemon.
type Handler func(ctx context.Context, e Event)

// Subscription is the handle returned by Subscribe; Unsubscribe
// removes the registration so the handler stops receiving events.
// Unsubscribe is idempotent — calling it after a previous
// Unsubscribe is a no-op.
type Subscription interface {
	Unsubscribe()
}

// Bus is the pub-sub interface. The Phase 2.1 implementation is
// the in-memory bus in this package; later phases may swap in
// alternative implementations (e.g. one with bounded async
// delivery) — the interface stays the same.
type Bus interface {
	// Subscribe registers handler for events published on the
	// given topic. The returned Subscription is the handle
	// for Unsubscribe; callers MUST hold it for the
	// subscription's intended lifetime (a dropped Subscription
	// is still active until Unsubscribe is called).
	Subscribe(topic Topic, handler Handler) Subscription

	// Publish delivers the event to every active subscription
	// whose topic matches the event's Topic(). Delivery order
	// across subscriptions on the same topic is registration
	// order; ordering across topics is unspecified.
	//
	// Publish blocks until every matched handler has returned.
	// In the synchronous model this is effectively a serial
	// callback dispatch — see Handler's contract for the
	// runtime expectations.
	Publish(ctx context.Context, e Event)
}

// workflowChainKey is the context key for the workflow chain
// trace per #147. Unexported to keep callers on the With /
// From helpers below.
type workflowChainKey struct{}

// WithWorkflowChain returns a derived ctx carrying the given
// chain. Producers reading WorkflowChainFromContext on their
// publish path use this to inherit the chain from the
// workflow-evaluation context: the engine sets the chain
// before invoking action runners, the writers read it on
// publish, downstream events carry it forward, and the engine
// detects cycles when the workflow's own name appears.
//
// A nil chain is fine — the producer reads "" and the
// published event carries an empty Chain field. The "first
// fire by an outside source" path (ingest, agent fill,
// operator action) goes through a ctx with no chain.
func WithWorkflowChain(ctx context.Context, chain []string) context.Context {
	return context.WithValue(ctx, workflowChainKey{}, chain)
}

// WorkflowChainFromContext returns the chain attached to ctx,
// or nil when none is set. Producers use this to populate the
// Chain field on events they emit. Returns a copy so the
// caller can append without mutating ctx-stored state.
func WorkflowChainFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(workflowChainKey{}).([]string)
	if len(v) == 0 {
		return nil
	}
	out := make([]string, len(v))
	copy(out, v)
	return out
}

// PendingEvents accumulates events to publish AFTER the
// caller releases a per-artifact write-lock per #154.
//
// **Why.** The in-process bus dispatches handlers synchronously
// on the publisher's goroutine. A handler that tries to take
// the same write-lock the publisher is holding deadlocks until
// the lock's timeout expires. Workflow handlers firing on
// edge_created emitted mid-ingest racing the ingest path's
// per-envelope hold hit this shape; the fix is to release the
// lock BEFORE the bus emits.
//
// **Usage.**
//
//	release, err := mgr.Acquire(artifact, holder)
//	if err != nil { return err }
//	defer release() // safety net for early returns
//	var pending eventbus.PendingEvents
//	// ... work that previously called bus.Publish(...) ...
//	pending.Add(eventbus.EntityCreatedEvent{...})
//	// ... more work ...
//	release()                       // explicit release
//	pending.Drain(ctx, bus)         // publish AFTER the lock is gone
//	return nil
//
// release() is idempotent (per writelocks contract); the
// `defer release()` stays as the early-return safety net.
type PendingEvents struct {
	events []Event
}

// Add queues an event for later Drain. Safe to call with a
// nil receiver — no-op, useful for callsites that thread an
// optional *PendingEvents through subfunctions.
func (p *PendingEvents) Add(e Event) {
	if p == nil {
		return
	}
	p.events = append(p.events, e)
}

// Drain publishes every queued event in order and resets the
// queue. Safe to call with a nil receiver (no-op) or a nil
// bus (no-op). Drain is one-shot; calling it twice is a no-op
// after the first.
func (p *PendingEvents) Drain(ctx context.Context, bus Bus) {
	if p == nil || bus == nil {
		return
	}
	for _, e := range p.events {
		bus.Publish(ctx, e)
	}
	p.events = nil
}

// Len reports the queued event count. Test-only / diagnostic.
func (p *PendingEvents) Len() int {
	if p == nil {
		return 0
	}
	return len(p.events)
}

// QueueOrPublish appends e to pending when pending is non-nil
// (deferred publish-after-unlock per #154), or immediately
// publishes to bus when pending is nil (caller is outside any
// locked scope — legacy direct-publish path preserved).
//
// Helper for code paths that have both shapes — fill /
// operator_fill / UGC call applyCanonicalTypeEdges with a
// pending queue inside the source lock; future non-locked
// callers (or tests) can pass nil to skip the queue.
func QueueOrPublish(ctx context.Context, bus Bus, pending *PendingEvents, e Event) {
	if pending != nil {
		pending.Add(e)
		return
	}
	if bus != nil {
		bus.Publish(ctx, e)
	}
}
