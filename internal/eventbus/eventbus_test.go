package eventbus

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSource_Constructor_RoundTrip pins the Source vocabulary:
// the two bare-string constants (agent, operator) and the
// workflow:<name> form built via WorkflowSource. IsWorkflow +
// WorkflowName recover the name; ValidateSource accepts all
// three shapes.
func TestSource_Constructor_RoundTrip(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Source("agent"), SourceAgent)
	assert.Equal(t, Source("operator"), SourceOperator)

	wf := WorkflowSource("amazon-receipt")
	assert.Equal(t, Source("workflow:amazon-receipt"), wf)
	assert.True(t, wf.IsWorkflow())
	assert.Equal(t, "amazon-receipt", wf.WorkflowName())

	// Bare-string sources don't satisfy IsWorkflow.
	assert.False(t, SourceAgent.IsWorkflow())
	assert.False(t, SourceOperator.IsWorkflow())
	assert.Equal(t, "", SourceAgent.WorkflowName())

	require.NoError(t, ValidateSource(SourceAgent))
	require.NoError(t, ValidateSource(SourceOperator))
	require.NoError(t, ValidateSource(wf))
}

// TestSource_Validate_Rejects covers the negative cases that
// ValidateSource catches: empty, unknown bare strings, and the
// workflow:<empty-name> degenerate.
func TestSource_Validate_Rejects(t *testing.T) {
	t.Parallel()
	assert.Error(t, ValidateSource(""))
	assert.Error(t, ValidateSource("admin"))
	assert.Error(t, ValidateSource("workflow:"))
	assert.Error(t, ValidateSource("Workflow:foo")) // case-sensitive
}

// TestEvents_Interface_Methods verifies the three concrete event
// types satisfy the Event interface and return the expected
// Topic + Source + OccurredAt for the bundled fields.
func TestEvents_Interface_Methods(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 9, 47, 0, 0, time.UTC)

	created := EntityCreatedEvent{ID: "e1", Kind: "email", SourceTag: SourceAgent, At: now}
	assert.Equal(t, TopicEntityCreated, created.Topic())
	assert.Equal(t, SourceAgent, created.Source())
	assert.Equal(t, now, created.OccurredAt())

	edge := EntityEdgeAddedEvent{
		FromID: "e1", ToID: "e2", EdgeType: "is_about",
		SourceTag: SourceOperator, At: now,
	}
	assert.Equal(t, TopicEntityEdgeAdded, edge.Topic())
	assert.Equal(t, SourceOperator, edge.Source())

	fill := FillCompletedEvent{
		EntityID: "e1", Gap: "is_newsletter",
		SourceTag: WorkflowSource("newsletter-classify"), At: now,
	}
	assert.Equal(t, TopicFillCompleted, fill.Topic())
	assert.True(t, fill.Source().IsWorkflow())
	assert.Equal(t, "newsletter-classify", fill.Source().WorkflowName())
}

// TestBus_Subscribe_Publish_RoundTrip is the load-bearing
// contract: a Subscribe + Publish pair delivers the event, and
// the delivered Event preserves all fields (the event-shape
// concrete type isn't lossy in transit).
func TestBus_Subscribe_Publish_RoundTrip(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var got []Event
	var mu sync.Mutex
	sub := bus.Subscribe(TopicEntityCreated, func(_ context.Context, e Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})
	defer sub.Unsubscribe()

	now := time.Now()
	in := EntityCreatedEvent{ID: "x", Kind: "email", SourceTag: SourceAgent, At: now}
	bus.Publish(context.Background(), in)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1)
	delivered, ok := got[0].(EntityCreatedEvent)
	require.True(t, ok, "concrete type preserved through Bus")
	assert.Equal(t, in, delivered, "all fields preserved verbatim")
}

// TestBus_TopicIsolation: a subscriber on topic A doesn't see
// events published on topic B. Per the closed-topic-set design,
// this is the only matching primitive.
func TestBus_TopicIsolation(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var createdCount, edgeCount, fillCount atomic.Int32
	defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		createdCount.Add(1)
	}).Unsubscribe()
	defer bus.Subscribe(TopicEntityEdgeAdded, func(_ context.Context, _ Event) {
		edgeCount.Add(1)
	}).Unsubscribe()
	defer bus.Subscribe(TopicFillCompleted, func(_ context.Context, _ Event) {
		fillCount.Add(1)
	}).Unsubscribe()

	ctx := context.Background()
	bus.Publish(ctx, EntityCreatedEvent{ID: "a", SourceTag: SourceAgent, At: time.Now()})
	bus.Publish(ctx, EntityCreatedEvent{ID: "b", SourceTag: SourceAgent, At: time.Now()})
	bus.Publish(ctx, EntityEdgeAddedEvent{FromID: "a", ToID: "b", SourceTag: SourceAgent, At: time.Now()})
	bus.Publish(ctx, FillCompletedEvent{EntityID: "a", SourceTag: SourceOperator, At: time.Now()})

	assert.Equal(t, int32(2), createdCount.Load())
	assert.Equal(t, int32(1), edgeCount.Load())
	assert.Equal(t, int32(1), fillCount.Load())
}

// TestBus_MultipleSubscribersSameTopic: all subscriptions on
// the same topic receive each published event. Delivery order
// across subs is registration order (documented contract).
func TestBus_MultipleSubscribersSameTopic(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var order []int
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		idx := i
		defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, idx)
		}).Unsubscribe()
	}

	bus.Publish(context.Background(), EntityCreatedEvent{ID: "x", SourceTag: SourceAgent, At: time.Now()})

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{0, 1, 2}, order, "delivery follows registration order")
}

// TestBus_Unsubscribe_StopsDelivery: after Unsubscribe, the
// handler is not invoked on subsequent Publish.
func TestBus_Unsubscribe_StopsDelivery(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var hits atomic.Int32
	sub := bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		hits.Add(1)
	})

	bus.Publish(context.Background(), EntityCreatedEvent{ID: "1", SourceTag: SourceAgent, At: time.Now()})
	require.Equal(t, int32(1), hits.Load())

	sub.Unsubscribe()
	bus.Publish(context.Background(), EntityCreatedEvent{ID: "2", SourceTag: SourceAgent, At: time.Now()})
	assert.Equal(t, int32(1), hits.Load(), "post-Unsubscribe Publish is not delivered")
}

// TestBus_Unsubscribe_Idempotent: calling Unsubscribe twice (or
// after the bus moves on) is safe and a no-op.
func TestBus_Unsubscribe_Idempotent(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()
	sub := bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {})
	sub.Unsubscribe()
	assert.NotPanics(t, sub.Unsubscribe, "double Unsubscribe is a no-op")
}

// TestBus_NilHandler_Returns_PreClosedSubscription: passing a
// nil handler to Subscribe doesn't register anything (no panic
// on a later Publish) and the returned Subscription is safe to
// Unsubscribe.
func TestBus_NilHandler_Returns_PreClosedSubscription(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()
	sub := bus.Subscribe(TopicEntityCreated, nil)
	require.NotNil(t, sub)
	assert.NotPanics(t, func() {
		bus.Publish(context.Background(), EntityCreatedEvent{ID: "x", SourceTag: SourceAgent, At: time.Now()})
	}, "nil-handler subscription doesn't dispatch")
	assert.NotPanics(t, sub.Unsubscribe)
}

// TestBus_PublishWithNoSubscribers: Publish on a fresh bus or
// to a topic with no subscribers is a no-op (no panic, no
// allocations beyond the snapshot).
func TestBus_PublishWithNoSubscribers(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()
	assert.NotPanics(t, func() {
		bus.Publish(context.Background(), EntityCreatedEvent{ID: "x", SourceTag: SourceAgent, At: time.Now()})
	})
}

// TestBus_PublishNilEvent: defensive — a nil Event passed to
// Publish is silently dropped rather than panicking.
func TestBus_PublishNilEvent(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()
	defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		t.Fatal("handler must not fire on nil event")
	}).Unsubscribe()
	assert.NotPanics(t, func() { bus.Publish(context.Background(), nil) })
}

// TestBus_SourceTagPreserved covers the load-bearing-for-Phase-5
// property: the Source tag flows through Publish unmodified, so
// future self-loop-suppression logic can rely on it.
func TestBus_SourceTagPreserved(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var captured Source
	var mu sync.Mutex
	defer bus.Subscribe(TopicFillCompleted, func(_ context.Context, e Event) {
		mu.Lock()
		defer mu.Unlock()
		captured = e.Source()
	}).Unsubscribe()

	cases := []Source{
		SourceAgent,
		SourceOperator,
		WorkflowSource("amazon-receipt"),
		WorkflowSource("github-notifications"),
	}
	for _, src := range cases {
		bus.Publish(context.Background(), FillCompletedEvent{
			EntityID: "e", Gap: "is_newsletter", SourceTag: src, At: time.Now(),
		})
		mu.Lock()
		assert.Equal(t, src, captured)
		mu.Unlock()
	}
}

// TestBus_HandlerCanSubscribeDuringPublish: a handler that
// subscribes a new handler while running must not deadlock on
// the bus's internal lock (Publish snapshots subs before
// dispatch).
func TestBus_HandlerCanSubscribeDuringPublish(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var innerHits atomic.Int32
	defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		// Sub from inside a handler — must not deadlock.
		bus.Subscribe(TopicEntityEdgeAdded, func(_ context.Context, _ Event) {
			innerHits.Add(1)
		})
	}).Unsubscribe()

	bus.Publish(context.Background(), EntityCreatedEvent{ID: "x", SourceTag: SourceAgent, At: time.Now()})
	bus.Publish(context.Background(), EntityEdgeAddedEvent{FromID: "x", ToID: "y", SourceTag: SourceAgent, At: time.Now()})

	assert.Equal(t, int32(1), innerHits.Load(),
		"inner subscription registered during outer Publish receives subsequent events")
}

// TestBus_HandlerCanUnsubscribeSelf: a handler that calls
// Unsubscribe on its own subscription mid-dispatch must not
// deadlock; the subscription is marked closed and the slice
// snapshot already in flight finishes its current dispatch.
func TestBus_HandlerCanUnsubscribeSelf(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var hits atomic.Int32
	var sub Subscription
	sub = bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		hits.Add(1)
		sub.Unsubscribe()
	})

	bus.Publish(context.Background(), EntityCreatedEvent{ID: "1", SourceTag: SourceAgent, At: time.Now()})
	bus.Publish(context.Background(), EntityCreatedEvent{ID: "2", SourceTag: SourceAgent, At: time.Now()})

	assert.Equal(t, int32(1), hits.Load(),
		"self-Unsubscribe stops delivery on the next Publish")
}

// TestBus_ConcurrentPublishSubscribe runs many goroutines
// publishing + subscribing + unsubscribing in parallel; the
// test is run with the race detector to catch any data race
// in the bus internals. Hit counts are not asserted strictly
// (timing-dependent); only that no race and no panic occurs.
func TestBus_ConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	const goroutines = 8
	const iters = 200

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				sub := bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {})
				bus.Publish(context.Background(), EntityCreatedEvent{
					ID: "x", SourceTag: SourceAgent, At: time.Now(),
				})
				sub.Unsubscribe()
			}
		}()
	}
	wg.Wait()
	// Sanity: bus didn't accumulate subscriptions; the next
	// Publish has nothing to dispatch to.
	bus.Publish(context.Background(), EntityCreatedEvent{ID: "y", SourceTag: SourceAgent, At: time.Now()})
}

// TestBus_NoGoroutineLeak runs the bus through a Subscribe +
// Publish + Unsubscribe cycle and asserts the runtime goroutine
// count returns to baseline. The synchronous-dispatch model
// promises no spawned goroutines; this test pins that.
func TestBus_NoGoroutineLeak(t *testing.T) {
	// Not t.Parallel — goroutine-count assertions need a quiet
	// runtime; running alongside other parallel tests would
	// race the counter.
	baseline := runtime.NumGoroutine()

	bus := NewMemoryBus()
	for i := 0; i < 50; i++ {
		sub := bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {})
		bus.Publish(context.Background(), EntityCreatedEvent{
			ID: "x", SourceTag: SourceAgent, At: time.Now(),
		})
		sub.Unsubscribe()
	}

	// Tiny grace so any incidental runtime goroutines from
	// stretchr or stdlib settle. The bus itself spawns none.
	time.Sleep(10 * time.Millisecond)
	after := runtime.NumGoroutine()
	assert.LessOrEqual(t, after, baseline+1,
		"bus must not leak goroutines (baseline=%d after=%d)", baseline, after)
}

// TestAllTopics_Closed_Set pins that AllTopics carries exactly
// the four Topic constants and nothing else. Future ADR
// amendments adding a topic must update both AllTopics and this
// test as a paired change.
func TestAllTopics_Closed_Set(t *testing.T) {
	t.Parallel()
	assert.Equal(t,
		[]Topic{TopicEntityCreated, TopicEntityEdgeAdded, TopicFillCompleted, TopicEntityUpdated},
		AllTopics)
}

// TestEntityUpdatedEvent_Interface pins the per-field event
// shape added in ADR-0024's 2026-05-21 amendment: Topic
// returns the constant, Source/OccurredAt/WorkflowChain pass
// through, and the per-field payload (EntityID/Kind/Field/Old/
// New) round-trips.
func TestEntityUpdatedEvent_Interface(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	chain := []string{"github-archive-on-close"}
	evt := EntityUpdatedEvent{
		EntityID:  "github:acme_proj_pr_42",
		Kind:      "github-pr",
		Field:     "data.state",
		Old:       "open",
		New:       "closed",
		SourceTag: SourceAgent,
		At:        at,
		Chain:     chain,
	}
	assert.Equal(t, TopicEntityUpdated, evt.Topic())
	assert.Equal(t, SourceAgent, evt.Source())
	assert.Equal(t, at, evt.OccurredAt())
	assert.Equal(t, chain, evt.WorkflowChain())
	assert.Equal(t, "github:acme_proj_pr_42", evt.EntityID)
	assert.Equal(t, "github-pr", evt.Kind)
	assert.Equal(t, "data.state", evt.Field)
	assert.Equal(t, "open", evt.Old)
	assert.Equal(t, "closed", evt.New)
}

// TestEntityUpdatedEvent_PublishSubscribe wires the new topic
// through the in-memory bus end-to-end: a subscriber on
// TopicEntityUpdated receives the published event, and
// subscribers on other topics do NOT.
func TestEntityUpdatedEvent_PublishSubscribe(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()
	var got []EntityUpdatedEvent
	var mu sync.Mutex
	bus.Subscribe(TopicEntityUpdated, func(_ context.Context, e Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e.(EntityUpdatedEvent))
	})

	// Subscriber on a sibling topic must NOT receive.
	var siblingHits int32
	bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		atomic.AddInt32(&siblingHits, 1)
	})

	bus.Publish(context.Background(), EntityUpdatedEvent{
		EntityID: "boardgame:test-game", Kind: "boardgame", Field: "data.owned",
		Old: false, New: true, SourceTag: SourceAgent, At: time.Now().UTC(),
	})

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1)
	assert.Equal(t, "boardgame:test-game", got[0].EntityID)
	assert.Equal(t, int32(0), atomic.LoadInt32(&siblingHits),
		"entity.updated must not deliver to entity.created subscribers")
}

func TestWorkflowChain_RoundtripsThroughContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if chain := WorkflowChainFromContext(ctx); chain != nil {
		t.Errorf("empty ctx should return nil chain, got %v", chain)
	}
	ctx2 := WithWorkflowChain(ctx, []string{"a", "b", "c"})
	got := WorkflowChainFromContext(ctx2)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("chain roundtrip failed: got %v", got)
	}
}

func TestWorkflowChain_FromContextReturnsCopy(t *testing.T) {
	t.Parallel()
	ctx := WithWorkflowChain(context.Background(), []string{"a", "b"})
	got1 := WorkflowChainFromContext(ctx)
	got1[0] = "MUTATED"
	got2 := WorkflowChainFromContext(ctx)
	if got2[0] != "a" {
		t.Errorf("WorkflowChainFromContext returned aliased slice — mutation leaked: %v", got2)
	}
}

// TestPendingEvents_ReleaseThenPublish_AvoidsLockReentry is the
// #154 regression test. The contract: events queued via
// PendingEvents.Add do NOT publish until Drain runs; with the
// LIFO defer pattern (Drain declared first, lock-release
// declared second), release runs before Drain, so a handler
// reacting to the published event can take the same lock the
// publisher held without deadlocking.
//
// The check uses a sync.Mutex as a stand-in for the per-artifact
// write-lock (writelocks pkg is downstream; this test stays in
// eventbus). A handler subscribed before Publish tries to Lock
// the mutex. If we'd called bus.Publish inside the locked
// section, the handler would block forever (mutex is held);
// the LIFO release-then-Drain pattern releases first, so the
// handler succeeds.
func TestPendingEvents_ReleaseThenPublish_AvoidsLockReentry(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var mu sync.Mutex
	handlerAcquired := make(chan struct{})

	defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		// If the publisher held the mutex while publishing, this
		// Lock would never return — the handler runs synchronously
		// on the publisher's goroutine. With release-then-publish,
		// the mutex is already free.
		mu.Lock()
		defer mu.Unlock()
		close(handlerAcquired)
	}).Unsubscribe()

	func() {
		mu.Lock()
		// LIFO: Drain defer declared first runs LAST — AFTER the
		// release (Unlock) defer declared second.
		var pending PendingEvents
		defer pending.Drain(context.Background(), bus)
		defer mu.Unlock()

		// Queue the event while holding the mutex; the prior shape
		// called bus.Publish directly here, which would block on
		// the handler's Lock attempt.
		pending.Add(EntityCreatedEvent{
			ID:        "test:1",
			Kind:      "test",
			SourceTag: SourceAgent,
			At:        time.Now().UTC(),
		})
	}()

	select {
	case <-handlerAcquired:
		// Handler took the lock after Drain — the publisher
		// released it as expected.
	case <-time.After(time.Second):
		t.Fatal("handler never acquired the lock — release-then-publish ordering broken")
	}
}

// TestPendingEvents_Drain_FIFO pins ordering: events come out
// of Drain in the order they went in (subscribers commonly
// rely on entity.created landing before entity.edge_added so
// FK probes see the row).
func TestPendingEvents_Drain_FIFO(t *testing.T) {
	t.Parallel()
	bus := NewMemoryBus()

	var seen []string
	var mu sync.Mutex
	defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, e Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, "created:"+e.(EntityCreatedEvent).ID)
	}).Unsubscribe()
	defer bus.Subscribe(TopicEntityEdgeAdded, func(_ context.Context, e Event) {
		mu.Lock()
		defer mu.Unlock()
		ee := e.(EntityEdgeAddedEvent)
		seen = append(seen, "edge:"+ee.FromID+"->"+ee.ToID)
	}).Unsubscribe()

	var pending PendingEvents
	now := time.Now().UTC()
	pending.Add(EntityCreatedEvent{ID: "a", Kind: "k", SourceTag: SourceAgent, At: now})
	pending.Add(EntityEdgeAddedEvent{FromID: "a", ToID: "b", EdgeType: "t", SourceTag: SourceAgent, At: now})
	pending.Add(EntityCreatedEvent{ID: "b", Kind: "k", SourceTag: SourceAgent, At: now})
	pending.Drain(context.Background(), bus)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 ||
		seen[0] != "created:a" ||
		seen[1] != "edge:a->b" ||
		seen[2] != "created:b" {
		t.Errorf("Drain did not preserve FIFO order: got %v", seen)
	}
}

// TestPendingEvents_NilSafety: Add / Drain / Len on a nil
// receiver are no-ops, and QueueOrPublish with nil pending
// falls through to immediate bus.Publish.
func TestPendingEvents_NilSafety(t *testing.T) {
	t.Parallel()
	var p *PendingEvents // nil
	assert.NotPanics(t, func() {
		p.Add(EntityCreatedEvent{ID: "x", SourceTag: SourceAgent, At: time.Now()})
	})
	assert.NotPanics(t, func() {
		p.Drain(context.Background(), NewMemoryBus())
	})
	assert.Equal(t, 0, p.Len())

	bus := NewMemoryBus()
	var hits atomic.Int32
	defer bus.Subscribe(TopicEntityCreated, func(_ context.Context, _ Event) {
		hits.Add(1)
	}).Unsubscribe()

	QueueOrPublish(context.Background(), bus, nil, EntityCreatedEvent{
		ID: "y", SourceTag: SourceAgent, At: time.Now(),
	})
	assert.Equal(t, int32(1), hits.Load(),
		"QueueOrPublish with nil pending falls through to immediate bus.Publish")

	// With a non-nil pending, the event queues — bus shouldn't
	// fire until Drain.
	var pending PendingEvents
	QueueOrPublish(context.Background(), bus, &pending, EntityCreatedEvent{
		ID: "z", SourceTag: SourceAgent, At: time.Now(),
	})
	assert.Equal(t, int32(1), hits.Load(),
		"QueueOrPublish with pending defers the publish")
	assert.Equal(t, 1, pending.Len())
	pending.Drain(context.Background(), bus)
	assert.Equal(t, int32(2), hits.Load(),
		"Drain publishes the queued event")
}

func TestEvent_WorkflowChain_Roundtrips(t *testing.T) {
	t.Parallel()
	chain := []string{"w1", "w2"}
	now := time.Now().UTC()
	cases := []Event{
		EntityCreatedEvent{ID: "a:b", Kind: "k", SourceTag: SourceAgent, At: now, Chain: chain},
		EntityEdgeAddedEvent{FromID: "a:b", ToID: "a:c", EdgeType: "t", SourceTag: SourceAgent, At: now, Chain: chain},
		FillCompletedEvent{EntityID: "a:b", Gap: "g", SourceTag: SourceAgent, At: now, Chain: chain},
	}
	for _, e := range cases {
		got := e.WorkflowChain()
		if len(got) != 2 || got[0] != "w1" || got[1] != "w2" {
			t.Errorf("WorkflowChain roundtrip on %T failed: got %v", e, got)
		}
	}
}
