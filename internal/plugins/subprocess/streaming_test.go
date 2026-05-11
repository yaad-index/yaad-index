package subprocess

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// streamingTestPlugin returns a minimal *Plugin sufficient for
// driving streamStdout directly. The reader doesn't touch any other
// Plugin field — only p.logger + p.name (and p.capabilities for
// validateStructured) — so the rest of the construction surface
// (path, timeouts) is irrelevant for these unit tests.
func streamingTestPlugin(t *testing.T) (*Plugin, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	caps := Capabilities{
		Name: "streaming-fake",
		SourceNamespace: "wikipedia",
	}
	return &Plugin{name: "streaming-fake", logger: logger, capabilities: caps}, &buf
}

// captureAll returns an EnvelopeFunc that appends every envelope
// to a slice. Used by the Stream-shape tests to count + inspect
// per-envelope delivery.
func captureAll() (*[]*plugins.FetchResult, plugins.EnvelopeFunc) {
	var all []*plugins.FetchResult
	return &all, func(e *plugins.FetchResult) error {
		all = append(all, e)
		return nil
	}
}

// captureControls returns a ControlFunc that appends every packet
// to a slice.
func captureControls() (*[]plugins.ControlPacket, plugins.ControlFunc) {
	var packets []plugins.ControlPacket
	return &packets, func(c plugins.ControlPacket) error {
		packets = append(packets, c)
		return nil
	}
}

// TestStreamStdout_SingleLineNDJSON pins the NDJSON-shape forward
// path: one JSON object per line, terminated with a newline.
func TestStreamStdout_SingleLineNDJSON(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{"title":"Tehran"},"provenance":[{"source":"wikipedia:fetch","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n")

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(in, onEnv, nil))
	require.Len(t, *envs, 1)
	require.NotNil(t, (*envs)[0].Entity)
	assert.Equal(t, "wikipedia:tehran", (*envs)[0].Entity.ID)
}

// TestStreamStdout_PrettyPrintedSingleObject pins the pre-migration
// shape (multi-line indented JSON, no trailing newline). json.Decoder
// transparently handles it as one value.
func TestStreamStdout_PrettyPrintedSingleObject(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(`{
		"ok": true,
		"structured": {
			"kind": "source",
			"name": "Tehran",
			"data": {"title": "Tehran"},
			"provenance": [{"source": "wikipedia:fetch", "fetched_at": "2026-05-10T14:00:00Z", "ok": true}]
		}
	}`)

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(in, onEnv, nil))
	require.Len(t, *envs, 1)
}

// TestStreamStdout_ZeroLineSilentExit pins the 0-emission case:
// silent exit (empty stdout) returns nil error and invokes
// onEnvelope zero times. The tracker's all-empty branch then maps
// it to 404 not_found.
func TestStreamStdout_ZeroLineSilentExit(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(nil, onEnv, nil))
	assert.Empty(t, *envs)
}

// TestStreamStdout_NLineDeliversAllEnvelopes pins ADR-0023's
// N-envelope contract: 3 source emissions → onEnvelope called 3
// times, in order, each carrying the decoded envelope.
func TestStreamStdout_NLineDeliversAllEnvelopes(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(
		`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`{"ok":true,"structured":{"kind":"source","name":"Berlin","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`{"ok":true,"structured":{"kind":"source","name":"Tokyo","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n",
	)

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(in, onEnv, nil))
	require.Len(t, *envs, 3, "all 3 envelopes must reach the callback in order")
	assert.Equal(t, "wikipedia:tehran", (*envs)[0].Entity.ID)
	assert.Equal(t, "wikipedia:berlin", (*envs)[1].Entity.ID)
	assert.Equal(t, "wikipedia:tokyo", (*envs)[2].Entity.ID)
}

// TestStreamStdout_ErrorSentinelSurfacedToControl pins the
// `_error` callback shape: when onControl is wired, error packets
// flow there instead of the logger.
func TestStreamStdout_ErrorSentinelSurfacedToControl(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(`{"_error":{"slug":"msg-001","kind":"parse","message":"malformed MIME"}}` + "\n")

	envs, onEnv := captureAll()
	packets, onControl := captureControls()
	require.NoError(t, p.streamStdout(in, onEnv, onControl))
	require.Empty(t, *envs)
	require.Len(t, *packets, 1)
	assert.Equal(t, plugins.ControlPacketError, (*packets)[0].Kind)
	assert.Equal(t, "msg-001", (*packets)[0].ErrorSlug)
	assert.Equal(t, "malformed MIME", (*packets)[0].ErrorMessage)
}

// TestStreamStdout_SummaryPacketSurfacedToControl pins the
// `_summary` callback shape.
func TestStreamStdout_SummaryPacketSurfacedToControl(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(`{"_summary":{"ingested":3,"errors":1,"duration_ms":4521}}` + "\n")

	envs, onEnv := captureAll()
	packets, onControl := captureControls()
	require.NoError(t, p.streamStdout(in, onEnv, onControl))
	require.Empty(t, *envs)
	require.Len(t, *packets, 1)
	assert.Equal(t, plugins.ControlPacketSummary, (*packets)[0].Kind)
	assert.Equal(t, 3, (*packets)[0].Ingested)
	assert.Equal(t, 1, (*packets)[0].Errors)
	assert.Equal(t, 4521, (*packets)[0].DurationMs)
}

// TestStreamStdout_NWithMidStreamError pins ADR-0023's "_error
// doesn't truncate" contract: source emissions before AND after the
// error sentinel both reach onEnvelope; the error reaches onControl.
func TestStreamStdout_NWithMidStreamError(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(
		`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`{"_error":{"slug":"msg-002","kind":"parse","message":"bad MIME"}}` + "\n" +
			`{"ok":true,"structured":{"kind":"source","name":"Berlin","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n",
	)

	envs, onEnv := captureAll()
	packets, onControl := captureControls()
	require.NoError(t, p.streamStdout(in, onEnv, onControl))
	require.Len(t, *envs, 2, "envelopes before AND after _error must surface")
	require.Len(t, *packets, 1, "_error must surface as one control packet")
	assert.Equal(t, "wikipedia:tehran", (*envs)[0].Entity.ID)
	assert.Equal(t, "wikipedia:berlin", (*envs)[1].Entity.ID)
}

// TestStreamStdout_NWithTerminalSummary pins ADR-0023's typical
// post-migration response: N source emissions followed by a
// terminal `_summary`. All envelopes deliver; summary surfaces.
func TestStreamStdout_NWithTerminalSummary(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(
		`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`{"ok":true,"structured":{"kind":"source","name":"Berlin","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`{"_summary":{"ingested":2,"errors":0,"duration_ms":120}}` + "\n",
	)

	envs, onEnv := captureAll()
	packets, onControl := captureControls()
	require.NoError(t, p.streamStdout(in, onEnv, onControl))
	require.Len(t, *envs, 2)
	require.Len(t, *packets, 1)
	assert.Equal(t, plugins.ControlPacketSummary, (*packets)[0].Kind)
	assert.Equal(t, 2, (*packets)[0].Ingested)
}

// TestStreamStdout_MisplacedSummaryDoesNotTruncate pins ADR-0023
// §2's "misplaced summary doesn't truncate the stream" rule: a
// `_summary` followed by another source emission still scans the
// trailing source.
func TestStreamStdout_MisplacedSummaryDoesNotTruncate(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(
		`{"_summary":{"ingested":99,"errors":0,"duration_ms":10}}` + "\n" +
			`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n",
	)

	envs, onEnv := captureAll()
	packets, onControl := captureControls()
	require.NoError(t, p.streamStdout(in, onEnv, onControl))
	require.Len(t, *envs, 1, "source emission AFTER misplaced _summary must still surface")
	require.Len(t, *packets, 1)
	assert.Equal(t, "wikipedia:tehran", (*envs)[0].Entity.ID)
}

// TestStreamStdout_FullyMalformedHardFails pins the plugin-bug
// contract: stdout that decodes to ZERO valid values returns a
// hard "decode response" error (preserves pre-ADR-0023 behavior
// for opaquely-broken plugins).
func TestStreamStdout_FullyMalformedHardFails(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	envs, onEnv := captureAll()
	err := p.streamStdout([]byte("not actually json"), onEnv, nil)
	require.Error(t, err, "fully-malformed stdout must surface as decode error")
	assert.Contains(t, err.Error(), "decode response")
	assert.Empty(t, *envs)
}

// TestStreamStdout_MalformedAfterFirstValueIsBestEffort pins the
// resilience contract: once at least one valid value has decoded,
// a trailing decode error is logged + the loop bails (best-effort).
// The valid envelopes still reach onEnvelope.
func TestStreamStdout_MalformedAfterFirstValueIsBestEffort(t *testing.T) {
	p, logs := streamingTestPlugin(t)

	in := []byte(
		`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`<not-actually-json>` + "\n",
	)

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(in, onEnv, nil))
	require.Len(t, *envs, 1)
	assert.Contains(t, logs.String(), "decode error past first value")
}

// TestStreamStdout_NonObjectValueSkipped pins the "JSON value but
// not a JSON object" defensive path: a top-level array, string, or
// number value isn't a valid envelope shape; the scanner logs +
// skips it without aborting the stream.
func TestStreamStdout_NonObjectValueSkipped(t *testing.T) {
	p, logs := streamingTestPlugin(t)

	in := []byte(`["array-not-object"]` + "\n" +
		`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n")

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(in, onEnv, nil))
	require.Len(t, *envs, 1)
	assert.Contains(t, logs.String(), "non-object JSON value")
}

// TestStreamStdout_OnEnvelopeAbortHaltsStream pins the callback-
// abort contract: a non-nil return from onEnvelope terminates the
// stream and surfaces as Stream's return error.
func TestStreamStdout_OnEnvelopeAbortHaltsStream(t *testing.T) {
	p, _ := streamingTestPlugin(t)

	in := []byte(
		`{"ok":true,"structured":{"kind":"source","name":"Tehran","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n" +
			`{"ok":true,"structured":{"kind":"source","name":"Berlin","data":{},"provenance":[{"source":"x","fetched_at":"2026-05-10T14:00:00Z","ok":true}]}}` + "\n",
	)

	count := 0
	abortErr := errStopForTest
	err := p.streamStdout(in, func(e *plugins.FetchResult) error {
		count++
		return abortErr
	}, nil)
	require.ErrorIs(t, err, errStopForTest)
	assert.Equal(t, 1, count, "callback abort must terminate stream after first envelope")
}

// TestStreamStdout_LegacyLogPathWhenOnControlNil pins the back-
// compat contract: when onControl is nil, control packets are
// logged through the plugin's logger (preserving a prior PR's behavior
// for callers that haven't migrated to the callback shape).
func TestStreamStdout_LegacyLogPathWhenOnControlNil(t *testing.T) {
	p, logs := streamingTestPlugin(t)

	in := []byte(
		`{"_error":{"slug":"msg-x","kind":"parse","message":"err-msg"}}` + "\n" +
			`{"_summary":{"ingested":7,"errors":2,"duration_ms":300}}` + "\n",
	)

	envs, onEnv := captureAll()
	require.NoError(t, p.streamStdout(in, onEnv, nil))
	require.Empty(t, *envs)

	logged := logs.String()
	assert.Contains(t, logged, "_error", "_error must be logged when onControl is nil")
	assert.Contains(t, logged, "msg-x")
	assert.Contains(t, logged, "_summary", "_summary must be logged when onControl is nil")
	assert.Contains(t, logged, "ingested=7")
}

// errStopForTest is a sentinel returned by TestStreamStdout_
// OnEnvelopeAbortHaltsStream's callback. Sentinel-shape so the
// test asserts ErrorIs behavior.
var errStopForTest = newSentinel("stop for test")

func newSentinel(msg string) error { return &sentinelErr{msg: msg} }

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }

// TestPeekBytes_NoTruncationWhenShort pins the peek helper: short
// input passes through verbatim (no truncation marker).
func TestPeekBytes_NoTruncationWhenShort(t *testing.T) {
	got := peekBytes([]byte("short"), 100)
	assert.Equal(t, "short", got)
	assert.NotContains(t, got, "more bytes")
}

// TestPeekBytes_TruncatesWithAnnotation pins the truncation
// annotation: long input gets cut + a "(N more bytes)" tail.
func TestPeekBytes_TruncatesWithAnnotation(t *testing.T) {
	long := []byte(strings.Repeat("x", 500))
	got := peekBytes(long, 100)
	assert.Contains(t, got, "(400 more bytes)")
}
