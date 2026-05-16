package actions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// TestFileTaskWriter_FreshCreate covers the find-or-create
// path: a missing task file gets created with frontmatter
// + the section header + the content line.
func TestFileTaskWriter_FreshCreate(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	err := w.AppendTaskSection(context.Background(),
		"review-queue", "boardgame-brass", "", "candidates",
		"Brass: Birmingham (2018)", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)

	got := readTask(t, vault, "review-queue-boardgame-brass.md")
	assert.Contains(t, got, "---\n")
	assert.Contains(t, got, "kind: task\n")
	assert.Contains(t, got, "workflow: review-queue\n")
	assert.Contains(t, got, "subject: boardgame-brass\n")
	assert.Contains(t, got, "## candidates\n")
	assert.Contains(t, got, "Brass: Birmingham (2018)\n")
}

// TestFileTaskWriter_AppendsToExistingSection: a second
// task_append to the same section + workflow adds the new
// line without duplicating the section header.
func TestFileTaskWriter_AppendsToExistingSection(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "candidates", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "candidates", "second", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	headerCount := strings.Count(got, "## candidates")
	assert.Equal(t, 1, headerCount, "single section header")
	assert.Contains(t, got, "first")
	assert.Contains(t, got, "second")
	assert.True(t, strings.Index(got, "first") < strings.Index(got, "second"),
		"insertion order preserved")
}

// TestFileTaskWriter_SkipDedupes: a duplicate content
// line with if_already_present=skip is a no-op.
func TestFileTaskWriter_SkipDedupes(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "candidates", "same", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "candidates", "same", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	count := strings.Count(got, "same")
	assert.Equal(t, 1, count, "duplicate line skipped")
}

// TestFileTaskWriter_AppendAnyway: append-anyway writes
// duplicate lines regardless of pre-existence.
func TestFileTaskWriter_AppendAnyway(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "line", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "line", parser.IfAlreadyPresentAppendAnyway))

	got := readTask(t, vault, "wf-subj.md")
	assert.Equal(t, 2, strings.Count(got, "line"))
}

// TestFileTaskWriter_Replace: replace rewrites the first
// matching line. Subsequent identical content with
// if_already_present=replace overwrites instead of
// appending (the section's other lines stay put).
func TestFileTaskWriter_Replace(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "match", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "other", parser.IfAlreadyPresentSkip))
	// Replace "match" with itself — should remain 1 occurrence.
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "match", parser.IfAlreadyPresentReplace))

	got := readTask(t, vault, "wf-subj.md")
	assert.Equal(t, 1, strings.Count(got, "match"))
	assert.Equal(t, 1, strings.Count(got, "other"),
		"replace touches only the matching line, not the section")
}

// TestFileTaskWriter_NewSection_InExistingFile: appending
// to a new section in an existing file adds the section
// header + content without touching prior sections.
func TestFileTaskWriter_NewSection_InExistingFile(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "a", "alpha", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "b", "beta", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	assert.Contains(t, got, "## a")
	assert.Contains(t, got, "## b")
	assert.Contains(t, got, "alpha")
	assert.Contains(t, got, "beta")
	assert.True(t, strings.Index(got, "## a") < strings.Index(got, "## b"),
		"section order preserved")
}

// TestFileTaskWriter_Slugify_HandlesUnsafeChars: workflow
// + subject with spaces / punctuation slugify to a
// filesystem-safe path.
func TestFileTaskWriter_Slugify_HandlesUnsafeChars(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	err := w.AppendTaskSection(context.Background(),
		"My Workflow", "Brass: Birmingham (2018)", "", "s", "c", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)

	// File should exist at the slugified path.
	_, err = os.Stat(filepath.Join(vault, "tasks", "my-workflow-brass-birmingham-2018.md"))
	assert.NoError(t, err, "slugified path created")
}

// TestFileTaskWriter_UnknownPolicy: an if_already_present
// value outside {skip, replace, append-anyway} returns a
// clear error (defensive; the parser's Validate enforces
// this upstream but the writer is the boundary check).
func TestFileTaskWriter_UnknownPolicy(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	// First write so the section exists.
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "first", parser.IfAlreadyPresentSkip))

	err := w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "first", "merge") // unknown policy
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not one of")
}

// TestFileTaskWriter_EmptyWorkflow_Rejected: defensive —
// workflow empty would produce a path under tasks/-subj.md.
func TestFileTaskWriter_EmptyWorkflow_Rejected(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)
	err := w.AppendTaskSection(context.Background(),
		"", "subj", "", "s", "c", parser.IfAlreadyPresentSkip)
	require.Error(t, err)
}

// TestFileTaskWriter_EmptySubject_Allowed: an empty
// subject is allowed (target-less manual workflows) and
// produces a path under `<vault>/tasks/<workflow>.md`.
func TestFileTaskWriter_EmptySubject_Allowed(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)
	err := w.AppendTaskSection(context.Background(),
		"daily-summary", "", "", "s", "c", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(vault, "tasks", "daily-summary.md"))
	assert.NoError(t, err)
}

// TestFileTaskWriter_DedupKeyStampedOnFirstCreate: when
// dedupKey is non-empty on first create, the frontmatter
// includes `dedup_key: <value>` so the task identity is
// inspectable by future surfaces (per ADR-0024 §"Per-pattern
// de-duplication"). Subsequent appends don't re-stamp.
func TestFileTaskWriter_DedupKeyStampedOnFirstCreate(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "wf|entity:1", "s", "first", parser.IfAlreadyPresentSkip))
	got := readTask(t, vault, "wf-subj.md")
	assert.Contains(t, got, "dedup_key: wf|entity:1\n")

	// Subsequent append doesn't change the dedup_key line.
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "wf|entity:1", "s", "second", parser.IfAlreadyPresentSkip))
	got = readTask(t, vault, "wf-subj.md")
	assert.Equal(t, 1, strings.Count(got, "dedup_key:"),
		"dedup_key stamped once on create; not re-stamped on append")
}

// TestFileTaskWriter_EmptyDedupKey_Omitted: empty dedupKey
// omits the frontmatter field entirely — preserves the
// shape from before Phase 5.A for workflows without
// dedup.key configured.
func TestFileTaskWriter_EmptyDedupKey_Omitted(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "s", "c", parser.IfAlreadyPresentSkip))
	got := readTask(t, vault, "wf-subj.md")
	assert.NotContains(t, got, "dedup_key:",
		"empty dedupKey omits the frontmatter field")
}

func readTask(t *testing.T, vault, filename string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(vault, "tasks", filename))
	require.NoError(t, err, "read task file")
	return string(body)
}
