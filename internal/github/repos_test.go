package github

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRepoList_HappyPath(t *testing.T) {
	t.Parallel()
	got, err := ValidateRepoList([]string{"acme/proj", " beta/widget ", "gamma/store"})
	require.NoError(t, err)
	assert.Equal(t, []RepoRef{
		{Owner: "acme", Repo: "proj"},
		{Owner: "beta", Repo: "widget"},
		{Owner: "gamma", Repo: "store"},
	}, got)
}

func TestValidateRepoList_EmptyReturnsErrNoRepos(t *testing.T) {
	t.Parallel()
	for _, raw := range [][]string{nil, {}, {""}, {"   "}} {
		_, err := ValidateRepoList(raw)
		assert.ErrorIs(t, err, ErrNoRepos, "raw=%v", raw)
	}
}

func TestValidateRepoList_WhitespaceEntriesSkipped(t *testing.T) {
	t.Parallel()
	got, err := ValidateRepoList([]string{"acme/proj", "", "  ", "beta/widget"})
	require.NoError(t, err)
	assert.Equal(t, []RepoRef{
		{Owner: "acme", Repo: "proj"},
		{Owner: "beta", Repo: "widget"},
	}, got)
}

func TestValidateRepoList_MalformedEntries(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"no-slash"},
		{"/missing-owner"},
		{"missing-repo/"},
		{"too/many/slashes"},
		{"acme/proj", "bad-entry"},
	}
	for _, raw := range cases {
		_, err := ValidateRepoList(raw)
		require.Error(t, err, "raw=%v", raw)
		var malformed *ErrMalformedRepo
		require.True(t, errors.As(err, &malformed), "raw=%v err=%v", raw, err)
		assert.NotEmpty(t, malformed.Entry, "raw=%v", raw)
	}
}

func TestRepoRef_Slash(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "acme/proj", RepoRef{Owner: "acme", Repo: "proj"}.Slash())
}
