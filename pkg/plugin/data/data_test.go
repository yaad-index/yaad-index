package data

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDataDir_ReadsEnv pins that DataDir() returns the
// YAAD_PLUGIN_DATA_DIR value verbatim when set.
//
// t.Setenv-using tests intentionally don't call t.Parallel —
// Setenv panics under parallel because process env is shared
// global mutable state.
func TestDataDir_ReadsEnv(t *testing.T) {
	t.Setenv(EnvDataDir, "/var/lib/yaad-test/plugin/instance")
	assert.Equal(t, "/var/lib/yaad-test/plugin/instance", DataDir())
}

// TestDataDir_EmptyWhenUnset pins the fallback: callers see an
// empty string (DefaultDataDir) when the daemon isn't providing
// a dir. Plugins MUST check for this case before writing durable
// state.
func TestDataDir_EmptyWhenUnset(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	assert.Equal(t, DefaultDataDir, DataDir())
	assert.Empty(t, DataDir())
}
