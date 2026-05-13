package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolvePluginStagingDir_YAMLWinsOverEnv pins the precedence
// chain: operator yaml > env > os.TempDir(). yaml-set value
// short-circuits the env + default branches.
func TestResolvePluginStagingDir_YAMLWinsOverEnv(t *testing.T) {
	t.Parallel()

	got := resolvePluginStagingDir("/operator/yaml/path", "/env/value")
	assert.Equal(t, "/operator/yaml/path", got,
		"yaml value must win over env and default")
}

// TestResolvePluginStagingDir_EnvWinsOverDefault pins the middle
// rung: env wins when yaml is empty. The default (os.TempDir()) is
// the last resort.
func TestResolvePluginStagingDir_EnvWinsOverDefault(t *testing.T) {
	t.Parallel()

	got := resolvePluginStagingDir("", "/env/value")
	assert.Equal(t, "/env/value", got,
		"env value must win when yaml is empty")
}

// TestResolvePluginStagingDir_DefaultIsOSTempDir pins the bottom of
// the chain: when both yaml and env are empty, fall back to
// os.TempDir() (not hardcoded /tmp). os.TempDir() respects $TMPDIR
// and picks containerized / per-user temp roots correctly.
func TestResolvePluginStagingDir_DefaultIsOSTempDir(t *testing.T) {
	t.Parallel()

	got := resolvePluginStagingDir("", "")
	assert.Equal(t, os.TempDir(), got,
		"empty yaml + empty env must fall through to os.TempDir()")
	assert.NotEmpty(t, got, "os.TempDir() always returns a non-empty path")
}

// TestResolvePluginStagingDir_EmptyStringsSkipped pins the empty-string
// handling: empty strings at any layer fall through to the next, they
// don't poison the resolution. Stops a bare `YAAD_PLUGIN_STAGING_DIR=`
// (unset-via-empty) from setting the staging root to "".
func TestResolvePluginStagingDir_EmptyStringsSkipped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		env  string
	}{
		{"both empty", "", ""},
		{"yaml empty env empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolvePluginStagingDir(tc.yaml, tc.env)
			assert.Equal(t, os.TempDir(), got)
		})
	}
}
