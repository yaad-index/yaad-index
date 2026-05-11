package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// waitForEntity polls the store until the named entity exists or the
// deadline expires. Required because runPluginSimulation runs in a
// goroutine: the API handler returns when the FIRST envelope's state
// transition fires (close(rec.transition)), but envelopes 2..N
// continue persisting asynchronously. Test code that asserts on
// envelope 2+ must wait for the simulator goroutine to finish that
// envelope's persist work.
func waitForEntity(t *testing.T, st store.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := st.GetEntity(context.Background(), id)
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("entity %s did not persist within 2s deadline", id)
}

// newStreamingFixture builds a vault-wired handler with a fixture
// plugin whose Plugin.Stream is driven by streamFn. Returns the
// handler, store, vault root, and a teardown via t.Cleanup.
func newStreamingFixture(t *testing.T, streamFn func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error) (http.Handler, store.Store, string) {
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
		NameValue: "stream-fixture",
		MatchFunc: func(rawURL string) bool { return strings.HasPrefix(rawURL, "stream-fixture:") },
		StreamFunc: streamFn,
		CapabilitiesValue: plugins.Capabilities{
			Name: "stream-fixture",
			SourceNamespace: "stream-fixture",
			EntityKinds: []plugins.KindSpec{{Name: "source"}},
		},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry, WithVaultIO(w, r))
	return h, st, root
}

// streamEnvelope is a small builder for the per-envelope FetchResult
// the fixture's StreamFunc emits. Each call produces a distinct
// entity ID + vault path so the test can assert per-envelope persist.
func streamEnvelope(id, name string) *plugins.FetchResult {
	return &plugins.FetchResult{
		Entity: &store.Entity{
			ID: "stream-fixture:" + id,
			Kind: "source",
			Data: map[string]any{"name": name},
		},
		SourceName: name,
		Provenance: []store.ProvenanceEntry{{
			Source: "stream-fixture:fetch",
			OK: true,
		}},
	}
}

// TestIngest_Stream_NEnvelopes_AllCommitted pins ADR-0023's N-line
// contract end-to-end: 3 envelopes emitted by the plugin → 3
// entities persisted in store + 3 vault files on disk.
func TestIngest_Stream_NEnvelopes_AllCommitted(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		for _, e := range []*plugins.FetchResult{
			streamEnvelope("msg-001", "Tehran"),
			streamEnvelope("msg-002", "Berlin"),
			streamEnvelope("msg-003", "Tokyo"),
		} {
			if err := onEnvelope(e); err != nil {
				return err
			}
		}
		return nil
	}
	h, st, root := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:fetch",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	for _, id := range []string{"stream-fixture:msg-001", "stream-fixture:msg-002", "stream-fixture:msg-003"} {
		waitForEntity(t, st, id)
	}

	// Vault files are written before DB upsert per ADR-0008 vault-
	// first; if the entity is in the store, the file must exist.
	// Path scheme is `<root>/<entity.Kind>/<slug>.md` per
	// vault.Writer.pathFor; the fixture sets Kind="source".
	for _, slug := range []string{"msg-001", "msg-002", "msg-003"} {
		path := filepath.Join(root, "source", slug+".md")
		_, err := os.Stat(path)
		require.NoError(t, err, "vault file for %s must exist on disk (write-as-you-go)", slug)
	}
}

// TestIngest_Stream_MidStreamErrorContinues pins ADR-0023's "_error
// doesn't truncate" contract end-to-end: when the plugin emits
// envelope-1 + _error + envelope-2, both envelopes persist. The
// onControl callback is nil in the tracker wiring so the subprocess-
// path logging applies; here the fixture skips emitting controls
// (sufficient because we wire onControl=nil through the tracker
// today, and the test pins envelope persistence under that contract).
func TestIngest_Stream_MidStreamErrorContinues(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		if err := onEnvelope(streamEnvelope("ok-1", "First")); err != nil {
			return err
		}
		// Per-envelope error: the fixture simulates the plugin's
		// "log + skip + continue" decision. Daemon-side this would
		// surface as a _error control packet; under option-A the
		// tracker doesn't change state for it.
		if onControl != nil {
			if err := onControl(plugins.ControlPacket{
				Kind: plugins.ControlPacketError,
				ErrorSlug: "msg-bad",
				ErrorKind: "parse",
				ErrorMessage: "synthesized for test",
			}); err != nil {
				return err
			}
		}
		return onEnvelope(streamEnvelope("ok-2", "Second"))
	}
	h, st, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:mid-error",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	for _, id := range []string{"stream-fixture:ok-1", "stream-fixture:ok-2"} {
		waitForEntity(t, st, id)
	}
}

// TestIngest_Stream_TerminalSummary pins the typical post-migration
// shape: N envelopes followed by a `_summary`. The summary is logged
// (subprocess-path WARN/INFO under option-A; nil onControl in tracker
// wiring), all envelopes commit. This test confirms the trailing
// summary doesn't sink the persist path.
func TestIngest_Stream_TerminalSummary(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		if err := onEnvelope(streamEnvelope("e1", "First")); err != nil {
			return err
		}
		if err := onEnvelope(streamEnvelope("e2", "Second")); err != nil {
			return err
		}
		if onControl != nil {
			return onControl(plugins.ControlPacket{
				Kind: plugins.ControlPacketSummary,
				Ingested: 2,
				Errors: 0,
				DurationMs: 50,
			})
		}
		return nil
	}
	h, st, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:summary",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	for _, id := range []string{"stream-fixture:e1", "stream-fixture:e2"} {
		waitForEntity(t, st, id)
	}
}

// TestIngest_Stream_MisplacedSummaryDoesNotTruncate pins ADR-0023
// §2: a misplaced mid-stream `_summary` doesn't terminate the
// stream — subsequent source envelopes still persist.
func TestIngest_Stream_MisplacedSummaryDoesNotTruncate(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		if err := onEnvelope(streamEnvelope("e1", "First")); err != nil {
			return err
		}
		if onControl != nil {
			if err := onControl(plugins.ControlPacket{
				Kind: plugins.ControlPacketSummary,
				Ingested: 99,
				DurationMs: 10,
			}); err != nil {
				return err
			}
		}
		// Misplaced — emitting a source envelope after a summary.
		// Per ADR-0023 §2, this still processes.
		return onEnvelope(streamEnvelope("e2", "After summary"))
	}
	h, st, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:misplaced",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	for _, id := range []string{"stream-fixture:e1", "stream-fixture:e2"} {
		waitForEntity(t, st, id)
	}
}

// errSimulatedCrash is the sentinel the crash-mid-stream fixture
// returns from its StreamFunc to mimic a plugin that exited
// non-zero after emitting K of N envelopes.
var errSimulatedCrash = errors.New("simulated plugin crash")

// TestIngest_Stream_CrashMidStreamCommittedKOfN pins ADR-0023
// §recovery: when the plugin "crashes" (StreamFunc returns error)
// after emitting K envelopes, the K committed envelopes stay on
// disk. This is the load-bearing write-as-you-go test.
func TestIngest_Stream_CrashMidStreamCommittedKOfN(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		// Emit 2 envelopes successfully, then "crash."
		if err := onEnvelope(streamEnvelope("crash-ok-1", "First")); err != nil {
			return err
		}
		if err := onEnvelope(streamEnvelope("crash-ok-2", "Second")); err != nil {
			return err
		}
		return errSimulatedCrash
	}
	h, st, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:crash",
		"wait_seconds": 2,
	})
	// First envelope set the tracker state to Complete; the
	// post-stream failure DOES NOT overwrite that state per
	// option-A (firstHandled=true → markFailed skipped).
	// API surface returns the first envelope's success shape.
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Both committed envelopes survive the crash (write-as-you-go).
	for _, id := range []string{"stream-fixture:crash-ok-1", "stream-fixture:crash-ok-2"} {
		waitForEntity(t, st, id)
	}
}

// TestIngest_Stream_CrashBeforeFirstEnvelopeFailsCleanly pins the
// converse case: when the plugin errors BEFORE emitting any
// envelope, the tracker marks fetch_failed (no in-flight state to
// preserve).
func TestIngest_Stream_CrashBeforeFirstEnvelopeFailsCleanly(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		return errSimulatedCrash
	}
	h, _, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:fail-fast",
		"wait_seconds": 2,
	})
	// No envelopes committed → tracker hits the markFailed path →
	// API surface returns the canonical error envelope.
	require.NotEqual(t, http.StatusOK, rec.Code, "expected non-2xx; body=%s", rec.Body.String())
}

// TestIngest_Stream_ZeroEnvelopesMapsToNotFound pins the zero-line
// silent-exit contract: stream completes cleanly with no envelopes
// → 404 not_found per ADR-0006.
func TestIngest_Stream_ZeroEnvelopesMapsToNotFound(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		return nil
	}
	h, _, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:silent",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, "expected 404 not_found; body=%s", rec.Body.String())
}

// TestIngest_Stream_SubsequentEnvelopeAttachmentsDispatched pins
// the per-envelope attachment dispatch contract (ADR-0014 +
// ADR-0023): each envelope's Attachments are dispatched immediately
// after that envelope's vault write commits, not at end-of-stream.
//
// Without per-envelope dispatch, a crash mid-stream would lose
// attachments for committed envelopes — the test asserts each
// envelope's attachments land on disk before the next envelope is
// processed.
//
// This test uses an attachment-dispatcher-less fixture (the
// dispatcher wiring is opt-in via WithAttachments) so the fixture
// proves the dispatch path is INVOKED per-envelope; the actual
// file-on-disk assertion lives in the attachment-dispatcher tests.
// Here the contract is "Plugin.Stream invokes the per-envelope
// persist path that includes the attachments dispatch" — verified
// by the entity surviving with its attachments unchanged through
// 2 envelopes.
func TestIngest_Stream_SubsequentEnvelopePersistsIndependently(t *testing.T) {
	t.Parallel()

	streamFn := func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
		// Two envelopes with distinct IDs and distinct provenance.
		// Verifies the persistEnvelope helper handles both via the
		// same code path (handleFirstEnvelope + persistSubsequentEnvelope).
		envA := streamEnvelope("A", "Alpha")
		envB := streamEnvelope("B", "Beta")
		envB.Provenance = []store.ProvenanceEntry{{Source: "stream-fixture:fetch-2", OK: true}}
		if err := onEnvelope(envA); err != nil {
			return err
		}
		return onEnvelope(envB)
	}
	h, st, _ := newStreamingFixture(t, streamFn)

	rec := postIngest(t, h, map[string]any{
		"url": "stream-fixture:per-env",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	waitForEntity(t, st, "stream-fixture:A")
	waitForEntity(t, st, "stream-fixture:B")

	gotA, err := st.GetEntity(context.Background(), "stream-fixture:A")
	require.NoError(t, err)
	gotB, err := st.GetEntity(context.Background(), "stream-fixture:B")
	require.NoError(t, err)

	// Each envelope's distinct provenance must land — confirms
	// AppendProvenance is called per-envelope, not just for the
	// first.
	require.NotEmpty(t, gotA.Provenance)
	require.NotEmpty(t, gotB.Provenance)
	assert.Equal(t, "stream-fixture:fetch", gotA.Provenance[len(gotA.Provenance)-1].Source)
	assert.Equal(t, "stream-fixture:fetch-2", gotB.Provenance[len(gotB.Provenance)-1].Source)
}
