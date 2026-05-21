package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v68/github"
)

// ErrTokenMissing surfaces from ResolveUserLogin when the
// operator hasn't wired YAAD_GITHUB_TOKEN into the plugin's
// env. Distinct from "token rejected by GitHub" (which
// surfaces as an HTTPError wrapping go-github's
// *github.ErrorResponse) so the binary can fail fast at
// startup with a clear operator message.
var ErrTokenMissing = errors.New("github: YAAD_GITHUB_TOKEN env var is empty or unset")

// HTTPError is the plugin-side error type for non-2xx
// upstream responses. Wraps go-github's *github.ErrorResponse
// so call sites can branch on Status without depending on the
// library type directly. The Body field is the upstream
// JSON message (size-capped to 2KiB by go-github's internal
// decoder).
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("github: HTTP %d from upstream", e.Status)
	}
	return fmt.Sprintf("github: HTTP %d from upstream: %s", e.Status, e.Body)
}

// asHTTPError converts a go-github error into our wrapper
// shape when the underlying type is `*github.ErrorResponse`.
// Returns nil otherwise (network errors, decode failures,
// context cancellation flow through verbatim from the
// caller's `fmt.Errorf("...: %w", err)` wrapping).
func asHTTPError(err error) *HTTPError {
	if err == nil {
		return nil
	}
	var gerr *gogithub.ErrorResponse
	if !errors.As(err, &gerr) || gerr.Response == nil {
		return nil
	}
	return &HTTPError{
		Status: gerr.Response.StatusCode,
		Body:   strings.TrimSpace(gerr.Message),
	}
}

// newClient constructs a *github.Client honoring the
// operator's `YAAD_GITHUB_TOKEN` + `YAAD_GITHUB_BASE_URL`
// settings. The default base URL (`api.github.com`) is
// reused when baseURL is empty.
//
// For non-default base URLs we set `client.BaseURL` directly
// rather than calling `WithEnterpriseURLs`. The library's
// helper auto-prepends `/api/v3/` to non-github.com hosts,
// which fights operator setups that already include the
// `/api/v3` suffix (per ADR-0026 §7's example
// `https://ghes.example.com/api/v3`) and breaks tests
// pointing at an httptest server. Direct override is exactly
// what both call sites want: the operator's URL flows through
// verbatim with a trailing-slash normalize.
//
// Empty token produces an unauthenticated client — GitHub's
// anonymous rate limit applies. Tests rely on this path.
func newClient(httpClient *http.Client, baseURL, token string) (*gogithub.Client, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	client := gogithub.NewClient(httpClient)
	if token != "" {
		client = client.WithAuthToken(token)
	}
	if baseURL != "" && baseURL != DefaultBaseURL {
		base := strings.TrimRight(baseURL, "/") + "/"
		parsed, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("github: parse base URL %q: %w", baseURL, err)
		}
		client.BaseURL = parsed
	}
	return client, nil
}

// ResolveUserLogin calls `GET /user` via go-github and
// returns the authenticated user's GitHub login. The login
// is the `<operator>` token the bulk-fetch's search query
// splices into `is:open involves:<operator>` per ADR-0026
// §4.
//
// Side effects: one HTTP round-trip per call. The plugin
// binary calls this once at startup and caches the result
// for the process lifetime; ResolveUserLogin itself stays a
// pure function (no package-level cache).
//
// Errors:
//   - ErrTokenMissing when token is empty.
//   - *HTTPError when GitHub returns non-2xx (401 invalid
//     token, 403 rate-limited, etc.).
//   - wrapped network/decode errors otherwise.
//
// Never logs the token. The Authorization header is set on
// go-github's transport and discarded after the call.
func ResolveUserLogin(ctx context.Context, client *http.Client, baseURL, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrTokenMissing
	}
	gc, err := newClient(client, baseURL, token)
	if err != nil {
		return "", err
	}
	user, _, err := gc.Users.Get(ctx, "")
	if err != nil {
		if httpErr := asHTTPError(err); httpErr != nil {
			return "", httpErr
		}
		return "", fmt.Errorf("github: GET /user: %w", err)
	}
	login := strings.TrimSpace(user.GetLogin())
	if login == "" {
		return "", errors.New("github: GET /user returned empty login")
	}
	return login, nil
}
