// CLI subcommands for the task surface per ADR-0024
// §"Agent surface". v1:
//   - `yaad-index task list [--errored]` — list tasks via
//     GET /v1/tasks.
//   - `yaad-index task load <id>` — load one task with body
//     via GET /v1/tasks/{id}.
//
// task.resolve lands in 6.C (the final Phase 6 cut).

package main

import (
	"fmt"
	"net/url"
)

// TaskCmd implements `yaad-index task <subcommand>`.
type TaskCmd struct {
	List    TaskListCmd    `cmd:"" help:"List workflow-produced tasks (per ADR-0024 §task.list)."`
	Load    TaskLoadCmd    `cmd:"" help:"Load one task's full body + frontmatter (per ADR-0024 §task.load)."`
	Resolve TaskResolveCmd `cmd:"" help:"Mark a task resolved + auto-archive per the workflow's auto_archive_on_done flag (per ADR-0024 §task.resolve)."`
}

// TaskListCmd implements `yaad-index task list`. Optional
// --errored filter routes to GET /v1/tasks?errored=true|false.
type TaskListCmd struct {
	Errored   string `name:"errored" enum:",true,false" default:"" help:"filter by frontmatter 'errored' field. 'true' shows only err-tasks; 'false' shows only normal tasks; omit to show both."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running yaad-index daemon."`
	Token     string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate."`
	Timeout   int    `name:"timeout" default:"30" help:"HTTP request timeout in seconds."`
}

// Run executes the task list CLI subcommand.
func (c *TaskListCmd) Run() error {
	u := c.DaemonURL + "/v1/tasks"
	if c.Errored != "" {
		u += "?errored=" + url.QueryEscape(c.Errored)
	}
	return runWorkflowGet(u, c.Token, c.Timeout)
}

// TaskLoadCmd implements `yaad-index task load <id>`.
type TaskLoadCmd struct {
	ID        string `arg:"" help:"task id (the file basename without .md, e.g. 'classify-boardgame-brass' or 'classify-err')."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running yaad-index daemon."`
	Token     string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate."`
	Timeout   int    `name:"timeout" default:"30" help:"HTTP request timeout in seconds."`
}

// Run executes the task load CLI subcommand.
func (c *TaskLoadCmd) Run() error {
	if c.ID == "" {
		return fmt.Errorf("task id is required")
	}
	return runWorkflowGet(c.DaemonURL+"/v1/tasks/"+url.PathEscape(c.ID), c.Token, c.Timeout)
}

// TaskResolveCmd implements `yaad-index task resolve <id>`.
// Routes to POST /v1/tasks/{id}/resolve.
type TaskResolveCmd struct {
	ID        string `arg:"" help:"task id (the file basename without .md)."`
	DaemonURL string `name:"daemon" env:"YAAD_INDEX_DAEMON_URL" default:"http://localhost:7433" help:"base URL of the running yaad-index daemon."`
	Token     string `name:"token" env:"YAAD_INDEX_TOKEN" help:"Bearer JWT for the daemon's auth gate."`
	Timeout   int    `name:"timeout" default:"30" help:"HTTP request timeout in seconds."`
}

// Run executes the task resolve CLI subcommand.
func (c *TaskResolveCmd) Run() error {
	if c.ID == "" {
		return fmt.Errorf("task id is required")
	}
	return runWorkflowPost(c.DaemonURL+"/v1/tasks/"+url.PathEscape(c.ID)+"/resolve", nil, c.Token, c.Timeout)
}
