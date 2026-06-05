package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeServer captures the path of each REST request and
// dispatches based on the path. Body / status come from the
// per-path handler map; missing paths return 404. Keeps
// each test's wire stub short.
type fakeServer struct {
	t        *testing.T
	handlers map[string]http.HandlerFunc
	calls    []string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	return &fakeServer{t: t, handlers: map[string]http.HandlerFunc{}}
}

func (f *fakeServer) serve(path string, h http.HandlerFunc) {
	f.handlers[path] = h
}

func (f *fakeServer) start() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.URL.Path)
		h, ok := f.handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
}

func TestFetchTarget_PR_HappyPath(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.serve("/repos/acme/proj/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer ghp_test", r.Header.Get("Authorization"))
		assert.Contains(t, r.Header.Get("Accept"), "application/vnd.github")
		_, _ = w.Write([]byte(`{
			"number": 42,
			"state": "open",
			"title": "Sample PR",
			"body": "Sample PR body markdown.",
			"html_url": "https://github.com/acme/proj/pull/42",
			"created_at": "2026-05-01T10:00:00Z",
			"updated_at": "2026-05-20T12:00:00Z",
			"comments": 3,
			"review_comments": 2,
			"user": {"login": "author-user"},
			"assignees": [{"login": "assignee-a"}],
			"requested_reviewers": [{"login": "reviewer-a"}, {"login": "reviewer-b"}],
			"base": {"ref": "main"},
			"head": {"ref": "feat/sample"},
			"labels": [{"name": "bug"}, {"name": "p1"}],
			"merged": false
		}`))
	})
	srv := fs.start()
	defer srv.Close()

	target := Target{Owner: "acme", Repo: "proj", Kind: ItemKindPR, Number: 42}
	item, err := FetchTarget(context.Background(), FetchOptions{BaseURL: srv.URL, Token: "ghp_test"}, target)
	require.NoError(t, err)

	assert.Equal(t, 42, item.Number)
	assert.Equal(t, ItemKindPR, item.Type)
	assert.Equal(t, "open", item.State)
	assert.Equal(t, "Sample PR", item.Title)
	assert.Equal(t, "Sample PR body markdown.", item.Body)
	assert.Equal(t, "https://github.com/acme/proj/pull/42", item.URL)
	assert.Equal(t, "author-user", item.Author)
	assert.Equal(t, []string{"assignee-a"}, item.Assignees)
	assert.Equal(t, []string{"reviewer-a", "reviewer-b"}, item.Reviewers)
	assert.Equal(t, 5, item.CommentCount, "comments + review_comments")
	assert.Equal(t, "main", item.BaseBranch)
	assert.Equal(t, "feat/sample", item.HeadBranch)
	assert.False(t, item.Merged)
	assert.Equal(t, []string{"bug", "p1"}, item.Labels)
}

func TestFetchTarget_Issue_HappyPath(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.serve("/repos/acme/proj/issues/99", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 99,
			"state": "closed",
			"title": "Sample issue",
			"body": "Issue body markdown.",
			"html_url": "https://github.com/acme/proj/issues/99",
			"created_at": "2026-04-01T08:00:00Z",
			"updated_at": "2026-05-10T16:00:00Z",
			"closed_at": "2026-05-10T16:00:00Z",
			"comments": 7,
			"user": {"login": "issue-author"},
			"assignees": [{"login": "assignee-x"}],
			"labels": [{"name": "enhancement"}]
		}`))
	})
	srv := fs.start()
	defer srv.Close()

	target := Target{Owner: "acme", Repo: "proj", Kind: ItemKindIssue, Number: 99}
	item, err := FetchTarget(context.Background(), FetchOptions{BaseURL: srv.URL, Token: "t"}, target)
	require.NoError(t, err)

	assert.Equal(t, ItemKindIssue, item.Type)
	assert.Equal(t, "closed", item.State)
	assert.NotNil(t, item.ClosedAt)
	assert.Equal(t, 7, item.CommentCount)
	assert.Empty(t, item.Reviewers, "issues have no reviewers")
	assert.Empty(t, item.BaseBranch, "issues have no branches")
	assert.Equal(t, []string{"enhancement"}, item.Labels)
}

// TestFetchTarget_LastCommentAt_GatedByCommentCount pins #451: the
// approximated last_comment_at is populated only when the item actually
// carries comments. A zero-comment issue/PR leaves it null so
// "commented within the last N days" predicates don't false-positive on
// the UpdatedAt proxy (which fires on any edit of a never-commented
// item).
func TestFetchTarget_LastCommentAt_GatedByCommentCount(t *testing.T) {
	t.Parallel()

	issueBody := func(number, comments int) string {
		return fmt.Sprintf(`{
			"number": %d, "state": "open", "title": "x",
			"html_url": "https://github.com/acme/proj/issues/%d",
			"created_at": "2026-05-01T10:00:00Z",
			"updated_at": "2026-05-20T12:00:00Z",
			"comments": %d,
			"user": {"login": "u"}
		}`, number, number, comments)
	}

	t.Run("zero comments yields nil last_comment_at", func(t *testing.T) {
		t.Parallel()
		fs := newFakeServer(t)
		fs.serve("/repos/acme/proj/issues/1", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(issueBody(1, 0)))
		})
		srv := fs.start()
		defer srv.Close()

		item, err := FetchTarget(context.Background(),
			FetchOptions{BaseURL: srv.URL, Token: "t"},
			Target{Owner: "acme", Repo: "proj", Kind: ItemKindIssue, Number: 1})
		require.NoError(t, err)
		assert.Equal(t, 0, item.CommentCount)
		assert.Nil(t, item.LastCommentAt, "zero-comment item must leave last_comment_at null")
	})

	t.Run("nonzero comments populates last_comment_at from updated_at", func(t *testing.T) {
		t.Parallel()
		fs := newFakeServer(t)
		fs.serve("/repos/acme/proj/issues/2", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(issueBody(2, 3)))
		})
		srv := fs.start()
		defer srv.Close()

		item, err := FetchTarget(context.Background(),
			FetchOptions{BaseURL: srv.URL, Token: "t"},
			Target{Owner: "acme", Repo: "proj", Kind: ItemKindIssue, Number: 2})
		require.NoError(t, err)
		require.NotNil(t, item.LastCommentAt, "commented item must populate last_comment_at")
		assert.Equal(t, item.UpdatedAt, *item.LastCommentAt, "approximated via updated_at")
	})

	t.Run("PR with only review_comments still counts as commented", func(t *testing.T) {
		t.Parallel()
		fs := newFakeServer(t)
		fs.serve("/repos/acme/proj/pulls/3", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"number": 3, "state": "open", "title": "x",
				"html_url": "https://github.com/acme/proj/pull/3",
				"created_at": "2026-05-01T10:00:00Z",
				"updated_at": "2026-05-20T12:00:00Z",
				"comments": 0, "review_comments": 2,
				"user": {"login": "u"}, "base": {"ref": "main"}, "head": {"ref": "f"}
			}`))
		})
		srv := fs.start()
		defer srv.Close()

		item, err := FetchTarget(context.Background(),
			FetchOptions{BaseURL: srv.URL, Token: "t"},
			Target{Owner: "acme", Repo: "proj", Kind: ItemKindPR, Number: 3})
		require.NoError(t, err)
		assert.Equal(t, 2, item.CommentCount, "comments + review_comments")
		require.NotNil(t, item.LastCommentAt, "review-comments-only PR is still commented")
	})
}

func TestFetchTarget_PR404_ShorthandFallsThroughToIssue(t *testing.T) {
	t.Parallel()
	// Shorthand defaults to PR (ItemKindPR). When the
	// /pulls/N route 404s, the fetch path retries against
	// /issues/N. This test exercises that path.
	fs := newFakeServer(t)
	fs.serve("/repos/acme/proj/pulls/123", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	fs.serve("/repos/acme/proj/issues/123", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 123,
			"state": "open",
			"title": "Issue numbered like a PR",
			"body": "",
			"html_url": "https://github.com/acme/proj/issues/123",
			"user": {"login": "u"}
		}`))
	})
	srv := fs.start()
	defer srv.Close()

	target := Target{Owner: "acme", Repo: "proj", Kind: ItemKindPR, Number: 123}
	item, err := FetchTarget(context.Background(), FetchOptions{BaseURL: srv.URL, Token: "t"}, target)
	require.NoError(t, err)
	assert.Equal(t, ItemKindIssue, item.Type, "fallback re-targets to issue")
	assert.Contains(t, fs.calls, "/repos/acme/proj/pulls/123")
	assert.Contains(t, fs.calls, "/repos/acme/proj/issues/123")
}

func TestFetchTarget_IssueEndpointReturnsPR_ReRoutesToPullsEndpoint(t *testing.T) {
	t.Parallel()
	// GitHub's /issues/N endpoint also returns PRs (PRs are
	// a subset of issues). The `pull_request` field is the
	// discriminator; fetchIssue must re-route to fetchPR for
	// PR-specific fields like `head` and `base`.
	fs := newFakeServer(t)
	fs.serve("/repos/acme/proj/issues/5", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 5,
			"state": "open",
			"title": "Actually a PR",
			"html_url": "https://github.com/acme/proj/pull/5",
			"user": {"login": "u"},
			"pull_request": {"url": "https://api.github.com/repos/acme/proj/pulls/5"}
		}`))
	})
	fs.serve("/repos/acme/proj/pulls/5", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 5,
			"state": "open",
			"title": "Actually a PR",
			"html_url": "https://github.com/acme/proj/pull/5",
			"user": {"login": "u"},
			"base": {"ref": "main"},
			"head": {"ref": "feat/p"}
		}`))
	})
	srv := fs.start()
	defer srv.Close()

	target := Target{Owner: "acme", Repo: "proj", Kind: ItemKindIssue, Number: 5}
	item, err := FetchTarget(context.Background(), FetchOptions{BaseURL: srv.URL, Token: "t"}, target)
	require.NoError(t, err)
	assert.Equal(t, ItemKindPR, item.Type)
	assert.Equal(t, "main", item.BaseBranch)
	assert.Equal(t, "feat/p", item.HeadBranch)
}

func TestFetchTarget_UserAgent_ContainsPluginIdentity(t *testing.T) {
	t.Parallel()
	// Per-plugin User-Agent: operator log filtering + GitHub's
	// abuse-detection attribution both rely on the plugin
	// identity being visible. The go-github default (`go-github`)
	// is too generic — pin that `yaad-github` shows up in the UA.
	var capturedUA string
	fs := newFakeServer(t)
	fs.serve("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		capturedUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"number":1,"user":{"login":"u"}}`))
	})
	srv := fs.start()
	defer srv.Close()

	_, err := FetchTarget(context.Background(),
		FetchOptions{BaseURL: srv.URL},
		Target{Owner: "o", Repo: "r", Kind: ItemKindPR, Number: 1})
	require.NoError(t, err)
	assert.Contains(t, capturedUA, PluginName,
		"User-Agent must carry the plugin identity for log-filtering + abuse-detection attribution; got %q", capturedUA)
}

func TestFetchTarget_AuthBearer_AppliedWhenTokenSet(t *testing.T) {
	t.Parallel()
	var capturedAuth string
	fs := newFakeServer(t)
	fs.serve("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"number":1,"user":{"login":"u"}}`))
	})
	srv := fs.start()
	defer srv.Close()

	_, err := FetchTarget(context.Background(),
		FetchOptions{BaseURL: srv.URL, Token: "ghp_real_token"},
		Target{Owner: "o", Repo: "r", Kind: ItemKindPR, Number: 1})
	require.NoError(t, err)
	assert.Equal(t, "Bearer ghp_real_token", capturedAuth)
}

func TestFetchTarget_NoToken_SendsUnauthenticated(t *testing.T) {
	t.Parallel()
	var capturedAuth string
	fs := newFakeServer(t)
	fs.serve("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"number":1,"user":{"login":"u"}}`))
	})
	srv := fs.start()
	defer srv.Close()

	_, err := FetchTarget(context.Background(),
		FetchOptions{BaseURL: srv.URL, Token: ""},
		Target{Owner: "o", Repo: "r", Kind: ItemKindPR, Number: 1})
	require.NoError(t, err)
	assert.Empty(t, capturedAuth,
		"empty token must not send any Authorization header (would surface as bad-creds-with-empty-token on GitHub)")
}

func TestFetchTarget_UpstreamError_ReturnsHTTPError(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.serve("/repos/o/r/issues/1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	srv := fs.start()
	defer srv.Close()

	_, err := FetchTarget(context.Background(),
		FetchOptions{BaseURL: srv.URL},
		Target{Owner: "o", Repo: "r", Kind: ItemKindIssue, Number: 1})
	require.Error(t, err)
	var httpErr *HTTPError
	require.True(t, errors.As(err, &httpErr))
	assert.Equal(t, http.StatusForbidden, httpErr.Status)
	assert.Contains(t, httpErr.Body, "rate limit")
}

func TestFetchTarget_BaseURLTrailingSlash_Normalized(t *testing.T) {
	t.Parallel()
	var capturedPath string
	fs := newFakeServer(t)
	fs.serve("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{"number":1,"user":{"login":"u"}}`))
	})
	srv := fs.start()
	defer srv.Close()

	_, err := FetchTarget(context.Background(),
		FetchOptions{BaseURL: srv.URL + "/", Token: ""},
		Target{Owner: "o", Repo: "r", Kind: ItemKindPR, Number: 1})
	require.NoError(t, err)
	assert.Equal(t, "/repos/o/r/pulls/1", capturedPath, "trailing slash on base URL must not double up the path")
}

func TestFetchTarget_GHESBaseURL_PreservesAPIPrefix(t *testing.T) {
	t.Parallel()
	var capturedPath string
	fs := newFakeServer(t)
	fs.serve("/api/v3/repos/team/svc/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{"number":1,"user":{"login":"u"}}`))
	})
	srv := fs.start()
	defer srv.Close()

	_, err := FetchTarget(context.Background(),
		FetchOptions{BaseURL: srv.URL + "/api/v3"},
		Target{Owner: "team", Repo: "svc", Kind: ItemKindPR, Number: 1})
	require.NoError(t, err)
	assert.Equal(t, "/api/v3/repos/team/svc/pulls/1", capturedPath)
}

func TestFetchTarget_ContextCancellation_Errors(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.serve("/repos/o/r/pulls/1", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"number":1,"user":{"login":"u"}}`))
	})
	srv := fs.start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := FetchTarget(ctx,
		FetchOptions{BaseURL: srv.URL, Client: &http.Client{}},
		Target{Owner: "o", Repo: "r", Kind: ItemKindPR, Number: 1})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "context canceled"),
		"want context-cancellation error, got %v", err)
}
