package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
)

// seedTaskEntity seeds a task DB row (kind=task) plus a raw 5-section
// task vault file at tasks/<slug>.md, and returns the file path. The
// file is written verbatim (not via the Entity Marshal path) so its
// 5-section marker structure is the on-disk truth — mirroring how
// FileTaskWriter lands tasks.
func seedTaskEntity(t *testing.T, st store.Store, root, id, notesSeed string) string {
	t.Helper()
	seedEntity(t, st, id, "task")
	body, err := actions.RenderTaskSections(actions.TaskSections{
		Prompt: "resolve the thing",
		Notes: notesSeed,
	})
	require.NoError(t, err)
	slug := id[strings.IndexByte(id, ':')+1:]
	path := filepath.Join(root, vault.KindDir("task"), slug+".md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// TestNotes_TaskKind_RoutesToFiveSectionNotes pins #343: a note posted
// to a task-kind target lands in the 5-section schema's notes section
// via the AddNote primitive, not the legacy `## Notes` table.
func TestNotes_TaskKind_RoutesToFiveSectionNotes(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "task:do-the-thing"
	path := seedTaskEntity(t, st, root, id, "")

	rec := postComments(t, h, id, map[string]any{"text": "first observation", "author": "agent:bob"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(raw)

	// The note landed inside the 5-section notes marker pair, with the
	// `- <date> (<author>): <text>` attribution shape.
	sections, perr := actions.ParseTaskSections(body)
	require.NoError(t, perr, "task body still parses as the 5-section schema")
	assert.Contains(t, sections.Notes, "first observation")
	assert.Contains(t, sections.Notes, "(agent:bob)")
	// No legacy yaad:notes table was appended — the note is in-section.
	assert.NotContains(t, body, "yaad:notes")
	// 5-section structure intact.
	for _, sec := range []string{"prompt", "edges", "todo", "freeform", "notes"} {
		assert.Contains(t, body, "<!-- yaad-index "+sec+" -->")
	}
}

// TestNotes_TaskKind_RoundTripByteStable pins the #343 acceptance
// criterion: mixed notes-endpoint calls on the same task keep the body
// parseable + byte-stable under parse->render.
func TestNotes_TaskKind_RoundTripByteStable(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "task:round-trip"
	path := seedTaskEntity(t, st, root, id, "")

	require.Equal(t, http.StatusCreated,
		postComments(t, h, id, map[string]any{"text": "note one", "author": "agent:a"}).Code)
	require.Equal(t, http.StatusCreated,
		postComments(t, h, id, map[string]any{"text": "note two", "author": "agent:b"}).Code)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(raw)

	sections, perr := actions.ParseTaskSections(body)
	require.NoError(t, perr)
	assert.Contains(t, sections.Notes, "note one")
	assert.Contains(t, sections.Notes, "note two")

	// parse -> render reproduces the on-disk body byte-for-byte.
	rendered, rerr := actions.RenderTaskSections(sections)
	require.NoError(t, rerr)
	assert.Equal(t, body, rendered, "parse->render is byte-stable after note appends")
}

// TestNotes_NonTaskKind_LegacyTablePreserved pins the other #343 branch:
// a note on a non-task entity keeps the legacy structured note model and
// never introduces a 5-section task body.
func TestNotes_NonTaskKind_LegacyTablePreserved(t *testing.T) {
	t.Parallel()
	h, _, root := newNotesFixture(t) // boardgame (non-task) entity

	rec := postComments(t, h, commentsTestEntityID, map[string]any{"text": "legacy note"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// Non-task targets keep the legacy structured note model — the note
	// is readable via the Entity round-trip, and the file carries no
	// 5-section task markers.
	ve := readVaultByID(t, root, "boardgame", commentsTestEntityID)
	require.Len(t, ve.Notes, 1)
	assert.Equal(t, "legacy note", ve.Notes[0].Text)

	path := filepath.Join(root, vault.KindDir("boardgame"), "note-test.md")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "<!-- yaad-index notes -->",
		"non-task note must not introduce a 5-section task body")
}
