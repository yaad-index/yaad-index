// Tests for the task_resolve action runner + the underlying
// resolveTaskLineInBody / FileTaskWriter.ResolveTaskLine
// transforms per #266.

package actions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestResolveTaskLineInBody_CheckFlipsUnchecked pins the
// check-mode happy path: an unchecked checkbox line under the
// named section flips to checked.
func TestResolveTaskLineInBody_CheckFlipsUnchecked(t *testing.T) {
	t.Parallel()
	body := `## pending-refetch

- [ ] acme/repo#42 — last gmail subject: foo
- [ ] acme/repo#43 — last gmail subject: bar
`
	out, err := resolveTaskLineInBody(body, "pending-refetch", "acme/repo#42", parser.TaskResolveModeCheck)
	require.NoError(t, err)
	assert.Contains(t, out, "- [x] acme/repo#42 — last gmail subject: foo")
	assert.Contains(t, out, "- [ ] acme/repo#43 — last gmail subject: bar",
		"only the matched line flips; siblings stay untouched")
}

// TestResolveTaskLineInBody_CheckAlreadyCheckedNoOp pins the
// idempotent end-state: a `- [x]` line stays `- [x]` (no
// duplicate write, no second flip).
func TestResolveTaskLineInBody_CheckAlreadyCheckedNoOp(t *testing.T) {
	t.Parallel()
	body := `## pending-refetch

- [x] acme/repo#42 — done
`
	out, err := resolveTaskLineInBody(body, "pending-refetch", "acme/repo#42", parser.TaskResolveModeCheck)
	require.NoError(t, err)
	assert.Equal(t, body, out, "already-checked line stays in place; body returned verbatim")
}

// TestResolveTaskLineInBody_RemoveStripsLine pins the remove-
// mode path: the matched line is dropped entirely.
func TestResolveTaskLineInBody_RemoveStripsLine(t *testing.T) {
	t.Parallel()
	body := `## pending-refetch

- [ ] acme/repo#42 — pending
- [ ] acme/repo#43 — pending
`
	out, err := resolveTaskLineInBody(body, "pending-refetch", "acme/repo#42", parser.TaskResolveModeRemove)
	require.NoError(t, err)
	assert.NotContains(t, out, "acme/repo#42")
	assert.Contains(t, out, "- [ ] acme/repo#43 — pending")
}

// TestResolveTaskLineInBody_NoMatchReturnsVerbatim pins the
// no-match path: a matchKey absent from the section leaves the
// body untouched (idempotent + no false-positive mutations).
func TestResolveTaskLineInBody_NoMatchReturnsVerbatim(t *testing.T) {
	t.Parallel()
	body := `## pending-refetch

- [ ] acme/repo#42 — pending
`
	out, err := resolveTaskLineInBody(body, "pending-refetch", "other/repo#1", parser.TaskResolveModeCheck)
	require.NoError(t, err)
	assert.Equal(t, body, out)
}

// TestResolveTaskLineInBody_MatchScopedToNamedSection pins the
// section-scoping rule: a matchKey-shaped line in a DIFFERENT
// section (e.g. resolved-already) doesn't get touched when the
// action targets `pending-refetch`.
func TestResolveTaskLineInBody_MatchScopedToNamedSection(t *testing.T) {
	t.Parallel()
	body := `## pending-refetch

- [ ] acme/repo#99 — pending

## resolved-already

- [x] acme/repo#42 — was already done
`
	out, err := resolveTaskLineInBody(body, "pending-refetch", "acme/repo#42", parser.TaskResolveModeCheck)
	require.NoError(t, err)
	assert.Equal(t, body, out,
		"matchKey scoped to pending-refetch section; resolved-already line untouched")
}

// TestResolveTaskLineInBody_FirstMatchWins pins the
// first-match contract: when two lines in the same section
// both have a matchKey-prefix, only the first flips. Workflow
// authors use a discriminating prefix to avoid collisions.
func TestResolveTaskLineInBody_FirstMatchWins(t *testing.T) {
	t.Parallel()
	body := `## pending-refetch

- [ ] acme/repo — first
- [ ] acme/repo — second
`
	out, err := resolveTaskLineInBody(body, "pending-refetch", "acme/repo", parser.TaskResolveModeCheck)
	require.NoError(t, err)
	assert.Contains(t, out, "- [x] acme/repo — first")
	assert.Contains(t, out, "- [ ] acme/repo — second")
}

// TestFileTaskWriter_ResolveTaskLine_MissingFileNoOp pins the
// cross-workflow no-op: a target file that doesn't exist
// returns nil + writes nothing. The originating workflow may
// never have fired.
func TestFileTaskWriter_ResolveTaskLine_MissingFileNoOp(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	w := NewFileTaskWriter(tmp, nil, nil, nil, nil)

	err := w.ResolveTaskLine(context.Background(), "never-fired", "no-subject", "pending-refetch", "key", parser.TaskResolveModeCheck)
	require.NoError(t, err, "missing file MUST resolve to nil per the #266 idempotence rule")

	// Nothing got written.
	entries, err := os.ReadDir(filepath.Join(tmp, "tasks"))
	if err == nil {
		assert.Empty(t, entries)
	}
}

// TestFileTaskWriter_ResolveTaskLine_FlipsAndPersists pins the
// end-to-end happy path: append a line via AppendTaskSection,
// flip via ResolveTaskLine, observe the persisted file.
func TestFileTaskWriter_ResolveTaskLine_FlipsAndPersists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	w := NewFileTaskWriter(tmp, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(ctx,
		"gmail-github-mentions", "to-refetch", "", "github:example",
		"pending-refetch", "- [ ] acme/repo#42 — pending", "skip",
	))
	require.NoError(t, w.ResolveTaskLine(ctx,
		"gmail-github-mentions", "to-refetch", "pending-refetch",
		"acme/repo#42", parser.TaskResolveModeCheck,
	))

	path := TaskVaultPath(tmp, "gmail-github-mentions", "to-refetch")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "- [x] acme/repo#42 — pending")
	assert.NotContains(t, string(data), "- [ ] acme/repo#42",
		"the unchecked line MUST be replaced; no duplicate left behind")
}

// TestFileTaskWriter_ResolveTaskLine_RemovePersists pins the
// remove-mode end-to-end shape against the real writer.
func TestFileTaskWriter_ResolveTaskLine_RemovePersists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	w := NewFileTaskWriter(tmp, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(ctx,
		"gmail-github-mentions", "to-refetch", "", "github:example",
		"pending-refetch", "- [ ] acme/repo#42 — stale", "skip",
	))
	require.NoError(t, w.ResolveTaskLine(ctx,
		"gmail-github-mentions", "to-refetch", "pending-refetch",
		"acme/repo#42", parser.TaskResolveModeRemove,
	))

	path := TaskVaultPath(tmp, "gmail-github-mentions", "to-refetch")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "acme/repo#42",
		"the line MUST be stripped from the file")
}

// TestRunner_TaskResolve_HappyPath pins the dispatcher
// routing + writer-call threading for #266: a task_resolve
// action lands on the TaskWriter's ResolveTaskLine with the
// right workflow / subject / section / matchKey / mode.
func TestRunner_TaskResolve_HappyPath(t *testing.T) {
	t.Parallel()
	w := &fakeTaskWriter{}
	r := New(Options{TaskWriter: w})

	wf := wfWithActions("github-resolve-on-update",
		parser.Action{TaskResolve: &parser.TaskResolveAction{
			Workflow: "gmail-github-mentions",
			Subject:  "to-refetch",
			Section:  "pending-refetch",
			MatchKey: "acme/repo#42",
			Mode:     parser.TaskResolveModeCheck,
		}},
	)
	dec := Decision{Workflow: "github-resolve-on-update", Subject: "any"}
	results := r.Run(context.Background(), wf, dec, Activation{})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)

	calls := w.resolveSnapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "gmail-github-mentions", calls[0].workflow)
	assert.Equal(t, "to-refetch", calls[0].subject)
	assert.Equal(t, "pending-refetch", calls[0].section)
	assert.Equal(t, "acme/repo#42", calls[0].matchKey)
	assert.Equal(t, parser.TaskResolveModeCheck, calls[0].mode)
}

// TestRunner_TaskResolve_NoTaskWriterReturnsConfigError pins
// the engine-misconfig surface: a Runner constructed without
// a TaskWriter surfaces a clear ActionResult.Err on task_resolve
// rather than silently dropping the action.
func TestRunner_TaskResolve_NoTaskWriterReturnsConfigError(t *testing.T) {
	t.Parallel()
	r := New(Options{}) // TaskWriter intentionally unset
	wf := wfWithActions("x",
		parser.Action{TaskResolve: &parser.TaskResolveAction{
			Workflow: "w", Subject: "s", Section: "sec",
			MatchKey: "k", Mode: parser.TaskResolveModeCheck,
		}},
	)
	results := r.Run(context.Background(), wf, Decision{Workflow: "x"}, Activation{})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "no TaskWriter wired")
}

// TestFileTaskWriter_ResolveTaskLine_RejectsUnknownMode pins
// the input-validation gate at the writer layer (defense in
// depth — the parser validates the same set at workflow load).
func TestFileTaskWriter_ResolveTaskLine_RejectsUnknownMode(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	w := NewFileTaskWriter(tmp, nil, nil, nil, nil)
	err := w.ResolveTaskLine(context.Background(),
		"any-workflow", "any-subject", "any-section", "any-key", "nonsense",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task_resolve mode")
}
