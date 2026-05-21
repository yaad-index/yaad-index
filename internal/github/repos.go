package github

import (
	"errors"
	"fmt"
	"strings"
)

// EnvRepos is the env var the operator sets to declare the
// list of `owner/repo` targets the bulk-fetch path iterates
// over. Comma-separated `owner/repo` entries; whitespace
// around each entry is trimmed. Empty entries (e.g. a
// trailing comma) are silently skipped.
//
// Per ADR-0026 §3 the operator's intent is a `repos:` list
// inside the config block; the daemon-side `PluginEntry`
// contract (`internal/config/config.go`) currently only
// accepts SCALAR config values + translates to env vars, so
// the plugin reads this comma-separated env shape instead of
// a structured list. The ADR-text example needs a refresh
// to match this reality (tracked as a follow-up).
const EnvRepos = "YAAD_GITHUB_REPOS"

// ErrNoRepos surfaces when the operator hasn't wired any
// repos. ADR-0006's strict-validation pattern says missing
// required operator config fails fast at startup; the
// command-shape fetch handler converts this to a clear
// stderr message + exit non-zero before any GitHub call.
var ErrNoRepos = errors.New("github: YAAD_GITHUB_REPOS env var is empty or unset")

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

// ParseRepoList splits + validates the comma-separated env
// value into a slice of RepoRef entries. Returns ErrNoRepos
// when the input is empty (after trim). Returns a
// *ErrMalformedRepo for any entry that doesn't match
// `owner/repo`.
//
// Duplicate entries are NOT deduped — operator gets what
// they wrote (the bulk-fetch loop's per-repo iteration is
// per-entry, not per-unique-pair). A future enhancement
// could dedup; for v1 the price of duplicate API calls is
// operator-visible + low-stakes.
func ParseRepoList(raw string) ([]RepoRef, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, ErrNoRepos
	}
	parts := strings.Split(raw, ",")
	out := make([]RepoRef, 0, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
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
		// All entries were whitespace-only (trailing commas).
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
