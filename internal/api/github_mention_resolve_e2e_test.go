// Verifies the #386 example workflow end to end: a github source entity
// (kind=github) reaching a terminal state fires entity.updated → the example
// `gmail-github-mentions-resolve` workflow's task_resolve action checks
// off the matching line in the gmail-github-mentions task. Proves the
// chain works with existing primitives (no new daemon code) and that
// the committed example file parses + functions.

package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

func TestExampleWorkflow_GithubMentionResolve_EndToEnd(t *testing.T) {
	t.Parallel()

	// Parse the committed example so the test pins the real file.
	exPath := filepath.Join("..", "..", "docs", "examples", "workflows",
		"gmail-github-mentions-resolve.md")
	wf, err := parser.ParseFile(exPath)
	require.NoError(t, err, "the committed #386 example workflow must parse + validate")

	root := t.TempDir()
	taskWriter := actions.NewFileTaskWriter(root, nil, nil, nil, nil, nil)
	runner := actions.New(actions.Options{TaskWriter: taskWriter})

	// Pre-create the gmail-github-mentions task the example resolves into,
	// with a synthetic line format leading with the PR's owner/repo#N ref
	// (the prefix resolveTaskLineInBody matches on).
	taskPath := actions.TaskVaultPath(root, "gmail-github-mentions", "pending")
	require.NoError(t, os.MkdirAll(filepath.Dir(taskPath), 0o755))
	const taskBody = `---
kind: task
workflow: gmail-github-mentions
subject: pending
---

## Open mentions

- [ ] owner/repo#42 — mention surfaced via gmail digest
- [ ] other/repo#99 — a still-open PR that must stay unchecked
`
	require.NoError(t, os.WriteFile(taskPath, []byte(taskBody), 0o644))

	bus := eventbus.NewMemoryBus()
	// The CEL entity is data-nested (entity.data.*). The terminal PR:
	// state=closed, with the url + number the match_key renders from.
	resolver := &triggerFakeResolver{entities: map[string]map[string]any{
		"github:owner-repo-42": {
			"id":   "github:owner-repo-42",
			"kind": "github",
			"data": map[string]any{
				"state":  "closed",
				"url":    "https://github.com/owner/repo/pull/42",
				"number": int64(42),
			},
		},
	}}
	eng, err := engine.New(engine.Options{
		Bus:      bus,
		Resolver: resolver,
		Runner:   runner,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	// The PR's state flips open -> closed: the daemon emits one
	// entity.updated per changed field (field = "data.state").
	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "github:owner-repo-42",
		Kind:     "github",
		Field:    "data.state",
		Old:      "open",
		New:      "closed",
		At:       time.Now().UTC(),
	})
	eng.WaitForIdle()

	got, err := os.ReadFile(taskPath)
	require.NoError(t, err)
	body := string(got)
	assert.Contains(t, body, "- [x] owner/repo#42",
		"the terminal PR's mention line is resolved (checked)")
	assert.Contains(t, body, "- [ ] other/repo#99",
		"the still-open PR's line is left untouched")
}

// TestExampleWorkflow_GithubMentionResolve_NonTerminalNoOp pins that
// the terminal-state condition is load-bearing: an entity.updated for a
// PR that is NOT closed must leave its mention line untouched.
func TestExampleWorkflow_GithubMentionResolve_NonTerminalNoOp(t *testing.T) {
	t.Parallel()

	exPath := filepath.Join("..", "..", "docs", "examples", "workflows",
		"gmail-github-mentions-resolve.md")
	wf, err := parser.ParseFile(exPath)
	require.NoError(t, err)

	root := t.TempDir()
	taskWriter := actions.NewFileTaskWriter(root, nil, nil, nil, nil, nil)
	runner := actions.New(actions.Options{TaskWriter: taskWriter})

	taskPath := actions.TaskVaultPath(root, "gmail-github-mentions", "pending")
	require.NoError(t, os.MkdirAll(filepath.Dir(taskPath), 0o755))
	const taskBody = `---
kind: task
workflow: gmail-github-mentions
subject: pending
---

## Open mentions

- [ ] owner/repo#7 — still under review
`
	require.NoError(t, os.WriteFile(taskPath, []byte(taskBody), 0o644))

	bus := eventbus.NewMemoryBus()
	resolver := &triggerFakeResolver{entities: map[string]map[string]any{
		"github:owner-repo-7": {
			"id":   "github:owner-repo-7",
			"kind": "github",
			"data": map[string]any{
				"state":  "open", // NOT terminal
				"url":    "https://github.com/owner/repo/pull/7",
				"number": int64(7),
			},
		},
	}}
	eng, err := engine.New(engine.Options{
		Bus: bus, Resolver: resolver, Runner: runner,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	bus.Publish(context.Background(), eventbus.EntityUpdatedEvent{
		EntityID: "github:owner-repo-7",
		Kind:     "github",
		Field:    "data.state",
		Old:      "draft",
		New:      "open",
		At:       time.Now().UTC(),
	})
	eng.WaitForIdle()

	got, err := os.ReadFile(taskPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [ ] owner/repo#7",
		"a non-terminal PR must leave its mention line unresolved")
}
