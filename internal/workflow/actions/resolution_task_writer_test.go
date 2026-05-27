// Tests for the resolution-task primitive per #304 Cut C3.1.
// Covers the typed frontmatter shape, the 5-tuple-derived
// idempotency probe, the body checklist render, the entity-
// mirror upsert + the required-field validation matrix.

package actions

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

func newResolutionDeferredFixture() *edgewrite.ResolutionDeferred {
	return &edgewrite.ResolutionDeferred{
		From:           "email:m1",
		EdgeType:       "mentions",
		TargetKind:     "boardgame",
		RawTarget:      "Brass",
		ResolverPlugin: "yaad-bgg",
		Options: map[string]plugins.DisambiguationOption{
			"boardgame:brass-birmingham": {Label: "Brass: Birmingham", Summary: "2018 Wallace"},
			"boardgame:brass-lancashire": {Label: "Brass: Lancashire", Summary: "2007 Wallace"},
		},
	}
}

func newResolutionTaskWriter(t *testing.T) (*FileTaskWriter, string, store.Store) {
	t.Helper()
	vaultRoot := t.TempDir()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	w := NewFileTaskWriter(vaultRoot, nil, st, nil, nil, slog.Default())
	return w, vaultRoot, st
}

// TestResolutionTaskKey_DeterministicAndSlugSafe pins that
// the key derivation is pure-function + filesystem-safe. The
// same payload always yields the same key; the key contains
// only slug-safe characters.
func TestResolutionTaskKey_DeterministicAndSlugSafe(t *testing.T) {
	t.Parallel()
	d := newResolutionDeferredFixture()
	k1 := ResolutionTaskKey(d)
	k2 := ResolutionTaskKey(d)
	assert.Equal(t, k1, k2, "key derivation is pure")
	assert.NotEmpty(t, k1)
	// gosimple/slug guarantees lowercase + [a-z0-9-] only.
	for _, r := range k1 {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		assert.True(t, ok, "key contains slug-unsafe rune %q in %q", r, k1)
	}
}

// TestResolutionTaskKey_RawTargetCasingCollapses pins that
// raw-target casing / whitespace differences hash to the
// same key — the locked normalization design relies on
// slug.Slug to collapse `Brass` / `brass` / `BRASS` to one
// idempotency identity.
func TestResolutionTaskKey_RawTargetCasingCollapses(t *testing.T) {
	t.Parallel()
	base := newResolutionDeferredFixture()
	upper := *base
	upper.RawTarget = "BRASS"
	mixed := *base
	mixed.RawTarget = "  Brass  "
	assert.Equal(t, ResolutionTaskKey(base), ResolutionTaskKey(&upper))
	assert.Equal(t, ResolutionTaskKey(base), ResolutionTaskKey(&mixed))
}

// TestResolutionTaskKey_NoFieldBoundaryCollision pins the
// PR-309 catch: a naive slug-and-join derivation collapses
// `|` separators and embedded `:` / `_` / `-` punctuation
// onto the same `-` shape, so structurally-different
// 5-tuples can hash to the same path. Length-prefixed SHA-256
// preserves field boundaries — these two tuples differ only
// in where the `-` "lives" between adjacent fields, and they
// MUST rotate the key.
func TestResolutionTaskKey_NoFieldBoundaryCollision(t *testing.T) {
	t.Parallel()
	a := &edgewrite.ResolutionDeferred{
		From: "x", EdgeType: "a-b", TargetKind: "boardgame",
		RawTarget: "Brass", ResolverPlugin: "p",
	}
	b := &edgewrite.ResolutionDeferred{
		From: "x-a", EdgeType: "b", TargetKind: "boardgame",
		RawTarget: "Brass", ResolverPlugin: "p",
	}
	assert.NotEqual(t, ResolutionTaskKey(a), ResolutionTaskKey(b),
		"length-prefixed hashing must distinguish field boundary shifts")
}

// TestResolutionTaskKey_DistinctTuplesDistinctKeys pins
// that any 5-tuple field change rotates the key. Workflow
// retries that DO change the source / kind / plugin land
// distinct tasks.
func TestResolutionTaskKey_DistinctTuplesDistinctKeys(t *testing.T) {
	t.Parallel()
	base := newResolutionDeferredFixture()
	baseKey := ResolutionTaskKey(base)

	cases := []struct {
		name  string
		mutate func(*edgewrite.ResolutionDeferred)
	}{
		{"From", func(d *edgewrite.ResolutionDeferred) { d.From = "email:m2" }},
		{"EdgeType", func(d *edgewrite.ResolutionDeferred) { d.EdgeType = "is_about" }},
		{"TargetKind", func(d *edgewrite.ResolutionDeferred) { d.TargetKind = "person" }},
		{"RawTarget", func(d *edgewrite.ResolutionDeferred) { d.RawTarget = "Caverna" }},
		{"ResolverPlugin", func(d *edgewrite.ResolutionDeferred) { d.ResolverPlugin = "yaad-wikipedia" }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mut := *base
			c.mutate(&mut)
			assert.NotEqual(t, baseKey, ResolutionTaskKey(&mut))
		})
	}
}

// TestWriteResolutionTask_FreshCreate is the happy-path
// shape round-trip: every locked frontmatter field lands +
// the body checklist renders one line per option.
func TestWriteResolutionTask_FreshCreate(t *testing.T) {
	t.Parallel()
	w, vaultRoot, _ := newResolutionTaskWriter(t)
	d := newResolutionDeferredFixture()

	taskID, created, err := w.WriteResolutionTask(context.Background(), d)
	require.NoError(t, err)
	assert.True(t, created, "first write materializes the file")
	assert.Equal(t, "task:"+ResolutionTaskKey(d), taskID)

	path := ResolutionTaskVaultPath(vaultRoot, d)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(body)

	// Frontmatter contract.
	assert.Contains(t, got, "kind: resolution-task\n")
	assert.Contains(t, got, "schema_version: 1\n")
	assert.Contains(t, got, "idempotency_key: "+ResolutionTaskKey(d)+"\n")
	assert.Contains(t, got, "from_id: email:m1\n")
	assert.Contains(t, got, "edge_type: mentions\n")
	assert.Contains(t, got, "target_kind: boardgame\n")
	assert.Contains(t, got, "resolver_plugin: yaad-bgg\n")
	assert.Contains(t, got, "normalized_raw_target: brass\n")
	assert.Contains(t, got, "raw_target: Brass\n")
	// Options block: sort-by-id order pins the rendered list.
	birmIdx := strings.Index(got, "boardgame:brass-birmingham")
	lancIdx := strings.Index(got, "boardgame:brass-lancashire")
	require.Positive(t, birmIdx)
	require.Positive(t, lancIdx)
	assert.Less(t, birmIdx, lancIdx, "options sort ascending by id")

	// Body checklist mirrors the options.
	assert.Contains(t, got, "## Resolution\n")
	assert.Contains(t, got, "- [ ] boardgame:brass-birmingham — Brass: Birmingham — 2018 Wallace\n")
	assert.Contains(t, got, "- [ ] boardgame:brass-lancashire — Brass: Lancashire — 2007 Wallace\n")
}

// TestWriteResolutionTask_IdempotencyOnSameTuple pins the
// load-bearing C3.1 contract: a second write with the same
// 5-tuple returns (taskID, false, nil) and leaves the file
// untouched — workflow retries collapse to one task.
func TestWriteResolutionTask_IdempotencyOnSameTuple(t *testing.T) {
	t.Parallel()
	w, vaultRoot, _ := newResolutionTaskWriter(t)
	d := newResolutionDeferredFixture()

	taskID1, created1, err := w.WriteResolutionTask(context.Background(), d)
	require.NoError(t, err)
	require.True(t, created1)

	path := ResolutionTaskVaultPath(vaultRoot, d)
	body1, err := os.ReadFile(path)
	require.NoError(t, err)

	// Second fire — same payload. Should idempotency-probe and skip.
	taskID2, created2, err := w.WriteResolutionTask(context.Background(), d)
	require.NoError(t, err)
	assert.Equal(t, taskID1, taskID2, "same tuple → same task id")
	assert.False(t, created2, "second write is the no-op idempotency hit")

	body2, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, body1, body2, "second write must not touch the file")
}

// TestWriteResolutionTask_DistinctTuplesDistinctFiles pins
// the other end of the idempotency contract: a workflow that
// fires with a genuinely different tuple lands a new task.
func TestWriteResolutionTask_DistinctTuplesDistinctFiles(t *testing.T) {
	t.Parallel()
	w, vaultRoot, _ := newResolutionTaskWriter(t)
	d1 := newResolutionDeferredFixture()
	d2 := *d1
	d2.RawTarget = "Caverna"

	id1, c1, err := w.WriteResolutionTask(context.Background(), d1)
	require.NoError(t, err)
	require.True(t, c1)

	id2, c2, err := w.WriteResolutionTask(context.Background(), &d2)
	require.NoError(t, err)
	require.True(t, c2)

	assert.NotEqual(t, id1, id2)
	_, err = os.Stat(ResolutionTaskVaultPath(vaultRoot, d1))
	assert.NoError(t, err)
	_, err = os.Stat(ResolutionTaskVaultPath(vaultRoot, &d2))
	assert.NoError(t, err)
}

// TestWriteResolutionTask_EntityMirror pins that the store
// row lands with the resolution-task fields on the Data map
// so Cut C3.3's resolve handler can read everything it
// needs from the store without re-parsing the frontmatter.
func TestWriteResolutionTask_EntityMirror(t *testing.T) {
	t.Parallel()
	w, _, st := newResolutionTaskWriter(t)
	d := newResolutionDeferredFixture()

	taskID, _, err := w.WriteResolutionTask(context.Background(), d)
	require.NoError(t, err)

	row, err := st.GetEntity(context.Background(), taskID)
	require.NoError(t, err)
	assert.Equal(t, "task", row.Kind)
	assert.Equal(t, "resolution-task", row.Data["kind_extra"])
	assert.Equal(t, 1, intFromAny(row.Data["schema_version"]))
	assert.Equal(t, "email:m1", row.Data["from_id"])
	assert.Equal(t, "mentions", row.Data["edge_type"])
	assert.Equal(t, "boardgame", row.Data["target_kind"])
	assert.Equal(t, "yaad-bgg", row.Data["resolver_plugin"])
	assert.Equal(t, "brass", row.Data["normalized_raw_target"])
	assert.Equal(t, "Brass", row.Data["raw_target"])
}

// TestWriteResolutionTask_RejectsMissingFields pins the
// input validation matrix — every required tuple field is
// rejected with a clear error when absent.
func TestWriteResolutionTask_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	w, _, _ := newResolutionTaskWriter(t)

	cases := []struct {
		name  string
		mutate func(*edgewrite.ResolutionDeferred)
	}{
		{"empty From", func(d *edgewrite.ResolutionDeferred) { d.From = "" }},
		{"empty EdgeType", func(d *edgewrite.ResolutionDeferred) { d.EdgeType = "" }},
		{"empty TargetKind", func(d *edgewrite.ResolutionDeferred) { d.TargetKind = "" }},
		{"empty RawTarget", func(d *edgewrite.ResolutionDeferred) { d.RawTarget = "  " }},
		{"empty ResolverPlugin", func(d *edgewrite.ResolutionDeferred) { d.ResolverPlugin = "" }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newResolutionDeferredFixture()
			c.mutate(d)
			_, _, err := w.WriteResolutionTask(context.Background(), d)
			require.Error(t, err)
		})
	}
}

// TestWriteResolutionTask_NilPayloadRejected pins the nil
// defensive branch — callers that lose the sentinel mid-
// chain shouldn't panic the writer.
func TestWriteResolutionTask_NilPayloadRejected(t *testing.T) {
	t.Parallel()
	w, _, _ := newResolutionTaskWriter(t)
	_, _, err := w.WriteResolutionTask(context.Background(), nil)
	require.Error(t, err)
}

// TestWriteResolutionTask_NoOptionsStillWrites pins the
// edge case where the plugin emitted zero options — the
// task still lands (resolve handler decides what to do);
// C3.1's writer doesn't gate on options-count.
func TestWriteResolutionTask_NoOptionsStillWrites(t *testing.T) {
	t.Parallel()
	w, vaultRoot, _ := newResolutionTaskWriter(t)
	d := newResolutionDeferredFixture()
	d.Options = nil

	_, created, err := w.WriteResolutionTask(context.Background(), d)
	require.NoError(t, err)
	require.True(t, created)
	body, err := os.ReadFile(ResolutionTaskVaultPath(vaultRoot, d))
	require.NoError(t, err)
	assert.NotContains(t, string(body), "- [ ]")
}

// TestResolutionTaskVaultPath_KeyShape pins that the
// vault-path helper composes the path the writer materializes
// AND that filename-only inspection (e.g. `ls tasks/`) yields
// the idempotency key.
func TestResolutionTaskVaultPath_KeyShape(t *testing.T) {
	t.Parallel()
	d := newResolutionDeferredFixture()
	root := t.TempDir()
	path := ResolutionTaskVaultPath(root, d)
	assert.Equal(t,
		filepath.Join(root, "tasks", ResolutionTaskKey(d)+".md"),
		path,
	)
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return -1
	}
}
