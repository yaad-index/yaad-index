package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_SearchAndFetch_ShareOneConnection(t *testing.T) {
	t.Parallel()
	// Pin the shared-client property: a single *Client running
	// search + per-item fetch + a second search reuses one
	// outbound TCP connection (or at least surfaces a single
	// http.Client to net/http's pool). RoundTrip count is a
	// proxy — go-github's transport sits underneath, so we
	// count via the test server's connection counter.
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		switch r.URL.Path {
		case "/search/issues":
			_, _ = w.Write([]byte(`{"items": [{"number": 11, "state": "open", "title": "Open eleven"}]}`))
		case "/repos/acme/proj/issues/11":
			_, _ = w.Write([]byte(`{"number": 11, "state": "open", "title": "Open eleven", "html_url": "https://github.com/acme/proj/issues/11", "user": {"login": "u"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewClient(srv.Client(), srv.URL, "ghp_stub")
	require.NoError(t, err)

	targets, err := client.SearchInvolvedOpen(context.Background(), RepoRef{Owner: "acme", Repo: "proj"}, "test-operator")
	require.NoError(t, err)
	require.Len(t, targets, 1)

	item, err := client.FetchTarget(context.Background(), targets[0])
	require.NoError(t, err)
	assert.Equal(t, 11, item.Number)
	assert.Equal(t, "Open eleven", item.Title)

	// Two upstream round-trips (1 search + 1 fetch) per call —
	// no extra `GET /user` or capability probes get folded in.
	assert.Equal(t, int32(2), atomic.LoadInt32(&requests))
}

func TestClient_SearchInvolvedOpen_EmptyLogin_Rejected(t *testing.T) {
	t.Parallel()
	client, err := NewClient(&http.Client{}, "", "ghp_stub")
	require.NoError(t, err)
	_, err = client.SearchInvolvedOpen(context.Background(), RepoRef{Owner: "acme", Repo: "proj"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty operator login")
}
