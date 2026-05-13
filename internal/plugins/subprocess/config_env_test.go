package subprocess

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPlugin_EnvNoConfigMatchesGlobalOnly pins the no-config case:
// a Plugin without WithConfigEnv returns pluginEnv() unchanged. No
// extra entries, no allocations beyond the base.
func TestPlugin_EnvNoConfigMatchesGlobalOnly(t *testing.T) {
	t.Parallel()

	p := &Plugin{name: "test"}
	got := p.env()
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

	got := p.env()
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
