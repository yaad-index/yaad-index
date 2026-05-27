// Integration tests for VaultEdgeWriter — exercise the
// canonical-edge + dataview-append path against a real
// in-memory store + a real vault on a temp dir. Unit-level
// coverage of the runner→writer plumbing lives in
// add_canonical_edge_test.go via the fakeEdgeWriter.

package actions

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

func newVaultEdgeWriterFixture(t *testing.T) (*VaultEdgeWriter, store.Store, *vault.Reader, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	kindReg := map[string]config.CanonicalKindConfig{
		"github-repository": {
			Gaps: map[string]config.GapSpec{
				"description": {Type: "string", Description: "repo description"},
			},
		},
	}

	// Seed a source entity (the workflow's trigger entity) so
	// CreateEdge's FK can land.
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID:   "email:m1",
		Kind: "email",
		Data: map[string]any{"subject": "[acme/widget] PR #42 review requested"},
	}))

	writer := NewVaultEdgeWriter(
		st, nil, r, w,
		writelocks.New(),
		kindReg,
		nil, // bus — not asserted here
		slog.New(slog.DiscardHandler),
	)
	return writer, st, r, root
}

// TestVaultEdgeWriter_HappyPath: a fresh canonical-edge fire
// slugifies the target name, ensures the thin label row,
// creates the edge.
func TestVaultEdgeWriter_HappyPath(t *testing.T) {
	t.Parallel()
	w, st, _, _ := newVaultEdgeWriterFixture(t)

	err := w.AddCanonicalEdge(context.Background(), "github-classify",
		"email:m1", "is_about", "github-repository", "acme/widget", nil)
	require.NoError(t, err)

	// Thin row materialized for the target.
	row, err := st.GetEntity(context.Background(), "github-repository:acme-widget")
	require.NoError(t, err)
	assert.Equal(t, "github-repository", row.Kind)

	// Edge persisted source → target.
	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "is_about", edges[0].Type)
	assert.Equal(t, "github-repository:acme-widget", edges[0].To)
}

// TestVaultEdgeWriter_IdempotentEdge: re-firing the same edge
// tuple does not duplicate (store.CreateEdge upserts on
// (type, from, to)). No vault file appears since data is nil.
func TestVaultEdgeWriter_IdempotentEdge(t *testing.T) {
	t.Parallel()
	w, st, _, root := newVaultEdgeWriterFixture(t)

	for i := 0; i < 3; i++ {
		err := w.AddCanonicalEdge(context.Background(), "wf",
			"email:m1", "is_about", "github-repository", "acme/widget", nil)
		require.NoError(t, err)
	}
	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1, "re-fires must not duplicate edges")

	// No vault file for the target — no data means no
	// auto-materialize trigger.
	_, statErr := os.Stat(filepath.Join(root, "ct", "github-repository", "acme-widget.md"))
	assert.Error(t, statErr, "no data → no vault file materialized")
}

// TestVaultEdgeWriter_DataAppendsParagraphAndAutoMaterializes:
// non-empty data triggers canonical.AppendDataviewParagraph,
// which auto-materializes the target's vault file and writes
// the sorted-key dataview paragraph between the yaad:dataview
// markers.
func TestVaultEdgeWriter_DataAppendsParagraphAndAutoMaterializes(t *testing.T) {
	t.Parallel()
	w, _, r, _ := newVaultEdgeWriterFixture(t)

	data := map[string]string{
		"reference": "42",
		"type":      "review",
	}
	err := w.AddCanonicalEdge(context.Background(), "github-classify",
		"email:m1", "is_about", "github-repository", "acme/widget", data)
	require.NoError(t, err)

	ve, err := r.ReadByID("github-repository", "github-repository:acme-widget")
	require.NoError(t, err)
	require.Len(t, ve.Dataview, 1, "one paragraph appended")
	got := ve.Dataview[0].Fields
	assert.Equal(t, "42", got["reference"])
	assert.Equal(t, "review", got["type"])
}

// TestVaultEdgeWriter_DataDedupesIdenticalParagraph: re-firing
// with identical data does not duplicate the dataview
// paragraph (canonical.AppendDataviewParagraph's sorted-key
// content-hash dedup).
func TestVaultEdgeWriter_DataDedupesIdenticalParagraph(t *testing.T) {
	t.Parallel()
	w, _, r, _ := newVaultEdgeWriterFixture(t)

	data := map[string]string{"reference": "42", "type": "review"}
	for i := 0; i < 3; i++ {
		err := w.AddCanonicalEdge(context.Background(), "wf",
			"email:m1", "is_about", "github-repository", "acme/widget", data)
		require.NoError(t, err)
	}
	ve, err := r.ReadByID("github-repository", "github-repository:acme-widget")
	require.NoError(t, err)
	require.Len(t, ve.Dataview, 1, "identical data must dedup")
}

// TestVaultEdgeWriter_DataAccumulatesDifferentValues: re-firing
// with different data accumulates a second paragraph
// (history-as-event-log).
func TestVaultEdgeWriter_DataAccumulatesDifferentValues(t *testing.T) {
	t.Parallel()
	w, _, r, _ := newVaultEdgeWriterFixture(t)

	err := w.AddCanonicalEdge(context.Background(), "wf",
		"email:m1", "is_about", "github-repository", "acme/widget",
		map[string]string{"reference": "42", "type": "review"})
	require.NoError(t, err)
	err = w.AddCanonicalEdge(context.Background(), "wf",
		"email:m1", "is_about", "github-repository", "acme/widget",
		map[string]string{"reference": "43", "type": "notification"})
	require.NoError(t, err)

	ve, err := r.ReadByID("github-repository", "github-repository:acme-widget")
	require.NoError(t, err)
	require.Len(t, ve.Dataview, 2, "different data accumulates")
}

// TestVaultEdgeWriter_SourceFKError: a source id that doesn't
// exist in the store surfaces store.CreateEdge's
// ErrMissingEntity wrapped through to the writer.
func TestVaultEdgeWriter_SourceFKError(t *testing.T) {
	t.Parallel()
	w, _, _, _ := newVaultEdgeWriterFixture(t)
	err := w.AddCanonicalEdge(context.Background(), "wf",
		"email:does-not-exist", "is_about", "github-repository", "acme/widget", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create edge")
}

// TestVaultEdgeWriter_EmptyTargetNameSlugError: whitespace-only
// targetName rejects at the service boundary (Cut C2's
// CreateCanonicalEdgeByName trims + validates before either
// the auto-resolve or slugify branch fires). Pre-Cut-C2 the
// rejection landed at slug.Slug returning ""; the
// strict-boundary check is the new shape.
func TestVaultEdgeWriter_EmptyTargetNameSlugError(t *testing.T) {
	t.Parallel()
	w, _, _, _ := newVaultEdgeWriterFixture(t)
	err := w.AddCanonicalEdge(context.Background(), "wf",
		"email:m1", "is_about", "github-repository", "   ", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "targetName is required")
}

// TestVaultEdgeWriter_KindPrefixStrip_DayCanonicalID pins the
// load-bearing ADR-0027 cut 1 fix: when target.name is the
// canonical-ID form (e.g. today() returns "day:2026-11-11"),
// the runner strips the leading "<targetKind>:" before
// slugifying so slug.Slug doesn't mangle the colon into a
// hyphen. Without the strip, the result would be the malformed
// "day:day-2026-11-11".
func TestVaultEdgeWriter_KindPrefixStrip_DayCanonicalID(t *testing.T) {
	t.Parallel()
	st, r, w := newPlainStoreVaultPair(t)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: "task:write-report", Kind: "task",
	}))
	writer := NewVaultEdgeWriter(st, nil, r, w, writelocks.New(), nil, nil,
		slog.New(slog.DiscardHandler))

	err := writer.AddCanonicalEdge(context.Background(), "today-stamp",
		"task:write-report", "due_on", "day", "day:2026-11-11", nil)
	require.NoError(t, err)

	// Target id is the clean canonical form, NOT day:day-2026-11-11.
	row, err := st.GetEntity(context.Background(), "day:2026-11-11")
	require.NoError(t, err, "day entity must be materialized with the canonical id")
	assert.Equal(t, "day", row.Kind)

	edges, err := st.GetEdgesFor(context.Background(), "task:write-report", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "due_on", edges[0].Type)
	assert.Equal(t, "day:2026-11-11", edges[0].To,
		"edge target must be the canonical day-id, not the double-prefixed form")
}

// TestVaultEdgeWriter_KindPrefixStrip_PreservesBareName pins
// that operator-named slugs continue to work — the strip is
// conservative (matches only the exact "<kind>:" prefix), so
// "My Daily Note" still slugifies to "my-daily-note".
func TestVaultEdgeWriter_KindPrefixStrip_PreservesBareName(t *testing.T) {
	t.Parallel()
	st, r, w := newPlainStoreVaultPair(t)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: "task:t1", Kind: "task",
	}))
	writer := NewVaultEdgeWriter(st, nil, r, w, writelocks.New(), nil, nil,
		slog.New(slog.DiscardHandler))

	err := writer.AddCanonicalEdge(context.Background(), "wf",
		"task:t1", "is_about_day", "day", "My Daily Note", nil)
	require.NoError(t, err)

	edges, err := st.GetEdgesFor(context.Background(), "task:t1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "day:my-daily-note", edges[0].To,
		"bare-name target still slugifies normally — strip is exact-prefix only")
}

// TestVaultEdgeWriter_KindPrefixStrip_OnlyExactKindPrefix pins
// that a leading colon-bearing token whose prefix does NOT match
// the target.kind is left alone (the operator's intent in that
// case is ambiguous — preserve their literal input rather than
// silently strip arbitrary text).
func TestVaultEdgeWriter_KindPrefixStrip_OnlyExactKindPrefix(t *testing.T) {
	t.Parallel()
	st, r, w := newPlainStoreVaultPair(t)
	require.NoError(t, st.SaveEntity(context.Background(), &store.Entity{
		ID: "task:t1", Kind: "task",
	}))
	writer := NewVaultEdgeWriter(st, nil, r, w, writelocks.New(), nil, nil,
		slog.New(slog.DiscardHandler))

	// target.kind=day but target.name has prefix "week:" — the
	// strip is conservative and leaves it alone, slug.Slug then
	// mangles the colon (operator gets a confusing id, but the
	// behavior is predictable).
	err := writer.AddCanonicalEdge(context.Background(), "wf",
		"task:t1", "references_day", "day", "week:2026-W11", nil)
	require.NoError(t, err)

	edges, err := st.GetEdgesFor(context.Background(), "task:t1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "day:week-2026-w11", edges[0].To,
		"non-matching prefix is left for slug.Slug to handle — operator's literal input is preserved")
}

// newPlainStoreVaultPair builds a fresh real-store + vault pair
// without the github-repository kind-registry seeding the main
// fixture does — used by the kind-prefix tests where the target
// kind is "day" not "github-repository".
func newPlainStoreVaultPair(t *testing.T) (store.Store, *vault.Reader, *vault.Writer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	return st, r, w
}
