package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
)

// fakePluginEnv mirrors the env-controlled fake-plugin pattern from
// internal/plugins/subprocess. Setting this var on a child process
// invocation of THIS test binary makes it behave as a plugin
// (responding to --version + --init) instead of running tests.
const fakePluginEnv = "YAAD_TEST_FAKE_PLUGIN_MAIN"

const (
	fakeModeOKv1 = "ok-v1" // --version → "0.1.0"; --init → caps with version=0.1.0
	fakeModeOKv2 = "ok-v2" // --version → "0.2.0"; --init → caps with version=0.2.0
	fakeModeNoVer = "no-ver" // --version → exits non-zero (probe fails)
	fakeModeBadCtor = "bad-ctor" // --version → "0.1.0"; --init → caps with malformed url_patterns regex
	fakeModeHashOld = "hash-old" // --version → "v0.1.0+oldhash"; --init → caps with version=v0.1.0+oldhash (alice2-index)
	fakeModeHashNewSameTag = "hash-new-same-tag" // --version → "v0.1.0+newhash"; --init → caps with version=v0.1.0+newhash — same tag, different build hash
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakePluginEnv); mode != "" {
		runFakePluginMain(mode)
		return
	}
	os.Exit(m.Run())
}

func runFakePluginMain(mode string) {
	args := os.Args
	switch {
	case len(args) >= 2 && args[1] == "--version":
		switch mode {
		case fakeModeOKv1, fakeModeBadCtor:
			_, _ = fmt.Fprintln(os.Stdout, "0.1.0")
			os.Exit(0)
		case fakeModeOKv2:
			_, _ = fmt.Fprintln(os.Stdout, "0.2.0")
			os.Exit(0)
		case fakeModeNoVer:
			_, _ = fmt.Fprintln(os.Stderr, "fake plugin: --version not supported")
			os.Exit(2)
		case fakeModeHashOld:
			_, _ = fmt.Fprintln(os.Stdout, "v0.1.0+oldhash")
			os.Exit(0)
		case fakeModeHashNewSameTag:
			_, _ = fmt.Fprintln(os.Stdout, "v0.1.0+newhash")
			os.Exit(0)
		}
	case len(args) >= 2 && args[1] == "--init":
		switch mode {
		case fakeModeOKv1, fakeModeNoVer:
			caps := map[string]any{
				"name": "fake",
				"version": "0.1.0",
				"url_patterns": []string{`^https?://example\.test/.*`},
				"entity_kinds": []any{},
			}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		case fakeModeOKv2:
			caps := map[string]any{
				"name": "fake",
				"version": "0.2.0",
				"url_patterns": []string{`^https?://example\.test/.*`},
				"entity_kinds": []any{},
			}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		case fakeModeBadCtor:
			caps := map[string]any{
				"name": "fake",
				"version": "0.1.0",
				"url_patterns": []string{`(invalid-unclosed`},
				"entity_kinds": []any{},
			}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		case fakeModeHashOld, fakeModeHashNewSameTag:
			version := "v0.1.0+oldhash"
			if mode == fakeModeHashNewSameTag {
				version = "v0.1.0+newhash"
			}
			caps := map[string]any{
				"name": "fake",
				"version": version,
				"url_patterns": []string{`^https?://example\.test/.*`},
				"entity_kinds": []any{},
			}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
	}
	_, _ = fmt.Fprintf(os.Stderr, "fake plugin: unknown mode %q args %v\n", mode, args)
	os.Exit(99)
}

// captureLogger wires a slog logger into a bytes.Buffer so per-test
// assertions can decode the JSON line stream and assert level + key
// fields per path. Returns the logger + the buffer + a decoder
// helper that splits and parses the lines.
func captureLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

// logRecord is the parsed shape of one slog JSON line. Only the
// fields the tests assert on are surfaced; the rest stays in Raw
// for ad-hoc checks.
type logRecord struct {
	Level string `json:"level"`
	Msg string `json:"msg"`
	Name string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
	Err string `json:"err,omitempty"`
	Cached string `json:"cached_version,omitempty"`
	Probed string `json:"probed_version,omitempty"`
	Hits int `json:"cache_hits,omitempty"`
	Misses int `json:"cache_misses,omitempty"`
	Fails int `json:"cache_failures,omitempty"`
	Failed []string `json:"failed_plugins,omitempty"`
	NumReg int `json:"registered,omitempty"`
	Raw map[string]any `json:"-"`
}

func parseLogLines(t *testing.T, buf *bytes.Buffer) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec logRecord
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "decode log line: %q", line)
		_ = json.Unmarshal([]byte(line), &rec.Raw)
		out = append(out, rec)
	}
	return out
}

func findLog(records []logRecord, msgSubstr string) *logRecord {
	for i, r := range records {
		if strings.Contains(r.Msg, msgSubstr) {
			return &records[i]
		}
	}
	return nil
}

func newFakePluginConfig(t *testing.T, mode string) *config.Config {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(fakePluginEnv, mode)
	return &config.Config{
		Plugins: []config.PluginEntry{
			{Name: "fake", Path: exe},
		},
	}
}

func newSeededStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRegisterPlugin_FirstStart_INFOMiss(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)
	cfg := newFakePluginConfig(t, fakeModeOKv1)

	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	miss := findLog(logs, "no cached capabilities")
	require.NotNil(t, miss, "first-start path must emit the no-cache INFO; got logs=%+v", logs)
	assert.Equal(t, "INFO", miss.Level, "first-start log is operator-routine, not warn")
	assert.Equal(t, "fake", miss.Name)

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 0, summary.Hits)
	assert.Equal(t, 1, summary.Misses)
	assert.Equal(t, 0, summary.Fails)
	assert.Empty(t, summary.Failed)
}

func TestRegisterPlugin_VersionChanged_INFOMiss(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)
	require.NoError(t, st.UpsertPluginCapabilities(context.Background(),
		"fake", "0.1.0", []byte(`{"version":"0.1.0"}`)))
	cfg := newFakePluginConfig(t, fakeModeOKv2) // probes 0.2.0 — mismatches cached 0.1.0

	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	verChange := findLog(logs, "plugin version changed")
	require.NotNil(t, verChange, "version-changed path must emit the bump INFO")
	assert.Equal(t, "INFO", verChange.Level, "version bump is operator-routine, not warn")
	assert.Equal(t, "0.1.0", verChange.Cached)
	assert.Equal(t, "0.2.0", verChange.Probed)

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 1, summary.Misses, "version-change counts as a miss in the summary tally")
}

func TestRegisterPlugin_CacheHit_NoExtraLog(t *testing.T) {
	// First pass: prime the cache via a real --init.
	logger, _ := captureLogger(t)
	st := newSeededStore(t)
	cfg := newFakePluginConfig(t, fakeModeOKv1)
	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	// Second pass: same store, same plugin, same version → cache hit.
	logger2, buf2 := captureLogger(t)
	_, err = buildPluginRegistry(logger2, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf2)
	for _, r := range logs {
		assert.NotEqual(t, "WARN", r.Level, "cache-hit path must not WARN: %+v", r)
		assert.NotEqual(t, "ERROR", r.Level, "cache-hit path must not ERROR: %+v", r)
	}
	registered := findLog(logs, "plugin registered")
	require.NotNil(t, registered)
	assert.Equal(t, "cache", registered.Source, "cache-hit `plugin registered` carries source=cache")

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 1, summary.Hits, "second pass: 1 hit")
	assert.Equal(t, 0, summary.Misses)
	assert.Equal(t, 0, summary.Fails)
}

// TestRegisterPlugin_HashOnlyChange_CacheStillHits pins alice2-index
//'s hash-strip contract: when --version output's tag prefix
// is unchanged but the build hash differs (typical of a rebuild
// that didn't bump the semver tag), the daemon's cache compare
// MUST treat both as the same cache key. Otherwise every rebuild
// would invalidate the cache even though Capabilities haven't
// changed — the wrong granularity per the issue's framing.
//
// First pass primes the cache with v0.1.0+oldhash. Second pass
// runs the same plugin with v0.1.0+newhash. Cache hits; no --init
// re-run; the registered-source is "cache".
func TestRegisterPlugin_HashOnlyChange_CacheStillHits(t *testing.T) {
	st := newSeededStore(t)
	logger, _ := captureLogger(t)
	cfg := newFakePluginConfig(t, fakeModeHashOld)
	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	// Switch to the new-hash mode. Same tag prefix (v0.1.0); only
	// the +<hash> suffix differs.
	logger2, buf2 := captureLogger(t)
	cfg2 := newFakePluginConfig(t, fakeModeHashNewSameTag)
	_, err = buildPluginRegistry(logger2, st, cfg2)
	require.NoError(t, err)

	logs := parseLogLines(t, buf2)
	registered := findLog(logs, "plugin registered")
	require.NotNil(t, registered)
	assert.Equal(t, "cache", registered.Source,
		"hash-only change must produce a cache hit per alice2-index")

	versionChanged := findLog(logs, "plugin version changed")
	assert.Nil(t, versionChanged,
		"hash-only change must NOT trigger the version-changed re-init log path")

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 1, summary.Hits, "second pass: 1 hit (hash-only change is a cache hit)")
	assert.Equal(t, 0, summary.Misses)
}

// TestRegisterPlugin_TagBumpInvalidates ensures the dual: when the
// SEMVER tag actually moves (not just the hash), cache misses and
// --init re-runs. Otherwise alice2-index's cache key would be
// too sticky.
func TestRegisterPlugin_TagBumpInvalidates(t *testing.T) {
	st := newSeededStore(t)
	logger, _ := captureLogger(t)
	cfg := newFakePluginConfig(t, fakeModeHashOld) // primes cache with v0.1.0+oldhash
	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	// fakeModeOKv2 emits "0.2.0" (no v prefix, no hash) — different
	// tag prefix from cached v0.1.0+oldhash → cache key changes.
	logger2, buf2 := captureLogger(t)
	cfg2 := newFakePluginConfig(t, fakeModeOKv2)
	_, err = buildPluginRegistry(logger2, st, cfg2)
	require.NoError(t, err)

	logs := parseLogLines(t, buf2)
	versionChanged := findLog(logs, "plugin version changed")
	require.NotNil(t, versionChanged,
		"tag bump must trigger the version-changed re-init log path")
}

func TestRegisterPlugin_MalformedCachedJSON_ERROR(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)
	require.NoError(t, st.UpsertPluginCapabilities(context.Background(),
		"fake", "0.1.0", []byte(`{not-actually-json`)))
	cfg := newFakePluginConfig(t, fakeModeOKv1)

	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	bad := findLog(logs, "cached caps malformed")
	require.NotNil(t, bad, "malformed-JSON path must emit the corrupt-cache ERROR")
	assert.Equal(t, "ERROR", bad.Level, "corrupt cached row is operator-actionable, not noise")
	assert.NotEmpty(t, bad.Err)
	assert.Equal(t, "fake", bad.Name)

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 1, summary.Fails, "malformed JSON counts as a failure in the summary tally")
	assert.Equal(t, []string{"fake"}, summary.Failed)
}

func TestRegisterPlugin_BadCachedRegex_ERROR(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)
	// Cache row's caps point at a malformed url_patterns regex —
	// NewWithCapabilities will fail to compile it.
	badCaps := []byte(`{"name":"fake","version":"0.1.0","url_patterns":["(invalid-unclosed"],"entity_kinds":[]}`)
	require.NoError(t, st.UpsertPluginCapabilities(context.Background(),
		"fake", "0.1.0", badCaps))
	cfg := newFakePluginConfig(t, fakeModeOKv1) // probes 0.1.0 → version match → tries cache hit

	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	bad := findLog(logs, "Plugin construction from cached caps failed")
	require.NotNil(t, bad, "ctor-failure path must emit the corrupt-cache ERROR")
	assert.Equal(t, "ERROR", bad.Level)

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 1, summary.Fails)
	assert.Equal(t, []string{"fake"}, summary.Failed)
}

func TestRegisterPlugin_ProbeFailure_INFOMiss(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)
	cfg := newFakePluginConfig(t, fakeModeNoVer)

	_, err := buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	probeFail := findLog(logs, "plugin version probe failed")
	require.NotNil(t, probeFail,
		"probe-failure path must emit the INFO breadcrumb; got logs=%+v", logs)
	assert.Equal(t, "INFO", probeFail.Level,
		"probe failure is operator-routine, not an alarm")
	assert.Equal(t, "fake", probeFail.Name)
	assert.NotEmpty(t, probeFail.Err,
		"probe-failure log should surface the underlying err so operators can grep")
	assert.Empty(t, probeFail.Probed,
		"probe-failure path: probed_version is empty when --version didn't return")

	assert.Nil(t, findLog(logs, "no cached capabilities"),
		"the post-probe first-start INFO must NOT fire on probe-failure — cache lookup never runs")

	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 0, summary.Hits)
	assert.Equal(t, 1, summary.Misses,
		"probe-failure lumps with first-start in the summary tally (cache wasn't consultable)")
	assert.Equal(t, 0, summary.Fails,
		"probe failure is a miss, not a failure — failures are reserved for cache-row corruption")
	assert.Empty(t, summary.Failed)
}

func TestBuildPluginRegistry_NoPlugins_NoSummary(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)

	_, err := buildPluginRegistry(logger, st, &config.Config{Plugins: nil})
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	assert.Nil(t, findLog(logs, "plugin cache summary"),
		"no plugins → no summary line (don't spam empty)")
}

func TestBuildPluginRegistry_SummaryAcrossMixedPlugins(t *testing.T) {
	logger, buf := captureLogger(t)
	st := newSeededStore(t)

	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(fakePluginEnv, fakeModeOKv1)

	// Prime the cache for one plugin so it lands as a hit, leave the
	// other to a first-start miss. Both share the same fake binary
	// (the env mode only differs across processes; within a single
	// buildPluginRegistry call all subprocess invocations inherit
	// the same env). For this mixed-tally test we use a single
	// fake-mode that round-trips cleanly; the prior cache row makes
	// one a hit, the missing row makes the other a miss.
	require.NoError(t, st.UpsertPluginCapabilities(context.Background(),
		"primed", "0.1.0",
		[]byte(`{"name":"fake","version":"0.1.0","url_patterns":["^https?://example\\.test/.*"],"entity_kinds":[]}`)))

	cfg := &config.Config{
		Plugins: []config.PluginEntry{
			{Name: "primed", Path: exe},
			{Name: "fresh", Path: exe},
		},
	}

	_, err = buildPluginRegistry(logger, st, cfg)
	require.NoError(t, err)

	logs := parseLogLines(t, buf)
	summary := findLog(logs, "plugin cache summary")
	require.NotNil(t, summary)
	assert.Equal(t, 1, summary.Hits, "primed plugin → cache hit")
	assert.Equal(t, 1, summary.Misses, "fresh plugin → first-start miss")
	assert.Equal(t, 0, summary.Fails)
	assert.Equal(t, 2, summary.NumReg)
}
