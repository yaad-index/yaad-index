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
func TestUserContent_SectionReplace_403OnCrossOperator(t *testing.T) {
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
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
}

// PUT succeeds when a different agent shares the same operator —
// per #377 the edit gate keys on operator pair-claim equality only;
// the original author is provenance, not a permission grant.
// Also pins the AC that the entity's stored `author` is preserved
// after a co-operator edit (provenance survives the operation).
func TestUserContent_SectionReplace_OperatorCanEditAcrossAgents(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	creatorTok := mintToken(t, signer, "agent-creator", "alice")
	otherAgentTok := mintToken(t, signer, "agent-editor", "alice") // same operator, different agent

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", creatorTok, map[string]any{
		"title": "Shared",
		"body": "## a\nA\n",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	etag := rec.Header().Get("ETag")

	rec = ugcReq(t, h, http.MethodPut, "/v1/user-content/user-content:shared/sections/a", otherAgentTok,
		map[string]any{"body": "co-operator edit lands\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// #377 AC: original author preserved on the entity after a
	// co-operator edit (provenance, not permission).
	got := readVaultByID(t, root, "user-content", "user-content:shared")
	assert.Equal(t, "agent-creator", got.Data["author"],
		"entity author must survive co-operator edits as provenance record")
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
	assert.Contains(t, body, "not_archived", "wire error code")
	assert.Contains(t, body, "/archive first", "hint points at archive path")

	// Untouched: still in DB, still active.
	got, err := st.GetEntity(context.Background(), "user-content:stillactive")
	require.NoError(t, err, "entity must still be in store after rejected DELETE")
	assert.Nil(t, got.ArchivedAt, "still active")
}

// DELETE 403 on cross-author attempt.
func TestUserContent_Delete_403OnCrossOperator(t *testing.T) {
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
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
}

// DELETE allowed for a different agent sharing the same operator.
// Both archive + delete legs honor the operator-equality check —
// either co-operator agent can perform either leg.
func TestUserContent_Delete_OperatorCanDeleteAcrossAgents(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	alice2Tok := mintToken(t, signer, "alice2", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", forgeTok, map[string]any{
		"title": "Shared",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Co-operator alice2 archives + deletes the entity charlie created.
	rec = ugcReq(t, h, http.MethodPost, "/v1/entities/user-content:shared/archive", alice2Tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "archive: body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:shared", alice2Tok, nil, nil)
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

// --- Section add (#299) --------------------------------------------------

func createUGCWithBody(t *testing.T, h http.Handler, tok, title, body string) string {
	t.Helper()
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", tok, map[string]any{
		"title": title,
		"body":  body,
		"tags":  []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)
	return etag
}

func TestUserContent_SectionAdd_AppendsAtEnd(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add1", "## First\nfirst\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add1/sections", tok,
		map[string]any{"heading": "Second", "body": "second body\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	newETag := rec.Header().Get("ETag")
	require.NotEqual(t, etag, newETag, "etag must advance")

	var got userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.True(t, got.OK)
	require.Equal(t, "Second", got.Section.Heading)

	v := readVaultByID(t, root, "user-content", "user-content:add1")
	require.Contains(t, v.CleanContent, "## First")
	require.Contains(t, v.CleanContent, "## Second")
	require.Contains(t, v.CleanContent, "second body")
}

func TestUserContent_SectionAdd_AfterSpecificSection(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add2", "## A\na\n## C\nc\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add2/sections", tok,
		map[string]any{"after_sec": "a", "heading": "B", "body": "b body\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	v := readVaultByID(t, root, "user-content", "user-content:add2")
	// Order in file: A, B, C.
	aPos := strings.Index(v.CleanContent, "## A")
	bPos := strings.Index(v.CleanContent, "## B")
	cPos := strings.Index(v.CleanContent, "## C")
	require.True(t, aPos < bPos && bPos < cPos, "expected A < B < C ordering; got positions A=%d B=%d C=%d", aPos, bPos, cPos)
}

func TestUserContent_SectionAdd_PrependWithAfterSecNegativeOne(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add3", "## Existing\nx\n")

	negOne := "-1"
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add3/sections", tok,
		map[string]any{"after_sec": negOne, "heading": "New", "body": "n\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	v := readVaultByID(t, root, "user-content", "user-content:add3")
	newPos := strings.Index(v.CleanContent, "## New")
	existingPos := strings.Index(v.CleanContent, "## Existing")
	require.True(t, newPos < existingPos, "New must precede Existing")
}

func TestUserContent_SectionAdd_412OnStaleEtag(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	createUGCWithBody(t, h, tok, "Add4", "## A\na\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add4/sections", tok,
		map[string]any{"heading": "B", "body": "b\n"},
		map[string]string{"If-Match": `"stale"`},
	)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, "body=%s", rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("ETag"))
}

func TestUserContent_SectionAdd_428WhenIfMatchMissing(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	createUGCWithBody(t, h, tok, "Add5", "## A\na\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add5/sections", tok,
		map[string]any{"heading": "B", "body": "b\n"}, nil,
	)
	require.Equal(t, http.StatusPreconditionRequired, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionAdd_403OnCrossOperator(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")
	etag := createUGCWithBody(t, h, forgeTok, "Add6", "## A\na\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add6/sections", intruderTok,
		map[string]any{"heading": "B", "body": "b\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "operator_mismatch")
}

// Pin the containment-aware sibling check: same slug at the same
// depth under DIFFERENT parents is legal per the containment
// model — they aren't siblings of each other. Adding a
// `### Notes` under `## B` must succeed even when `## A` already
// contains `### Notes`. The response must echo the NEW section
// (located by byte offset), not the pre-existing same-slug
// section under the other parent.
func TestUserContent_SectionAdd_AllowsSameSlugUnderDifferentParents(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add9", "## A\nA body\n### Notes\nan note\n## B\nB body\n")

	// Add `### Notes` under ## B (afterSec=b → parent ## B at depth 2).
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add9/sections", tok,
		map[string]any{"after_sec": "b", "heading": "Notes", "body": "another note\n", "depth": 3},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusCreated, rec.Code,
		"adding `### Notes` under ## B must NOT collide with `### Notes` under ## A — body=%s", rec.Body.String())

	v := readVaultByID(t, root, "user-content", "user-content:add9")
	// Both `### Notes` headings now present (one under each parent).
	require.Equal(t, 2, strings.Count(v.CleanContent, "### Notes"),
		"two `### Notes` headings, one under each parent")

	// Response must echo the NEW section, not the pre-existing
	// same-slug one under ## A. Locate ## B's heading byte offset
	// in the post-write body and assert the returned section's
	// ByteOffset is AFTER it.
	bHeadingPos := strings.Index(v.CleanContent, "## B")
	require.GreaterOrEqual(t, bHeadingPos, 0)

	var got userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.True(t, got.OK)
	require.Equal(t, "Notes", got.Section.Heading)
	require.Greater(t, got.Section.ByteOffset, bHeadingPos,
		"response must echo the newly-inserted ### Notes under ## B, not the pre-existing one under ## A")
	require.Equal(t, "another note\n", got.Section.Body,
		"response body must be the new section's body, not the old one's")
}

// Pin the containment-aware sibling check for the same-parent
// collision path stays — adding `### Notes` AGAIN under ## A must
// still 409.
func TestUserContent_SectionAdd_RejectsSameSlugUnderSameParent(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add10", "## A\nA body\n### Notes\nan note\n## B\nB body\n")

	// Add `### Notes` after the existing `### Notes` (after_sec=notes)
	// — same parent ## A, must reject.
	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add10/sections", tok,
		map[string]any{"after_sec": "notes", "heading": "Notes", "body": "dup\n", "depth": 3},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionAdd_409OnSlugCollision(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add7", "## Same\nx\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add7/sections", tok,
		map[string]any{"heading": "Same", "body": "y\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "conflict")
}

func TestUserContent_SectionAdd_400OnMissingHeading(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Add8", "## A\na\n")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:add8/sections", tok,
		map[string]any{"body": "b\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// --- Section rename (#299) ----------------------------------------------

func TestUserContent_SectionRename_HappyPath(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Rn1", "## Old\nbody A\n### nested\nx\n## B\nb\n")

	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn1/sections/old/heading", tok,
		map[string]any{"new_heading": "New"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	newETag := rec.Header().Get("ETag")
	require.NotEqual(t, etag, newETag)

	var got userContentSectionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.Equal(t, "New", got.Section.Heading)

	v := readVaultByID(t, root, "user-content", "user-content:rn1")
	require.Contains(t, v.CleanContent, "## New\nbody A")
	require.NotContains(t, v.CleanContent, "## Old\n", "old heading line removed")
	require.Contains(t, v.CleanContent, "### nested\nx", "nested heading preserved")
	require.Contains(t, v.CleanContent, "## B\nb", "sibling preserved")
}

func TestUserContent_SectionRename_412OnStaleEtag(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	createUGCWithBody(t, h, tok, "Rn2", "## A\nx\n")

	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn2/sections/a/heading", tok,
		map[string]any{"new_heading": "B"},
		map[string]string{"If-Match": `"stale"`},
	)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionRename_403OnCrossOperator(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")
	etag := createUGCWithBody(t, h, forgeTok, "Rn3", "## A\nx\n")

	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn3/sections/a/heading", intruderTok,
		map[string]any{"new_heading": "B"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// Pin the containment-aware sibling check on the rename path: the
// new heading-slug can match a section under a DIFFERENT parent
// without colliding (they aren't siblings).
func TestUserContent_SectionRename_AllowsSameSlugUnderDifferentParents(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Rn7",
		"## A\nA body\n### Notes\nan note\n## B\nB body\n### Other\nother\n")

	// Rename ### Other (under ## B) to "Notes". The other ### Notes
	// is under ## A — different parent, so no collision.
	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn7/sections/other/heading", tok,
		map[string]any{"new_heading": "Notes"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code,
		"renaming ### Other under ## B to ### Notes must NOT collide with ### Notes under ## A — body=%s", rec.Body.String())

	v := readVaultByID(t, root, "user-content", "user-content:rn7")
	require.Equal(t, 2, strings.Count(v.CleanContent, "### Notes"),
		"both `### Notes` headings present, one under each parent")
}

func TestUserContent_SectionRename_409OnSiblingCollision(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Rn4", "## A\na\n## B\nb\n")

	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn4/sections/a/heading", tok,
		map[string]any{"new_heading": "B"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "conflict")
}

func TestUserContent_SectionRename_404OnUnknownSection(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Rn5", "## A\nx\n")

	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn5/sections/missing/heading", tok,
		map[string]any{"new_heading": "X"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionRename_400OnPreHeadingSection(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Rn6", "intro\n## A\nx\n")

	rec := ugcReq(t, h, http.MethodPatch, "/v1/user-content/user-content:rn6/sections/0/heading", tok,
		map[string]any{"new_heading": "X"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// --- Section delete (#299) ----------------------------------------------

func TestUserContent_SectionDelete_HappyPath(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Dl1", "## A\na\n## B\nb\n## C\nc\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl1/sections/b", tok, nil,
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	newETag := rec.Header().Get("ETag")
	require.NotEqual(t, etag, newETag)

	var got userContentSectionDeleteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	require.True(t, got.OK)
	require.Equal(t, 1, got.RemovedIdx, "## B was at positional index 1 in the original parse")

	v := readVaultByID(t, root, "user-content", "user-content:dl1")
	require.NotContains(t, v.CleanContent, "## B")
	require.Contains(t, v.CleanContent, "## A")
	require.Contains(t, v.CleanContent, "## C")
}

func TestUserContent_SectionDelete_RemovesNestedSubtree(t *testing.T) {
	t.Parallel()
	h, _, root, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Dl2", "## Parent\np\n### Child\nc\n## Sib\ns\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl2/sections/parent", tok, nil,
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	v := readVaultByID(t, root, "user-content", "user-content:dl2")
	require.NotContains(t, v.CleanContent, "## Parent")
	require.NotContains(t, v.CleanContent, "### Child", "nested heading deleted with parent")
	require.Contains(t, v.CleanContent, "## Sib")
}

func TestUserContent_SectionDelete_412OnStaleEtag(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	createUGCWithBody(t, h, tok, "Dl3", "## A\na\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl3/sections/a", tok, nil,
		map[string]string{"If-Match": `"stale"`},
	)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionDelete_428WhenIfMatchMissing(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	createUGCWithBody(t, h, tok, "Dl4", "## A\na\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl4/sections/a", tok, nil, nil)
	require.Equal(t, http.StatusPreconditionRequired, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionDelete_403OnCrossOperator(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	forgeTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")
	etag := createUGCWithBody(t, h, forgeTok, "Dl5", "## A\na\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl5/sections/a", intruderTok, nil,
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionDelete_404OnUnknownSection(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Dl6", "## A\na\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl6/sections/missing", tok, nil,
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

func TestUserContent_SectionDelete_400OnPreHeadingSection(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Dl7", "intro\n## A\nx\n")

	rec := ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:dl7/sections/0", tok, nil,
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// --- Round-trip (#299 acceptance) --------------------------------------

func TestUserContent_Section_RoundTrip_GetAddGetDeleteGet(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	tok := mintToken(t, signer, "the implementer", "alice")
	etag := createUGCWithBody(t, h, tok, "Rt1", "## A\na\n")

	// GET — section B not present.
	rec := ugcReq(t, h, http.MethodGet, "/v1/user-content/user-content:rt1/sections", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), `"heading":"B"`)

	// ADD B.
	rec = ugcReq(t, h, http.MethodPost, "/v1/user-content/user-content:rt1/sections", tok,
		map[string]any{"heading": "B", "body": "b\n"},
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	etag = rec.Header().Get("ETag")

	// GET — section B present.
	rec = ugcReq(t, h, http.MethodGet, "/v1/user-content/user-content:rt1/sections", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"heading":"B"`)

	// DELETE B.
	rec = ugcReq(t, h, http.MethodDelete, "/v1/user-content/user-content:rt1/sections/b", tok, nil,
		map[string]string{"If-Match": etag},
	)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// GET — section B absent again.
	rec = ugcReq(t, h, http.MethodGet, "/v1/user-content/user-content:rt1/sections", tok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), `"heading":"B"`)
}

// TestUserContent_GenericArchive_403OnCrossOperator pins the nava-
// catch bypass closed per #377: a UGC entity reachable via
// POST /v1/entities/{id}/archive must enforce the same
// operator-equality gate as POST /v1/user-content/{id}/archive,
// otherwise a cross-operator caller can archive a UGC entity through
// the generic surface without ever hitting `operator_mismatch`.
func TestUserContent_GenericArchive_403OnCrossOperator(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	ownerTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", ownerTok, map[string]any{
		"title": "Cross-op archive bait",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = ugcReq(t, h, http.MethodPost,
		"/v1/entities/user-content:cross-op-archive-bait/archive", intruderTok, nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
}

// TestUserContent_GenericDelete_403OnCrossOperator pins the second
// half of the nava-catch closure: DELETE /v1/entities/{id} also
// gates UGC kind through canEditByOperator. Archiving via the
// owner first (legitimate) so the destroy-side gate is the only
// thing that can let the intruder through; intruder must hit 403
// before the archive-state check ever runs.
func TestUserContent_GenericDelete_403OnCrossOperator(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	ownerTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", ownerTok, map[string]any{
		"title": "Cross-op delete bait",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	// Legitimate archive via owner so the row is in destroy-eligible state.
	rec = ugcReq(t, h, http.MethodPost,
		"/v1/entities/user-content:cross-op-delete-bait/archive", ownerTok, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "owner-archive must succeed")

	// Intruder hits the generic delete on an archived UGC entity —
	// must get 403 from the operator gate, NOT 200 (silent destroy)
	// and NOT 409 (must-archive-first bypass on a fresh entity).
	rec = ugcReq(t, h, http.MethodDelete,
		"/v1/entities/user-content:cross-op-delete-bait", intruderTok, nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
}

// TestUserContent_GenericDelete_ActiveRow_403BeforeArchiveCheck pins
// the order-of-checks fix from the second nava-catch on #377:
// the operator gate must run BEFORE the ADR-0018 archive-first
// gate so a cross-operator intruder learns 403 (operator_mismatch),
// not 409 (not_archived). Otherwise the lifecycle hint
// leaks existence + active state of someone else's UGC entity. The
// UGC-specific delete path puts authorization first for the same
// reason — see user_content_write.go:1356.
func TestUserContent_GenericDelete_ActiveRow_403BeforeArchiveCheck(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newAuthedUGCFixture(t)
	ownerTok := mintToken(t, signer, "the implementer", "alice")
	intruderTok := mintToken(t, signer, "stranger", "different-op")

	rec := ugcReq(t, h, http.MethodPost, "/v1/user-content", ownerTok, map[string]any{
		"title": "Cross-op active probe",
		"body": "x",
		"tags": []string{"x"},
	}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Intruder hits generic delete on the ACTIVE row. Must get 403
	// from the operator gate, NOT 409 from the archive-first check.
	rec = ugcReq(t, h, http.MethodDelete,
		"/v1/entities/user-content:cross-op-active-probe", intruderTok, nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_mismatch")
	assert.NotContains(t, rec.Body.String(), "not_archived",
		"lifecycle hint must not leak to a cross-operator caller")
}
