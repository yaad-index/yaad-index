package github

import (
	"context"
	"fmt"
	"time"

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
	return searchInvolvedQuery(ctx, client, repo,
		fmt.Sprintf("is:open involves:%s repo:%s", login, repo.Slash()))
}

// searchInvolvedClosedRecent is the closed-window companion to
// searchInvolved per ADR-0026 §6 (2026-05-21 amendment): every
// closed PR + issue the operator is involved in with upstream
// activity in the last `days`-day window. The `updated:>=<date>`
// filter is GitHub Search's native rolling-window operator —
// stateless on the plugin side, no last-sync cursor needed.
//
// `now` is the reference instant the window is anchored against;
// the caller passes its own clock for testability. `days` is
// validated by the caller (ParseRecentDays); a non-positive
// value would produce an oddly-shaped query string but is not
// re-validated here.
func searchInvolvedClosedRecent(ctx context.Context, client *gogithub.Client, repo RepoRef, login string, now time.Time, days int) ([]Target, error) {
	return searchInvolvedQuery(ctx, client, repo,
		fmt.Sprintf("is:closed involves:%s repo:%s updated:>=%s",
			login, repo.Slash(), FormatRecentSince(now, days)))
}

// searchInvolvedQuery runs the paginated Search.Issues call for
// any pre-built query string. Shared by the open + closed-recent
// search paths.
func searchInvolvedQuery(ctx context.Context, client *gogithub.Client, repo RepoRef, query string) ([]Target, error) {
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

// fetchItemViaClient is the client-bound dispatch from a
// Target into the kind-specific fetcher (PR or issue). The
// `Client.FetchTarget` method delegates here so the per-item
// fetch path stays decoupled from go-github client
// construction.
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
