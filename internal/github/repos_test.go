package github

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepoList_HappyPath(t *testing.T) {
	t.Parallel()
	got, err := ParseRepoList("acme/proj, beta/widget , gamma/store")
	require.NoError(t, err)
	assert.Equal(t, []RepoRef{
		{Owner: "acme", Repo: "proj"},
		{Owner: "beta", Repo: "widget"},
		{Owner: "gamma", Repo: "store"},
	}, got)
}

func TestParseRepoList_EmptyReturnsErrNoRepos(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "   ", "\t\n"} {
		_, err := ParseRepoList(raw)
		assert.ErrorIs(t, err, ErrNoRepos, "raw=%q", raw)
	}
}

func TestParseRepoList_TrailingCommaSkipped(t *testing.T) {
	t.Parallel()
	got, err := ParseRepoList("acme/proj,,beta/widget,")
	require.NoError(t, err)
	assert.Equal(t, []RepoRef{
		{Owner: "acme", Repo: "proj"},
		{Owner: "beta", Repo: "widget"},
	}, got)
}

func TestParseRepoList_AllWhitespaceEntriesReturnsErrNoRepos(t *testing.T) {
	t.Parallel()
	_, err := ParseRepoList(",,, ,")
	assert.ErrorIs(t, err, ErrNoRepos)
}

func TestParseRepoList_MalformedEntries(t *testing.T) {
	t.Parallel()
	cases := []string{
		"no-slash",
		"/missing-owner",
		"missing-repo/",
		"too/many/slashes",
		"acme/proj,bad-entry",
	}
	for _, raw := range cases {
		_, err := ParseRepoList(raw)
		require.Error(t, err, "raw=%q", raw)
		var malformed *ErrMalformedRepo
		require.True(t, errors.As(err, &malformed), "raw=%q err=%v", raw, err)
		assert.NotEmpty(t, malformed.Entry, "raw=%q", raw)
	}
}

func TestRepoRef_Slash(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "acme/proj", RepoRef{Owner: "acme", Repo: "proj"}.Slash())
}
