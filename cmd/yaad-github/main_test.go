package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/github"
)

func TestRun_VersionMode_PrintsPluginVersion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, nil, &stdout, &stderr)
	assert.Equal(t, 0, code)
	assert.Equal(t, github.PluginVersion+"\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRun_InitMode_EmitsCapabilitiesJSON(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--init"}, nil, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr=%s", stderr.String())

	var doc capabilitiesDoc
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc),
		"--init output must be valid JSON; got: %s", stdout.String())

	// Pin every field against ADR-0026 §1.
	assert.Equal(t, "github", doc.Name)
	assert.Equal(t, github.PluginVersion, doc.Version)
	assert.Len(t, doc.URLPatterns, 3, "three patterns: PR / issue / shorthand")
	require.Len(t, doc.EntityKinds, 1)
	assert.Equal(t, "source", doc.EntityKinds[0].Name, "ADR-0021 universal source kind")
	assert.Empty(t, doc.EdgeKinds, "edge_kinds is reserved for plugin-emitted source-shape edge type registry; yaad-github emits via canonical_edge_types_emitted")
	assert.Equal(t, github.KnownCanonicalKinds, doc.CanonicalKindsEmitted)
	assert.Equal(t, github.KnownCanonicalEdgeTypes, doc.CanonicalEdgeTypesEmitted)
	assert.False(t, doc.SupportsSearch, "ADR-0026 §1: supports_search=false")
	assert.Equal(t, "github", doc.SourceNamespace, "ADR-0026 §2 Option A: single namespace")
	assert.Equal(t, 900, doc.CacheTTLSeconds, "ADR-0026 §1: 15min default")
	assert.Equal(t, []string{"fetch"}, doc.Commands)
}

func TestRun_InitMode_RespectsInstanceNameEnv(t *testing.T) {
	// ADR-0026 §7: an operator running two instances of the
	// same binary distinguishes them via the YAML's `name:`
	// entry; the binary mirrors that into the shorthand
	// pattern via EnvInstanceName.
	t.Setenv(EnvInstanceName, "github-work")
	t.Setenv(github.EnvBaseURL, "https://ghes.example.com/api/v3")

	var stdout bytes.Buffer
	code := run([]string{"--init"}, nil, &stdout, &bytes.Buffer{})
	require.Equal(t, 0, code)

	var doc capabilitiesDoc
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc))
	require.Len(t, doc.URLPatterns, 3)
	assert.Contains(t, doc.URLPatterns[0], "ghes\\.example\\.com",
		"PR pattern must use the GHES host")
	assert.Contains(t, doc.URLPatterns[2], "github-work",
		"shorthand pattern must use the operator-chosen instance name")
	assert.NotContains(t, doc.URLPatterns[2], "(?i)^github:",
		"GHES instance must not claim the bare 'github:' shorthand")
}

func TestRun_BadFlag_ReturnsTwo(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{"--no-such-flag"}, nil, &bytes.Buffer{}, &stderr)
	assert.Equal(t, 2, code)
	assert.NotEmpty(t, stderr.String(), "bad flag must emit to stderr")
}

func TestRun_CommandFetch_MissingRepos_EmitsErrorEnvelope(t *testing.T) {
	// Empty YAAD_GITHUB_REPOS must surface as a single
	// `_error` control packet on stdout + exit non-zero, so
	// the daemon-side NDJSON consumer logs the cause + the
	// run terminates cleanly without an inflight GitHub call.
	t.Setenv(github.EnvRepos, "")
	t.Setenv(github.EnvToken, "ghp_stub")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	assert.Equal(t, 1, code)

	var pkt struct {
		Error        string `json:"_error"`
		ErrorMessage string `json:"_error_message"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &pkt),
		"stdout should be a single JSON line: %q", stdout.String())
	assert.Equal(t, "config_missing", pkt.Error)
	assert.Contains(t, pkt.ErrorMessage, "YAAD_GITHUB_REPOS")
	assert.Contains(t, stderr.String(), "YAAD_GITHUB_REPOS")
}

func TestRun_CommandFetch_MissingToken_EmitsAuthErrorEnvelope(t *testing.T) {
	// Token unset surfaces as `auth_failed` after the repo
	// list parses; the operator-login resolution path is the
	// first network-touching step and fails closed.
	t.Setenv(github.EnvRepos, "acme/proj")
	t.Setenv(github.EnvToken, "")
	t.Setenv(github.EnvBaseURL, "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	assert.Equal(t, 1, code)

	var pkt struct {
		Error string `json:"_error"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &pkt))
	assert.Equal(t, "auth_failed", pkt.Error)
}

func TestRun_CommandFetch_HappyPath_StreamsEnvelopesAndSummary(t *testing.T) {
	// End-to-end bulk fetch against a stubbed upstream:
	//   - GET /user resolves the operator login once.
	//   - Search returns two open items (one PR, one issue).
	//   - Each item's full GET produces a source-shape envelope.
	//   - A trailing `_summary` control packet closes the stream.
	//
	// Mirrors yaad-gmail's `{"_summary": {...}}` shape so the
	// daemon's NDJSON consumer treats both plugins uniformly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"test-operator"}`))
		case "/search/issues":
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, "involves:test-operator") || !strings.Contains(q, "repo:acme/proj") {
				http.Error(w, "bad query: "+q, http.StatusBadRequest)
				return
			}
			switch {
			case strings.Contains(q, "is:open"):
				_, _ = w.Write([]byte(`{
					"total_count": 2,
					"items": [
						{"number": 7, "pull_request": {"url": "https://api.example.test/repos/acme/proj/pulls/7"}, "state": "open", "title": "PR seven"},
						{"number": 9, "state": "open", "title": "Issue nine"}
					]
				}`))
			case strings.Contains(q, "is:closed"):
				// Empty closed-recent set keeps this test
				// focused on the open path; the closed-search
				// wiring is exercised in
				// TestRun_CommandFetch_ClosedRecent_*.
				_, _ = w.Write([]byte(`{"items": []}`))
			default:
				http.Error(w, "unrecognised search query: "+q, http.StatusBadRequest)
			}
		case "/repos/acme/proj/pulls/7":
			_, _ = w.Write([]byte(`{
				"number": 7,
				"state": "open",
				"title": "PR seven",
				"body": "pr body",
				"html_url": "https://github.com/acme/proj/pull/7",
				"user": {"login": "author-a"}
			}`))
		case "/repos/acme/proj/issues/9":
			_, _ = w.Write([]byte(`{
				"number": 9,
				"state": "open",
				"title": "Issue nine",
				"body": "issue body",
				"html_url": "https://github.com/acme/proj/issues/9",
				"user": {"login": "author-b"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv(github.EnvBaseURL, srv.URL)
	t.Setenv(github.EnvToken, "ghp_stub")
	t.Setenv(github.EnvRepos, "acme/proj")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr=%s", stderr.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 3, "two envelopes + one _summary: %q", stdout.String())

	// First two lines must be source-shape envelopes (not _summary).
	for i := 0; i < 2; i++ {
		var env struct {
			OK         bool `json:"ok"`
			Structured struct {
				Kind string         `json:"kind"`
				Name string         `json:"name"`
				Data map[string]any `json:"data"`
			} `json:"structured"`
			Summary map[string]any `json:"_summary"`
		}
		require.NoError(t, json.Unmarshal([]byte(lines[i]), &env), "line %d: %s", i, lines[i])
		assert.Nil(t, env.Summary, "line %d should be an envelope, not _summary", i)
		assert.True(t, env.OK)
		assert.Equal(t, "source", env.Structured.Kind)
		// Pin `data.state` — Cut 4's daemon-side inferential
		// archive logic reads this field to detect open→closed
		// transitions. Open-items-only this cut means every
		// emitted envelope must carry "open" here.
		assert.Equal(t, "open", env.Structured.Data["state"], "line %d", i)
	}

	// Trailing line must be the _summary packet.
	var summary struct {
		Summary struct {
			Repos          int   `json:"repos"`
			Emitted        int   `json:"emitted"`
			Errors         int   `json:"errors"`
			DurationMillis int64 `json:"duration_ms"`
		} `json:"_summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &summary), "_summary: %s", lines[2])
	assert.Equal(t, 1, summary.Summary.Repos)
	assert.Equal(t, 2, summary.Summary.Emitted)
	assert.Equal(t, 0, summary.Summary.Errors)
	assert.GreaterOrEqual(t, summary.Summary.DurationMillis, int64(0))
}

// TestRun_CommandFetch_InvalidRecentDays_EmitsErrorEnvelope:
// a non-positive YAAD_GITHUB_RECENT_DAYS surfaces as a single
// `_error` packet + exit non-zero before any GitHub call.
func TestRun_CommandFetch_InvalidRecentDays_EmitsErrorEnvelope(t *testing.T) {
	t.Setenv(github.EnvRepos, "acme/proj")
	t.Setenv(github.EnvToken, "ghp_stub")
	t.Setenv(github.EnvRecentDays, "not-a-number")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	assert.Equal(t, 1, code)

	var pkt struct {
		Error        string `json:"_error"`
		ErrorMessage string `json:"_error_message"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &pkt))
	assert.Equal(t, "config_invalid", pkt.Error)
	assert.Contains(t, pkt.ErrorMessage, "YAAD_GITHUB_RECENT_DAYS")
}

// TestRun_CommandFetch_ClosedRecent_EmittedAlongsideOpen: the
// closed-recent sweep emits one envelope per matched item in
// addition to the open sweep's items per ADR-0026 §6 (2026-05-21
// amendment). The closed envelope carries `data.state == "closed"`.
func TestRun_CommandFetch_ClosedRecent_EmittedAlongsideOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"test-operator"}`))
		case "/search/issues":
			q := r.URL.Query().Get("q")
			switch {
			case strings.Contains(q, "is:open"):
				_, _ = w.Write([]byte(`{"items": [{"number": 7, "state": "open", "title": "Open seven"}]}`))
			case strings.Contains(q, "is:closed"):
				assert.Contains(t, q, "updated:>=",
					"closed-recent search must include the updated:>= filter")
				_, _ = w.Write([]byte(`{"items": [{"number": 9, "state": "closed", "title": "Closed nine"}]}`))
			default:
				http.Error(w, "unknown query: "+q, http.StatusBadRequest)
			}
		case "/repos/acme/proj/issues/7":
			_, _ = w.Write([]byte(`{"number": 7, "state": "open", "title": "Open seven", "body": "b", "html_url": "https://github.com/acme/proj/issues/7", "user": {"login": "u"}}`))
		case "/repos/acme/proj/issues/9":
			_, _ = w.Write([]byte(`{"number": 9, "state": "closed", "title": "Closed nine", "body": "b", "html_url": "https://github.com/acme/proj/issues/9", "user": {"login": "u"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv(github.EnvBaseURL, srv.URL)
	t.Setenv(github.EnvToken, "ghp_stub")
	t.Setenv(github.EnvRepos, "acme/proj")
	t.Setenv(github.EnvRecentDays, "7")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr=%s", stderr.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 3, "one open envelope + one closed envelope + one _summary: %q", stdout.String())

	states := []string{}
	for i := 0; i < 2; i++ {
		var env struct {
			Structured struct {
				Data map[string]any `json:"data"`
			} `json:"structured"`
		}
		require.NoError(t, json.Unmarshal([]byte(lines[i]), &env))
		states = append(states, env.Structured.Data["state"].(string))
	}
	assert.ElementsMatch(t, []string{"open", "closed"}, states,
		"both open and closed states surface in the same sweep")

	var summary struct {
		Summary struct {
			Emitted int `json:"emitted"`
		} `json:"_summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &summary))
	assert.Equal(t, 2, summary.Summary.Emitted)
}

// TestRun_CommandFetch_OpenAndClosedDedup: an item appearing in
// both the open and the closed-recent results (transition mid-
// sweep) emits only once.
func TestRun_CommandFetch_OpenAndClosedDedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"test-operator"}`))
		case "/search/issues":
			q := r.URL.Query().Get("q")
			switch {
			case strings.Contains(q, "is:open"):
				_, _ = w.Write([]byte(`{"items": [{"number": 7, "state": "open", "title": "Mid-flight seven"}]}`))
			case strings.Contains(q, "is:closed"):
				_, _ = w.Write([]byte(`{"items": [{"number": 7, "state": "closed", "title": "Mid-flight seven"}]}`))
			default:
				http.Error(w, "unknown query: "+q, http.StatusBadRequest)
			}
		case "/repos/acme/proj/issues/7":
			_, _ = w.Write([]byte(`{"number": 7, "state": "closed", "title": "Mid-flight seven", "body": "b", "html_url": "https://github.com/acme/proj/issues/7", "user": {"login": "u"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv(github.EnvBaseURL, srv.URL)
	t.Setenv(github.EnvToken, "ghp_stub")
	t.Setenv(github.EnvRepos, "acme/proj")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr=%s", stderr.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 2, "deduped → one envelope + one _summary: %q", stdout.String())

	var summary struct {
		Summary struct {
			Emitted int `json:"emitted"`
		} `json:"_summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &summary))
	assert.Equal(t, 1, summary.Summary.Emitted,
		"item present in both search responses dedups to one emit")
}

func TestRun_CommandUnknown_ReturnsTwo(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{"--command", "no-such-command"}, nil, &bytes.Buffer{}, &stderr)
	assert.Equal(t, 2, code, "unknown commands must exit with a flag-error code")
	assert.Contains(t, stderr.String(), "unknown --command")
	assert.Contains(t, stderr.String(), "no-such-command")
}

func TestResolveOperatorLogin_NoToken_ReturnsErrTokenMissing(t *testing.T) {
	// Auth-wiring sanity for Cut 1: the helper Cut 2 + Cut 3
	// will call must fail closed when the operator hasn't wired
	// YAAD_GITHUB_TOKEN. Exercised via the public env-var path
	// so the test mirrors how the binary's main() reaches the
	// helper at fetch time.
	t.Setenv(github.EnvToken, "")
	t.Setenv(github.EnvBaseURL, "")
	_, err := resolveOperatorLogin(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, github.ErrTokenMissing,
		"resolveOperatorLogin must surface ErrTokenMissing so main() fails fast at fetch invocation")
}

func TestAuthTimeout_IsConservative(t *testing.T) {
	// Pin the startup-budget constant. Cut-2/Cut-3 callers
	// expect this to stay short (plugin --init / fetch entry
	// points can't hang on a slow upstream).
	assert.LessOrEqual(t, authTimeout.Seconds(), float64(30),
		"authTimeout must stay ≤30s so daemon-side plugin startup doesn't stall on a dead network")
}

func TestRun_URLShapeStdin_FetchesAndEmitsEnvelope(t *testing.T) {
	// URL-shape ingest path (Cut 2): the binary reads the
	// ingest request, parses the URL, hits the GitHub REST
	// API, and emits one source-shape envelope on stdout.
	// This exercises the wiring end-to-end against a
	// stubbed upstream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/proj/pulls/42" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
			"number": 42,
			"state": "open",
			"title": "End-to-end test PR",
			"body": "Hello from a stub.",
			"html_url": "https://github.com/acme/proj/pull/42",
			"user": {"login": "test-user"}
		}`))
	}))
	defer srv.Close()

	t.Setenv(github.EnvBaseURL, srv.URL)
	t.Setenv(github.EnvToken, "ghp_stub")

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader(`{"operation":"ingest","url":"https://github.com/acme/proj/pull/42"}`)
	code := run([]string{}, stdin, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr=%s", stderr.String())

	// Envelope is single-line NDJSON ending in `\n`.
	out := stdout.String()
	require.True(t, strings.HasSuffix(out, "\n"))
	var env struct {
		OK         bool `json:"ok"`
		Structured struct {
			Kind string         `json:"kind"`
			Name string         `json:"name"`
			Data map[string]any `json:"data"`
		} `json:"structured"`
		RawContent string   `json:"raw_content"`
		Notations  []string `json:"notations"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.True(t, env.OK)
	assert.Equal(t, "source", env.Structured.Kind)
	assert.Equal(t, "acme_proj_pr_42", env.Structured.Name,
		"ADR-0026 §2 slug-target shape via daemon slug.Slug(name)")
	assert.Equal(t, "pr", env.Structured.Data["type"])
	assert.Equal(t, "Hello from a stub.", env.RawContent)
	require.NotEmpty(t, env.Notations)
	assert.Equal(t, "https://github.com/acme/proj/pull/42", env.Notations[0])
}

func TestRun_URLShapeStdin_EmptyStdin_Errors(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "empty stdin")
}

func TestRun_URLShapeStdin_UnsupportedOperation_Errors(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{}, strings.NewReader(`{"operation":"frobnicate","url":"x"}`), &bytes.Buffer{}, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "unsupported operation")
}

func TestRun_URLShapeStdin_MalformedTarget_Errors(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{}, strings.NewReader(`{"operation":"ingest","url":"not a target"}`), &bytes.Buffer{}, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "parse target")
}
