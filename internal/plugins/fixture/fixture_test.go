package fixture

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

func TestNew_BuildsMatchAndConstantFetch(t *testing.T) {
	t.Parallel()

	want := &plugins.FetchResult{
		Entity: &store.Entity{ID: "test:fixed", Kind: "test"},
		Provenance: []store.ProvenanceEntry{
			{Source: "fixture:test", OK: true},
		},
	}
	p := New("test", "needle", want)

	assert.Equal(t, "test", p.Name())
	assert.True(t, p.Match("https://example.test/url-with-needle-substring"),
		"Match should be true on URL containing the needle")
	assert.False(t, p.Match("https://example.test/url-without"),
		"Match should be false on URL without the needle")

	got, err := p.Fetch(context.Background(), "https://example.test/needle/x")
	require.NoError(t, err)
	assert.Same(t, want, got, "Fetch must pass-through the constant pointer")
}

func TestPlugin_FetchFuncTakesPrecedenceOverFetchValue(t *testing.T) {
	t.Parallel()

	called := 0
	p := &Plugin{
		NameValue: "p",
		MatchFunc: func(string) bool { return true },
		FetchValue: &plugins.FetchResult{Entity: &store.Entity{ID: "from-value"}},
		FetchFunc: func(_ context.Context, _ string) (*plugins.FetchResult, error) {
			called++
			return &plugins.FetchResult{Entity: &store.Entity{ID: "from-func"}}, nil
		},
	}

	got, err := p.Fetch(context.Background(), "x")
	require.NoError(t, err)
	assert.Equal(t, "from-func", got.Entity.ID, "FetchFunc precedence")
	assert.Equal(t, 1, called, "FetchFunc call count")
}

func TestPlugin_FetchErrorTakesPrecedenceOverFetchValue(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	p := &Plugin{
		NameValue: "p",
		MatchFunc: func(string) bool { return true },
		FetchValue: &plugins.FetchResult{Entity: &store.Entity{ID: "from-value"}},
		FetchError: sentinel,
	}

	_, err := p.Fetch(context.Background(), "x")
	assert.ErrorIs(t, err, sentinel, "FetchError precedence")
}

func TestPlugin_FetchWithoutAnythingReturnsError(t *testing.T) {
	t.Parallel()

	p := &Plugin{NameValue: "p", MatchFunc: func(string) bool { return true }}
	_, err := p.Fetch(context.Background(), "x")
	assert.Error(t, err, "Fetch with no FetchFunc/Error/Value should fail loudly")
}
