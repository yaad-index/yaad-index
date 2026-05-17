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
		st, r, w,
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

// TestVaultEdgeWriter_EmptyTargetNameSlugError: the slug
// derivation rejects names that slugify to the empty string
// (just whitespace / punctuation). The error trace points at
// the slug step so the operator can fix the workflow.
func TestVaultEdgeWriter_EmptyTargetNameSlugError(t *testing.T) {
	t.Parallel()
	w, _, _, _ := newVaultEdgeWriterFixture(t)
	err := w.AddCanonicalEdge(context.Background(), "wf",
		"email:m1", "is_about", "github-repository", "   ", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty slug")
}
