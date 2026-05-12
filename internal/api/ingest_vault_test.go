package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newAPIWithVault builds a handler with vault wiring active. Returns
// the handler, the store, and the vault root so the test can read
// back the markdown file the ingest path produced.
func newAPIWithVault(t *testing.T) (http.Handler, store.Store, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r))
	return h, st, root
}

// readVaultFile loads the entity at <root>/<kind>/<slug>.md or fails
// the test loudly. Used by the ingest-writes-vault tests to assert
// the on-disk shape after a successful POST /v1/ingest.
func readVaultFile(t *testing.T, root, kind, slug string) *vault.Entity {
	t.Helper()
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	got, err := r.ReadFile(filepath.Join(root, kind, slug+".md"))
	require.NoError(t, err, "read vault file %s/%s.md", kind, slug)
	return got
}

func TestIngest_WritesVaultFileBeforeDB_BrassBirmingham(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)

	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// DB row landed.
	got, err := st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame", got.Kind)

	// Vault file landed too — and matches the schema.
	v := readVaultFile(t, root, "boardgame", "brass-birmingham")
	assert.Equal(t, "boardgame:brass-birmingham", v.ID)
	assert.Equal(t, "boardgame", v.Kind)
	assert.Equal(t, "fixture", v.Plugin, "fixture path uses 'fixture' as plugin name")
	assert.Equal(t, "Brass: Birmingham", v.Data["title"])
	assert.NotEmpty(t, v.Provenance, "provenance accumulated in frontmatter")
	assert.Empty(t, v.Gaps, "complete state has no gaps")
}

func TestIngest_WritesVaultFile_NeedsFill_GapsInFrontmatter(t *testing.T) {
	t.Parallel()

	h, _, root := newAPIWithVault(t)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())

	v := readVaultFile(t, root, "boardgame", "needs-fill-stub")
	assert.Equal(t, stubNeedsFillEntityID, v.ID)
	assert.ElementsMatch(t,
		[]string{"summary", "tags", "complexity_assessment"},
		v.Gaps,
		"needs_fill ingest writes the gap field-name set into frontmatter")
	// Per ADR-0015: plugin-emitted body content is wrapped in
	// `<!-- alice2:plugin start/end -->` markers. The plugin-side
	// content is preserved verbatim between the markers.
	wantBody := vault.PluginBodyStartMarker + "\n<stub-cleaned content>\n" +
		vault.PluginBodyEndMarker + "\n"
	assert.Equal(t, wantBody, v.CleanContent,
		"plugin body content lands inside the marker pair (ADR-0015)")
}

// TestIngest_ReingestPreservesOperatorBodyEdits pins ADR-0015's
// user-visible promise: an operator who appends `## Notes` to a
// plugin-emitted entity's body keeps that section across re-ingests.
// The plugin content between the markers is replaced; everything
// outside survives byte-for-byte.
//
// Setup: first ingest writes the plugin-wrapped body. Test then
// hand-edits the vault file to append an operator section after the
// end marker. Re-ingest must preserve the operator section.
func TestIngest_ReingestPreservesOperatorBodyEdits(t *testing.T) {
	t.Parallel()

	h, _, root := newAPIWithVault(t)

	// First ingest — plants the marker-wrapped plugin body.
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "first ingest body=%s", rec.Body.String())

	v1 := readVaultFile(t, root, "boardgame", "needs-fill-stub")
	require.Contains(t, v1.CleanContent, vault.PluginBodyStartMarker)
	require.Contains(t, v1.CleanContent, vault.PluginBodyEndMarker)

	// Operator appends a `## Notes` section after the end marker.
	// Hand-edit the vault entity through the writer (simulates
	// Obsidian write).
	const operatorNotes = "\n\n## My playthrough notes\n" +
		"Played this with Eli last weekend, the second round we both went heavy on cotton.\n"
	v1.CleanContent = v1.CleanContent + operatorNotes

	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(v1), "operator hand-edit (append `## Notes`)")

	// Re-ingest — plugin re-emits, daemon re-wraps. Operator's
	// `## Notes` MUST survive (it's outside the marker pair).
	rec = postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "re-ingest body=%s", rec.Body.String())

	v2 := readVaultFile(t, root, "boardgame", "needs-fill-stub")
	assert.Contains(t, v2.CleanContent, "## My playthrough notes",
		"operator section heading must survive re-ingest")
	assert.Contains(t, v2.CleanContent, "Played this with Eli last weekend",
		"operator section body must survive re-ingest")
	assert.Contains(t, v2.CleanContent, "<stub-cleaned content>",
		"plugin region content still present after re-ingest")

	// Exactly one marker pair after re-ingest — no duplicates.
	assert.Equal(t, 1, strings.Count(v2.CleanContent, vault.PluginBodyStartMarker),
		"exactly one start marker after re-ingest")
	assert.Equal(t, 1, strings.Count(v2.CleanContent, vault.PluginBodyEndMarker),
		"exactly one end marker after re-ingest")
}

func TestIngest_ReingestSameURL_VaultProvenanceAccumulates(t *testing.T) {
	t.Parallel()

	h, _, root := newAPIWithVault(t)

	for i := 0; i < 3; i++ {
		rec := postIngest(t, h, map[string]any{
			"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
			"wait_seconds": 2,
		})
		require.Equal(t, http.StatusOK, rec.Code, "iteration %d body=%s", i, rec.Body.String())
	}

	v := readVaultFile(t, root, "boardgame", "brass-birmingham")
	assert.Len(t, v.Provenance, 3, "vault accumulates one provenance entry per ingest call")
}

// TestIngest_VaultWriteFailureAborts asserts that when the vault
// write fails, the DB is not updated. Implemented via a tracker
// constructed with a writer rooted at a missing parent directory —
// the writer's Write call fails because os.MkdirAll would succeed
// but os.CreateTemp inside a non-existent dir fails. Use a path
// inside t.TempDir() that we then make non-writable, simulating a
// real disk-full / permissions failure.
func TestIngest_VaultWriteFailureAborts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based vault failure can't be simulated as root")
	}
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	// Make the vault root read-only; subsequent MkdirAll(<root>/<kind>)
	// will fail with EACCES. Restore on cleanup so t.TempDir's deletion
	// works.
	require.NoError(t, os.Chmod(root, 0o500))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r))

	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})

	// Vault write failure surfaces as 500 internal_error per the
	// ADR-0008 "no partial state" contract. The exact wire shape uses
	// the existing assertErrorEnvelope helper.
	assertErrorEnvelope(t, rec, http.StatusInternalServerError, "internal_error", "vault file")

	// And the DB is empty: the entity never reached UpsertEntity
	// because the vault write blocked first.
	_, err = st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"DB must NOT have the entity when the vault write failed")
}

// TestIngest_NoVaultConfigured_StillWorksDBOnly locks the backwards-
// compatible path: tests + dev binaries that don't pass WithVaultIO
// continue to ingest into the DB without producing a vault file.
func TestIngest_NoVaultConfigured_StillWorksDBOnly(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t) // no WithVaultIO

	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got, err := st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame", got.Kind)
}

// TestIngest_ReingestPreservesAgentFilledState locks ADR-0008's
// "agent fills survive re-ingest" property: a fill of `summary` /
// `tags` / `comments` written into the vault frontmatter (simulating
// what a prior PR will do) survives a subsequent re-ingest of the same
// URL. The re-ingest reads the existing file, accumulates new
// provenance, and writes back — agent-filled fields are preserved
// because buildVaultEntity inherits them from the existing file.
func TestIngest_ReingestPreservesAgentFilledState(t *testing.T) {
	t.Parallel()

	h, _, root := newAPIWithVault(t)

	// First ingest writes the initial vault file.
	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	// Simulate an agent fill by editing the vault file directly. PR
	// will land the API path that does this for real; this test
	// just locks the ingest-side contract that the next ingest call
	// preserves what's already in frontmatter.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	v := readVaultFile(t, root, "boardgame", "brass-birmingham")
	v.Summary = "Heavy economic euro by Martin Wallace."
	v.Tags = []string{"economic", "heavy-euro"}
	require.NoError(t, w.Write(v))

	// Re-ingest the same URL. The vault read-merge-write must keep
	// the agent-filled fields intact while accumulating provenance.
	rec = postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "re-ingest body=%s", rec.Body.String())

	got := readVaultFile(t, root, "boardgame", "brass-birmingham")
	assert.Equal(t, "Heavy economic euro by Martin Wallace.", got.Summary,
		"summary survives re-ingest")
	assert.ElementsMatch(t, []string{"economic", "heavy-euro"}, got.Tags,
		"tags survive re-ingest")
	assert.Len(t, got.Provenance, 2,
		"re-ingest appends provenance to the existing list")
}

// TestIngest_ForceRefetchPreservesUserContent pins the operator's
// requirement: re-ingest with force_refetch=true MUST refresh
// plugin-provided fields (data, provenance, notations, etc.) but
// MUST NOT clobber user-added content (comments Per the prior design, edges
// added by the agent). The contract was inherited from +
// — this test locks the regression boundary.
func TestIngest_ForceRefetchPreservesUserContent(t *testing.T) {
	t.Parallel()

	h, _, root := newAPIWithVault(t)

	// First ingest establishes the entity in the vault.
	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	// Simulate an agent comment landing on the entity (post-).
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	v := readVaultFile(t, root, "boardgame", "brass-birmingham")
	// Comments serialize to body-table date precision Per the prior design,'s
	// rendering shape; the writer normalizes to date-only on round-
	// trip. Use midnight UTC so the assertion below hits cleanly.
	commentDate := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	v.Comments = []vault.Comment{
		{
			Date: commentDate,
			Text: "Played this last weekend, the canal phase is brilliant.",
			Author: "the implementer",
			Operator: "alice",
		},
	}
	require.NoError(t, w.Write(v))

	// force_refetch=true bypasses the cache → plugin runs → vault is
	// re-written with fresh plugin-provided data. The merger
	// (buildVaultEntity) MUST preserve the comments from the existing
	// vault file.
	rec = postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
		"force_refetch": true,
	})
	require.Equal(t, http.StatusOK, rec.Code, "force_refetch body=%s", rec.Body.String())

	got := readVaultFile(t, root, "boardgame", "brass-birmingham")
	require.Len(t, got.Comments, 1, "user-added comment must survive force_refetch")
	assert.Equal(t, "Played this last weekend, the canal phase is brilliant.", got.Comments[0].Text)
	assert.Equal(t, "the implementer", got.Comments[0].Author)
	assert.Equal(t, "alice", got.Comments[0].Operator)
	assert.True(t, commentDate.Equal(got.Comments[0].Date),
		"comment date round-trips: want %s, got %s", commentDate, got.Comments[0].Date)

	// Plugin-provided fields refresh — the new ingest stamped a fresh
	// provenance entry on top of the prior one.
	assert.GreaterOrEqual(t, len(got.Provenance), 2,
		"force_refetch appends a fresh provenance entry alongside the existing one")
}

// TestIngest_StampsProvenanceInOperatorTimezone pins yaad-index
// PR-B: provenance fetched_at is stamped via clock.Now() so the
// operator-configured timezone propagates through the vault write
// path. Default (clock unset) → UTC; SetLocation(Europe/Berlin) →
// the next ingest's freshly-stamped fetched_at carries the Berlin
// location.
//
// NOT t.Parallel — clock.SetLocation writes package-level global
// state. Running concurrently with any other test that reads
// clock.Now() (or that ingests through the same code path) would
// race on the atomic.Pointer load. The save/restore via t.Cleanup
// keeps siblings unaffected when this runs serially.
func TestIngest_StampsProvenanceInOperatorTimezone(t *testing.T) {
	h, _, root := newAPIWithVault(t)

	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	prev := clock.Location()
	t.Cleanup(func() { clock.SetLocation(prev) })
	clock.SetLocation(berlin)

	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	v := readVaultFile(t, root, "boardgame", "brass-birmingham")
	require.NotEmpty(t, v.Provenance)
	require.NotNil(t, v.Provenance[0].FetchedAt)
	got := v.Provenance[0].FetchedAt.Location()
	// YAML round-trips a tagged Location — Marshal preserves the
	// offset string. The reader resolves "+02:00" to a fixed Local
	// instance, not the named "Europe/Berlin" zone (Go's YAML lib
	// can't reconstruct the named zone from offset alone). So we
	// assert on the OFFSET equality at the stamped instant rather
	// than name equality.
	stamped := *v.Provenance[0].FetchedAt
	_, gotOffset := stamped.Zone()
	_, wantOffset := stamped.In(berlin).Zone()
	assert.Equal(t, wantOffset, gotOffset,
		"provenance fetched_at must carry the operator-TZ offset; got location %s", got)
}
