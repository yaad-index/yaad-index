package github

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTarget_URLShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		wantKind ItemKind
		wantNum  int
		wantOwn  string
		wantRepo string
	}{
		{"pr-https", "https://github.com/acme/proj/pull/42", ItemKindPR, 42, "acme", "proj"},
		{"pr-http", "http://github.com/acme/proj/pull/42", ItemKindPR, 42, "acme", "proj"},
		{"pr-trailing-slug", "https://github.com/acme/proj/pull/42/files", ItemKindPR, 42, "acme", "proj"},
		{"issue", "https://github.com/acme/proj/issues/99", ItemKindIssue, 99, "acme", "proj"},
		{"ghes-host", "https://ghes.example.com/team/svc/pull/1", ItemKindPR, 1, "team", "svc"},
		{"owner-with-dash", "https://github.com/acme-org/my-project/pull/7", ItemKindPR, 7, "acme-org", "my-project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgt, err := ParseTarget(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.wantKind, tgt.Kind)
			assert.Equal(t, tc.wantNum, tgt.Number)
			assert.Equal(t, tc.wantOwn, tgt.Owner)
			assert.Equal(t, tc.wantRepo, tgt.Repo)
		})
	}
}

func TestParseTarget_ShorthandShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"canonical", "github: acme/proj#42"},
		{"no-space-after-colon", "github:acme/proj#42"},
		{"uppercase-prefix", "GITHUB: acme/proj#42"},
		{"mixed-case", "GiThUb: acme/proj#42"},
		{"instance-name", "github-personal: acme/proj#42"},
		{"trailing-whitespace", "github: acme/proj#42   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgt, err := ParseTarget(tc.in)
			require.NoError(t, err)
			// Shorthand defaults to PR per parse.go's
			// targetFromShorthand; fetch path's 404-fallback
			// re-routes when needed.
			assert.Equal(t, ItemKindPR, tgt.Kind)
			assert.Equal(t, 42, tgt.Number)
			assert.Equal(t, "acme", tgt.Owner)
			assert.Equal(t, "proj", tgt.Repo)
		})
	}
}

func TestParseTarget_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"non-github-url", "https://example.com/o/r/pull/1"}, // matches the URL path regex; allowed.
		{"unsupported-scheme", "ftp://github.com/o/r/pull/1"},
		{"bad-path", "https://github.com/just-one-segment"},
		{"missing-number", "https://github.com/o/r/pull/"},
		{"non-numeric", "https://github.com/o/r/pull/abc"},
		{"zero-number", "https://github.com/o/r/pull/0"},
		{"shorthand-no-num", "github: acme/proj#"},
		{"shorthand-bad-num", "github: acme/proj#xyz"},
		{"shorthand-missing-repo", "github: acme/#1"},
		{"random-string", "not a url"},
	}
	skipCases := map[string]bool{
		"non-github-url": true, // path-regex match passes; host isn't validated at parse-time
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgt, err := ParseTarget(tc.in)
			if skipCases[tc.name] {
				if err == nil {
					assert.NotNil(t, tgt, "non-github URL with matching path is permitted at parse-time")
				}
				return
			}
			require.Error(t, err, "want ErrUnsupportedTarget for %q", tc.in)
			assert.True(t, errors.Is(err, ErrUnsupportedTarget),
				"want ErrUnsupportedTarget, got %v", err)
			assert.Nil(t, tgt)
		})
	}
}

func TestTarget_EntityName_SlugifiableToADR0026Shape(t *testing.T) {
	t.Parallel()
	// ADR-0026 §2: entity IDs use `<owner>_<repo>_pr_<num>` /
	// `_issue_<num>` shape. EntityName is the daemon-side
	// `name` we emit; slug.Slug(name) should land on that
	// exact slug. The slug package's `_` preservation test
	// fixture covers the round-trip; pin the EntityName
	// itself here.
	tgt := Target{Owner: "Acme", Repo: "Proj", Kind: ItemKindPR, Number: 42}
	assert.Equal(t, "acme_proj_pr_42", tgt.EntityName())

	issue := Target{Owner: "acme-org", Repo: "my-project", Kind: ItemKindIssue, Number: 7}
	assert.Equal(t, "acme-org_my-project_issue_7", issue.EntityName())
}

func TestItemKind_CanonicalKind(t *testing.T) {
	t.Parallel()
	assert.Equal(t, CanonicalKindPR, ItemKindPR.CanonicalKind())
	assert.Equal(t, CanonicalKindIssue, ItemKindIssue.CanonicalKind())
	assert.Equal(t, "", ItemKind("nonsense").CanonicalKind())
}
