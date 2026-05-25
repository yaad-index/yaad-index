package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

// TestCommandFanOut_AsyncSpawnsAllInstances_NoDedupCollision pins
// the ADR-0028 §4 fix from the PR-253 cold-review: fan-out across
// N instances must produce N distinct tracker records even in the
// async (waitSeconds=0) path. Without the instance-aware dedup
// key, the second through Nth attempts would subscribe to the
// first record's invocation-key entry and skip their own
// subprocess spawn — silent-success failure where the wire
// response says "queued for all N" but only one instance runs.
func TestCommandFanOut_AsyncSpawnsAllInstances_NoDedupCollision(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal"},
		{Name: "work"},
		{Name: "ops"},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Each instance must produce its own spawn; nothing collapses
	// via the tracker's invocation-key dedup.
	assert.Equal(t, int32(3), spawns.Load(),
		"3 instances → 3 distinct subprocess spawns (dedup key must include instance)")

	var resp ingestFanOutResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Result, 3)
	// All three instances reach complete (the fixture always emits
	// a single envelope per spawn). Each carries a distinct
	// instance name in the aggregate.
	got := []string{resp.Result[0].Instance, resp.Result[1].Instance, resp.Result[2].Instance}
	assert.Equal(t, []string{"personal", "work", "ops"}, got)
}

// TestCommandFanOut_PerInstanceEnvReachesPlugin pins the ADR-0028
// §3 + §4 (Cut 4) per-instance env splice — the dispatch layer
// builds YAAD_PLUGIN_CONFIG + InstanceEntry.Env entries from the
// active instance and threads them into the invocation ctx via
// plugins.WithExtraEnv. The fixture plugin's StreamFunc captures
// the per-call ExtraEnv so the test asserts: each per-instance
// spawn sees its own env, distinct from the other instance's
// env. Without the per-call env splice (the silent-success
// failure mode the PR-253 cold-review surfaced), all
// instances would see the same registry-build-time env and the
// per-instance values would never reach the subprocess.
func TestCommandFanOut_PerInstanceEnvReachesPlugin(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	type capturedCall struct {
		envFromCtx []string
	}
	var (
		mu sync.Mutex
		calls []capturedCall
	)

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			mu.Lock()
			calls = append(calls, capturedCall{
				envFromCtx: plugins.ExtraEnvFromContext(ctx),
			})
			mu.Unlock()
			return onEnvelope(&plugins.FetchResult{
				Entity: &store.Entity{
					ID:   "gmail:msg-" + rawURL,
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

	instances := []config.InstanceEntry{
		{
			Name:   "personal",
			Config: map[string]any{"account": "ops@example.test"},
			Env:    map[string]string{"YAAD_GMAIL_ACCOUNT": "ops@example.test"},
		},
		{
			Name:   "work",
			Config: map[string]any{"account": "work@example.test"},
			Env:    map[string]string{"YAAD_GMAIL_ACCOUNT": "work@example.test"},
		},
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry,
		WithPluginInstances(map[string][]string{"gmail": {"personal", "work"}}),
		WithPluginInstanceConfigs(map[string][]config.InstanceEntry{"gmail": instances}),
	)

	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 2, "fan-out across 2 instances → 2 distinct Stream calls")

	// Each call's ctx must carry the instance's specific env. The
	// per-call ExtraEnv shape is `[]string` with `KEY=VALUE`
	// entries: YAAD_PLUGIN_CONFIG (built from the per-instance
	// Config map) + InstanceEntry.Env entries spliced on top.
	assertEnvContains(t, calls[0].envFromCtx, "YAAD_GMAIL_ACCOUNT=ops@example.test")
	assertEnvContains(t, calls[1].envFromCtx, "YAAD_GMAIL_ACCOUNT=work@example.test")

	// Cross-check: neither instance saw the OTHER instance's env
	// (silent mis-attribution would manifest as both calls seeing
	// the same value).
	assertEnvNotContains(t, calls[0].envFromCtx, "YAAD_GMAIL_ACCOUNT=work@example.test")
	assertEnvNotContains(t, calls[1].envFromCtx, "YAAD_GMAIL_ACCOUNT=ops@example.test")

	// Per-instance YAAD_PLUGIN_CONFIG MUST carry the instance's
	// Config payload (built via config.PluginConfigEnv at dispatch
	// time). The exact JSON shape includes the `_name` daemon-
	// injection + the operator's Config keys.
	assertEnvHasPrefixWithSubstring(t, calls[0].envFromCtx, "YAAD_PLUGIN_CONFIG=", `"account":"ops@example.test"`)
	assertEnvHasPrefixWithSubstring(t, calls[1].envFromCtx, "YAAD_PLUGIN_CONFIG=", `"account":"work@example.test"`)
}

func assertEnvContains(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	assert.Failf(t, "env entry missing", "want %q in %v", want, env)
}

func assertEnvNotContains(t *testing.T, env []string, unwanted string) {
	t.Helper()
	for _, e := range env {
		if e == unwanted {
			assert.Failf(t, "env entry leaked", "unwanted %q present in %v", unwanted, env)
			return
		}
	}
}

func assertEnvHasPrefixWithSubstring(t *testing.T, env []string, prefix, substr string) {
	t.Helper()
	for _, e := range env {
		if strings.HasPrefix(e, prefix) && strings.Contains(e, substr) {
			return
		}
	}
	assert.Failf(t, "env entry missing prefix+substring combo",
		"want entry starting with %q containing %q in %v", prefix, substr, env)
}

// TestCommandFanOut_EnvOnlyInstance_YAADPluginConfigPresent pins
// the ADR-0028 §1 env-only instance shape (gmail-style: env-only
// instance with no config: block) — buildInstanceEnv must emit
// YAAD_PLUGIN_CONFIG unconditionally so the daemon-injected
// `_name` field reaches the plugin. Skipping the call when
// Config is empty would strip YAAD_PLUGIN_CONFIG entirely and
// plugins that read `_name` (yaad-bgg, yaad-wikipedia, yaad-
// github) would lose daemon identity for those instances.
func TestCommandFanOut_EnvOnlyInstance_YAADPluginConfigPresent(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	var (
		mu          sync.Mutex
		capturedEnv []string
	)

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		StreamFunc: func(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
			mu.Lock()
			capturedEnv = plugins.ExtraEnvFromContext(ctx)
			mu.Unlock()
			return onEnvelope(&plugins.FetchResult{
				Entity: &store.Entity{
					ID:   "gmail:env-only",
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

	// Single instance with ONLY env (no config block) — the
	// gmail-style shape from ADR-0028 §1.
	instances := []config.InstanceEntry{
		{
			Name: "personal",
			Env:  map[string]string{"YAAD_GMAIL_ACCOUNT": "ops@example.test"},
		},
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, registry,
		WithPluginInstances(map[string][]string{"gmail": {"personal"}}),
		WithPluginInstanceConfigs(map[string][]config.InstanceEntry{"gmail": instances}),
	)

	rec := postIngest(t, h, map[string]any{
		"url":          "gmail/personal: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, capturedEnv, "env-only instance must still produce extraEnv")
	// YAAD_PLUGIN_CONFIG MUST be present even when instance.Config
	// is empty — the daemon-injected `_name` field always lands.
	assertEnvHasPrefixWithSubstring(t, capturedEnv, "YAAD_PLUGIN_CONFIG=", `"_name":"gmail"`)
	// And the env-only entry from InstanceEntry.Env reaches the
	// subprocess too.
	assertEnvContains(t, capturedEnv, "YAAD_GMAIL_ACCOUNT=ops@example.test")
}

// --- ADR-0028 Cut 5: enabled flag ---

func boolPtr(b bool) *bool { return &b }

// TestCommandFanOut_EnabledFalse_FanOutSkipsDisabled pins ADR-0028
// §7 (Cut 5): disabled instances are invisible to the bare-plugin
// fan-out walk. Two enabled + one disabled → only the 2 enabled
// instances spawn; the disabled one is absent from the aggregate.
func TestCommandFanOut_EnabledFalse_FanOutSkipsDisabled(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal"},
		{Name: "work", Enabled: boolPtr(false)},
		{Name: "ops"},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	assert.Equal(t, int32(2), spawns.Load(),
		"disabled instance must be skipped — only 2 of 3 spawn")

	var resp ingestFanOutResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Result, 2, "aggregate must omit the disabled instance")
	got := []string{resp.Result[0].Instance, resp.Result[1].Instance}
	assert.Equal(t, []string{"personal", "ops"}, got,
		"enabled instances surface in declaration order, disabled filtered out")
}

// TestCommandFanOut_EnabledFalse_InstanceScopedRejects pins that
// the explicit `<plugin>/<disabled-instance>: !<cmd>` form rejects
// with 400 instance_disabled — operator turned the target off
// deliberately; the daemon surfaces that explicitly rather than
// silently routing to another instance.
func TestCommandFanOut_EnabledFalse_InstanceScopedRejects(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal"},
		{Name: "work", Enabled: boolPtr(false)},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail/work: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "instance_disabled")
	assert.Contains(t, rec.Body.String(), "work")
	assert.Equal(t, int32(0), spawns.Load(),
		"disabled-instance rejection must skip the subprocess spawn")
}

// TestCommandFanOut_EnabledFalse_AllDisabled_NoEnabledInstances pins
// the rare-but-real case: all instances disabled. Bare-plugin
// dispatch returns `no_enabled_instances` (operator's invocation
// hits a fully-off plugin); per-instance fan-out doesn't run.
func TestCommandFanOut_EnabledFalse_AllDisabled_NoEnabledInstances(t *testing.T) {
	t.Parallel()
	h, spawns := newFanOutFixture(t, []config.InstanceEntry{
		{Name: "personal", Enabled: boolPtr(false)},
		{Name: "work", Enabled: boolPtr(false)},
	})
	rec := postIngest(t, h, map[string]any{
		"url":          "gmail: !fetch",
		"wait_seconds": 1,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no_enabled_instances")
	assert.Equal(t, int32(0), spawns.Load(),
		"all-disabled state must not spawn any subprocess")
}
