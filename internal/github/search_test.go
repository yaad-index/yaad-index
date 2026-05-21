package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchInvolvedOpen_HappyPath(t *testing.T) {
	t.Parallel()
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/search/issues", r.URL.Path)
		sawQuery = r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{
			"total_count": 2,
			"items": [
				{"number": 7, "pull_request": {"url": "https://api.example.test/repos/acme/proj/pulls/7"}, "state": "open", "title": "PR seven"},
				{"number": 9, "state": "open", "title": "Issue nine"}
			]
		}`))
	}))
	defer srv.Close()

	got, err := SearchInvolvedOpen(context.Background(), FetchOptions{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   "ghp_stub",
	}, RepoRef{Owner: "acme", Repo: "proj"}, "test-operator")
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, Target{Owner: "acme", Repo: "proj", Kind: ItemKindPR, Number: 7}, got[0])
	assert.Equal(t, Target{Owner: "acme", Repo: "proj", Kind: ItemKindIssue, Number: 9}, got[1])
	assert.Contains(t, sawQuery, "is:open")
	assert.Contains(t, sawQuery, "involves:test-operator")
	assert.Contains(t, sawQuery, "repo:acme/proj")
}

func TestSearchInvolvedOpen_PaginationWalksAllPages(t *testing.T) {
	t.Parallel()
	var calls int
	// go-github walks pagination via the upstream's Link header
	// (`rel="next"`); the search loop continues until the header
	// is absent. Emit a "next" link from page 1 → assert both
	// pages are visited and items concatenate in order.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			w.Header().Set("Link", `<`+srv.URL+`/search/issues?q=x&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"items": [{"number": 1, "state": "open"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items": [{"number": 2, "state": "open"}]}`))
	}))
	defer srv.Close()

	got, err := SearchInvolvedOpen(context.Background(), FetchOptions{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   "ghp_stub",
	}, RepoRef{Owner: "acme", Repo: "proj"}, "test-operator")
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "search must walk both pages")
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].Number)
	assert.Equal(t, 2, got[1].Number)
}

func TestSearchInvolvedOpen_EmptyLogin_Rejected(t *testing.T) {
	t.Parallel()
	_, err := SearchInvolvedOpen(context.Background(), FetchOptions{
		Client:  &http.Client{},
		BaseURL: "",
		Token:   "ghp_stub",
	}, RepoRef{Owner: "acme", Repo: "proj"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty operator login")
}

func TestSearchInvolvedOpen_NonOKUpstream_WrapsHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := SearchInvolvedOpen(context.Background(), FetchOptions{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   "ghp_bad",
	}, RepoRef{Owner: "acme", Repo: "proj"}, "test-operator")
	require.Error(t, err)
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusUnauthorized, httpErr.Status)
}

func TestFetchInvolvedOpenAcrossRepos_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/issues":
			q := r.URL.Query().Get("q")
			require.Contains(t, q, "involves:test-operator")
			switch {
			case strings.Contains(q, "repo:acme/proj"):
				_, _ = w.Write([]byte(`{"items": [{"number": 7, "state": "open", "title": "Issue seven"}]}`))
			case strings.Contains(q, "repo:beta/widget"):
				_, _ = w.Write([]byte(`{"items": []}`))
			default:
				http.NotFound(w, r)
			}
		case "/repos/acme/proj/issues/7":
			_, _ = w.Write([]byte(`{"number": 7, "state": "open", "title": "Issue seven", "body": "b", "html_url": "https://github.com/acme/proj/issues/7", "user": {"login": "u"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	items, err := FetchInvolvedOpenAcrossRepos(context.Background(), BulkFetchOptions{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         "ghp_stub",
		OperatorLogin: "test-operator",
		Repos: []RepoRef{
			{Owner: "acme", Repo: "proj"},
			{Owner: "beta", Repo: "widget"},
		},
	})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, 7, items[0].Number)
	assert.Equal(t, "Issue seven", items[0].Title)
}
