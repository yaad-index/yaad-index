// CLI subcommands for the workflow engine per ADR-0024
// §"Agent surface". Currently:
//   - `yaad-index workflow trigger <name> [input]` — manual
//     workflow trigger via the daemon's POST /v1/workflows/
//     trigger endpoint.
//
// Future v1.x additions (`list` / `discover`) wire to the
// MCP / HTTP surface defined in the ADR. The Trigger subcommand
// is the load-bearing one — external host cron uses it for
// time-based shapes (daily-summary, weekly-roll-up).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// WorkflowCmd implements `yaad-index workflow <subcommand>`.
// Mounted at the kong tree root alongside Plugins / Cache.
type WorkflowCmd struct {
	List     WorkflowListCmd     `cmd:"" help:"List registered workflows (per ADR-0024 §workflow.list)."`
	Discover WorkflowDiscoverCmd `cmd:"" help:"List workflows that match a given entity (per ADR-0024 §workflow.discover)."`
	Trigger  WorkflowTriggerCmd  `cmd:"" help:"Manually trigger a registered workflow (per ADR-0024 §workflow.trigger)."`
}

// WorkflowListCmd implements `yaad-index workflow list`.
// Hits GET /v1/workflows + pretty-prints the JSON envelope.
type WorkflowListCmd struct {
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running yaad-index daemon."`
	Token     string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate."`
	Timeout   int    `name:"timeout" default:"30" help:"HTTP request timeout in seconds."`
}

// Run executes the workflow list CLI subcommand.
func (c *WorkflowListCmd) Run() error {
	return runWorkflowGet(c.DaemonURL+"/v1/workflows", c.Token, c.Timeout)
}

// WorkflowDiscoverCmd implements `yaad-index workflow
// discover <entity-id>`. Hits GET /v1/workflows/discover?
// entity=<id> + pretty-prints the JSON envelope.
type WorkflowDiscoverCmd struct {
	EntityID  string `arg:"" help:"entity id (<kind>:<slug>) to discover matching workflows against."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running yaad-index daemon."`
	Token     string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate."`
	Timeout   int    `name:"timeout" default:"30" help:"HTTP request timeout in seconds."`
}

// Run executes the workflow discover CLI subcommand.
func (c *WorkflowDiscoverCmd) Run() error {
	u := c.DaemonURL + "/v1/workflows/discover?entity=" + url.QueryEscape(c.EntityID)
	return runWorkflowGet(u, c.Token, c.Timeout)
}

// runWorkflowGet is the shared GET helper for the list +
// discover CLI subcommands. Builds the request, sends, and
// pretty-prints the response body to stdout. Non-zero exit
// on transport / auth / 4xx-5xx errors.
func runWorkflowGet(reqURL, token string, timeoutSec int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch to %s: %w", reqURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(out))
	}
	fmt.Println(string(out))
	return nil
}

// WorkflowTriggerCmd implements `yaad-index workflow trigger
// <name> [input]`. Dispatches to POST /v1/workflows/trigger
// against the running daemon. Empty input is allowed for
// trigger.type=manual workflows; entity-id input is the
// common shape; URL routing is a Phase 3.C+ daemon follow-up.
type WorkflowTriggerCmd struct {
	Name      string `arg:"" help:"workflow name (matches the frontmatter 'name' on the workflow file in vault/workflows/)."`
	Input     string `arg:"" optional:"" default:"" help:"input shape per ADR-0024: empty (target-less, manual-only), entity ID (<kind>:<slug>), or URL (URL routing is Phase 3.C+; currently treated as entity-id and missing-id surfaces as MissingRef)."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running yaad-index daemon."`
	Token     string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate."`
	Timeout   int    `name:"timeout" default:"30" help:"HTTP request timeout in seconds."`
}

// Run executes the workflow trigger CLI subcommand: builds
// the POST body, dispatches to /v1/workflows/trigger, and
// pretty-prints the resulting Decision to stdout. Non-zero
// exit on transport / auth / 4xx-5xx errors.
func (c *WorkflowTriggerCmd) Run() error {
	body, err := json.Marshal(map[string]string{
		"name":  c.Name,
		"input": c.Input,
	})
	if err != nil {
		return fmt.Errorf("marshal workflow trigger body: %w", err)
	}

	url := c.DaemonURL + "/v1/workflows/trigger"
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.Timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	client := &http.Client{Timeout: time.Duration(c.Timeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(out))
	}
	fmt.Println(string(out))
	return nil
}
