package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	gogithub "github.com/google/go-github/v68/github"
)

// Client is the bulk-pass shared-client handle. Constructed
// once per `--command fetch` invocation, then reused across
// every search + per-item fetch so the M+N calls share one
// connection pool + one go-github wrapper instead of paying
// the construction cost per call.
//
// The wrapped `*gogithub.Client` is intentionally unexported
// so callers can't reach past the package's facade. The
// only authenticated paths the plugin exercises are the
// search query (ADR-0026 §4) and the per-item GET (ADR-0026
// §1) — both surfaced as methods below.
type Client struct {
	inner *gogithub.Client
}

// NewClient wraps the unexported newClient constructor so the
// bulk-fetch caller can build one shared client up front.
// httpClient.Timeout is enforced per-request by net/http, so
// a 30s timeout here gives each downstream call its own 30s
// budget without leaking the broader run budget the outer
// ctx enforces.
func NewClient(httpClient *http.Client, baseURL, token string) (*Client, error) {
	inner, err := newClient(httpClient, baseURL, token)
	if err != nil {
		return nil, err
	}
	return &Client{inner: inner}, nil
}

// SearchInvolvedOpen mirrors the free function but reuses the
// receiver's shared client. Per-repo search; pagination + 404
// + non-2xx handling identical to the free path.
func (c *Client) SearchInvolvedOpen(ctx context.Context, repo RepoRef, login string) ([]Target, error) {
	if login == "" {
		return nil, errors.New("github: Client.SearchInvolvedOpen: empty operator login (resolve via ResolveUserLogin first)")
	}
	return searchInvolved(ctx, c.inner, repo, login)
}

// SearchInvolvedClosedRecent runs the closed-window companion to
// SearchInvolvedOpen per ADR-0026 §6 (2026-05-21 amendment) —
// every closed PR + issue the operator is involved in whose
// upstream `updated` timestamp falls within the last `days`-day
// window. `now` is the reference instant the window is anchored
// against; callers pass their own clock for testability.
func (c *Client) SearchInvolvedClosedRecent(ctx context.Context, repo RepoRef, login string, now time.Time, days int) ([]Target, error) {
	if login == "" {
		return nil, errors.New("github: Client.SearchInvolvedClosedRecent: empty operator login (resolve via ResolveUserLogin first)")
	}
	return searchInvolvedClosedRecent(ctx, c.inner, repo, login, now, days)
}

// FetchTarget mirrors the free function but reuses the
// receiver's shared client. PR-vs-issue dispatch + 404
// fallback + non-2xx handling identical to the free path.
func (c *Client) FetchTarget(ctx context.Context, t Target) (*Item, error) {
	return fetchItemViaClient(ctx, c.inner, t)
}
