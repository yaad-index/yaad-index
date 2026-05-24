package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// seedUserContentEntity writes a UGC entity to BOTH the store and the
// vault file. PR-B endpoints are read-only — we use the lower-level
// vault.Writer surface to set up state until PR-C lands the POST
// endpoint that does this through the API.
func seedUserContentEntity(t *testing.T, st store.Store, root, id, body string, tags []string) {
	t.Helper()
	seedEntity(t, st, id, userContentKind)
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: userContentKind,
		Source: []string{"user/default"},
		Data: map[string]any{"id": id, "title": "Test UGC"},
		Tags: tags,
		CleanContent: body,
		Provenance: []vault.ProvenanceEntry{
			{Source: "user", OK: true},
		},
	}))
}

func getUserContent(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// GET /v1/user-content/{id} returns the entity envelope + first-page
// of parsed sections. Section parsing follows the containment model
// from PR-A — every ATX heading is one addressable section, deeper
// nested headings are textually included in the parent's body.
func TestUserContent_Read_HappyPath(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	body := "intro paragraph\n\n## Books I Loved\nfiction list here\n## Notes\nrandom thoughts\n"
	seedUserContentEntity(t, st, root, "user-content:my-note", body, []string{"note", "personal"})

	rec := getUserContent(t, h, "/v1/user-content/user-content:my-note")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got userContentEntityResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "user-content:my-note", got.ID)
	assert.Equal(t, "user-content", got.Kind)
	assert.Equal(t, []string{"note", "personal"}, got.Tags)
	require.NotEmpty(t, got.Provenance)
	assert.Equal(t, "user", got.Provenance[0].Source)

	// Three sections: pre-heading body, ## Books I Loved, ## Notes.
	require.Len(t, got.Sections.Entries, 3)
	assert.Empty(t, got.Sections.NextCursor, "single-page payload has no cursor")

	assert.Equal(t, 0, got.Sections.Entries[0].Index)
	assert.Equal(t, 0, got.Sections.Entries[0].Depth)
	assert.Equal(t, "", got.Sections.Entries[0].Heading)
	assert.Contains(t, got.Sections.Entries[0].Body, "intro paragraph")

	assert.Equal(t, 1, got.Sections.Entries[1].Index)
	assert.Equal(t, 2, got.Sections.Entries[1].Depth)
	assert.Equal(t, "Books I Loved", got.Sections.Entries[1].Heading)
	assert.Equal(t, "books-i-loved", got.Sections.Entries[1].HeadingSlug)
	assert.Contains(t, got.Sections.Entries[1].Body, "fiction list")

	assert.Equal(t, "Notes", got.Sections.Entries[2].Heading)
	assert.Equal(t, "notes", got.Sections.Entries[2].HeadingSlug)
}

// 404 when the id resolves to a non-user-content kind (a stray entity
// id collision with a different source must not leak through this
// surface).
func TestUserContent_Read_404OnWrongKind(t *testing.T) {
	t.Parallel()

	h, st, _ := newAPIWithVault(t)
	// Seed an entity with the user-content prefix but a different kind.
	seedEntity(t, st, "user-content:trespasser", "boardgame")

	rec := getUserContent(t, h, "/v1/user-content/user-content:trespasser")
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not a user-content entity")
}

// 400 when the {id} doesn't carry the user-content: prefix.
func TestUserContent_Read_400OnBadIDPrefix(t *testing.T) {
	t.Parallel()

	h, _, _ := newAPIWithVault(t)
	rec := getUserContent(t, h, "/v1/user-content/wikipedia:tehran")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "id must start with")
	assert.Contains(t, rec.Body.String(), "user-content:")
}

// 404 when the entity simply doesn't exist.
func TestUserContent_Read_404OnMissing(t *testing.T) {
	t.Parallel()

	h, _, _ := newAPIWithVault(t)
	rec := getUserContent(t, h, "/v1/user-content/user-content:nope")
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// GET /v1/user-content/{id}/sections returns a paginated list with
// no entity envelope. Default page size kicks in.
func TestUserContent_SectionsList_HappyPath(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	body := "# A\nbody A\n## A.1\nnested\n# B\nbody B\n# C\nbody C\n"
	seedUserContentEntity(t, st, root, "user-content:multi", body, []string{"x"})

	rec := getUserContent(t, h, "/v1/user-content/user-content:multi/sections")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got struct {
		OK bool `json:"ok"`
		Entries []userContentSection `json:"entries"`
		NextCursor string `json:"next_cursor,omitempty"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	require.Len(t, got.Entries, 4) // # A (containment), ## A.1, # B, # C

	assert.Equal(t, "A", got.Entries[0].Heading)
	assert.Equal(t, 1, got.Entries[0].Depth)
	// Containment: # A's body INCLUDES ## A.1 + nested content.
	assert.Contains(t, got.Entries[0].Body, "## A.1")
	assert.Contains(t, got.Entries[0].Body, "nested")

	assert.Equal(t, "A.1", got.Entries[1].Heading)
	assert.Equal(t, 2, got.Entries[1].Depth)
	assert.Equal(t, "nested\n", got.Entries[1].Body)

	assert.Equal(t, "B", got.Entries[2].Heading)
	assert.Equal(t, "C", got.Entries[3].Heading)
}

// Pagination: when the section list exceeds `limit`, a next_cursor
// rides on the response and the next call resolves the remainder.
func TestUserContent_SectionsList_Pagination(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	// 5 sections, limit=2 → page 1 (entries[0..2)), page 2 (entries[2..4)),
	// page 3 (entries[4..5)).
	body := "## one\nA\n## two\nB\n## three\nC\n## four\nD\n## five\nE\n"
	seedUserContentEntity(t, st, root, "user-content:paged", body, []string{"x"})

	rec := getUserContent(t, h, "/v1/user-content/user-content:paged/sections?limit=2")
	require.Equal(t, http.StatusOK, rec.Code, "page1 body=%s", rec.Body.String())
	var page1 struct {
		OK bool `json:"ok"`
		Entries []userContentSection `json:"entries"`
		NextCursor string `json:"next_cursor,omitempty"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
	require.Len(t, page1.Entries, 2)
	assert.Equal(t, "one", page1.Entries[0].Heading)
	assert.Equal(t, "two", page1.Entries[1].Heading)
	require.NotEmpty(t, page1.NextCursor)

	rec = getUserContent(t, h, "/v1/user-content/user-content:paged/sections?limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, rec.Code, "page2 body=%s", rec.Body.String())
	var page2 struct {
		OK bool `json:"ok"`
		Entries []userContentSection `json:"entries"`
		NextCursor string `json:"next_cursor,omitempty"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page2))
	require.Len(t, page2.Entries, 2)
	assert.Equal(t, "three", page2.Entries[0].Heading)
	assert.Equal(t, "four", page2.Entries[1].Heading)
	require.NotEmpty(t, page2.NextCursor)

	rec = getUserContent(t, h, "/v1/user-content/user-content:paged/sections?limit=2&cursor="+page2.NextCursor)
	require.Equal(t, http.StatusOK, rec.Code, "page3 body=%s", rec.Body.String())
	var page3 struct {
		OK bool `json:"ok"`
		Entries []userContentSection `json:"entries"`
		NextCursor string `json:"next_cursor,omitempty"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page3))
	require.Len(t, page3.Entries, 1)
	assert.Equal(t, "five", page3.Entries[0].Heading)
	assert.Empty(t, page3.NextCursor, "last page emits no cursor")
}

// Bad cursor → 400.
func TestUserContent_SectionsList_400OnBadCursor(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	seedUserContentEntity(t, st, root, "user-content:foo", "## a\nx\n", []string{"x"})

	rec := getUserContent(t, h, "/v1/user-content/user-content:foo/sections?cursor=not-base64-anything!")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// limit=0 / negative / non-integer → 400.
func TestUserContent_SectionsList_400OnBadLimit(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	seedUserContentEntity(t, st, root, "user-content:foo", "## a\nx\n", []string{"x"})

	for _, bad := range []string{"0", "-1", "abc"} {
		rec := getUserContent(t, h, "/v1/user-content/user-content:foo/sections?limit="+bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, "limit=%s body=%s", bad, rec.Body.String())
	}
}

// limit > sectionsMaxLimit gets clamped to the cap and returns a
// successful response (not 400). Documented behavior: agents passing
// limit=1000 get sectionsMaxLimit results, no error.
func TestUserContent_SectionsList_ClampsHugeLimit(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	seedUserContentEntity(t, st, root, "user-content:foo", "## a\nx\n## b\ny\n", []string{"x"})

	rec := getUserContent(t, h, "/v1/user-content/user-content:foo/sections?limit=999999")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// GET /v1/user-content/{id}/sections/{sec} resolves by positional
// index and by heading slug.
func TestUserContent_Section_HappyPath(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	body := "## Books I Loved\nfiction list\n## Notes\nrandom thoughts\n"
	seedUserContentEntity(t, st, root, "user-content:my-note", body, []string{"x"})

	// Address by slug.
	rec := getUserContent(t, h, "/v1/user-content/user-content:my-note/sections/books-i-loved")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "user-content:my-note", got.ID)
	assert.Equal(t, "Books I Loved", got.Section.Heading)
	assert.Equal(t, "fiction list\n", got.Section.Body)

	// Address by positional index.
	rec = getUserContent(t, h, "/v1/user-content/user-content:my-note/sections/1")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var byIdx userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&byIdx))
	assert.Equal(t, "Notes", byIdx.Section.Heading)
}

// 404 when the section address doesn't resolve.
func TestUserContent_Section_404OnUnknownAddress(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	seedUserContentEntity(t, st, root, "user-content:my-note", "## one\nx\n", []string{"x"})

	for _, bad := range []string{"no-such-section", "99"} {
		rec := getUserContent(t, h, "/v1/user-content/user-content:my-note/sections/"+bad)
		require.Equal(t, http.StatusNotFound, rec.Code, "addr=%s body=%s", bad, rec.Body.String())
	}
}

// Duplicate-slug case: server returns 404 on the slug address; the
// agent must fall back to positional index. Per the prior design, disambiguation
// rule.
func TestUserContent_Section_404OnDuplicateSlug(t *testing.T) {
	t.Parallel()

	h, st, root := newAPIWithVault(t)
	body := "## Notes\nfirst\n## Notes\nsecond\n"
	seedUserContentEntity(t, st, root, "user-content:dup", body, []string{"x"})

	rec := getUserContent(t, h, "/v1/user-content/user-content:dup/sections/notes")
	require.Equal(t, http.StatusNotFound, rec.Code, "duplicate slug must not auto-resolve; body=%s", rec.Body.String())

	// Positional addressing succeeds.
	rec = getUserContent(t, h, "/v1/user-content/user-content:dup/sections/1")
	require.Equal(t, http.StatusOK, rec.Code, "positional fallback must succeed; body=%s", rec.Body.String())
	var got userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "Notes", got.Section.Heading)
	assert.Equal(t, "second\n", got.Section.Body)
}

// vault_required when the handler runs without WithVaultIO. Mirrors
// the notes handler's degradation.
func TestUserContent_503OnNoVault(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	seedEntity(t, st, "user-content:foo", userContentKind)

	h := NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), st, testRegistryWithSeed())

	for _, target := range []string{
		"/v1/user-content/user-content:foo",
		"/v1/user-content/user-content:foo/sections",
		"/v1/user-content/user-content:foo/sections/0",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "target=%s body=%s", target, rec.Body.String())
		assert.True(t, strings.Contains(rec.Body.String(), "vault_required"))
	}
}
