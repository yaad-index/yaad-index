package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	gogithub "github.com/google/go-github/v68/github"
)

// Item is the normalized in-memory shape the fetch path
// produces for either a PR or an issue. The plugin converts
// it into the wire envelope via WriteEnvelope; everything in
// here is what we need for the envelope plus a few fields the
// archive-lifecycle check (Cut 4) reads (`State`).
//
// Fields not directly mirroring go-github's struct shape are
// re-computed at fetch time so the wire shape stays decoupled
// from upstream API drift.
type Item struct {
	Target Target

	Number    int
	Type      ItemKind
	State     string // "open" / "closed" (PRs also surface "merged" via separate flag below)
	Title     string
	Body      string // raw markdown; lands in `raw_content`
	URL       string // canonical web URL (e.g. https://github.com/.../pull/N)
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  *time.Time
	MergedAt  *time.Time // PR-only

	Author    string   // login of the creator
	Assignees []string // logins
	Reviewers []string // PR-only, logins of requested reviewers (post-merge state may be empty)

	CommentCount int
	// LastCommentAt is the time the most recent comment was
	// posted. Approximated via UpdatedAt today — Cut 3's bulk
	// fetch may refine this with a per-item comments timeline
	// probe.
	LastCommentAt *time.Time

	// PR-only extras.
	BaseBranch string
	HeadBranch string
	Merged     bool

	// Issue-only extras.
	Labels []string
}

// IsPR is convenience for envelope-emission branching.
func (i *Item) IsPR() bool { return i.Type == ItemKindPR }

// FetchOptions bundles the per-call inputs to the fetch path.
type FetchOptions struct {
	Client  *http.Client // nil → default with 30s timeout
	BaseURL string       // empty → DefaultBaseURL (`api.github.com`)
	Token   string       // empty → unauthenticated request (rate-limited; tests use this)
}

// FetchTarget routes to PR vs issue based on the Target's
// Kind. For shorthand inputs (which default to PR per
// parse.go's targetFromShorthand), a PR-fetch that 404s
// re-routes to the issue endpoint. Symmetric re-route on the
// issue side: GitHub's `/issues/N` returns PRs via a
// `pull_request` link; we re-fetch through `/pulls/N` so
// PR-specific fields (`base`, `head`, `merged`) survive.
func FetchTarget(ctx context.Context, opts FetchOptions, t Target) (*Item, error) {
	client, err := newClient(opts.Client, opts.BaseURL, opts.Token)
	if err != nil {
		return nil, err
	}

	switch t.Kind {
	case ItemKindPR:
		item, err := fetchPR(ctx, client, t)
		if err == nil {
			return item, nil
		}
		// Shorthand defaults to PR; on 404, fall through to
		// the issue endpoint. Issue-vs-PR is unified per-repo
		// on GitHub so a shorthand can't pre-disambiguate.
		// Note: fetchPR has already converted the go-github
		// error into our wrapper, so we unwrap *HTTPError
		// directly here (not the upstream type).
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			issueTarget := t
			issueTarget.Kind = ItemKindIssue
			return fetchIssue(ctx, client, issueTarget)
		}
		return nil, err
	case ItemKindIssue:
		return fetchIssue(ctx, client, t)
	default:
		return nil, fmt.Errorf("github: unsupported target kind %q", t.Kind)
	}
}

func fetchPR(ctx context.Context, client *gogithub.Client, t Target) (*Item, error) {
	pr, _, err := client.PullRequests.Get(ctx, t.Owner, t.Repo, t.Number)
	if err != nil {
		if httpErr := asHTTPError(err); httpErr != nil {
			return nil, httpErr
		}
		return nil, fmt.Errorf("github: PullRequests.Get %s/%s#%d: %w", t.Owner, t.Repo, t.Number, err)
	}
	return prFromGoGithub(t, pr), nil
}

func fetchIssue(ctx context.Context, client *gogithub.Client, t Target) (*Item, error) {
	iss, _, err := client.Issues.Get(ctx, t.Owner, t.Repo, t.Number)
	if err != nil {
		if httpErr := asHTTPError(err); httpErr != nil {
			return nil, httpErr
		}
		return nil, fmt.Errorf("github: Issues.Get %s/%s#%d: %w", t.Owner, t.Repo, t.Number, err)
	}
	// go-github's `Issues.Get` also returns PRs (PRs are a
	// subset of issues on the API). `IsPullRequest()` is the
	// discriminator; re-route to `PullRequests.Get` for
	// PR-specific fields like `head`, `base`, `merged`.
	if iss.IsPullRequest() {
		prTarget := t
		prTarget.Kind = ItemKindPR
		return fetchPR(ctx, client, prTarget)
	}
	return issueFromGoGithub(t, iss), nil
}

// prFromGoGithub translates go-github's PR struct into our
// normalized Item. Decouples the wire envelope from upstream
// API drift.
func prFromGoGithub(t Target, pr *gogithub.PullRequest) *Item {
	item := &Item{
		Target:        t,
		Number:        nonZeroOr(pr.GetNumber(), t.Number),
		Type:          ItemKindPR,
		State:         pr.GetState(),
		Title:         pr.GetTitle(),
		Body:          pr.GetBody(),
		URL:           pr.GetHTMLURL(),
		CreatedAt:     pr.GetCreatedAt().Time,
		UpdatedAt:     pr.GetUpdatedAt().Time,
		ClosedAt:      timestampPtr(pr.ClosedAt),
		MergedAt:      timestampPtr(pr.MergedAt),
		Merged:        pr.GetMerged(),
		Author:        userLogin(pr.User),
		Assignees:     usersToLogins(pr.Assignees),
		Reviewers:     usersToLogins(pr.RequestedReviewers),
		CommentCount:  pr.GetComments() + pr.GetReviewComments(),
		LastCommentAt: nonZeroTimePtr(pr.GetUpdatedAt().Time),
		Labels:        labelsToNames(pr.Labels),
	}
	if pr.Base != nil {
		item.BaseBranch = pr.Base.GetRef()
	}
	if pr.Head != nil {
		item.HeadBranch = pr.Head.GetRef()
	}
	return item
}

// issueFromGoGithub translates go-github's Issue struct into
// our normalized Item. Same decoupling rationale.
func issueFromGoGithub(t Target, iss *gogithub.Issue) *Item {
	return &Item{
		Target:        t,
		Number:        nonZeroOr(iss.GetNumber(), t.Number),
		Type:          ItemKindIssue,
		State:         iss.GetState(),
		Title:         iss.GetTitle(),
		Body:          iss.GetBody(),
		URL:           iss.GetHTMLURL(),
		CreatedAt:     iss.GetCreatedAt().Time,
		UpdatedAt:     iss.GetUpdatedAt().Time,
		ClosedAt:      timestampPtr(iss.ClosedAt),
		Author:        userLogin(iss.User),
		Assignees:     usersToLogins(iss.Assignees),
		CommentCount:  iss.GetComments(),
		LastCommentAt: nonZeroTimePtr(iss.GetUpdatedAt().Time),
		Labels:        issueLabelsToNames(iss.Labels),
	}
}

// --- helpers (decoupling layer between go-github types + Item) ---

func userLogin(u *gogithub.User) string {
	if u == nil {
		return ""
	}
	return u.GetLogin()
}

func usersToLogins(us []*gogithub.User) []string {
	if len(us) == 0 {
		return nil
	}
	out := make([]string, 0, len(us))
	for _, u := range us {
		login := userLogin(u)
		if login == "" {
			continue
		}
		out = append(out, login)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func labelsToNames(ls []*gogithub.Label) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		name := l.GetName()
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// issueLabelsToNames handles the alternate slice type
// go-github returns on the Issue struct.
func issueLabelsToNames(ls []*gogithub.Label) []string {
	return labelsToNames(ls)
}

func timestampPtr(ts *gogithub.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.Time
	if t.IsZero() {
		return nil
	}
	return &t
}

func nonZeroOr(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

func nonZeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
