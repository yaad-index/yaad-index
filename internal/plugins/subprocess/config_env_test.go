package subprocess

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// TestPlugin_EnvNoConfigMatchesGlobalOnly pins the no-config case:
// a Plugin without WithConfigEnv returns pluginEnv() unchanged. No
// extra entries, no allocations beyond the base.
func TestPlugin_EnvNoConfigMatchesGlobalOnly(t *testing.T) {
	t.Parallel()

	p := &Plugin{name: "test"}
	got := p.envFor(context.Background())
	assert.Equal(t, pluginEnv(), got,
		"Plugin without config env must match the global pluginEnv() verbatim")
}

// TestPlugin_EnvWithConfigAppendsAfterGlobals pins the order: per-
// plugin config-env entries land AFTER the global env entries so
// they override shell-env values with the same name (Go's
// exec.Cmd.Env is last-write-wins via the runtime). yaad-index #7
// makes the operator-yaml authoritative when both are set.
func TestPlugin_EnvWithConfigAppendsAfterGlobals(t *testing.T) {
	t.Parallel()

	configEnv := []string{
		"BGG_API_KEY=from-config",
		"BGG_TIMEOUT_SECONDS=30",
	}
	p := &Plugin{name: "bgg", configEnv: configEnv}

	got := p.envFor(context.Background())
	base := pluginEnv()
	require := assert.New(t)
	require.Equal(len(base)+len(configEnv), len(got),
		"per-plugin env appends to global env without dropping entries")
	require.Equal(base, got[:len(base)],
		"global env entries must come first")
	require.Equal(configEnv, got[len(base):],
		"per-plugin entries land at the tail")
}

// TestWithConfigEnv_OverridesPriorCall pins the contract that
// repeat calls to WithConfigEnv replace rather than append — the
// daemon constructs Plugin once per operator yaml entry; multiple
// option passes should not accumulate.
func TestWithConfigEnv_OverridesPriorCall(t *testing.T) {
	t.Parallel()

	p := &Plugin{name: "bgg"}
	WithConfigEnv([]string{"BGG_API_KEY=first"})(p)
	WithConfigEnv([]string{"BGG_API_KEY=second"})(p)

	assert.Equal(t, []string{"BGG_API_KEY=second"}, p.configEnv,
		"repeated WithConfigEnv replaces; doesn't accumulate")
}

// TestWithConfigEnv_NilOrEmptyClears pins the nil/empty input: a
// WithConfigEnv(nil) call clears any prior config-env on the
// Plugin. Helps the daemon reset between reload paths.
func TestWithConfigEnv_NilOrEmptyClears(t *testing.T) {
	t.Parallel()

	p := &Plugin{name: "bgg", configEnv: []string{"BGG_API_KEY=keep"}}
	WithConfigEnv(nil)(p)
	assert.Empty(t, p.configEnv, "WithConfigEnv(nil) must clear prior entries")
}

// TestPlugin_EnvForSplicesPerCallExtraEnv pins the ADR-0028 §3 +
// §4 (Cut 4) contract: the per-call ExtraEnv stamped into ctx
// via plugins.WithExtraEnv lands AFTER the per-plugin configEnv
// in cmd.Env so per-call values override registered values on
// duplicate keys. Mirrors the registered-overrides-shell
// precedence the legacy configEnv had over shell env.
func TestPlugin_EnvForSplicesPerCallExtraEnv(t *testing.T) {
	t.Parallel()

	configEnv := []string{"BGG_API_KEY=registered"}
	p := &Plugin{name: "bgg", configEnv: configEnv}

	ctx := plugins.WithExtraEnv(context.Background(), []string{
		"BGG_API_KEY=per-call-override", // duplicate key → per-call wins
		"YAAD_PLUGIN_CONFIG={\"_name\":\"bgg\"}",
	})
	got := p.envFor(ctx)

	// Per-call entries must appear AFTER registered configEnv so
	// exec.Cmd.Env's last-wins-on-dup rule resolves to the
	// per-call value.
	registeredIdx := indexOf(got, "BGG_API_KEY=registered")
	overrideIdx := indexOf(got, "BGG_API_KEY=per-call-override")
	require.GreaterOrEqual(t, registeredIdx, 0, "registered entry missing from env: %v", got)
	require.GreaterOrEqual(t, overrideIdx, 0, "per-call override missing from env: %v", got)
	assert.Greater(t, overrideIdx, registeredIdx,
		"per-call ExtraEnv must land AFTER registered configEnv for exec last-wins precedence")

	// YAAD_PLUGIN_CONFIG (per-call only — registered configEnv
	// didn't include it) must be present.
	assert.GreaterOrEqual(t, indexOf(got, "YAAD_PLUGIN_CONFIG={\"_name\":\"bgg\"}"), 0,
		"per-call YAAD_PLUGIN_CONFIG must reach the env slice")
}

// TestPlugin_EnvForNoExtraEqualsLegacyEnv pins back-compat: a
// nil-extra-env ctx produces the same env as the pre-Cut-4
// p.env() (which envFor replaces) — pluginEnv() + configEnv.
func TestPlugin_EnvForNoExtraEqualsLegacyEnv(t *testing.T) {
	t.Parallel()

	configEnv := []string{"BGG_API_KEY=registered"}
	p := &Plugin{name: "bgg", configEnv: configEnv}

	got := p.envFor(context.Background())
	expected := append(pluginEnv(), configEnv...)
	assert.Equal(t, expected, got,
		"envFor with no ExtraEnv must match legacy pluginEnv()+configEnv shape")
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}
