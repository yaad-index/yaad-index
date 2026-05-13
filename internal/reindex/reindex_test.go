package reindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

func newTestEnv(t *testing.T) (*Reindexer, store.Store, *vault.Writer, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New")
	t.Cleanup(func() { _ = st.Close() })

	vaultRoot := t.TempDir()
	w, err := vault.NewWriter(vaultRoot)
	require.NoError(t, err, "vault.NewWriter")

	r, err := New(st, vaultRoot, nil, nil)
	require.NoError(t, err, "reindex.New")

	return r, st, w, vaultRoot
}

func newEntity(t *testing.T, id, kind string) *vault.Entity {
	t.Helper()
	return &vault.Entity{
		ID: id,
		Kind: kind,
		Plugin: "test",
		Data: map[string]any{"title": id},
		Summary: "summary for " + id,
	}
}

func TestReindex_EmptyVaultIsNoop(t *testing.T) {
	t.Parallel()

	r, _, _, _ := newTestEnv(t)
	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, "incremental", summary.Mode)
	assert.Equal(t, 0, summary.Scanned)
	assert.Equal(t, 0, summary.Parsed)
	assert.Empty(t, summary.Errors)
}

func TestReindex_NewFilePicksUpEntity(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	want := newEntity(t, "wikipedia:foo", "wikipedia-article")
	require.NoError(t, w.Write(want))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Scanned)
	assert.Equal(t, 1, summary.Parsed)
	assert.Equal(t, 1, summary.EntitiesCreated)
	assert.Equal(t, 0, summary.EntitiesUpdated)
	assert.Empty(t, summary.Errors)

	got, err := st.GetEntity(context.Background(), want.ID)
	require.NoError(t, err)
	assert.Equal(t, want.Kind, got.Kind)
	assert.Equal(t, "wikipedia:foo", got.Data["title"])
}

func TestReindex_UnchangedFileIsSkipped(t *testing.T) {
	t.Parallel()

	r, _, w, _ := newTestEnv(t)
	require.NoError(t, w.Write(newEntity(t, "wikipedia:foo", "wikipedia-article")))

	first, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	require.Equal(t, 1, first.Parsed, "first walk parses the new file")

	second, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, second.Scanned, "second walk still sees the file")
	assert.Equal(t, 0, second.Parsed, "but skips it (mtime + hash unchanged)")
	assert.Equal(t, 1, second.Skipped)
}

func TestReindex_ChangedFileIsReparsed(t *testing.T) {
	t.Parallel()

	r, st, w, vaultRoot := newTestEnv(t)
	e := newEntity(t, "wikipedia:foo", "wikipedia-article")
	require.NoError(t, w.Write(e))

	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)

	// Mutate via direct file write so mtime + hash both change. Use
	// the Writer to keep file structure valid; bump mtime manually
	// since on a fast filesystem two writes within the same
	// nanosecond can produce identical mtimes.
	e.Summary = "updated summary"
	require.NoError(t, w.Write(e))
	bumped := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(vaultRoot, e.Kind, "foo.md"), bumped, bumped))

	second, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, second.Scanned)
	assert.Equal(t, 1, second.Parsed, "changed file re-parsed")
	assert.Equal(t, 0, second.Skipped)
	assert.Equal(t, 0, second.EntitiesCreated, "existing path → not a create")
	assert.Equal(t, 1, second.EntitiesUpdated)

	got, err := st.GetEntity(context.Background(), e.ID)
	require.NoError(t, err)
	assert.Equal(t, "wikipedia:foo", got.Data["title"], "data round-trips")
}

func TestReindex_DeletedFileCascadesEntityRemoval(t *testing.T) {
	t.Parallel()

	r, st, w, vaultRoot := newTestEnv(t)
	require.NoError(t, w.Write(newEntity(t, "wikipedia:foo", "wikipedia-article")))
	require.NoError(t, w.Write(newEntity(t, "wikipedia:bar", "wikipedia-article")))

	first, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 2, first.EntitiesCreated)

	// Remove foo.md from disk; bar.md remains.
	require.NoError(t, os.Remove(filepath.Join(vaultRoot, "wikipedia-article", "foo.md")))

	second, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, second.Scanned, "only bar.md remains")
	assert.Equal(t, 1, second.Skipped)
	assert.Equal(t, 1, second.EntitiesDeleted, "foo cascade-deleted")

	_, err = st.GetEntity(context.Background(), "wikipedia:foo")
	assert.ErrorIs(t, err, store.ErrNotFound)

	bar, err := st.GetEntity(context.Background(), "wikipedia:bar")
	require.NoError(t, err)
	assert.Equal(t, "wikipedia:bar", bar.ID)
}

func TestReindex_FullModeWipesAndRebuilds(t *testing.T) {
	t.Parallel()

	r, st, w, vaultRoot := newTestEnv(t)
	require.NoError(t, w.Write(newEntity(t, "wikipedia:keep", "wikipedia-article")))

	_, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	require.Len(t, listRows(t, st), 1)

	// Add a stranded bookkeeping row that points at a no-longer-existing
	// entity (simulate divergent state). --full must clear it.
	require.NoError(t, st.UpsertReindexFile(context.Background(), store.ReindexFile{
		Path: filepath.Join(vaultRoot, "wikipedia-article", "stranded.md"),
		Mtime: time.Now().UTC(),
		ContentHash: "deadbeef",
		LastIndexedAt: time.Now().UTC(),
		EntityID: "wikipedia:stranded",
		EntityKind: "wikipedia-article",
	}))
	require.Len(t, listRows(t, st), 2)

	full, err := r.Run(context.Background(), Full)
	require.NoError(t, err)
	assert.Equal(t, "full", full.Mode)
	assert.Equal(t, 1, full.Scanned, "the kept file")
	assert.Equal(t, 1, full.Parsed)
	assert.Equal(t, 1, full.EntitiesCreated, "after wipe, every parse is a create")
	assert.Equal(t, 0, full.EntitiesUpdated)
	assert.Equal(t, 0, full.EntitiesDeleted, "full mode skips disappeared-file cascade")

	rows := listRows(t, st)
	require.Len(t, rows, 1, "stranded row gone after wipe; only the live one remains")
	assert.Equal(t, "wikipedia:keep", rows[0].EntityID)
}

func TestReindex_EdgesArePersisted(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	src := newEntity(t, "wikipedia:src", "wikipedia-article")
	dst := newEntity(t, "wikipedia:dst", "wikipedia-article")
	require.NoError(t, w.Write(dst))

	src.Edges = []vault.Edge{
		{Type: "designed", To: "wikipedia:dst"},
	}
	require.NoError(t, w.Write(src))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Parsed)
	assert.Equal(t, 1, summary.EdgeRowsWritten)
	assert.Empty(t, summary.Errors)

	edges, err := st.GetEdgesFor(context.Background(), "wikipedia:src", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "designed", edges[0].Type)
	assert.Equal(t, "wikipedia:dst", edges[0].To)
}

// TestReindex_ForwardEdgeReferenceLandsOnSecondPass: when a vault
// file's edge points at an entity not yet upserted in this walk
// (filepath.WalkDir's lexicographic order means `src.md` may be
// visited before `tgt.md`), CreateEdge fails with ErrMissingEntity
// and the error is recorded. A subsequent walk picks up the edge
// once both endpoints are present.
func TestReindex_ForwardEdgeReferenceLandsOnSecondPass(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)

	// Lexicographic order of slugs: aa.md before zz.md. Edge from aa → zz.
	src := newEntity(t, "wikipedia:aa", "wikipedia-article")
	src.Edges = []vault.Edge{{Type: "designed", To: "wikipedia:zz"}}
	tgt := newEntity(t, "wikipedia:zz", "wikipedia-article")
	require.NoError(t, w.Write(src))
	require.NoError(t, w.Write(tgt))

	first, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 2, first.Parsed)
	require.Len(t, first.Errors, 1, "forward edge ref records one error")
	assert.Contains(t, first.Errors[0], "missing entity")

	// On second walk, src is unchanged (skipped), so the edge isn't
	// re-attempted yet — that's a known incremental limitation.
	// Forcing a re-parse via mtime bump exercises the recovery.
	bumped := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(r.vaultRoot, "wikipedia-article", "aa.md"), bumped, bumped))

	second, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Empty(t, second.Errors, "second walk: edge resolves, no errors")

	edges, err := st.GetEdgesFor(context.Background(), "wikipedia:aa", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "wikipedia:zz", edges[0].To)
}

// TestReindex_ReplacesProvenanceFromVaultFrontmatter pins ADR-0009's
// canonical-source contract: the vault frontmatter `provenance:` list
// is authoritative; reindex reconciles the DB-side rows to match. Any
// prior store-side rows for the entity get dropped, the vault list gets
// inserted in order. The "DB has anything that isn't in the vault list"
// state is impossible after a successful reindex pass.
func TestReindex_ReplacesProvenanceFromVaultFrontmatter(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	ctx := context.Background()

	// Seed: entity in store with a prior provenance row that DOESN'T
	// match what the vault is about to advertise. Reindex should
	// replace this with the vault list.
	require.NoError(t, st.UpsertEntity(ctx, &store.Entity{
		ID: "wikipedia:reconcile",
		Kind: "wikipedia-article",
		Data: map[string]any{"title": "wikipedia:reconcile"},
	}))
	t1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, st.AppendProvenance(ctx, "wikipedia:reconcile",
		[]store.ProvenanceEntry{{Source: "stale:store", FetchedAt: &t1, OK: true}}))

	// Vault file declares two different rows.
	t2 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 1, 0, 0, 1, 0, time.UTC)
	want := newEntity(t, "wikipedia:reconcile", "wikipedia-article")
	want.Provenance = []vault.ProvenanceEntry{
		{Source: "wikipedia:fetch", FetchedAt: &t2, OK: true},
		{Source: "agent:bob", FilledAt: &t3, OK: true},
	}
	require.NoError(t, w.Write(want))

	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	got, err := st.GetEntity(ctx, "wikipedia:reconcile")
	require.NoError(t, err)
	require.Len(t, got.Provenance, 2,
		"DB provenance after reindex: want exactly the vault list (stale row dropped, vault entries inserted)")
	gotSources := []string{got.Provenance[0].Source, got.Provenance[1].Source}
	assert.Equal(t, []string{"wikipedia:fetch", "agent:bob"}, gotSources,
		"provenance order: want vault-frontmatter order")
	assert.Nil(t, got.Provenance[1].FetchedAt,
		"agent:bob row: FetchedAt should be nil (it's a fill row)")
	assert.NotNil(t, got.Provenance[1].FilledAt,
		"agent:bob row: FilledAt should be set")
}

// TestReindex_VaultProvenanceEmptyDropsAllStoreRows pins the
// vault-list-became-empty case ADR-0009 explicitly calls out: a vault
// file with no `provenance:` block (or an empty list) drops every
// prior store row for that entity. The entity row itself stays — only
// provenance is replaced with the empty list.
func TestReindex_VaultProvenanceEmptyDropsAllStoreRows(t *testing.T) {
	t.Parallel()

	r, st, w, _ := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, st.UpsertEntity(ctx, &store.Entity{
		ID: "wikipedia:no-prov",
		Kind: "wikipedia-article",
		Data: map[string]any{"title": "wikipedia:no-prov"},
	}))
	t1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, st.AppendProvenance(ctx, "wikipedia:no-prov",
		[]store.ProvenanceEntry{{Source: "stale:store", FetchedAt: &t1, OK: true}}))

	// Vault file has no provenance entries — nil/empty slice.
	want := newEntity(t, "wikipedia:no-prov", "wikipedia-article")
	want.Provenance = nil
	require.NoError(t, w.Write(want))

	_, err := r.Run(ctx, Incremental)
	require.NoError(t, err)

	got, err := st.GetEntity(ctx, "wikipedia:no-prov")
	require.NoError(t, err)
	assert.Empty(t, got.Provenance,
		"DB provenance after reindex of vault-with-no-prov: want empty (stale store row dropped)")
	assert.Equal(t, "wikipedia:no-prov", got.Data["title"],
		"entity data: want unchanged (only provenance was reconciled)")
}

func TestReindex_RejectsRelativeVaultRoot(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	_, err = New(st, "relative/path", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestReindex_IgnoresHiddenAndNonMarkdownFiles(t *testing.T) {
	t.Parallel()

	r, _, w, vaultRoot := newTestEnv(t)
	require.NoError(t, w.Write(newEntity(t, "wikipedia:visible", "wikipedia-article")))

	// Drop a hidden temp file (matches the writer's `.<slug>.md.tmp-*`
	// pattern) and a non-markdown file in the same kind dir. Reindex
	// must skip both.
	kindDir := filepath.Join(vaultRoot, "wikipedia-article")
	require.NoError(t, os.WriteFile(filepath.Join(kindDir, ".hidden.md.tmp-x"), []byte("---\nid: x\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(kindDir, "notes.txt"), []byte("not markdown"), 0o644))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Scanned, "only visible.md")
	assert.Equal(t, 1, summary.Parsed)
	assert.Empty(t, summary.Errors)
}

// TestReindex_SkipsArchiveSubtree pins ADR-0018 step 2's walker
// rule: files under `<vault>/_archive/` are NOT scanned. Archived
// entities live as DB rows; their vault files in `_archive/` are
// the durable record but reindex won't try to parse + reconcile
// them as if they were active. The test seeds one active file and
// one archived file (manually placed under `_archive/`) — only
// the active one shows up in the summary.
func TestReindex_SkipsArchiveSubtree(t *testing.T) {
	t.Parallel()

	r, _, w, vaultRoot := newTestEnv(t)
	require.NoError(t, w.Write(newEntity(t, "wikipedia:active", "wikipedia-article")))

	// Manually place a vault-shaped file under `_archive/` to
	// simulate a previously-archived entity. Body shape doesn't
	// matter — the walker should never read this file.
	archiveDir := filepath.Join(vaultRoot, "_archive", "wikipedia-article")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(archiveDir, "archived.md"),
		[]byte("---\nid: wikipedia:archived\nkind: wikipedia-article\n---\n"),
		0o644,
	))

	summary, err := r.Run(context.Background(), Incremental)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Scanned, "want only the active file scanned (archive subtree skipped)")
	assert.Equal(t, 1, summary.Parsed)
	assert.Empty(t, summary.Errors)
}

// TestReindex_ClearsDriftCountersOnSuccess pins yaad-index #31:
// after a successful reindex.Run, the dropped_canonical_{kinds,
// edges} tables are wiped. Pre-existing rows (from earlier ingest
// drift under a now-corrected config) and rows the reindex pass
// itself accrued both vanish — the "operator consumed drift signal"
// semantic.
func TestReindex_ClearsDriftCountersOnSuccess(t *testing.T) {
	t.Parallel()

	r, st, _, _ := newTestEnv(t)
	ctx := context.Background()

	// Pre-seed stale drift counters as if an earlier ingest under a
	// pre-corrective-config state had dropped emissions.
	require.NoError(t, st.IncDroppedCanonicalKind(ctx, "wikipedia", "person"))
	require.NoError(t, st.IncDroppedCanonicalEdge(ctx, "wikipedia", "is_about"))

	pre, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, pre, "fixture sanity: stale kind drift present pre-reindex")

	summary, err := r.Run(ctx, Incremental)
	require.NoError(t, err)
	assert.Empty(t, summary.Errors,
		"clear ops are best-effort but in a healthy store should not surface in summary.Errors")

	kinds, err := st.ListDroppedCanonicalKinds(ctx)
	require.NoError(t, err)
	assert.Empty(t, kinds, "kind drift counter cleared after successful reindex")

	edges, err := st.ListDroppedCanonicalEdges(ctx)
	require.NoError(t, err)
	assert.Empty(t, edges, "edge drift counter cleared after successful reindex")
}

func listRows(t *testing.T, st store.Store) []store.ReindexFile {
	t.Helper()
	rows, err := st.ListReindexFiles(context.Background())
	require.NoError(t, err)
	return rows
}
