package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newAuthedUGCFixture builds a vault-wired handler with RequireAuth
// enforcement on, returning a signer for the tests to mint tokens.
func newAuthedUGCFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
	)
	return h, st, root, signer
}

func ugcReq(t *testing.T, h http.Handler, method, target, bearer string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, target, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// POST /v1/user-content happy path: stamps author + operator,
// derives slug from title, writes vault + DB, returns 201 with ETag.
func TestUserContent_Create_HappyPath(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Books I Loved",
		"body": "## Fiction\nGreat reads here.\n## Non-fiction\nDeep books.\n",
		"tags": []string{"books", "personal"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("ETag"))
	assert.Equal(t, "/v1/user-content/user-content:books-i-loved", rec.Header().Get("Location"))

	var got userContentEntityResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "user-content:books-i-loved", got.ID)
	assert.Equal(t, "user-content", got.Kind)
	assert.Equal(t, []string{"books", "personal"}, got.Tags)
	assert.Equal(t, "the implementer", got.Data["author"])
	assert.Equal(t, "alice", got.Data["operator"])
	assert.Equal(t, "Books I Loved", got.Data["title"])
	require.NotEmpty(t, got.Provenance)
	assert.Equal(t, "user", got.Provenance[0].Source)
	require.Len(t, got.Sections.Entries, 2)
	assert.Equal(t, "Fiction", got.Sections.Entries[0].Heading)
	assert.Equal(t, "Non-fiction", got.Sections.Entries[1].Heading)

	// Vault file landed.
	v := readVaultByID(t, root, "user-content", "user-content:books-i-loved")
	assert.Equal(t, "the implementer", v.Data["author"])
	assert.Equal(t, "alice", v.Data["operator"])
	assert.Contains(t, v.CleanContent, "## Fiction")

	// Store row landed.
	dbe, err := st.GetEntity(context.Background(), "user-content:books-i-loved")
	require.NoError(t, err)
	assert.Equal(t, "user-content", dbe.Kind)
}

// POST validates required fields.
func TestUserContent_Create_RequiresTitleAndTags(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing title", map[string]any{"body": "x", "tags": []string{"a"}}, http.StatusBadRequest},
		{"empty title", map[string]any{"title": " ", "body": "x", "tags": []string{"a"}}, http.StatusBadRequest},
		{"missing tags", map[string]any{"title": "ok", "body": "x"}, http.StatusBadRequest},
		{"empty tags", map[string]any{"title": "ok", "body": "x", "tags": []string{}}, http.StatusBadRequest},
		{"unslugifiable title", map[string]any{"title": "***", "body": "x", "tags": []string{"a"}}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, tc.body, nil)
			require.Equal(t, tc.want, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// POST returns 409 on slug collision.
func TestUserContent_Create_409OnCollision(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	body := map[string]any{
		"title": "My Note",
		"body": "x",
		"tags": []string{"x"},
	}
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, body, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "first create body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, body, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "second create body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "conflict")
}

// PUT happy path: replace a section, etag advances.
func TestUserContent_SectionReplace_HappyPath(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Notes",
		"body": "## Section A\noriginal A\n## Section B\noriginal B\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())
	createETag := rec.Header().Get("ETag")
	require.NotEmpty(t, createETag)

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:notes/sections/section-a", tok,
		map[string]any{"body": "rewritten A\n"},
		map[string]string{"If-Match": createETag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "put body=%s", rec.Body.String())
	newETag := rec.Header().Get("ETag")
	assert.NotEqual(t, createETag, newETag, "etag must advance on edit")

	var got userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "Section A", got.Section.Heading)
	assert.Equal(t, "rewritten A\n", got.Section.Body)

	// Vault file reflects the edit and Section B is untouched.
	v := readVaultByID(t, root, "user-content", "user-content:notes")
	assert.Contains(t, v.CleanContent, "rewritten A")
	assert.Contains(t, v.CleanContent, "original B")
	assert.NotContains(t, v.CleanContent, "original A")
}

// PUT 412 on If-Match mismatch (stale etag).
func TestUserContent_SectionReplace_412OnStaleEtag(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "etag-test",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:etag-test/sections/a", tok,
		map[string]any{"body": "B\n"},
		map[string]string{"If-Match": `"deadbeefdeadbeef"`},
	)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "precondition_failed")
	assert.NotEmpty(t, rec.Header().Get("ETag"), "412 must carry the current ETag for retry")
}

// PUT 428 when If-Match header is missing (precondition required).
func TestUserContent_SectionReplace_428WhenIfMatchMissing(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "no-ifmatch",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:no-ifmatch/sections/a", tok,
		map[string]any{"body": "B\n"}, nil,
	)
	require.Equal(t, http.StatusPreconditionRequired, rec.Code, "body=%s", rec.Body.String())
}

// PUT 403 when a different agent (different operator) tries to edit.
func TestUserContent_SectionReplace_403OnCrossAuthor(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", forgeTok, map[string]any{
		"title": "Locked",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	etag := rec.Header().Get("ETag")

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:locked/sections/a", intruderTok,
		map[string]any{"body": "intruder edit\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "author_mismatch")
}

// PUT succeeds when a different agent shares the same operator (the
// "operator on behalf of any agent" path per ADR-0012).
func TestUserContent_SectionReplace_OperatorCanEditAcrossAgents(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	yaadTok := mintToken(t, signer, "alice2", "alice") // same operator as charlie

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", forgeTok, map[string]any{
		"title": "Shared",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	etag := rec.Header().Get("ETag")

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:shared/sections/a", yaadTok,
		map[string]any{"body": "alice2's edit on charlie's UGC\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// PUT 404 when the section address doesn't resolve.
func TestUserContent_SectionReplace_404OnUnknownSection(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Sec404",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	etag := rec.Header().Get("ETag")

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:sec404/sections/no-such-section", tok,
		map[string]any{"body": "x\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// DELETE happy path: archives, then DELETE removes vault file +
// store rows. Per ADR-0018 step 4 the active-entity DELETE returns
// 409; the operator archive-then-delete two-step is the only path.
func TestUserContent_Delete_HappyPath(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Deleteme",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Archive first (lifecycle prerequisite per ADR-0018 step 4).
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/user-content:deleteme/archive", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "archive prerequisite: body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:deleteme", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got userContentDeleteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.True(t, got.Deleted)

	// Store row gone.
	_, err := st.GetEntity(context.Background(), "user-content:deleteme")
	require.Error(t, err, "entity must be removed from store after DELETE")

	// Vault file gone.
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	_, err = r.ReadByID("user-content", "user-content:deleteme")
	require.Error(t, err, "vault file must be removed")
	require.True(t, vault.IsNotExist(err), "want IsNotExist, got %v", err)
}

// DELETE on active (non-archived) UGC entity returns 409 with the
// ADR-0018 step 4 archive-first hint. Mirror of the entity-route
// state-machine test, exercised via the user-content delete handler
// which has its own pre-flight ArchivedAt gate.
func TestUserContent_Delete_ConflictOnActive(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "StillActive",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:stillactive", tok, nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "must archive before delete", "wire error code")
	assert.Contains(t, body, "/archive first", "hint points at archive path")

	// Untouched: still in DB, still active.
	got, err := st.GetEntity(context.Background(), "user-content:stillactive")
	require.NoError(t, err, "entity must still be in store after rejected DELETE")
	assert.Nil(t, got.ArchivedAt, "still active")
}

// DELETE 403 on cross-author attempt.
func TestUserContent_Delete_403OnCrossAuthor(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	ownerTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", ownerTok, map[string]any{
		"title": "Protected",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:protected", intruderTok, nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "author_mismatch")
}

// DELETE allowed for a different agent sharing the same operator.
// Both archive + delete legs honor the operator-equality check —
// either co-operator agent can perform either leg.
func TestUserContent_Delete_OperatorCanDeleteAcrossAgents(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	yaadTok := mintToken(t, signer, "alice2", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", forgeTok, map[string]any{
		"title": "Shared",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Co-operator alice2 archives + deletes the entity charlie created.
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/user-content:shared/archive", yaadTok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "archive: body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:shared", yaadTok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// Dev-mode (AnonymousAuth bypass): unauthenticated PUT/DELETE work
// without any author/operator enforcement, so existing
// auth.required=false tests + non-auth deploys keep functioning.
func TestUserContent_DevMode_AnonymousBypass(t *testing.T) {
	t.Parallel()
	// No auth wired — handler uses AnonymousAuth.
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	rdr, err := vault.NewReader(root)
	require.NoError(t, err)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(), WithVaultIO(w, rdr))

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", "", map[string]any{
		"title": "Anon",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	etag := rec.Header().Get("ETag")

	// Edit and delete by the same anonymous claim succeed without
	// author/operator enforcement. The state-machine still applies —
	// archive before destroy per ADR-0018 step 4.
	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:anon/sections/a", "",
		map[string]any{"body": "edited\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "PUT body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/user-content:anon/archive", "", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "archive: body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:anon", "", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "DELETE body=%s", rec.Body.String())
}

// Sanity: GET after PUT shows the edit, ETag advanced.
func TestUserContent_Roundtrip_CreateEditRead(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "RT",
		"body": "## a\nfirst\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	originalETag := rec.Header().Get("ETag")

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:rt/sections/a", tok,
		map[string]any{"body": "second\n"},
		map[string]string{"If-Match": originalETag},
	)
	require.Equal(t, http.StatusOK, rec.Code)
	newETag := rec.Header().Get("ETag")
	require.NotEqual(t, originalETag, newETag)

	rec = ugcReq(t, h, http.MethodGet, "/v1/user-content/user-content:rt", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, newETag, rec.Header().Get("ETag"))
	var got userContentEntityResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Len(t, got.Sections.Entries, 1)
	assert.Equal(t, "second\n", got.Sections.Entries[0].Body)

	// Provenance accumulated: create row + edit row.
	require.GreaterOrEqual(t, len(got.Provenance), 2)
}

// Sanity: timestamps are stamped on success-path provenance rows.
func TestUserContent_Create_ProvenanceTimestamp(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")

	before := time.Now().UTC().Add(-time.Second)
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": "Ts",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	after := time.Now().UTC().Add(time.Second)

	v := readVaultByID(t, root, "user-content", "user-content:ts")
	require.Len(t, v.Provenance, 1)
	require.NotNil(t, v.Provenance[0].FetchedAt)
	require.True(t, v.Provenance[0].FetchedAt.After(before) && v.Provenance[0].FetchedAt.Before(after),
		"FetchedAt must be a stamped timestamp inside the test window; got %v", v.Provenance[0].FetchedAt)
}
