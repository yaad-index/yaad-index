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
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	err := w.AppendTaskSection(context.Background(),
		"review-queue", "boardgame-acme", "", "", "candidates",
		"Acme Game (2026)", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)

	got := readTask(t, vault, "review-queue-boardgame-acme.md")
	assert.Contains(t, got, "---\n")
	assert.Contains(t, got, "kind: task\n")
	assert.Contains(t, got, "workflow: review-queue\n")
	assert.Contains(t, got, "subject: boardgame-acme\n")
	assert.Contains(t, got, "## candidates\n")
	assert.Contains(t, got, "Acme Game (2026)\n")
}

// TestFileTaskWriter_AppendsToExistingSection: a second
// task_append to the same section + workflow adds the new
// line without duplicating the section header.
func TestFileTaskWriter_AppendsToExistingSection(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "second", parser.IfAlreadyPresentSkip))

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
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "same", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "same", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	count := strings.Count(got, "same")
	assert.Equal(t, 1, count, "duplicate line skipped")
}

// TestFileTaskWriter_AppendAnyway: append-anyway writes
// duplicate lines regardless of pre-existence.
func TestFileTaskWriter_AppendAnyway(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "line", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "line", parser.IfAlreadyPresentAppendAnyway))

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
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "match", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "other", parser.IfAlreadyPresentSkip))
	// Replace "match" with itself — should remain 1 occurrence.
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "match", parser.IfAlreadyPresentReplace))

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
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "a", "alpha", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "b", "beta", parser.IfAlreadyPresentSkip))

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
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	err := w.AppendTaskSection(context.Background(),
		"My Workflow", "Acme Game (2026)", "", "", "s", "c", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)

	// File should exist at the slugified path.
	_, err = os.Stat(filepath.Join(vault, "tasks", "my-workflow-acme-game-2026.md"))
	assert.NoError(t, err, "slugified path created")
}

// TestFileTaskWriter_UnknownPolicy: an if_already_present
// value outside {skip, replace, append-anyway} returns a
// clear error (defensive; the parser's Validate enforces
// this upstream but the writer is the boundary check).
func TestFileTaskWriter_UnknownPolicy(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	// First write so the section exists.
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "first", parser.IfAlreadyPresentSkip))

	err := w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "first", "merge") // unknown policy
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not one of")
}

// TestFileTaskWriter_EmptyWorkflow_Rejected: defensive —
// workflow empty would produce a path under tasks/-subj.md.
func TestFileTaskWriter_EmptyWorkflow_Rejected(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)
	err := w.AppendTaskSection(context.Background(),
		"", "subj", "", "", "s", "c", parser.IfAlreadyPresentSkip)
	require.Error(t, err)
}

// TestFileTaskWriter_EmptySubject_Allowed: an empty
// subject is allowed (target-less manual workflows) and
// produces a path under `<vault>/tasks/<workflow>.md`.
func TestFileTaskWriter_EmptySubject_Allowed(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)
	err := w.AppendTaskSection(context.Background(),
		"daily-summary", "", "", "", "s", "c", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(vault, "tasks", "daily-summary.md"))
	assert.NoError(t, err)
}

// TestFileTaskWriter_MissingRefs_AppendsSection: a fresh
// task gets a `## Missing references` section after
// EnsureMissingRefsSection runs with non-empty refs.
func TestFileTaskWriter_MissingRefs_AppendsSection(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.EnsureMissingRefsSection(context.Background(),
		"wf", "subj", []string{"boardgame:absent", "person:gone"}))

	got := readTask(t, vault, "wf-subj.md")
	assert.Contains(t, got, "## Missing references\n")
	assert.Contains(t, got, "- boardgame:absent\n")
	assert.Contains(t, got, "- person:gone\n")
}

// TestFileTaskWriter_MissingRefs_SyncsOnReFire: a second
// EnsureMissingRefsSection call with a different ref list
// replaces the section body — refs that resolved on the
// re-eval don't linger in the task body.
func TestFileTaskWriter_MissingRefs_SyncsOnReFire(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.EnsureMissingRefsSection(context.Background(),
		"wf", "subj", []string{"id:a", "id:b"}))
	require.NoError(t, w.EnsureMissingRefsSection(context.Background(),
		"wf", "subj", []string{"id:c"}))

	got := readTask(t, vault, "wf-subj.md")
	assert.Contains(t, got, "- id:c\n")
	assert.NotContains(t, got, "- id:a")
	assert.NotContains(t, got, "- id:b")
	// Section still exists, just with new refs.
	assert.Equal(t, 1, strings.Count(got, "## Missing references"))
}

// TestFileTaskWriter_MissingRefs_EmptyRemovesSection: a
// re-eval that resolves all refs (refs=empty) removes the
// `## Missing references` section entirely (self-heal per
// ADR-0024 §"Missing-reference handling").
func TestFileTaskWriter_MissingRefs_EmptyRemovesSection(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "candidates", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.EnsureMissingRefsSection(context.Background(),
		"wf", "subj", []string{"id:a"}))
	require.NoError(t, w.EnsureMissingRefsSection(context.Background(),
		"wf", "subj", nil))

	got := readTask(t, vault, "wf-subj.md")
	assert.NotContains(t, got, "## Missing references",
		"section gone after refs resolved")
	assert.Contains(t, got, "## candidates", "operator's section preserved")
	assert.Contains(t, got, "first")
}

// TestFileTaskWriter_MissingRefs_FileAbsent_NoOp: calling
// EnsureMissingRefsSection on a (workflow, subject) without
// a task file does nothing — no error, no file created.
// task_append owns the file-create responsibility.
func TestFileTaskWriter_MissingRefs_FileAbsent_NoOp(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.EnsureMissingRefsSection(context.Background(),
		"wf", "subj", []string{"id:a"}))
	_, err := os.Stat(filepath.Join(vault, "tasks", "wf-subj.md"))
	require.Error(t, err, "no file created when task absent")
	assert.True(t, os.IsNotExist(err))
}

// TestFileTaskWriter_DedupKeyStampedOnFirstCreate: when
// dedupKey is non-empty on first create, the frontmatter
// includes `dedup_key: <value>` so the task identity is
// inspectable by future surfaces (per ADR-0024 §"Per-pattern
// de-duplication"). Subsequent appends don't re-stamp.
func TestFileTaskWriter_DedupKeyStampedOnFirstCreate(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "wf|entity:1", "", "s", "first", parser.IfAlreadyPresentSkip))
	got := readTask(t, vault, "wf-subj.md")
	assert.Contains(t, got, "dedup_key: wf|entity:1\n")

	// Subsequent append doesn't change the dedup_key line.
	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "wf|entity:1", "", "s", "second", parser.IfAlreadyPresentSkip))
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
	w := NewFileTaskWriter(vault, nil, nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "", "s", "c", parser.IfAlreadyPresentSkip))
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

// TestFileTaskWriter_ViaSection_FreshCreate (per #163): first
// append on a missing task file produces both frontmatter
// `via:` list AND the body `## Via` section, each with a
// single entry naming the firing workflow + triggering entity.
func TestFileTaskWriter_ViaSection_FreshCreate(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	kinds := canonicalKindsForTest() // {boardgame, person, gmail}
	w := NewFileTaskWriter(vault, kinds, nil, nil, nil)

	err := w.AppendTaskSection(context.Background(),
		"linkedin-hiring", "hiring-2026-05", "", "gmail:msg-1",
		"alerts", "Acme is hiring", parser.IfAlreadyPresentSkip)
	require.NoError(t, err)

	got := readTask(t, vault, "linkedin-hiring-hiring-2026-05.md")
	// Frontmatter via list shape (yaml.v3 4-space indent).
	assert.Contains(t, got, "via:")
	assert.Contains(t, got, "    - workflow: linkedin-hiring\n      entity: gmail:msg-1")
	// Body `## Via` section with wikilinks.
	assert.Contains(t, got, "## Via\n")
	assert.Contains(t, got, "- [[linkedin-hiring]] from [[gmail:msg-1]]\n")
	// Section header order: frontmatter → ## Via → ## alerts.
	viaIdx := strings.Index(got, "## Via")
	alertsIdx := strings.Index(got, "## alerts")
	require.Greater(t, viaIdx, 0)
	require.Greater(t, alertsIdx, viaIdx, "## Via above ## alerts")
}

// TestFileTaskWriter_ViaSection_DedupSameWorkflowSameEntity:
// re-firing the same (workflow, entity) pair leaves a single
// via entry in both frontmatter list AND body section.
func TestFileTaskWriter_ViaSection_DedupSameWorkflowSameEntity(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	kinds := canonicalKindsForTest()
	w := NewFileTaskWriter(vault, kinds, nil, nil, nil)
	ctx := context.Background()

	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts", "second", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	// One workflow entry in frontmatter (yaml.v3 4-space indent).
	assert.Equal(t, 1, strings.Count(got, "    - workflow: wf\n      entity: gmail:msg-1"),
		"dedup: same (workflow, entity) pair keeps single via entry")
	// One body line in `## Via`.
	assert.Equal(t, 1, strings.Count(got, "- [[wf]] from [[gmail:msg-1]]"),
		"dedup mirrors in body section")
}

// TestDedupAndPrepend_DifferentWorkflowSameEntity exercises
// the pure helper for the "different workflow, same entity →
// new entry prepended" case named in #163's spec. Lives as a
// unit test on dedupAndPrepend rather than an integration
// test because each workflow writes to its own task file path
// (`<workflow>-<subject>.md`) — the integration scenario
// can't share a task file under the current path scheme.
// The helper still has to behave correctly in case a future
// shared-task scheme changes that.
func TestDedupAndPrepend_DifferentWorkflowSameEntity(t *testing.T) {
	t.Parallel()
	list := []viaEntry{{Workflow: "wf-a", Entity: "gmail:msg-1"}}
	got := dedupAndPrepend(list, viaEntry{Workflow: "wf-b", Entity: "gmail:msg-1"})
	require.Len(t, got, 2)
	assert.Equal(t, "wf-b", got[0].Workflow, "newest-first: wf-b prepended above wf-a")
	assert.Equal(t, "wf-a", got[1].Workflow)
}

// TestDedupAndPrepend_DedupKeepsOriginalPosition: re-firing
// an existing (workflow, entity) entry does NOT move it to
// the top — original position preserved per spec.
func TestDedupAndPrepend_DedupKeepsOriginalPosition(t *testing.T) {
	t.Parallel()
	list := []viaEntry{
		{Workflow: "wf-b", Entity: "gmail:msg-2"},
		{Workflow: "wf-a", Entity: "gmail:msg-1"},
	}
	got := dedupAndPrepend(list, viaEntry{Workflow: "wf-a", Entity: "gmail:msg-1"})
	require.Len(t, got, 2, "duplicate dropped")
	assert.Equal(t, "wf-b", got[0].Workflow,
		"existing wf-a stays at its original position; wf-b unchanged at top")
	assert.Equal(t, "wf-a", got[1].Workflow)
}

// TestFileTaskWriter_ViaSection_PrependsNewEntity: same
// workflow firing with a different entity prepends a new
// via entry in both places.
func TestFileTaskWriter_ViaSection_PrependsNewEntity(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, canonicalKindsForTest(), nil, nil, nil)
	ctx := context.Background()

	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts", "first", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-2", "alerts", "second", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	idx2 := strings.Index(got, "- [[wf]] from [[gmail:msg-2]]")
	idx1 := strings.Index(got, "- [[wf]] from [[gmail:msg-1]]")
	require.Greater(t, idx2, 0)
	require.Greater(t, idx1, 0)
	assert.Less(t, idx2, idx1,
		"newest entity prepended above earlier one")
}

// TestFileTaskWriter_ViaSection_UnknownEntityLiteral: when
// entityID is empty (manual trigger / no entity context), the
// breadcrumb stores + renders the `unknown` literal. The
// rendered form is NOT wrapped in `[[ ]]` because `unknown`
// isn't an entity slug.
func TestFileTaskWriter_ViaSection_UnknownEntityLiteral(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, canonicalKindsForTest(), nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "" /*entityID empty → unknown*/, "alerts", "x", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	assert.Contains(t, got, "      entity: unknown\n",
		"empty entityID stored as `unknown` literal in frontmatter")
	assert.Contains(t, got, "- [[wf]] from unknown\n",
		"`unknown` rendered bare in body section (no [[ ]] wrap)")
}

// TestFileTaskWriter_ViaSection_FrontmatterAndBodyStaySynced:
// after multiple appends to the same task file, the
// frontmatter via list and body `## Via` section list the same
// (workflow, entity) pairs in the same order (newest-first,
// with re-fires deduping in place).
func TestFileTaskWriter_ViaSection_FrontmatterAndBodyStaySynced(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, canonicalKindsForTest(), nil, nil, nil)
	ctx := context.Background()

	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts", "1a", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-2", "alerts", "2b", parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts", "1a-2", parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	// Frontmatter list: msg-2 prepended ahead of msg-1; the
	// 3rd append dedups against the existing msg-1 entry and
	// leaves it at position 2. yaml.v3 emits 4-space indent.
	frontmatterOrder := []string{
		"- workflow: wf\n      entity: gmail:msg-2",
		"- workflow: wf\n      entity: gmail:msg-1",
	}
	for i, want := range frontmatterOrder {
		idx := strings.Index(got, want)
		require.Greater(t, idx, 0, "frontmatter entry %d present: %s", i, want)
	}
	// Body section: same order.
	bodyOrder := []string{
		"- [[wf]] from [[gmail:msg-2]]",
		"- [[wf]] from [[gmail:msg-1]]",
	}
	prevIdx := -1
	for i, want := range bodyOrder {
		idx := strings.Index(got, want)
		require.Greater(t, idx, 0, "body entry %d present: %s", i, want)
		assert.Greater(t, idx, prevIdx, "body order matches frontmatter")
		prevIdx = idx
	}
}

// TestFileTaskWriter_ViaSection_ContentWrappedWhenEntityShape:
// CEL-rendered body content that happens to be entity-shaped
// (matches `<kind>:<id>` with kind in registry) gets wrapped
// in `[[ ]]`. Plain strings pass through.
func TestFileTaskWriter_ViaSection_ContentWrappedWhenEntityShape(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	w := NewFileTaskWriter(vault, canonicalKindsForTest(), nil, nil, nil)
	ctx := context.Background()

	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts",
		"gmail:msg-1" /*content rendered to entity shape*/, parser.IfAlreadyPresentSkip))
	require.NoError(t, w.AppendTaskSection(ctx,
		"wf", "subj", "", "gmail:msg-1", "alerts",
		"plain text" /*content non-entity-shape*/, parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	// Entity-shaped content wrapped.
	assert.Contains(t, got, "[[gmail:msg-1]]\n",
		"entity-shaped content wrapped via maybeWrapEntity")
	// Plain text passes through unchanged.
	assert.Contains(t, got, "plain text\n")
}

// TestFileTaskWriter_ViaSection_UnknownKindNotWrapped: a
// content string of the shape `<kind>:<id>` whose `kind` is
// NOT in the writer's canonical-kinds registry passes through
// unwrapped (guards against false-positive wrapping of
// timestamp / scheme strings).
func TestFileTaskWriter_ViaSection_UnknownKindNotWrapped(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	// Registry omits "package" kind.
	w := NewFileTaskWriter(vault, canonicalKindsForTest(), nil, nil, nil)

	require.NoError(t, w.AppendTaskSection(context.Background(),
		"wf", "subj", "", "gmail:msg-1", "alerts",
		"package:something" /*kind not in registry*/, parser.IfAlreadyPresentSkip))

	got := readTask(t, vault, "wf-subj.md")
	assert.NotContains(t, got, "[[package:something]]",
		"unknown kind passes through unwrapped")
	assert.Contains(t, got, "package:something\n",
		"raw content preserved")
}
