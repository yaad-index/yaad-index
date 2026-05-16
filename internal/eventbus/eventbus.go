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
)

// AllTopics lists the closed topic set so callers (validators,
// tests, future enumerators) don't hard-code three string
// literals.
var AllTopics = []Topic{
	TopicEntityCreated,
	TopicEntityEdgeAdded,
	TopicFillCompleted,
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
}

func (e EntityCreatedEvent) Topic() Topic          { return TopicEntityCreated }
func (e EntityCreatedEvent) Source() Source        { return e.SourceTag }
func (e EntityCreatedEvent) OccurredAt() time.Time { return e.At }

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
}

func (e EntityEdgeAddedEvent) Topic() Topic          { return TopicEntityEdgeAdded }
func (e EntityEdgeAddedEvent) Source() Source        { return e.SourceTag }
func (e EntityEdgeAddedEvent) OccurredAt() time.Time { return e.At }

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
}

func (e FillCompletedEvent) Topic() Topic          { return TopicFillCompleted }
func (e FillCompletedEvent) Source() Source        { return e.SourceTag }
func (e FillCompletedEvent) OccurredAt() time.Time { return e.At }

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
