// Dispatch CLI subcommands (per alice2-index + ADR-0022 §6):
//
// - `alice2-index command <plugin> <cmd>` — dispatches a command-shape
// invocation. Concatenates args into `<plugin>: !<cmd>` and POSTs
// to the running daemon's /v1/ingest endpoint as the `url` field.
//
// - `alice2-index fetch <plugin> <pattern>` — dispatches a URL-shape
// invocation. Concatenates args into `<plugin>: <pattern>` and
// POSTs to /v1/ingest.
//
// Both subcommands are typed wrappers around the same wire format the
// daemon already accepts. No new endpoint, no new request shape — the
// canonical single-string input shape `<plugin>: <suffix>` flows
// through. The daemon discriminates command-shape via the `!` sigil
// (per ADR-0022 §2 + the parser landed in).
//
// Auth: the `--token` flag carries a Bearer JWT in the Authorization
// header. The daemon-side operator-only-claim enforcement lands in
// — until then the CLI passes whatever token the operator supplies
// through unmodified, and a daemon running with auth disabled accepts
// the request without inspecting it.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// dispatchHTTPTimeout caps the CLI's HTTP-client wall-clock. Set
// well above dispatchDefaultWaitSeconds so a long-poll up to the
// daemon's max (300s per ADR-0002) doesn't trip the client. The
// daemon enforces its own bound; this is just a CLI-side
// belt-and-suspenders to bound a hung daemon.
const dispatchHTTPTimeout = 6 * time.Minute

// CommandCmd implements `alice2-index command <plugin> <cmd>`.
//
// Concatenates positional args into `<plugin>: !<cmd>` and POSTs to
// the daemon. Per ADR-0022 §6 and the post- parser contract, the
// daemon recognizes the `!` sigil + routes to a command-dispatch
// path (validation today via; full job system Per the prior design,).
//
// External cron uses this shape to schedule polling commands like
// `alice2-index command gmail fetch`.
type CommandCmd struct {
	Plugin string `arg:"" help:"plugin name (must match the plugin's --init Capabilities.Name and be allowlisted in config)."`
	Command string `arg:"" help:"command name (must match an entry in the plugin's Capabilities.Commands list)."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running alice2-index daemon."`
	Token string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate. MUST be an operator-only token (Subject == Operator) per alice2-index / ADR-0022 §6 — issue with 'alice2-index issue-token --operator-only --operator <name>'. Pair-claim tokens reject 403."`
	Wait int `name:"wait" default:"60" help:"long-poll wait_seconds; matches the daemon's /v1/ingest field. 0 → async-only mode; the call returns the queued shape immediately."`
}

// Run executes the command-shape dispatch.
func (c *CommandCmd) Run() error {
	if strings.TrimSpace(c.Plugin) == "" {
		return errors.New("plugin name is required")
	}
	if strings.TrimSpace(c.Command) == "" {
		return errors.New("command name is required")
	}
	input := fmt.Sprintf("%s: !%s", c.Plugin, c.Command)
	return runDispatch(c.DaemonURL, c.Token, input, c.Wait, os.Stdout)
}

// FetchCmd implements `alice2-index fetch <plugin> <pattern>`.
//
// Concatenates positional args into `<plugin>: <pattern>` and POSTs
// to the daemon. URL-shape ingest — same shape an agent would POST
// directly. This is the operator's manual-fetch surface for plugins
// that have URL-pattern dispatch (yaad-wikipedia, yaad-bgg).
//
// Pattern is captured as a single positional arg; operators with
// multi-token patterns (e.g. "ticket to ride") quote them at the
// shell.
type FetchCmd struct {
	Plugin string `arg:"" help:"plugin name (must match a registered plugin's Capabilities.Name)."`
	Pattern string `arg:"" help:"URL or shorthand pattern the plugin's url_patterns accept. Quote multi-token patterns at the shell."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running alice2-index daemon."`
	Token string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate (see command's --token; same deferral)."`
	Wait int `name:"wait" default:"60" help:"long-poll wait_seconds. 0 → async-only mode."`
}

// Run executes the URL-shape dispatch.
func (c *FetchCmd) Run() error {
	if strings.TrimSpace(c.Plugin) == "" {
		return errors.New("plugin name is required")
	}
	if strings.TrimSpace(c.Pattern) == "" {
		return errors.New("pattern is required")
	}
	input := fmt.Sprintf("%s: %s", c.Plugin, c.Pattern)
	return runDispatch(c.DaemonURL, c.Token, input, c.Wait, os.Stdout)
}

// runDispatch is the shared HTTP path: marshal the request body, POST
// to <daemon>/v1/ingest, decode + render the response. Surfaces the
// daemon's HTTP status code in the returned error so cron / shell
// scripts can branch on success vs. failure via the exit code (kong
// translates a non-nil Run() error into exit 1).
func runDispatch(daemonURL, token, input string, waitSeconds int, stdout io.Writer) error {
	body, err := json.Marshal(map[string]any{
		"url": input,
		"wait_seconds": waitSeconds,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	endpoint := strings.TrimRight(daemonURL, "/") + "/v1/ingest"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: dispatchHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Pretty-print the response body verbatim — the daemon already
	// emits structured JSON envelopes (complete / queued / needs_fill /
	// disambiguation / error). Operators inspect the JSON shape; cron
	// branches on the exit code below.
	if err := writePrettyJSON(stdout, respBody); err != nil {
		// Fall back to raw body when the response isn't JSON (the
		// daemon's middleware should always emit JSON, but a proxy
		// or LB rejecting earlier could intercept; surfacing the
		// raw bytes beats an opaque "decode failed" message).
		_, _ = fmt.Fprintln(stdout, string(respBody))
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return nil
}

// writePrettyJSON re-marshals body with 2-space indent for human
// readability + writes to dst with a trailing newline. Returns
// non-nil on any decode failure so the caller can surface raw bytes.
func writePrettyJSON(dst io.Writer, body []byte) error {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	out, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if _, err := dst.Write(out); err != nil {
		return err
	}
	_, err = dst.Write([]byte{'\n'})
	return err
}
