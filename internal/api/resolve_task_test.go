// Tests for the #304 Cut C3.3 resolution-task resolve flow.
// Covers the on-disk frontmatter parser, the resolve-time
// edge branching (create / unchanged / rewrite via Cut B's
// update_edge_target), the option-must-be-in-recorded-list
// gate, and the end-to-end HTTP path.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/tasks"
)

// newAPIWithResolutionTaskWiring constructs an api.Handler
// wired with a tasks reader / writer + a fake SyncIngester
// so the Cut C3.3 HTTP path can be exercised end-to-end.
// Returns the handler, store, the on-disk tasks/ dir (so
// tests can drop resolution-task files into it), and the
// fake ingester (so tests can stage IngestByName returns).
func newAPIWithResolutionTaskWiring(t *testing.T) (http.Handler, store.Store, string, *fakeSyncIngester) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	vault := t.TempDir()
	tasksDir := filepath.Join(vault, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))
	reader := tasks.NewReader(vault)
	writer := tasks.NewWriter(vault)
	ingester := &fakeSyncIngester{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithTasksReader(reader),
		WithTasksWriter(writer),
		WithSyncIngester(ingester),
	)
	return h, st, tasksDir, ingester
}

func writeResolutionTaskFile(t *testing.T, dir, id string, fm map[string]any, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// Render frontmatter via yaml-equivalent ordering by writing
	// the lines explicitly — gives the tests control over the
	// order and lets us assert against parsing without depending
	// on yaml.Marshal's key sort.
	var b strings.Builder
	b.WriteString("---\n")
	for k, v := range fm {
		switch x := v.(type) {
		case string:
			fmt.Fprintf(&b, "%s: %s\n", k, x)
		case int:
			fmt.Fprintf(&b, "%s: %d\n", k, x)
		case []map[string]string:
			fmt.Fprintf(&b, "%s:\n", k)
			for _, opt := range x {
				fmt.Fprintf(&b, "  - id: %q\n", opt["id"])
				if l := opt["label"]; l != "" {
					fmt.Fprintf(&b, "    label: %q\n", l)
				}
			}
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte(b.String()), 0o644))
}

// TestParseResolutionTaskFile_HappyPath pins that the on-disk
// shape FileTaskWriter.WriteResolutionTask renders (per Cut
// C3.1) round-trips through parseResolutionTaskFile.
func TestParseResolutionTaskFile_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeResolutionTaskFile(t, dir, "boardgame-deadbeef", map[string]any{
		"kind":                  "resolution-task",
		"schema_version":        1,
		"idempotency_key":       "boardgame-deadbeef",
		"from_id":               "email:m1",
		"edge_type":             "mentions",
		"target_kind":           "boardgame",
		"resolver_plugin":       "yaad-bgg",
		"normalized_raw_target": "brass",
		"raw_target":            "Brass",
		"options": []map[string]string{
			{"id": "boardgame:brass-birmingham", "label": "Brass: Birmingham"},
			{"id": "boardgame:brass-lancashire", "label": "Brass: Lancashire"},
		},
	}, "## Resolution\n\n- [ ] boardgame:brass-birmingham\n- [ ] boardgame:brass-lancashire\n")

	fm, err := parseResolutionTaskFile(dir, "boardgame-deadbeef")
	require.NoError(t, err)
	assert.Equal(t, "resolution-task", fm.Kind)
	assert.Equal(t, 1, fm.SchemaVersion)
	assert.Equal(t, "email:m1", fm.FromID)
	assert.Equal(t, "mentions", fm.EdgeType)
	assert.Equal(t, "boardgame", fm.TargetKind)
	assert.Equal(t, "yaad-bgg", fm.ResolverPlugin)
	assert.Equal(t, "brass", fm.NormalizedRawTarget)
	assert.Equal(t, "Brass", fm.RawTarget)
	require.Len(t, fm.Options, 2)
	assert.Equal(t, "boardgame:brass-birmingham", fm.Options[0].ID)
}

// TestParseResolutionTaskFile_PathTraversalRejected pins
// that ids carrying `/` or `\` reject before any read —
// defends against an attacker passing `../etc/passwd`
// shaped strings into the resolve handler.
func TestParseResolutionTaskFile_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	_, err := parseResolutionTaskFile(t.TempDir(), "../etc/passwd")
	require.Error(t, err)
}

// TestParseResolutionTaskFile_WrongKindRejected pins the
// kind-guard: a legacy text-task file (kind: task) is NOT a
// resolution-task — the resolve handler must surface that
// rather than mis-resolving against the wrong shape.
func TestParseResolutionTaskFile_WrongKindRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeResolutionTaskFile(t, dir, "legacy-task", map[string]any{
		"kind": "task",
	}, "")
	_, err := parseResolutionTaskFile(dir, "legacy-task")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not resolution-task")
}

// TestParseResolutionTaskFile_NotFound pins the typed-error
// shape so the HTTP layer can branch to 404.
func TestParseResolutionTaskFile_NotFound(t *testing.T) {
	t.Parallel()
	_, err := parseResolutionTaskFile(t.TempDir(), "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestResolveOptionInTask_MatchAndMiss pins the option-must-
// be-in-recorded-list gate.
func TestResolveOptionInTask_MatchAndMiss(t *testing.T) {
	t.Parallel()
	fm := &resolutionTaskFM{
		Options: []resolutionTaskFMOption{
			{ID: "boardgame:brass-birmingham"},
			{ID: "boardgame:brass-lancashire"},
		},
	}
	got, err := resolveOptionInTask(fm, "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got.ID)

	_, err = resolveOptionInTask(fm, "boardgame:caverna")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the recorded options")

	_, err = resolveOptionInTask(fm, "   ")
	require.Error(t, err)
}

// TestResolveResolutionTaskEdge_CreatesFresh pins the
// no-prior-edge branch: the resolve handler lands a fresh
// edge from (from_id, edge_type) to the chosen entity.
func TestResolveResolutionTaskEdge_CreatesFresh(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")

	finalID, outcome, err := resolveResolutionTaskEdge(context.Background(), st,
		"email:m1", "mentions", "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", finalID)
	assert.Equal(t, resolveEdgeCreated, outcome)

	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "boardgame:brass-birmingham", edges[0].To)
}

// TestResolveResolutionTaskEdge_UnchangedOnIdempotentResolve
// pins the no-op idempotency path: if the edge already
// points at the chosen target, the handler does nothing.
func TestResolveResolutionTaskEdge_UnchangedOnIdempotentResolve(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass-birmingham",
	}))

	finalID, outcome, err := resolveResolutionTaskEdge(context.Background(), st,
		"email:m1", "mentions", "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", finalID)
	assert.Equal(t, resolveEdgeUnchanged, outcome)
}

// TestResolveResolutionTaskEdge_RewritesStaleTarget pins
// the Cut B handoff: a prior edge with To != chosen routes
// through store.UpdateEdgeTarget for the atomic rewrite.
func TestResolveResolutionTaskEdge_RewritesStaleTarget(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEntity(t, st, "boardgame:brass-lancashire", "boardgame")
	// Prior edge points at lancashire — operator now picks
	// birmingham; resolveResolutionTaskEdge rewrites.
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass-lancashire",
	}))

	finalID, outcome, err := resolveResolutionTaskEdge(context.Background(), st,
		"email:m1", "mentions", "boardgame:brass-birmingham")
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", finalID)
	assert.Equal(t, resolveEdgeRewritten, outcome)

	edges, err := st.GetEdgesFor(context.Background(), "email:m1", []string{"mentions"})
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "boardgame:brass-birmingham", edges[0].To)
}

// fakeSyncIngester is a SyncIngester stub the resolve-flow
// HTTP test wires so the handler doesn't need a real plugin
// registry + tracker. Records the IngestByName call shape so
// the test asserts the resolve handler routed the operator's
// pick through the resolver plugin per the locked design.
type fakeSyncIngester struct {
	gotPlugin     string
	gotTargetKind string
	gotName       string
	returnID      string
	returnOptions map[string]plugins.DisambiguationOption
	returnErr     error
}

func (f *fakeSyncIngester) IngestURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", errors.New("fakeSyncIngester: IngestURL not implemented")
}

func (f *fakeSyncIngester) IngestByName(_ context.Context, pluginName, targetKind, name string, _ time.Duration) (string, map[string]plugins.DisambiguationOption, error) {
	f.gotPlugin = pluginName
	f.gotTargetKind = targetKind
	f.gotName = name
	return f.returnID, f.returnOptions, f.returnErr
}

// TestHTTPResolveResolutionTask_EndToEnd pins the wire path:
// POST /v1/tasks/{id}/resolve with {option: "..."} reads the
// on-disk task → re-ingests via the resolver plugin → lands
// the canonical edge → archives the task → returns the
// resolution-task wire envelope.
func TestHTTPResolveResolutionTask_EndToEnd(t *testing.T) {
	t.Parallel()
	h, st, dir, ingester := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "boardgame-deadbeef", map[string]any{
		"kind":            "resolution-task",
		"schema_version":  1,
		"idempotency_key": "boardgame-deadbeef",
		"from_id":         "email:m1",
		"edge_type":       "mentions",
		"target_kind":     "boardgame",
		"resolver_plugin": "yaad-bgg",
		"raw_target":      "Brass",
		"options": []map[string]string{
			{"id": "boardgame:brass-birmingham"},
		},
	}, "## Resolution\n")
	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	ingester.returnID = "boardgame:brass-birmingham"

	body := strings.NewReader(`{"option":"boardgame:brass-birmingham"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/boardgame-deadbeef/resolve", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp resolveResolutionTaskResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "boardgame-deadbeef", resp.ID)
	assert.True(t, resp.AutoArchived)
	assert.Equal(t, "boardgame:brass-birmingham", resp.ChosenID)
	assert.Equal(t, string(resolveEdgeCreated), resp.EdgeOutcome)
	assert.Equal(t, "email:m1", resp.FromID)
	assert.Equal(t, "mentions", resp.EdgeType)
	assert.Equal(t, "boardgame", resp.TargetKind)

	// Ingester routed correctly.
	assert.Equal(t, "yaad-bgg", ingester.gotPlugin)
	assert.Equal(t, "boardgame", ingester.gotTargetKind)
	assert.Equal(t, "boardgame:brass-birmingham", ingester.gotName)

	// Edge landed.
	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "boardgame:brass-birmingham", edges[0].To)

	// Active task file archived.
	_, err = os.Stat(filepath.Join(dir, "boardgame-deadbeef.md"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "_archive", "boardgame-deadbeef.md"))
	assert.NoError(t, err)
}

// TestHTTPResolveResolutionTask_OptionNotInList pins the
// gate: an operator pick that isn't in the task's recorded
// options rejects with 400.
func TestHTTPResolveResolutionTask_OptionNotInList(t *testing.T) {
	t.Parallel()
	h, _, dir, _ := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "boardgame-deadbeef", map[string]any{
		"kind":            "resolution-task",
		"schema_version":  1,
		"from_id":         "email:m1",
		"edge_type":       "mentions",
		"target_kind":     "boardgame",
		"resolver_plugin": "yaad-bgg",
		"raw_target":      "Brass",
		"options": []map[string]string{
			{"id": "boardgame:brass-birmingham"},
		},
	}, "")

	body := strings.NewReader(`{"option":"boardgame:not-in-list"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/boardgame-deadbeef/resolve", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not in the recorded options")
}

// TestHTTPResolveResolutionTask_StaleRewrite pins the
// Cut B handoff at the HTTP layer: a prior edge for the
// (from, type) tuple pointing at a different target is
// redirected via update_edge_target; the wire envelope
// reports edge_outcome=rewritten.
func TestHTTPResolveResolutionTask_StaleRewrite(t *testing.T) {
	t.Parallel()
	h, st, dir, ingester := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "boardgame-deadbeef", map[string]any{
		"kind":            "resolution-task",
		"schema_version":  1,
		"from_id":         "email:m1",
		"edge_type":       "mentions",
		"target_kind":     "boardgame",
		"resolver_plugin": "yaad-bgg",
		"raw_target":      "Brass",
		"options": []map[string]string{
			{"id": "boardgame:brass-birmingham"},
		},
	}, "")
	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")
	seedEntity(t, st, "boardgame:brass-lancashire", "boardgame")
	require.NoError(t, st.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass-lancashire",
	}))
	ingester.returnID = "boardgame:brass-birmingham"

	body := strings.NewReader(`{"option":"boardgame:brass-birmingham"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/boardgame-deadbeef/resolve", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp resolveResolutionTaskResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, string(resolveEdgeRewritten), resp.EdgeOutcome)
}

// TestHTTPResolveResolutionTask_DisambiguationOnReingest
// pins the safety net: if the plugin re-disambiguates on the
// option-as-shorthand re-ingest (the option set shifted
// since the workflow fired), the wire surfaces 409 so the
// operator re-fires the workflow.
func TestHTTPResolveResolutionTask_DisambiguationOnReingest(t *testing.T) {
	t.Parallel()
	h, _, dir, ingester := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "boardgame-deadbeef", map[string]any{
		"kind":            "resolution-task",
		"schema_version":  1,
		"from_id":         "email:m1",
		"edge_type":       "mentions",
		"target_kind":     "boardgame",
		"resolver_plugin": "yaad-bgg",
		"raw_target":      "Brass",
		"options": []map[string]string{
			{"id": "boardgame:brass-birmingham"},
		},
	}, "")
	ingester.returnOptions = map[string]plugins.DisambiguationOption{
		"boardgame:brass-birmingham": {Label: "Brass: Birmingham"},
		"boardgame:brass-lancashire": {Label: "Brass: Lancashire"},
	}

	body := strings.NewReader(`{"option":"boardgame:brass-birmingham"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/boardgame-deadbeef/resolve", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
}

// TestHTTPResolveResolutionTask_LegacyTaskRejected pins
// that a legacy text-task (kind: task) cannot be resolved
// through the resolution-task branch — the `option` field
// only applies to typed resolution-tasks.
func TestHTTPResolveResolutionTask_LegacyTaskRejected(t *testing.T) {
	t.Parallel()
	h, _, dir, _ := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "legacy", map[string]any{
		"kind": "task",
	}, "")

	body := strings.NewReader(`{"option":"boardgame:x"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/legacy/resolve", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHTTPResolve_EmptyBodyChunkedLegacyPath pins the PR-311
// catch: when a client POSTs with an unknown Content-Length
// (chunked encoding or no Content-Length header at all) and
// an empty body, json.Decoder.Decode returns io.EOF. The
// resolve handler must treat that as the legacy path — not
// surface a 400 decode error. ContentLength=-1 simulates the
// shape httptest doesn't auto-populate.
func TestHTTPResolve_EmptyBodyChunkedLegacyPath(t *testing.T) {
	t.Parallel()
	h, _, dir, _ := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "legacy", map[string]any{
		"kind":     "task",
		"workflow": "wf",
	}, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/legacy/resolve",
		http.NoBody) // non-nil empty body
	req.ContentLength = -1 // simulate chunked-encoding / unknown-length
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusBadRequest, rec.Code,
		"empty chunked body must route through the legacy path, not 400 on json.EOF; body=%s", rec.Body.String())
	assert.True(t, rec.Code < 500, "no server error expected; got %d (%s)", rec.Code, rec.Body.String())
}

// TestHTTPResolve_LegacyPathStillWorks pins that an empty
// body on a legacy text-task continues to route through the
// pre-C3 resolve path — the C3.3 branch is additive.
func TestHTTPResolve_LegacyPathStillWorks(t *testing.T) {
	t.Parallel()
	h, _, dir, _ := newAPIWithResolutionTaskWiring(t)
	writeResolutionTaskFile(t, dir, "legacy", map[string]any{
		"kind":     "task",
		"workflow": "wf",
	}, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/legacy/resolve", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Either 200 or whatever the legacy path returns — assert it's
	// NOT a 400 from the resolution-task branch.
	assert.NotEqual(t, http.StatusBadRequest, rec.Code)
	assert.True(t, rec.Code < 500, "legacy resolve should not error out at server")
	// Drain body so the response is closed cleanly.
	_, _ = io.Copy(io.Discard, rec.Body)
}
