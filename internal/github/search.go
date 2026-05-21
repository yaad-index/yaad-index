package github

import (
	"context"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v68/github"
)

// SearchInvolvedOpen runs the bulk-fetch search query for
// ONE repo: every PR + issue currently in open state where
// `login` is `involves:` (author + assignee + mentioned +
// commenter + reviewer per ADR-0026 §4).
//
// Returns a list of Targets (kind correctly populated from
// the upstream's PR-vs-issue discriminator) so the caller
// can iterate via FetchTarget for the full per-item shape.
// Search results carry only a trimmed shape; the full
// per-item fields (body, base branch, reviewers, etc.)
// require a follow-up GET per item.
//
// Pagination: GitHub's Search.Issues caps at 1000 results
// per query and paginates with `per_page=100`; go-github
// transparently handles the per-page cap. We walk all pages
// until empty to surface the full involved set. For repos
// with >1000 hits the caller hits the upstream limit — fine
// for personal-coordinator use; a future enhancement could
// add per-call narrowing (date-window, label filter).
//
// Errors:
//   - Wraps upstream non-2xx via *HTTPError (auth, rate
//     limit, repo-doesn't-exist).
//   - Empty `login` rejected with ErrTokenMissing-shaped
//     diagnostic — bulk fetch can't construct the query
//     without it.
func SearchInvolvedOpen(ctx context.Context, opts FetchOptions, repo RepoRef, login string) ([]Target, error) {
	if login == "" {
		return nil, fmt.Errorf("github: SearchInvolvedOpen: empty operator login (resolve via ResolveUserLogin first)")
	}
	client, err := newClient(opts.Client, opts.BaseURL, opts.Token)
	if err != nil {
		return nil, err
	}
	return searchInvolved(ctx, client, repo, login)
}

// searchInvolved is the client-bound implementation; lets the
// bulk-fetch caller reuse a single client across many repos
// without re-constructing per call. Exported callers use
// SearchInvolvedOpen which owns client construction.
func searchInvolved(ctx context.Context, client *gogithub.Client, repo RepoRef, login string) ([]Target, error) {
	query := fmt.Sprintf("is:open involves:%s repo:%s", login, repo.Slash())
	opts := &gogithub.SearchOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	var targets []Target
	for {
		result, resp, err := client.Search.Issues(ctx, query, opts)
		if err != nil {
			if httpErr := asHTTPError(err); httpErr != nil {
				return nil, httpErr
			}
			return nil, fmt.Errorf("github: Search.Issues query=%q: %w", query, err)
		}
		for _, issue := range result.Issues {
			targets = append(targets, targetFromSearchHit(repo, issue))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return targets, nil
}

// targetFromSearchHit translates one go-github search result
// into a Target. The PR-vs-issue discriminator comes from
// `issue.IsPullRequest()` (go-github's typed helper); the
// repo comes from the search scope (`repo:` query parameter)
// since the Issue struct's URL field carries it but parsing
// is finicky.
func targetFromSearchHit(repo RepoRef, issue *gogithub.Issue) Target {
	kind := ItemKindIssue
	if issue.IsPullRequest() {
		kind = ItemKindPR
	}
	return Target{
		Owner:  repo.Owner,
		Repo:   repo.Repo,
		Kind:   kind,
		Number: issue.GetNumber(),
	}
}

// BulkFetchOptions bundles the bulk-pass inputs. Same shape
// as FetchOptions but carries the operator login + repo set
// so the caller doesn't have to re-derive them per item.
type BulkFetchOptions struct {
	Client        *http.Client
	BaseURL       string
	Token         string
	OperatorLogin string
	Repos         []RepoRef
}

// FetchInvolvedOpenAcrossRepos walks every configured repo
// and returns the full normalized Item for each open PR +
// issue the operator is involved in. Combines per-repo
// search with per-item FetchTarget so the caller emits one
// envelope per Item directly.
//
// The caller (cmd/yaad-github/main.go's runCommandFetch)
// drives emission so each item lands on stdout as soon as
// its fetch completes — preserves the ADR-0023 streaming-
// NDJSON contract (envelope-per-line, no batch accumulation).
// This helper returns a slice instead because it's the
// composition shape Cut-3 tests need; the binary calls the
// component pieces directly for streaming.
func FetchInvolvedOpenAcrossRepos(ctx context.Context, opts BulkFetchOptions) ([]*Item, error) {
	client, err := newClient(opts.Client, opts.BaseURL, opts.Token)
	if err != nil {
		return nil, err
	}
	var items []*Item
	for _, repo := range opts.Repos {
		targets, err := searchInvolved(ctx, client, repo, opts.OperatorLogin)
		if err != nil {
			return items, fmt.Errorf("github: search %s: %w", repo.Slash(), err)
		}
		for _, t := range targets {
			item, err := fetchItemViaClient(ctx, client, t)
			if err != nil {
				return items, fmt.Errorf("github: fetch %s/%s#%d: %w", t.Owner, t.Repo, t.Number, err)
			}
			items = append(items, item)
		}
	}
	return items, nil
}

// fetchItemViaClient is the client-bound shim FetchTarget
// delegates to in the bulk-fetch path; reuses the same
// client across the per-repo search loop so we don't
// reconstruct per item.
func fetchItemViaClient(ctx context.Context, client *gogithub.Client, t Target) (*Item, error) {
	switch t.Kind {
	case ItemKindPR:
		return fetchPR(ctx, client, t)
	case ItemKindIssue:
		return fetchIssue(ctx, client, t)
	default:
		return nil, fmt.Errorf("github: unsupported target kind %q", t.Kind)
	}
}
