// Tests for the per-workflow CRUD surface per #277:
//   - GET    /v1/workflows/{name}
//   - PUT    /v1/workflows/{name}
//   - DELETE /v1/workflows/{name}

package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
)

// validWorkflowMarkdown returns a minimal-but-parseable workflow
// file body with the given `name`. Used by the PUT-path tests so
// each one can set the name independently without re-typing the
// frontmatter / yaml shape.
func validWorkflowMarkdown(name string) string {
	return `---
name: ` + name + `
status: active
---

# ` + name + `

` + "```yaml\n" +
		`allowed_plugins: []
trigger:
  type: manual
actions:
  - add_note:
      content: "hello from ` + name + `"
` + "```\n"
}

// newCRUDFixture builds an api Handler wired with a workflow
// directory + nothing else. Returns the handler + the dir so
// individual tests can seed files / assert post-conditions on
// disk.
func newCRUDFixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	dir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, nil, WithWorkflowDir(dir))
	return h, dir
}

// TestWorkflowGet_HappyPath pins the GET path: a seeded file at
// `<dir>/<name>.md` returns its raw body as text/markdown.
func TestWorkflowGet_HappyPath(t *testing.T) {
	t.Parallel()
	h, dir := newCRUDFixture(t)
	body := validWorkflowMarkdown("alpha-flow")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alpha-flow.md"), []byte(body), 0o644))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/alpha-flow", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "text/markdown; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, body, rec.Body.String())
}

// TestWorkflowGet_NotFound pins the 404 path: a missing file
// returns a structured not_found error.
func TestWorkflowGet_NotFound(t *testing.T) {
	t.Parallel()
	h, _ := newCRUDFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/never-existed", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not_found")
}

// TestWorkflowGet_InvalidName pins the 400 path: a path-segment
// that doesn't match workflowNameRE rejects before any FS access.
func TestWorkflowGet_InvalidName(t *testing.T) {
	t.Parallel()
	h, _ := newCRUDFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/Has-Uppercase", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid")
}

// TestWorkflowDefine_HappyPath pins the PUT path: a valid body
// at a new name writes the file at `<dir>/<name>.md` + returns
// 200.
func TestWorkflowDefine_HappyPath(t *testing.T) {
	t.Parallel()
	h, dir := newCRUDFixture(t)
	body := validWorkflowMarkdown("beta-flow")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflows/beta-flow", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	written, err := os.ReadFile(filepath.Join(dir, "beta-flow.md"))
	require.NoError(t, err)
	assert.Equal(t, body, string(written))
}

// TestWorkflowDefine_Idempotent pins overwrite semantics: a
// second PUT with new content replaces the file in place.
func TestWorkflowDefine_Idempotent(t *testing.T) {
	t.Parallel()
	h, dir := newCRUDFixture(t)

	first := validWorkflowMarkdown("gamma-flow")
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPut, "/v1/workflows/gamma-flow", strings.NewReader(first))
	h.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Second PUT with the same name + slightly different
	// content. The action's content changes; everything else
	// matches.
	second := strings.Replace(first, "hello from gamma-flow", "second iteration", 1)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPut, "/v1/workflows/gamma-flow", strings.NewReader(second))
	h.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, "body=%s", rec2.Body.String())

	written, err := os.ReadFile(filepath.Join(dir, "gamma-flow.md"))
	require.NoError(t, err)
	assert.Contains(t, string(written), "second iteration")
	assert.NotContains(t, string(written), "hello from gamma-flow")
}

// TestWorkflowDefine_RejectsMalformedBody pins #277's
// pre-validation contract: a body that fails parser.Parse
// returns 422 + nothing is written to disk.
func TestWorkflowDefine_RejectsMalformedBody(t *testing.T) {
	t.Parallel()
	h, dir := newCRUDFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflows/delta-flow",
		strings.NewReader("this is not a workflow file"))
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid")

	_, err := os.Stat(filepath.Join(dir, "delta-flow.md"))
	assert.True(t, os.IsNotExist(err),
		"rejected body must NOT leave a half-written file on disk")
}

// TestWorkflowDefine_RejectsNameMismatch pins the
// path-name-vs-frontmatter-name canonicalization rule: a body
// whose `name:` field doesn't match the path segment returns 400
// + nothing is written.
func TestWorkflowDefine_RejectsNameMismatch(t *testing.T) {
	t.Parallel()
	h, dir := newCRUDFixture(t)
	body := validWorkflowMarkdown("frontmatter-says-this")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflows/path-says-that",
		strings.NewReader(body))
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "frontmatter")

	_, err := os.Stat(filepath.Join(dir, "path-says-that.md"))
	assert.True(t, os.IsNotExist(err))
}

// TestWorkflowDefine_RejectsPathTraversal pins the
// resolveWorkflowFilePath gate: a name attempting `../escape`
// is rejected by the workflowNameRE pre-check before any FS
// access.
func TestWorkflowDefine_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	h, _ := newCRUDFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflows/..%2Fescape",
		strings.NewReader("anything"))
	h.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusOK, rec.Code,
		"path-traversal attempt MUST NOT succeed; got 200")
}

// TestWorkflowDelete_HappyPath pins the DELETE path: a seeded
// file is removed, response carries `existed: true`.
func TestWorkflowDelete_HappyPath(t *testing.T) {
	t.Parallel()
	h, dir := newCRUDFixture(t)
	path := filepath.Join(dir, "epsilon-flow.md")
	require.NoError(t, os.WriteFile(path, []byte(validWorkflowMarkdown("epsilon-flow")), 0o644))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/epsilon-flow", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"existed":true`)

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

// TestWorkflowDelete_IdempotentOnMissing pins the no-op path:
// deleting a non-existent file returns 200 with `existed:
// false` (idempotent end-state-driver semantics per #277).
func TestWorkflowDelete_IdempotentOnMissing(t *testing.T) {
	t.Parallel()
	h, _ := newCRUDFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/never-existed", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"existed":false`)
}

// TestWorkflowDefine_MissingWorkflowDirReturns500 pins the
// non-existent-dir contract from #277 review feedback: when the
// configured workflowDir does NOT exist at PUT time (operator
// pointed the daemon at the wrong path, or boot-time MkdirAll
// was skipped), the define returns 500 with a structured error
// rather than silently writing to an unexpected location.
//
// In production, cmd/yaad-index/main.go MkdirAlls the dir at
// boot before passing it to WithWorkflowDir, so this 500 path
// only fires when the dir is removed mid-runtime — operators
// see a clear error in the response + the log.
func TestWorkflowDefine_MissingWorkflowDirReturns500(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	parent := t.TempDir()
	missing := filepath.Join(parent, "does-not-exist")
	// NOT MkdirAll'd — simulates the operator pointing at an
	// uncreated path, or the dir being removed mid-runtime.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, nil, WithWorkflowDir(missing))

	body := validWorkflowMarkdown("orphan-flow")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflows/orphan-flow", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "internal_error")
}

// TestWorkflowCRUD_UnregisteredWithoutWorkflowDir pins the
// opt-in shape: a Handler built WITHOUT WithWorkflowDir leaves
// the per-workflow routes unregistered (404). Existing list /
// discover / trigger routes stay registered via
// WithWorkflowEngine (separate option).
func TestWorkflowCRUD_UnregisteredWithoutWorkflowDir(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, nil) // no WithWorkflowDir

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/any-name", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
