// Package e2e is the multi-plugin end-to-end test harness
// (yaad-index #1). Builds yaad-index + plugin binaries to a
// tempdir, spawns the daemon against a tempdir vault + sqlite
// store, routes plugin upstream calls through a caller-supplied
// httptest.Server, and exposes an HTTP client for test bodies.
//
// Tests build under `//go:build e2e`; invoke via `make e2e`.

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Harness is the daemon-under-test wrapper. Tests construct via
// Start and drive HTTP requests via Client / BaseURL.
type Harness struct {
	// BaseURL is the http://127.0.0.1:NNNN root the daemon serves on.
	BaseURL string
	// VaultPath is the daemon's vault root for disk-state assertions.
	VaultPath string
	// DBPath is the SQLite file the daemon writes to.
	DBPath string
	// Client has a 30s timeout to fail fast on daemon hangs.
	Client *http.Client

	t          *testing.T
	cmd        *exec.Cmd
	stderr     *strings.Builder
	binDir     string
	pluginBins map[string]string
}

// HarnessConfig is the caller-supplied setup for Start.
type HarnessConfig struct {
	// Plugins names the plugins to build + register. Each entry's
	// Name is the plugin-name AND the cmd/ subdirectory; the
	// binary at `cmd/<Name>/main.go` is built and registered.
	Plugins []PluginConfig
	// CanonicalKinds populates the operator's `canonical_kinds:`
	// map on the daemon config. Empty / nil → no canonical
	// activation.
	CanonicalKinds map[string]CanonicalKindEntry
	// CanonicalEdgeTypes names the edge types the daemon's guard
	// admits. Empty / nil → no canonical edges materialize.
	CanonicalEdgeTypes []string
}

// PluginConfig is one entry under HarnessConfig.Plugins.
type PluginConfig struct {
	// Name is the plugin-name AND the cmd/ subdirectory.
	Name string
	// Env is the per-plugin env-var map. Merged into the daemon's
	// environment before spawn; subprocess plugins inherit it.
	Env map[string]string
}

// CanonicalKindEntry is the minimal `canonical_kinds:` per-kind
// shape the harness writes to config.yaml. Single-string `gaps:`
// map matches the pre-ADR-0016 shorthand which
// config.GapSpec.UnmarshalYAML still accepts.
type CanonicalKindEntry struct {
	Gaps map[string]string `yaml:"gaps,omitempty"`
}

// Start builds the daemon + plugin binaries, writes a config.yaml,
// spawns the daemon, polls readiness, and returns the Harness.
// Setup failures t.Fatal. Teardown registered via t.Cleanup.
func Start(t *testing.T, cfg HarnessConfig) *Harness {
	t.Helper()

	binDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "vault")
	require.NoError(t, os.MkdirAll(vaultPath, 0o755))
	dbPath := filepath.Join(t.TempDir(), "yaad.db")

	// Build the daemon binary.
	daemonBin := filepath.Join(binDir, "yaad-index")
	buildBinary(t, "./cmd/yaad-index", daemonBin)

	// Build each plugin binary the test asked for.
	pluginBins := make(map[string]string, len(cfg.Plugins))
	for _, p := range cfg.Plugins {
		bin := filepath.Join(binDir, p.Name)
		buildBinary(t, "./cmd/"+p.Name, bin)
		pluginBins[p.Name] = bin
	}

	// Allocate a port via net.Listen(:0) + close — the daemon
	// re-binds to it in its boot. Race window is acceptable for
	// a test harness; the daemon retries via http-poll readiness
	// below if the spawn finds the port grabbed.
	port := allocPort(t)
	bindAddr := fmt.Sprintf("127.0.0.1:%d", port)
	baseURL := "http://" + bindAddr

	// Write the operator config.
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, configFile{
		Plugins:            cfg.Plugins,
		PluginBins:         pluginBins,
		VaultPath:          vaultPath,
		CanonicalKinds:     cfg.CanonicalKinds,
		CanonicalEdgeTypes: cfg.CanonicalEdgeTypes,
	})

	// AuthRequired=false bypasses JWT so tests don't mint tokens.
	stderr := &strings.Builder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, daemonBin, "serve",
		"--bind="+bindAddr,
		"--db-path="+dbPath,
		"--config="+configPath,
		"--auth-required=false",
	)
	// Per-plugin env vars flow through subprocess.pluginEnv to
	// each plugin spawn.
	cmd.Env = append(os.Environ(), mergePluginEnv(cfg.Plugins)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start(), "spawn daemon")

	h := &Harness{
		BaseURL:    baseURL,
		VaultPath:  vaultPath,
		DBPath:     dbPath,
		Client:     &http.Client{Timeout: 30 * time.Second},
		t:          t,
		cmd:        cmd,
		stderr:     stderr,
		binDir:     binDir,
		pluginBins: pluginBins,
	}

	require.NoError(t, h.waitReady(ctx, 30*time.Second), "daemon not ready; stderr=\n%s", stderr.String())

	t.Cleanup(func() { h.stop() })

	return h
}

// waitReady polls GET /v1/structure until 200 or timeout.
func (h *Harness) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/v1/structure", nil)
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}
		resp, err := h.Client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon not ready after %s", timeout)
}

// stop SIGTERMs the daemon, waits up to 5s, then SIGKILLs.
// Dumps stderr on test failure. Called from t.Cleanup.
func (h *Harness) stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
	if t := h.t; t != nil && t.Failed() {
		// On a failed test the stderr is invaluable. Dump it.
		t.Logf("daemon stderr:\n%s", h.stderr.String())
	}
}

// PostJSON marshals body as JSON, POSTs to the daemon, returns
// (status, body). Network errors fail the test.
func (h *Harness) PostJSON(path string, body any) (status int, respBody []byte) {
	h.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(h.t, err)
	req, err := http.NewRequest(http.MethodPost, h.BaseURL+path, strings.NewReader(string(raw)))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp.StatusCode, respBody
}

// GetJSON is the GET counterpart to PostJSON.
func (h *Harness) GetJSON(path string) (status int, respBody []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.BaseURL+path, nil)
	require.NoError(h.t, err)
	resp, err := h.Client.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp.StatusCode, respBody
}

// buildBinary compiles `pkg` (a relative import path like
// "./cmd/yaad-index") to `out`.
func buildBinary(t *testing.T, pkg, out string) {
	t.Helper()
	// `go build ./cmd/...` needs to run from the module root, not
	// the e2e/ test directory.
	moduleRoot := findModuleRoot(t)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = moduleRoot
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "build %s: %s", pkg, output)
}

// findModuleRoot walks up from cwd until it finds a go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	for dir := wd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatalf("no go.mod found walking up from %s", wd)
	return ""
}

// allocPort grabs a free TCP port via net.Listen(:0) + close. A
// race window exists between close and daemon bind; waitReady
// catches the rare failure.
func allocPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// mergePluginEnv flattens per-plugin Env maps into a "KEY=VALUE"
// slice ready for cmd.Env append.
func mergePluginEnv(plugins []PluginConfig) []string {
	out := []string{}
	for _, p := range plugins {
		for k, v := range p.Env {
			out = append(out, k+"="+v)
		}
	}
	return out
}
