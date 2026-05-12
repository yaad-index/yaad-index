package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIngestServer captures the most recent /v1/ingest request body
// + Authorization header, and returns whatever response the test
// configures. Used by the dispatch CLI tests to verify the wire
// shape the CLI produces, without standing up the real daemon.
type fakeIngestServer struct {
	server *httptest.Server
	gotBody string
	gotAuth string
	respBody string
	respCode int
}

func newFakeIngestServer(t *testing.T, respCode int, respBody string) *fakeIngestServer {
	t.Helper()
	f := &fakeIngestServer{respCode: respCode, respBody: respBody}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/ingest" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotBody = string(body)
		f.gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.respCode)
		_, _ = w.Write([]byte(f.respBody))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// TestCommandCmd_BuildsBangShapeInput pins the wire-shape contract
// for `yaad-index command <plugin> <cmd>`: the request body's `url`
// field is the concatenated `<plugin>: !<cmd>` single-string input
// per ADR-0022 §6.
func TestCommandCmd_BuildsBangShapeInput(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusOK, `{"ok":true,"state":"queued","status":"queued"}`)

	cmd := &CommandCmd{
		Plugin: "gmail",
		Command: "fetch",
		DaemonURL: srv.server.URL,
		Wait: 0,
	}
	require.NoError(t, cmd.Run())

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(srv.gotBody), &got))
	assert.Equal(t, "gmail: !fetch", got["url"],
		"command-shape input must be the bang-sigil concatenation")
}

// TestFetchCmd_BuildsURLShapeInput pins the wire-shape for
// `yaad-index fetch <plugin> <pattern>`: the request body's `url` is
// `<plugin>: <pattern>` (no bang sigil).
func TestFetchCmd_BuildsURLShapeInput(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusOK, `{"ok":true,"state":"queued","status":"queued"}`)

	cmd := &FetchCmd{
		Plugin: "wikipedia",
		Pattern: "Tehran",
		DaemonURL: srv.server.URL,
		Wait: 0,
	}
	require.NoError(t, cmd.Run())

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(srv.gotBody), &got))
	assert.Equal(t, "wikipedia: Tehran", got["url"],
		"URL-shape input must be the namespace + pattern concatenation, no sigil")
}

// TestFetchCmd_PreservesMultiTokenPattern pins that a multi-word
// pattern (like a BGG search query) reaches the daemon verbatim.
func TestFetchCmd_PreservesMultiTokenPattern(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusOK, `{"ok":true,"state":"queued","status":"queued"}`)

	cmd := &FetchCmd{
		Plugin: "bgg",
		Pattern: "ticket to ride",
		DaemonURL: srv.server.URL,
		Wait: 0,
	}
	require.NoError(t, cmd.Run())

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(srv.gotBody), &got))
	assert.Equal(t, "bgg: ticket to ride", got["url"])
}

// TestCommandCmd_PassesBearerToken pins the auth header plumbing:
// when --token is set, the request carries Authorization: Bearer.
// (Daemon-side operator-only-claim enforcement lands in; this
// test just confirms the CLI plumbs the token through.)
func TestCommandCmd_PassesBearerToken(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusOK, `{"ok":true,"state":"queued","status":"queued"}`)

	cmd := &CommandCmd{
		Plugin: "gmail",
		Command: "fetch",
		DaemonURL: srv.server.URL,
		Token: "test-jwt-12345",
		Wait: 0,
	}
	require.NoError(t, cmd.Run())

	assert.Equal(t, "Bearer test-jwt-12345", srv.gotAuth,
		"--token must surface as Authorization: Bearer header")
}

// TestCommandCmd_OmitsAuthHeaderWhenNoToken pins that an empty
// --token doesn't produce a "Authorization: Bearer " header (with
// a trailing space + nothing else) — that would be malformed.
func TestCommandCmd_OmitsAuthHeaderWhenNoToken(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusOK, `{"ok":true,"state":"queued","status":"queued"}`)

	cmd := &CommandCmd{
		Plugin: "gmail",
		Command: "fetch",
		DaemonURL: srv.server.URL,
		Wait: 0,
	}
	require.NoError(t, cmd.Run())
	assert.Empty(t, srv.gotAuth, "no --token → no Authorization header")
}

// TestCommandCmd_MissingPluginErrors pins the input validation:
// empty plugin name returns an error before any HTTP call.
func TestCommandCmd_MissingPluginErrors(t *testing.T) {
	t.Parallel()
	cmd := &CommandCmd{Plugin: "", Command: "fetch"}
	err := cmd.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin")
}

// TestCommandCmd_MissingCommandErrors pins same for empty command.
func TestCommandCmd_MissingCommandErrors(t *testing.T) {
	t.Parallel()
	cmd := &CommandCmd{Plugin: "gmail", Command: ""}
	err := cmd.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command")
}

// TestFetchCmd_MissingPluginErrors pins the input validation
// symmetric to CommandCmd: empty plugin name returns an error
// before any HTTP call.
func TestFetchCmd_MissingPluginErrors(t *testing.T) {
	t.Parallel()
	cmd := &FetchCmd{Plugin: "", Pattern: "Tehran"}
	err := cmd.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin")
}

// TestFetchCmd_MissingPatternErrors pins same for empty pattern.
func TestFetchCmd_MissingPatternErrors(t *testing.T) {
	t.Parallel()
	cmd := &FetchCmd{Plugin: "wikipedia", Pattern: ""}
	err := cmd.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pattern")
}

// TestRunDispatch_SurfacesDaemon4xxAsError pins that a 4xx/5xx
// response from the daemon surfaces as a non-nil error so cron /
// shell scripts get exit 1. Body is still pretty-printed to stdout
// so the operator can see WHY.
func TestRunDispatch_SurfacesDaemon4xxAsError(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusBadRequest,
		`{"ok":false,"error":"invalid_input","message":"plugin \"gmail\" has no command \"sync\""}`)

	var stdout bytes.Buffer
	err := runDispatch(srv.server.URL, "", "gmail: !sync", 0, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, stdout.String(), "invalid_input",
		"error response body must still surface to stdout")
}

// TestRunDispatch_PrettyPrintsResponseBody pins the human-readable
// stdout shape: 2-space indented JSON with a trailing newline. cron
// can pipe to jq either way; humans get a readable shape.
func TestRunDispatch_PrettyPrintsResponseBody(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusOK, `{"ok":true,"state":"complete","entity":{"id":"x:y","kind":"source"}}`)

	var stdout bytes.Buffer
	require.NoError(t, runDispatch(srv.server.URL, "", "x: y", 0, &stdout))

	out := stdout.String()
	assert.Contains(t, out, "\"ok\": true")
	assert.Contains(t, out, "\"id\": \"x:y\"")
	assert.True(t, strings.HasSuffix(out, "\n"),
		"output must end with newline for shell-friendly piping")
}

// TestRunDispatch_FallsBackToRawOnNonJSONBody pins the defensive
// path: a proxy / LB returning non-JSON (e.g. an HTML error page)
// still surfaces the raw bytes to stdout instead of erroring on
// decode.
func TestRunDispatch_FallsBackToRawOnNonJSONBody(t *testing.T) {
	t.Parallel()
	srv := newFakeIngestServer(t, http.StatusBadGateway, "<html>upstream 502</html>")

	var stdout bytes.Buffer
	err := runDispatch(srv.server.URL, "", "x: y", 0, &stdout)
	require.Error(t, err) // non-2xx → error
	assert.Contains(t, stdout.String(), "<html>upstream 502</html>")
}

// TestRunDispatch_BuildsCorrectEndpointPath pins `/v1/ingest` is the
// daemon path the CLI POSTs to (vs. some hypothetical /v1/command
// or /v1/dispatch — ADR-0022 §6 was explicit that the existing
// dispatch endpoint is reused).
func TestRunDispatch_BuildsCorrectEndpointPath(t *testing.T) {
	t.Parallel()
	hits := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"state":"queued","status":"queued"}`))
	}))
	t.Cleanup(srv.Close)

	require.NoError(t, runDispatch(srv.URL, "", "x: y", 0, io.Discard))
	require.NoError(t, runDispatch(srv.URL+"/", "", "x: y", 0, io.Discard))

	for _, path := range hits {
		assert.Equal(t, "/v1/ingest", path,
			"CLI must POST to /v1/ingest regardless of trailing slash on daemon URL")
	}
}
