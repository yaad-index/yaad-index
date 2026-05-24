package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// newFanOutFixture builds a handler with a "gmail" fixture plugin
// declaring supports_instances:true + commands:["fetch"] + a
// per-instance config carrying two instances (`personal`, `work`).
// Each invocation captures the active instance via the per-call
// spawn counter so tests can assert fan-out vs single-instance
// dispatch behavior.
func newFanOutFixture(t *testing.T, instances []config.InstanceEntry) (http.Handler, *atomic.Int32) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	var spawns atomic.Int32
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			n := spawns.Add(1)
			// Emit a synthetic entity per invocation so the
			// tracker reaches Complete cleanly. ID varies per
			// call so the fan-out aggregate distinguishes the
			// per-instance results.
			return onEnvelope(&plugins.FetchResult{
				Entity: &store.Entity{
					ID:   "gmail:msg-" + string(rune('0'+n)),
					Kind: "source",
					Data: map[string]any{"subject": rawURL},
				},
				Provenance: []store.ProvenanceEntry{{Source: "gmail:fetch", OK: true}},
			})
		},
		CapabilitiesValue: plugins.Capabilities{
			Name:              "gmail",
			SourceNamespace:   "gmail",
			EntityKinds:       []plugins.KindSpec{{Name: "source"}},
			Commands:          []plugins.CommandSpec{{Name: "fetch"}},
			SupportsInstances: true,
		},
	})

	pluginInstanceConfigs := map[string][]config.InstanceEntry{
		"gmail": instances,
	}
	pluginInstances := map[string][]string{
		"gmail": instanceNames(instances),
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry,
		WithPluginInstances(pluginInstances),
		WithPluginInstanceConfigs(pluginInstanceConfigs),
	)
	return h, &spawns
}

// TestCommandFanOut_BarePlugin_MultiInstance pins the canonical
// ADR-0028 §4 bare-plugin fan-out: two instances → handler walks
// both serially in declaration order, response shape is the
// aggregate ingestFanOutResponse with per-instance results.
func TestCommandFanOut_BarePlugin_MultiInstance(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal", Config: map[string]any{"account": "a@b.com"}},
		{Name: "work", Config: map[string]any{"account": "w@b.com"}},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp ingestFanOutResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "fan_out", resp.State)
	assert.Equal(t, "gmail", resp.Plugin)
	require.Len(t, resp.Result, 2, "two instances → two result entries")
	assert.Equal(t, "personal", resp.Result[0].Instance,
		"results preserve operator-config declaration order")
	assert.Equal(t, "work", resp.Result[1].Instance)
	// Both reach complete state (the fixture always emits a single
	// envelope per spawn).
	assert.Equal(t, "complete", resp.Result[0].State)
	assert.Equal(t, "complete", resp.Result[1].State)
	assert.NotEmpty(t, resp.Result[0].EntityID)
	assert.NotEmpty(t, resp.Result[1].EntityID)
	assert.Equal(t, int32(2), spawns.Load(),
		"fan-out must invoke each instance exactly once")
}

// TestCommandFanOut_InstanceScoped_Single pins the
// `<plugin>/<instance>: !<cmd>` form: handler routes to exactly
// the named instance, emits the regular single-attempt response
// shape (NOT the fan_out aggregate).
func TestCommandFanOut_InstanceScoped_Single(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal", Config: map[string]any{"account": "a@b.com"}},
		{Name: "work", Config: map[string]any{"account": "w@b.com"}},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail/work: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Single-attempt shape, NOT the fan_out aggregate.
	var resp ingestCompleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "complete", resp.State)
	assert.Equal(t, int32(1), spawns.Load(),
		"instance-scoped form must invoke exactly one instance")
}

// TestCommandFanOut_InstanceScoped_UnknownInstance pins the
// reject path: unknown instance name → 404 unknown_instance with
// the configured-instances list named in the error for diagnostics.
func TestCommandFanOut_InstanceScoped_UnknownInstance(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal"},
		{Name: "work"},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail/ghost: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown_instance")
	assert.Contains(t, rec.Body.String(), "ghost")
	assert.Contains(t, rec.Body.String(), "personal",
		"diagnostic must list configured instances so operator can correlate")
	assert.Equal(t, int32(0), spawns.Load(),
		"unknown-instance rejection must skip the subprocess spawn")
}

// TestCommandFanOut_SingleConfiguredInstance_CollapsesToSingleAttempt
// pins the back-compat shape: a plugin with one configured instance
// (operator omits the `instances:` block or declares exactly one)
// invoked with the bare form still emits the regular single-attempt
// response, not the fan_out aggregate. Pre-Cut-4 callers see no
// shape change for the common single-instance deployment.
func TestCommandFanOut_SingleConfiguredInstance_CollapsesToSingleAttempt(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "default"},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp ingestCompleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "complete", resp.State,
		"single-instance plugin must emit the single-attempt shape, NOT fan_out")
	assert.Equal(t, int32(1), spawns.Load())
}

// TestCommandFanOut_ErrorContinuesWalk pins the ADR-0028 §4
// "per-instance error logged + reported in aggregate response;
// fan-out continues" contract: when one instance's run errors,
// subsequent instances still run and their results land in the
// aggregate.
func TestCommandFanOut_ErrorContinuesWalk(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	var spawns atomic.Int32
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			n := spawns.Add(1)
			if n == 1 {
				// First instance errors (an empty envelope set
				// produces "no result" failure downstream).
				return nil
			}
			return onEnvelope(&plugins.FetchResult{
				Entity: &store.Entity{
					ID:   "gmail:msg-2",
					Kind: "source",
					Data: map[string]any{"subject": "ok"},
				},
				Provenance: []store.ProvenanceEntry{{Source: "gmail:fetch", OK: true}},
			})
		},
		CapabilitiesValue: plugins.Capabilities{
			Name:              "gmail",
			SourceNamespace:   "gmail",
			EntityKinds:       []plugins.KindSpec{{Name: "source"}},
			Commands:          []plugins.CommandSpec{{Name: "fetch"}},
			SupportsInstances: true,
		},
	})

	instances := []config.InstanceEntry{
		{Name: "broken"},
		{Name: "working"},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry,
		WithPluginInstances(map[string][]string{"gmail": {"broken", "working"}}),
		WithPluginInstanceConfigs(map[string][]config.InstanceEntry{"gmail": instances}),
	)

	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp ingestFanOutResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Result, 2)
	assert.Equal(t, "broken", resp.Result[0].Instance)
	assert.Equal(t, "failed", resp.Result[0].State,
		"first instance's error must be reported as failed in the aggregate")
	assert.NotEmpty(t, resp.Result[0].Error)
	assert.Equal(t, "working", resp.Result[1].Instance)
	assert.Equal(t, "complete", resp.Result[1].State,
		"second instance must run even though the first errored — per ADR-0028 §4")
	assert.Equal(t, int32(2), spawns.Load(),
		"both instances must spawn despite the first instance's error")
}
