package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// dedupFixtureOpts configures the controllable fixture plugin used
// by the dedup tests.
type dedupFixtureOpts struct {
	spawns *atomic.Int32
	streamErr error // if non-nil, runner returns this error
	envelopeID string // appended to "dedup-fixture:" for the entity ID
	emitOptions bool // emit a disambiguation envelope instead of an entity
	disambigOption string

	// barrier (when non-nil) blocks the StreamFunc until the test
	// closes it. Used to pin "two concurrent ingests collapse" by
	// holding the runner alive while the second arrival reaches
	// beginAttempt's dedup check.
	barrier <-chan struct{}
	// runnerStarted (when non-nil) is closed by StreamFunc on entry
	// so the test can observe "the runner is in-flight" before
	// firing the second concurrent request.
	runnerStarted chan struct{}
}

// newDedupFixture builds a vault-wired handler with a fixture plugin
// driven by opts. Returns the handler + store + the shared spawn
// counter (also reachable via opts.spawns).
func newDedupFixture(t *testing.T, opts dedupFixtureOpts) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "dedup-fixture",
		MatchFunc: func(rawURL string) bool { return strings.HasPrefix(rawURL, "dedup://") },
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			opts.spawns.Add(1)
			if opts.runnerStarted != nil {
				close(opts.runnerStarted)
			}
			if opts.barrier != nil {
				select {
				case <-opts.barrier:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if opts.streamErr != nil {
				return opts.streamErr
			}
			if opts.emitOptions {
				return onEnvelope(&plugins.FetchResult{
					Options: map[string]plugins.DisambiguationOption{
						opts.disambigOption: {Label: "Disambig Choice"},
					},
				})
			}
			return onEnvelope(&plugins.FetchResult{
				Entity: &store.Entity{
					ID: "dedup-fixture:" + opts.envelopeID,
					Kind: "source",
					Data: map[string]any{"name": "Test"},
				},
				Provenance: []store.ProvenanceEntry{{
					Source: "dedup-fixture:fetch",
					OK: true,
				}},
			})
		},
		CapabilitiesValue: plugins.Capabilities{
			Name: "dedup-fixture",
			SourceNamespace: "dedup-fixture",
			EntityKinds: []plugins.KindSpec{{Name: "source"}},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r))
	return h, st
}

// TestIngest_Dedup_TwoConcurrentCallsCollapseToOneSpawn pins the
// load-bearing contract: two /v1/ingest calls on the same URL,
// the first held in-flight via the fixture barrier while the second
// arrives, spawn ONE subprocess (StreamFunc invocation). Both calls
// receive the same Complete response with the same entity ID.
//
// The barrier is what makes this deterministic — without it, the
// runner can finish before the second arrival reaches beginAttempt,
// trivially clearing byInvocationKey and admitting a second spawn.
// Real concurrent dispatch in production has the same shape (Stream
// holds during subprocess wall-clock); the barrier is the test
// equivalent.
func TestIngest_Dedup_TwoConcurrentCallsCollapseToOneSpawn(t *testing.T) {
	t.Parallel()

	var spawns atomic.Int32
	barrier := make(chan struct{})
	runnerStarted := make(chan struct{})
	h, _ := newDedupFixture(t, dedupFixtureOpts{
		spawns: &spawns,
		envelopeID: "dedup-1",
		barrier: barrier,
		runnerStarted: runnerStarted,
	})

	url := "dedup://example.test/dedup-1"

	rec1Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec1Ch <- postIngest(t, h, map[string]any{
			"url": url,
			"wait_seconds": 2,
		})
	}()

	// Wait until the runner is in-flight before firing call 2,
	// so call 2 deterministically lands on the subscriber path.
	select {
	case <-runnerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start within 2s")
	}

	rec2Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec2Ch <- postIngest(t, h, map[string]any{
			"url": url,
			"wait_seconds": 2,
		})
	}()

	// Give call 2 a moment to reach beginAttempt + register as
	// subscriber before releasing the runner.
	time.Sleep(50 * time.Millisecond)
	close(barrier)

	rec1 := <-rec1Ch
	rec2 := <-rec2Ch
	for i, rec := range []*httptest.ResponseRecorder{rec1, rec2} {
		require.Equal(t, http.StatusOK, rec.Code, "result[%d] body=%s", i, rec.Body.String())
		got := decodeComplete(t, rec)
		assert.Equal(t, "complete", got.Status, "result[%d]", i)
		assert.Equal(t, "dedup-fixture:dedup-1", got.Entity.ID, "result[%d]", i)
	}
	assert.Equal(t, int32(1), spawns.Load(),
		"two concurrent ingests of the same URL must spawn exactly ONE plugin invocation")
}

// TestIngest_Dedup_DisambiguationFidelity pins the disambiguation
// inheritance contract per option-D: when the runner returns
// Options, every subscriber surfaces the same disambiguation
// envelope. Without record-sharing, the second caller would either
// not_found (if it fell through to GetEntity) or re-spawn the
// plugin (if dedup wasn't wired).
func TestIngest_Dedup_DisambiguationFidelity(t *testing.T) {
	t.Parallel()

	var spawns atomic.Int32
	barrier := make(chan struct{})
	runnerStarted := make(chan struct{})
	h, _ := newDedupFixture(t, dedupFixtureOpts{
		spawns: &spawns,
		emitOptions: true,
		disambigOption: "candidate-A",
		barrier: barrier,
		runnerStarted: runnerStarted,
	})

	url := "dedup://example.test/disambig"

	rec1Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec1Ch <- postIngest(t, h, map[string]any{
			"url": url,
			"wait_seconds": 2,
		})
	}()
	<-runnerStarted

	rec2Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec2Ch <- postIngest(t, h, map[string]any{
			"url": url,
			"wait_seconds": 2,
		})
	}()
	time.Sleep(50 * time.Millisecond)
	close(barrier)

	rec1 := <-rec1Ch
	rec2 := <-rec2Ch
	for i, rec := range []*httptest.ResponseRecorder{rec1, rec2} {
		require.Equal(t, http.StatusOK, rec.Code, "result[%d] body=%s", i, rec.Body.String())
		got := decodeDisambiguation(t, rec)
		assert.Equal(t, "disambiguation", got.Status, "result[%d]", i)
		require.Contains(t, got.Options, "candidate-A",
			"result[%d] must surface the runner's disambiguation Options", i)
	}
	assert.Equal(t, int32(1), spawns.Load(),
		"concurrent disambiguation requests must collapse to one spawn")
}

// TestIngest_Dedup_FailureFidelity pins the failure inheritance
// contract: when the runner errors, every subscriber surfaces the
// same failure envelope.
func TestIngest_Dedup_FailureFidelity(t *testing.T) {
	t.Parallel()

	var spawns atomic.Int32
	barrier := make(chan struct{})
	runnerStarted := make(chan struct{})
	h, _ := newDedupFixture(t, dedupFixtureOpts{
		spawns: &spawns,
		streamErr: errors.New("simulated upstream failure"),
		barrier: barrier,
		runnerStarted: runnerStarted,
	})

	url := "dedup://example.test/fail"

	rec1Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec1Ch <- postIngest(t, h, map[string]any{
			"url": url,
			"wait_seconds": 2,
		})
	}()
	<-runnerStarted

	rec2Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec2Ch <- postIngest(t, h, map[string]any{
			"url": url,
			"wait_seconds": 2,
		})
	}()
	time.Sleep(50 * time.Millisecond)
	close(barrier)

	rec1 := <-rec1Ch
	rec2 := <-rec2Ch
	for i, rec := range []*httptest.ResponseRecorder{rec1, rec2} {
		require.NotEqual(t, http.StatusOK, rec.Code,
			"result[%d] must surface failure (body=%s)", i, rec.Body.String())
	}
	assert.Equal(t, int32(1), spawns.Load(),
		"concurrent failing dispatches must collapse to one spawn")
}

// TestIngest_Dedup_SequentialCallsAfterTerminalRespawn pins the
// post-completion behavior: after the runner reaches a terminal
// state, byInvocationKey is cleared. A subsequent ingest of the
// SAME URL spawns fresh (cache-bypass via force_refetch is the
// only way to actually re-fetch through the plugin since the
// notation cache hits first; the test uses force_refetch to
// bypass and prove the dedup-key reset).
func TestIngest_Dedup_SequentialCallsAfterTerminalRespawn(t *testing.T) {
	t.Parallel()

	var spawns atomic.Int32
	h, _ := newDedupFixture(t, dedupFixtureOpts{
		spawns: &spawns,
		envelopeID: "seq-1",
	})

	url := "dedup://example.test/sequential"

	rec1 := postIngest(t, h, map[string]any{
		"url": url,
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec1.Code, "first call body=%s", rec1.Body.String())

	// Second call AFTER the first completes. force_refetch=true
	// bypasses the notation/cache lookup so the dispatch hits
	// beginAttempt fresh; the byInvocationKey entry must have
	// been cleared by the runner's terminal-state set, so this
	// spawns a SECOND time (not the same job-id as the first).
	rec2 := postIngest(t, h, map[string]any{
		"url": url,
		"wait_seconds": 2,
		"force_refetch": true,
	})
	require.Equal(t, http.StatusOK, rec2.Code, "second call body=%s", rec2.Body.String())

	assert.Equal(t, int32(2), spawns.Load(),
		"sequential calls (with force_refetch) must spawn twice — byInvocationKey must clear on terminal state")
}

// TestIngest_Dedup_DistinctURLsDoNotCollide pins that two ingests
// with different URLs (even on the same plugin) get distinct
// invocationKeys and don't collapse.
func TestIngest_Dedup_DistinctURLsDoNotCollide(t *testing.T) {
	t.Parallel()

	var spawns atomic.Int32
	h, _ := newDedupFixture(t, dedupFixtureOpts{
		spawns: &spawns,
		envelopeID: "distinct-1",
	})

	rec1 := postIngest(t, h, map[string]any{
		"url": "dedup://example.test/url-A",
		"wait_seconds": 2,
	})
	rec2 := postIngest(t, h, map[string]any{
		"url": "dedup://example.test/url-B",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec1.Code, "rec1 body=%s", rec1.Body.String())
	require.Equal(t, http.StatusOK, rec2.Code, "rec2 body=%s", rec2.Body.String())

	assert.Equal(t, int32(2), spawns.Load(),
		"distinct URLs must produce distinct invocationKeys → distinct spawns")
}

// TestIngest_Dedup_SubscriberCancelDoesNotBreakRunner pins that a
// subscriber whose long-poll context cancels (caller dropped) does
// NOT affect the runner or other subscribers. This is implicit in
// the design — subscribers are weak readers of the runner's record
// — but the test pins it explicitly so a future refactor doesn't
// quietly couple the two.
func TestIngest_Dedup_SubscriberCancelDoesNotBreakRunner(t *testing.T) {
	t.Parallel()

	var spawns atomic.Int32
	h, _ := newDedupFixture(t, dedupFixtureOpts{
		spawns: &spawns,
		envelopeID: "cancel-test",
	})

	url := "dedup://example.test/cancel"

	// Fire a request whose context will cancel before the runner
	// completes. The runner is fast in this fixture, so we use a
	// very short wait_seconds + an explicit short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest",
		strings.NewReader(`{"url":"`+url+`","wait_seconds":2}`))
	req = req.WithContext(ctx)
	canceledRec := httptest.NewRecorder()
	h.ServeHTTP(canceledRec, req)
	// Don't assert on canceledRec — its outcome depends on race
	// between handler-cancel and runner-completion; either is OK.

	// Real subscriber: should succeed.
	finalRec := postIngest(t, h, map[string]any{
		"url": url,
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, finalRec.Code, "final call body=%s", finalRec.Body.String())

	// Spawn count should be 1 OR 2 depending on whether the
	// runner finished before the cancelled call's beginAttempt
	// landed. Either way the assertion is "we didn't break."
	got := spawns.Load()
	assert.True(t, got == 1 || got == 2,
		"spawns should be 1 (cancel landed during runner) or 2 (cancel landed after runner cleared byInvocationKey); got %d", got)
}
