package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Item is the normalized in-memory shape the fetch path
// produces for either a PR or an issue. The plugin converts
// it into the wire envelope via ToEnvelope; everything in
// here is what we need for the envelope plus a few fields the
// archive-lifecycle check (Cut 4) reads (`State`).
//
// Fields not on the GitHub object (e.g. `Number` re-derived
// from the URL, `Type` for envelope branching, etc.) are
// re-computed at fetch time rather than stored on the wire
// shape's `data:` map directly — keeps the wire shape clean
// against the API.
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

	Author    string  // login of the creator
	Assignees []string // logins
	Reviewers []string // PR-only, logins of requested reviewers (post-merge state may be empty)

	CommentCount int
	// LastCommentAt is the time the most recent comment was
	// posted. Approximated via UpdatedAt today — Cut 3's bulk
	// fetch may refine this with a per-item comments timeline
	// probe. For Cut 2 (single-item) we accept the same
	// approximation.
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
// Keeps the call sites short and the test-fixture wire
// minimal.
type FetchOptions struct {
	Client  *http.Client // nil → default with 30s timeout
	BaseURL string       // empty → DefaultBaseURL
	Token   string       // empty → unauthenticated request (rate-limited; tests use this)
}

// FetchTarget routes to FetchPR or FetchIssue based on the
// Target's Kind. For shorthand inputs (which default to PR
// per parse.go's targetFromShorthand), a PR-fetch that 404s
// re-routes to the issue endpoint.
func FetchTarget(ctx context.Context, opts FetchOptions, t Target) (*Item, error) {
	switch t.Kind {
	case ItemKindPR:
		item, err := fetchPR(ctx, opts, t)
		if err == nil {
			return item, nil
		}
		// Shorthand defaults to PR; if the upstream says 404,
		// the number probably names an issue. Don't fall
		// through for explicit-URL inputs — those signal
		// intent. We can't tell apart "shorthand input" from
		// "URL pull input" at this layer, so the fallback is
		// always tried on a 404. The cost is one extra round
		// trip on an invalid PR-URL input; the benefit is
		// shorthand-as-issue support.
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			issueTarget := t
			issueTarget.Kind = ItemKindIssue
			return fetchIssue(ctx, opts, issueTarget)
		}
		return nil, err
	case ItemKindIssue:
		return fetchIssue(ctx, opts, t)
	default:
		return nil, fmt.Errorf("github: unsupported target kind %q", t.Kind)
	}
}

func fetchPR(ctx context.Context, opts FetchOptions, t Target) (*Item, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d",
		strings.TrimRight(ResolveBaseURL(opts.BaseURL), "/"),
		t.Owner, t.Repo, t.Number,
	)
	var raw prPayload
	if err := getJSON(ctx, opts, endpoint, &raw); err != nil {
		return nil, err
	}
	return raw.intoItem(t), nil
}

func fetchIssue(ctx context.Context, opts FetchOptions, t Target) (*Item, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d",
		strings.TrimRight(ResolveBaseURL(opts.BaseURL), "/"),
		t.Owner, t.Repo, t.Number,
	)
	var raw issuePayload
	if err := getJSON(ctx, opts, endpoint, &raw); err != nil {
		return nil, err
	}
	// GitHub's `/issues/{n}` endpoint also returns PRs (PRs
	// are a subset of issues). The `pull_request` field
	// distinguishes — its presence means "this is a PR
	// reached via the issues endpoint". Re-route to fetchPR
	// for fidelity (PR-specific fields like `mergeable_state`,
	// `head`, `base` aren't on the issue shape).
	if raw.PullRequest != nil {
		prTarget := t
		prTarget.Kind = ItemKindPR
		return fetchPR(ctx, opts, prTarget)
	}
	return raw.intoItem(t), nil
}

// getJSON does one authenticated REST round-trip. Common
// path between PR and issue fetches; keeps header + error-
// handling logic in one place.
func getJSON(ctx context.Context, opts FetchOptions, endpoint string, out any) error {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("github: build GET %s: %w", endpoint, err)
	}
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", PluginName+"/"+PluginVersion)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github: GET %s: %w", endpoint, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decode response from %s: %w", endpoint, err)
	}
	return nil
}

// --- Wire decode types (private; mapped into Item via intoItem). ---

type userPayload struct {
	Login string `json:"login"`
}

type labelPayload struct {
	Name string `json:"name"`
}

type branchPayload struct {
	Ref string `json:"ref"`
}

// prPayload is the trimmed-down shape we decode from
// `/repos/.../pulls/N`. GitHub returns ~80 fields; we pull
// only what the envelope needs. Unknown fields ignored by
// default — forward-compat with API growth.
type prPayload struct {
	Number             int            `json:"number"`
	State              string         `json:"state"`
	Title              string         `json:"title"`
	Body               string         `json:"body"`
	HTMLURL            string         `json:"html_url"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	ClosedAt           *time.Time     `json:"closed_at"`
	MergedAt           *time.Time     `json:"merged_at"`
	Merged             bool           `json:"merged"`
	Comments           int            `json:"comments"`
	ReviewComments     int            `json:"review_comments"`
	User               *userPayload   `json:"user"`
	Assignees          []userPayload  `json:"assignees"`
	RequestedReviewers []userPayload  `json:"requested_reviewers"`
	Base               *branchPayload `json:"base"`
	Head               *branchPayload `json:"head"`
	Labels             []labelPayload `json:"labels"`
}

func (p *prPayload) intoItem(t Target) *Item {
	item := &Item{
		Target:        t,
		Number:        nonZeroOr(p.Number, t.Number),
		Type:          ItemKindPR,
		State:         p.State,
		Title:         p.Title,
		Body:          p.Body,
		URL:           p.HTMLURL,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		ClosedAt:      p.ClosedAt,
		MergedAt:      p.MergedAt,
		Merged:        p.Merged,
		Author:        loginOr(p.User),
		Assignees:     loginsFrom(p.Assignees),
		Reviewers:     loginsFrom(p.RequestedReviewers),
		CommentCount:  p.Comments + p.ReviewComments,
		LastCommentAt: nonZeroTimePtr(p.UpdatedAt),
		Labels:        labelsFrom(p.Labels),
	}
	if p.Base != nil {
		item.BaseBranch = p.Base.Ref
	}
	if p.Head != nil {
		item.HeadBranch = p.Head.Ref
	}
	return item
}

// issuePayload is the trimmed-down shape we decode from
// `/repos/.../issues/N`. The `pull_request` field flips to
// non-nil when GitHub's REST surface returns a PR through
// this endpoint — see fetchIssue's re-route comment.
type issuePayload struct {
	Number      int            `json:"number"`
	State       string         `json:"state"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	HTMLURL     string         `json:"html_url"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	ClosedAt    *time.Time     `json:"closed_at"`
	Comments    int            `json:"comments"`
	User        *userPayload   `json:"user"`
	Assignees   []userPayload  `json:"assignees"`
	Labels      []labelPayload `json:"labels"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

func (p *issuePayload) intoItem(t Target) *Item {
	return &Item{
		Target:        t,
		Number:        nonZeroOr(p.Number, t.Number),
		Type:          ItemKindIssue,
		State:         p.State,
		Title:         p.Title,
		Body:          p.Body,
		URL:           p.HTMLURL,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		ClosedAt:      p.ClosedAt,
		Author:        loginOr(p.User),
		Assignees:     loginsFrom(p.Assignees),
		CommentCount:  p.Comments,
		LastCommentAt: nonZeroTimePtr(p.UpdatedAt),
		Labels:        labelsFrom(p.Labels),
	}
}

// --- helpers ---

func loginOr(u *userPayload) string {
	if u == nil {
		return ""
	}
	return u.Login
}

func loginsFrom(us []userPayload) []string {
	if len(us) == 0 {
		return nil
	}
	out := make([]string, 0, len(us))
	for _, u := range us {
		if u.Login == "" {
			continue
		}
		out = append(out, u.Login)
	}
	return out
}

func labelsFrom(ls []labelPayload) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		if l.Name == "" {
			continue
		}
		out = append(out, l.Name)
	}
	return out
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
