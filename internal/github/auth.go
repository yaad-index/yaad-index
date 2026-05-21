package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrTokenMissing surfaces from ResolveUserLogin when the
// operator hasn't wired YAAD_GITHUB_TOKEN into the plugin's
// env. Distinct from "token rejected by GitHub" (which surfaces
// as an HTTPError) so the binary can fail fast at startup with
// a clear operator message.
var ErrTokenMissing = errors.New("github: YAAD_GITHUB_TOKEN env var is empty or unset")

// HTTPError carries a non-2xx status from the GitHub REST API.
// The body is captured (size-capped) so the caller can log it
// or surface a debug hint without exposing the token itself —
// the auth header is set by ResolveUserLogin, not by the
// HTTPError's body.
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

// userResponse is the trimmed-down shape we decode out of
// `GET /user`. GitHub returns ~30 fields; only `login` is
// load-bearing for the involves-query construction the bulk
// fetch needs (per ADR-0026 §4).
type userResponse struct {
	Login string `json:"login"`
}

// ResolveUserLogin calls `GET <baseURL>/user` with the supplied
// PAT and returns the authenticated user's GitHub login. The
// login is the `<operator>` token the bulk-fetch's search
// query splices into `is:open involves:<operator>` per
// ADR-0026 §4.
//
// Side effects: one HTTP round-trip per call. The plugin
// binary calls this once at startup and caches the result for
// the process lifetime; ResolveUserLogin itself stays a pure
// function (no package-level cache).
//
// Errors:
//   - ErrTokenMissing when token is empty.
//   - *HTTPError when GitHub returns non-2xx (401 invalid
//     token, 403 rate-limited, etc.).
//   - wrapped network/decode errors otherwise.
//
// Never logs the token. The Authorization header is set on the
// request and discarded after the call returns.
func ResolveUserLogin(ctx context.Context, client *http.Client, baseURL, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrTokenMissing
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	endpoint := strings.TrimRight(ResolveBaseURL(baseURL), "/") + "/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("github: build GET /user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", PluginName+"/"+PluginVersion)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET /user: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Cap body capture so a verbose upstream error doesn't
		// blow up the log line. 2KiB is plenty for a JSON error
		// envelope.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	var parsed userResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("github: decode GET /user response: %w", err)
	}
	if strings.TrimSpace(parsed.Login) == "" {
		return "", errors.New("github: GET /user returned empty login")
	}
	return parsed.Login, nil
}
