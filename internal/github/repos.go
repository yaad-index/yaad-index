package github

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoRepos surfaces when the operator hasn't wired any
// repos in the structured `config:` block. ADR-0006's strict-
// validation pattern says missing required operator config
// fails fast at startup; the command-shape fetch handler
// converts this to an `_error` envelope + non-zero exit
// before any GitHub call.
var ErrNoRepos = errors.New("github: config.repos is empty or unset")

// ErrMalformedRepo surfaces when an entry doesn't match the
// `owner/repo` shape. Wraps the offending entry in the error
// text so the operator sees which line of their config needs
// fixing.
type ErrMalformedRepo struct {
	Entry string
}

func (e *ErrMalformedRepo) Error() string {
	return fmt.Sprintf("github: malformed repo entry %q (want `owner/repo`)", e.Entry)
}

// RepoRef is one `owner/repo` target. The bulk-fetch path
// iterates over a list of these per ADR-0026 §1.
type RepoRef struct {
	Owner string
	Repo  string
}

// Slash returns the canonical `owner/repo` string, suitable
// for splicing into a GitHub Search query.
func (r RepoRef) Slash() string {
	return r.Owner + "/" + r.Repo
}

// ValidateRepoList walks the operator-supplied list of
// `owner/repo` strings (from the structured `config:` block
// per ADR-0026 §3) and produces validated RepoRef entries.
// Empty list or all-whitespace entries → ErrNoRepos.
// Returns a *ErrMalformedRepo for any entry that doesn't
// match `owner/repo`.
//
// Duplicate entries are NOT deduped — operator gets what
// they wrote (the bulk-fetch loop's per-repo iteration is
// per-entry, not per-unique-pair).
func ValidateRepoList(entries []string) ([]RepoRef, error) {
	if len(entries) == 0 {
		return nil, ErrNoRepos
	}
	out := make([]RepoRef, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		owner, repo, ok := splitRepo(entry)
		if !ok {
			return nil, &ErrMalformedRepo{Entry: entry}
		}
		out = append(out, RepoRef{Owner: owner, Repo: repo})
	}
	if len(out) == 0 {
		return nil, ErrNoRepos
	}
	return out, nil
}

// splitRepo parses `owner/repo`. Rejects empty owner /
// empty repo / >1 slash. Permissive on what GitHub-side
// characters are valid — defers to GitHub itself to reject
// `nonexistent-owner/nonexistent-repo` at search time.
func splitRepo(entry string) (string, string, bool) {
	idx := strings.IndexByte(entry, '/')
	if idx < 0 {
		return "", "", false
	}
	owner := entry[:idx]
	rest := entry[idx+1:]
	if owner == "" || rest == "" {
		return "", "", false
	}
	if strings.ContainsRune(rest, '/') {
		return "", "", false
	}
	return owner, rest, true
}
