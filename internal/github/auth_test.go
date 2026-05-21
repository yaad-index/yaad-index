package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveUserLogin_EmptyToken_ReturnsErrTokenMissing(t *testing.T) {
	t.Parallel()
	login, err := ResolveUserLogin(context.Background(), nil, "https://api.github.com", "")
	assert.Equal(t, "", login)
	assert.True(t, errors.Is(err, ErrTokenMissing))
}

func TestResolveUserLogin_WhitespaceToken_ReturnsErrTokenMissing(t *testing.T) {
	t.Parallel()
	_, err := ResolveUserLogin(context.Background(), nil, "https://api.github.com", "   \t  ")
	assert.True(t, errors.Is(err, ErrTokenMissing),
		"whitespace-only token should fail closed before any network call")
}

func TestResolveUserLogin_HappyPath_ReturnsLogin(t *testing.T) {
	t.Parallel()

	var capturedAuth string
	var capturedAccept string
	var capturedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"sample-user","id":12345}`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	login, err := ResolveUserLogin(context.Background(), client, srv.URL, "ghp_sample_token")
	require.NoError(t, err)
	assert.Equal(t, "sample-user", login)
	assert.Equal(t, "/user", capturedPath, "ResolveUserLogin must hit <base>/user")
	assert.Equal(t, "Bearer ghp_sample_token", capturedAuth,
		"Authorization header must be Bearer <token>")
	assert.Equal(t, "application/vnd.github+json", capturedAccept,
		"Accept header must be the GitHub JSON content type")
}

func TestResolveUserLogin_TrailingSlashBaseURL_StripsBeforeAppending(t *testing.T) {
	t.Parallel()
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{"login":"u"}`))
	}))
	defer srv.Close()

	// Base URL with a trailing slash should produce `/user`, not
	// `//user`.
	_, err := ResolveUserLogin(context.Background(), nil, srv.URL+"/", "token")
	require.NoError(t, err)
	assert.Equal(t, "/user", capturedPath)
}

func TestResolveUserLogin_GHESBaseURL_PreservesAPIPathPrefix(t *testing.T) {
	t.Parallel()
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{"login":"u"}`))
	}))
	defer srv.Close()

	// GHES base URLs include `/api/v3` — that prefix must
	// survive into the GET /user URL.
	_, err := ResolveUserLogin(context.Background(), nil, srv.URL+"/api/v3", "token")
	require.NoError(t, err)
	assert.Equal(t, "/api/v3/user", capturedPath)
}

func TestResolveUserLogin_Non2xx_ReturnsHTTPErrorWithBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`))
	}))
	defer srv.Close()

	_, err := ResolveUserLogin(context.Background(), nil, srv.URL, "ghp_bad")
	require.Error(t, err)
	var httpErr *HTTPError
	require.True(t, errors.As(err, &httpErr), "want *HTTPError, got %T", err)
	assert.Equal(t, http.StatusUnauthorized, httpErr.Status)
	assert.Contains(t, httpErr.Body, "Bad credentials")
}

func TestResolveUserLogin_EmptyLogin_RejectedWithError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"","id":0}`))
	}))
	defer srv.Close()

	_, err := ResolveUserLogin(context.Background(), nil, srv.URL, "token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty login")
}

func TestResolveUserLogin_ContextCancellation_PropagatesError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"login":"u"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := ResolveUserLogin(ctx, &http.Client{}, srv.URL, "token")
	require.Error(t, err)
	// Context-cancellation error path; don't assert specific
	// wrapping (different Go versions wrap differently), just
	// confirm an error landed.
}

func TestHTTPError_ErrorString(t *testing.T) {
	t.Parallel()
	e := &HTTPError{Status: 403, Body: "rate limited"}
	assert.Contains(t, e.Error(), "403")
	assert.Contains(t, e.Error(), "rate limited")

	// Body-less variant still emits the status.
	bare := &HTTPError{Status: 500}
	assert.Contains(t, bare.Error(), "500")
}
