package wikipedia

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestPlugin wires the plugin to a fake Wikipedia API exposed by
// the supplied handler. apiHostOverride is set to the httptest.Server's
// full URL so Match still routes via the production "host ends in
// wikipedia.org" rule, but Fetch hits the local server.
func newTestPlugin(t *testing.T, h http.Handler) (*Plugin, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := New(
		WithHTTPClient(srv.Client()),
		WithAPIHostOverride(srv.URL),
	)
	return p, srv
}

func TestPlugin_MatchAcceptsWikipediaURLsAndShorthand(t *testing.T) {
	t.Parallel()
	p := New()

	cases := []struct {
		input string
		want bool
	}{
		{"https://en.wikipedia.org/wiki/Go_(programming_language)", true},
		{"https://de.wikipedia.org/wiki/Foo", true},
		{"http://en.wikipedia.org/wiki/Bar", true},
		{"https://en.m.wikipedia.org/wiki/Baz", true},
		{"https://en.wikipedia.org/api/rest_v1/page/summary/Foo", false}, // not /wiki/
		{"https://example.com/wiki/Go", false}, // wrong host
		{"https://wikipedia.invalid/wiki/Go", false}, // host doesn't end in wikipedia.org
		{"not a url", false},
		{"", false},

		// Shorthand inputs. Case-insensitive prefix; whitespace after the
		// colon is optional; the topic itself must start with a non-
		// whitespace character.
		{"wikipedia: Iran", true},
		{"wikipedia:Iran", true},
		{"Wikipedia: Iran", true},
		{"WIKIPEDIA: Go (programming language)", true},
		{"wikipedia:", false},
		{"wikipedia: ", false},
		{"wiki: Iran", false}, // wrong prefix (`wiki:` alone isn't us)
		{"wikipedia.org: Iran", false}, // not the shorthand shape
	}
	for _, c := range cases {
		got := p.Match(c.input)
		if got != c.want {
			t.Errorf("Match(%q): want %v, got %v", c.input, c.want, got)
		}
	}
}

func TestPlugin_FetchHappyPath(t *testing.T) {
	t.Parallel()

	const title = "Go_(programming_language)"
	const summaryPath = "/api/rest_v1/page/summary/" + title
	const actionPath = "/w/api.php"
	const wantTitle = "Go (programming language)"
	const wantRawContent = "Go is a statically typed, compiled programming language designed at Google. " +
		"This is the full plaintext body that the action API returns — many paragraphs longer than the summary."

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header: want %q, got %q", "application/json", got)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("User-Agent header: want non-empty (Wikipedia requires it)")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case summaryPath:
			_, _ = fmt.Fprintf(w, `{
				"title": %q,
				"lang": "en"
			}`, wantTitle)
		case actionPath:
			// Verify the action-API query carries the right
			// parameters — explaintext on, formatversion 2, prop
			// extracts, and the title.
			q := r.URL.Query()
			if q.Get("action") != "query" || q.Get("prop") != "extracts" ||
				q.Get("explaintext") != "1" || q.Get("formatversion") != "2" ||
				q.Get("titles") != title {
				t.Errorf("action API query mismatch: %v", q)
			}
			_, _ = fmt.Fprintf(w, `{
				"query": {
					"pages": [
						{"pageid": 12345, "title": %q, "extract": %q}
					]
				}
			}`, wantTitle, wantRawContent)
		default:
			t.Errorf("upstream path: unexpected %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	p := New(
		WithHTTPClient(server.Client()),
		WithAPIHostOverride(server.URL),
	)

	outcome, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/"+title)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("outcome.Article: want non-nil for URL input, got Options=%+v", outcome.Options)
	}
	article := outcome.Article

	// Per ADR-0021: plugin emits descriptive Name; daemon's
	// slug.Slug derives the source-node ID at translation time.
	if article.Name != wantTitle {
		t.Errorf("article.Name: want %q, got %q", wantTitle, article.Name)
	}
	if article.Data["title"] != wantTitle {
		t.Errorf("data.title: want %q, got %v", wantTitle, article.Data["title"])
	}
	if _, present := article.Data["extract"]; present {
		t.Errorf("data.extract: must NOT be set post- a prior PR (RawContent carries the body now)")
	}
	if article.Data["lang"] != "en" {
		t.Errorf("data.lang: want %q, got %v", "en", article.Data["lang"])
	}
	wantURL := "https://en.wikipedia.org/wiki/" + title
	if article.Data["url"] != wantURL {
		t.Errorf("data.url: want %q, got %v", wantURL, article.Data["url"])
	}
	if article.RawContent != wantRawContent {
		t.Errorf("article.RawContent: want %q, got %q", wantRawContent, article.RawContent)
	}

	if len(article.Provenance) != 1 {
		t.Fatalf("provenance: want 1 entry, got %d", len(article.Provenance))
	}
	// Per ADR-0021: plugin no longer slugifies; provenance source
	// is the canonical resolved URL (URL-stable identifier).
	if want := "https://en.wikipedia.org/wiki/Go_(programming_language)"; article.Provenance[0].Source != want {
		t.Errorf("prov[0].source: want %q, got %q", want, article.Provenance[0].Source)
	}
	if article.Provenance[0].FetchedAt.IsZero() {
		t.Errorf("prov[0].FetchedAt: want non-zero")
	}
	if !article.Provenance[0].OK {
		t.Errorf("prov[0].OK: want true on a successful fetch")
	}
}

// TestBuildActionAPIURL_QueryEncodesTitle pins the cold-reviewer's a prior PR catch:
// the action API's titles parameter is a query-string value, not a
// path segment, so encoding rules differ. A literal `+` in a path
// is fine; in a query value it's interpreted as a space. The
// builder must path-decode the input (which arrives in path-encoded
// form from u.EscapedPath) and re-encode for query context.
func TestBuildActionAPIURL_QueryEncodesTitle(t *testing.T) {
	t.Parallel()

	// Path-encoded input: a literal `+` in a Wikipedia title
	// (e.g. "C++" → "C%2B%2B" via url.PathEscape, or "C++" if a
	// caller passed it pre-decoded). Either way, the query
	// component must be `C%2B%2B` so the upstream parses titles=C++
	// (not titles=C which is what `+` decodes to in a query).
	got := buildActionAPIURL("en.wikipedia.org", "", "C%2B%2B")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if want, gotTitle := "C++", u.Query().Get("titles"); gotTitle != want {
		t.Errorf("titles param after path-decode + query-encode round-trip: want %q, got %q", want, gotTitle)
	}
	// Direct ampersand-in-title test: an `&` in a title MUST be
	// query-encoded so it doesn't get parsed as a parameter
	// separator. e.g. "Rock & Roll" → titles=Rock+%26+Roll.
	got = buildActionAPIURL("en.wikipedia.org", "", "Rock_%26_Roll")
	u, err = url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if want, gotTitle := "Rock_&_Roll", u.Query().Get("titles"); gotTitle != want {
		t.Errorf("titles param with `&`: want %q, got %q", want, gotTitle)
	}
}

// TestPlugin_FetchExtractFailureIsNonFatal pins ADR-0008 partial-
// degradation behavior: when the action-API call fails, Fetch still
// succeeds with an empty RawContent. The summary-derived structured
// fields (id/kind/data/provenance) still flow through; the agent
// can re-ingest with force_refetch when the action API recovers.
// TestPlugin_FetchCanonicalizesOnRedirect pins's
// fix: when Wikipedia's REST API returns the canonical post-redirect
// title, the entity slug is built from THAT title, not from the
// input URL's path segment. Multiple input URLs that all redirect
// to the same Wikipedia article must produce a single entity ID.
//
// Pre-fix bug: ingesting `Brass:_Birmingham`, `Brass:_Lancashire`,
// and `Brass_(board_game)` produced three separate entities even
// though all three resolved to the same article upstream. Fix
// substitutes summary.Title at slug-build time; this test exercises
// all three input paths against a fake API that returns the same
// canonical summary regardless of the requested path.
func TestPlugin_FetchCanonicalizesOnRedirect(t *testing.T) {
	t.Parallel()

	const canonicalTitle = "Brass (board game)"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			// Same canonical summary regardless of which redirect-
			// source was requested. Mirrors Wikipedia's real
			// behavior: summary always reflects the redirect
			// target.
			_, _ = fmt.Fprintf(w, `{"title":%q,"lang":"en"}`, canonicalTitle)
		case r.URL.Path == "/w/api.php":
			_, _ = fmt.Fprintf(w, `{"query":{"pages":[{"pageid":1,"title":%q,"extract":"Brass: Birmingham is a 2018 strategy game."}]}}`, canonicalTitle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	p := New(
		WithHTTPClient(server.Client()),
		WithAPIHostOverride(server.URL),
	)

	inputs := []string{
		"https://en.wikipedia.org/wiki/Brass:_Birmingham",
		"https://en.wikipedia.org/wiki/Brass:_Lancashire",
		"https://en.wikipedia.org/wiki/Brass_(board_game)",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			outcome, err := p.Fetch(context.Background(), input)
			if err != nil {
				t.Fatalf("Fetch(%q): %v", input, err)
			}
			if outcome.Article == nil {
				t.Fatalf("Fetch(%q): want Article, got Options=%+v", input, outcome.Options)
			}
			// Per ADR-0021: plugin emits descriptive Name; daemon
			// derives the slug. All three redirect-source inputs must
			// converge on the same canonical Name → same daemon-side
			// slug.
			if outcome.Article.Name != canonicalTitle {
				t.Errorf("Fetch(%q): article.Name want %q, got %q",
					input, canonicalTitle, outcome.Article.Name)
			}
		})
	}
}

func TestPlugin_FetchExtractFailureIsNonFatal(t *testing.T) {
	t.Parallel()

	const title = "PartialFail"
	const summaryPath = "/api/rest_v1/page/summary/" + title

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case summaryPath:
			_, _ = fmt.Fprint(w, `{"title":"PartialFail","lang":"en"}`)
		case "/w/api.php":
			http.Error(w, `{"error":"upstream broke"}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	p := New(
		WithHTTPClient(server.Client()),
		WithAPIHostOverride(server.URL),
	)

	outcome, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/"+title)
	if err != nil {
		t.Fatalf("Fetch: want success despite extract failure, got err: %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("outcome.Article: want non-nil")
	}
	if outcome.Article.Name != "PartialFail" {
		t.Errorf("article.Name: %q", outcome.Article.Name)
	}
	if outcome.Article.RawContent != "" {
		t.Errorf("article.RawContent: want empty when action API fails, got %q", outcome.Article.RawContent)
	}
}

func TestPlugin_FetchUpstream404IsErrNotFoundUpstream(t *testing.T) {
	t.Parallel()

	p, _ := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"…not_found","title":"Not found."}`))
	}))

	_, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/Definitely_Not_An_Article")
	if !errors.Is(err, ErrNotFoundUpstream) {
		t.Errorf("Fetch on 404: want ErrNotFoundUpstream, got %v", err)
	}
}

func TestPlugin_FetchUpstream5xxReturnsWrappedError(t *testing.T) {
	t.Parallel()

	p, _ := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"upstream broke"}`))
	}))

	_, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/Anything")
	if err == nil {
		t.Fatalf("want error on 500, got nil")
	}
	if errors.Is(err, ErrNotFoundUpstream) {
		t.Errorf("5xx must NOT be classified as ErrNotFoundUpstream")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error: want to mention status 500, got %q", err.Error())
	}
}

func TestPlugin_FetchHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	p, _ := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Fetch(ctx, "https://en.wikipedia.org/wiki/Anything")
	if err == nil {
		t.Fatalf("want error on context cancel, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Errorf("error: want context-related, got %q", err.Error())
	}
}

func TestPlugin_FetchOnUnparseableJSONReturnsError(t *testing.T) {
	t.Parallel()

	p, _ := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not actually json`))
	}))

	_, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/Anything")
	if err == nil {
		t.Fatalf("want error on bad JSON, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error: want to mention decode, got %q", err.Error())
	}
}

func TestPlugin_FetchOnEmptyTitleReturnsError(t *testing.T) {
	t.Parallel()

	p, _ := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"extract":"x","lang":"en"}`))
	}))

	_, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/Anything")
	if err == nil {
		t.Fatalf("want error on missing title, got nil")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error: want to mention title, got %q", err.Error())
	}
}

func TestPlugin_FetchURLPathBuildsCorrectAPICall(t *testing.T) {
	t.Parallel()

	const apiPath = "/api/rest_v1/page/summary/"

	// seenPath is captured ONLY for the summary endpoint — the
	// action-API extract call also fires but it's
	// orthogonal to what this test asserts (which is that the
	// summary URL is built from the input correctly).
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, apiPath) {
			seenPath = r.URL.Path
			_, _ = w.Write([]byte(`{"title":"x","lang":"en"}`))
			return
		}
		// Action-API extract call — return a benign empty extract.
		_, _ = w.Write([]byte(`{"query":{"pages":[{"pageid":1,"title":"x","extract":""}]}}`))
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))

	cases := []struct {
		input string
		expect string
	}{
		{"https://en.wikipedia.org/wiki/Go_(programming_language)", apiPath + "Go_(programming_language)"},
		{"https://de.wikipedia.org/wiki/Klingonisch", apiPath + "Klingonisch"},
		{"https://en.wikipedia.org/wiki/Special:Search", apiPath + "Special:Search"},
	}
	for _, c := range cases {
		seenPath = ""
		if _, err := p.Fetch(context.Background(), c.input); err != nil {
			t.Fatalf("Fetch(%q): %v", c.input, err)
		}
		if seenPath != c.expect {
			t.Errorf("upstream path for %q: want %q, got %q", c.input, c.expect, seenPath)
		}
	}
}

func TestPlugin_FetchShorthandResolvesWithDefaultLang(t *testing.T) {
	t.Parallel()

	const apiPathPrefix = "/api/rest_v1/page/summary/"

	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, apiPathPrefix):
			seenPath = r.URL.Path
			_, _ = fmt.Fprint(w, `{"title":"Iran","lang":"en"}`)
		case r.URL.Query().Get("list") == "search":
			// Shorthand triggers search-first; single-result response
			// makes Fetch proceed to the article path.
			_, _ = fmt.Fprint(w, `{"query":{"search":[{"title":"Iran","pageid":1,"snippet":"-"}]}}`)
		default:
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"pageid":1,"title":"Iran","extract":"Iran is a country."}]}}`)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))

	outcome, err := p.Fetch(context.Background(), "wikipedia: Iran")
	if err != nil {
		t.Fatalf("Fetch(shorthand): %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("outcome.Article: want non-nil for single-search-result, got Options=%+v", outcome.Options)
	}
	if seenPath != apiPathPrefix+"Iran" {
		t.Errorf("upstream API path: want %q, got %q", apiPathPrefix+"Iran", seenPath)
	}
	if outcome.Article.Name != "Iran" {
		t.Errorf("article.Name: want %q, got %q", "Iran", outcome.Article.Name)
	}
	if got, want := outcome.Article.Data["url"], "https://en.wikipedia.org/wiki/Iran"; got != want {
		t.Errorf("data.url (canonical): want %q, got %q", want, got)
	}
}

func TestPlugin_FetchShorthandHonoursWithLang(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"Iran","lang":"de"}`)
		case r.URL.Query().Get("list") == "search":
			_, _ = fmt.Fprint(w, `{"query":{"search":[{"title":"Iran","pageid":1,"snippet":"-"}]}}`)
		default:
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"pageid":1,"title":"Iran","extract":""}]}}`)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(
		WithHTTPClient(srv.Client()),
		WithAPIHostOverride(srv.URL),
		WithLang("de"),
	)

	outcome, err := p.Fetch(context.Background(), "wikipedia: Iran")
	if err != nil {
		t.Fatalf("Fetch(shorthand de): %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("outcome.Article: want non-nil")
	}
	if got, want := outcome.Article.Data["url"], "https://de.wikipedia.org/wiki/Iran"; got != want {
		t.Errorf("data.url with WithLang(\"de\"): want %q, got %q", want, got)
	}
}

func TestPlugin_FetchShorthandWithSpacesInTopic(t *testing.T) {
	t.Parallel()

	const apiPathPrefix = "/api/rest_v1/page/summary/"

	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, apiPathPrefix):
			seenPath = r.URL.Path
			_, _ = fmt.Fprint(w, `{"title":"Go (programming language)","lang":"en"}`)
		case r.URL.Query().Get("list") == "search":
			_, _ = fmt.Fprint(w, `{"query":{"search":[{"title":"Go (programming language)","pageid":1,"snippet":"-"}]}}`)
		default:
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"pageid":1,"title":"Go (programming language)","extract":""}]}}`)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))

	outcome, err := p.Fetch(context.Background(), "wikipedia: Go (programming language)")
	if err != nil {
		t.Fatalf("Fetch(shorthand with spaces): %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("outcome.Article: want non-nil")
	}
	// Spaces in shorthand topics convert to underscores per Wikipedia's
	// URL convention; parens stay literal (PathEscape leaves them).
	wantPath := apiPathPrefix + "Go_(programming_language)"
	if seenPath != wantPath {
		t.Errorf("upstream API path: want %q, got %q", wantPath, seenPath)
	}
	if outcome.Article.Name != "Go (programming language)" {
		t.Errorf("article.Name: want %q, got %q",
			"Go (programming language)", outcome.Article.Name)
	}
	if got, want := outcome.Article.Data["url"], "https://en.wikipedia.org/wiki/Go_(programming_language)"; got != want {
		t.Errorf("data.url: want %q, got %q", want, got)
	}
}

func TestPlugin_FetchShorthandPercentEncodesNonASCII(t *testing.T) {
	t.Parallel()

	// München → M%C3%BCnchen in the URL path. PathEscape handles the
	// UTF-8 byte sequence; this regression test trips if we ever swap
	// the encoding strategy and break non-ASCII titles.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"München","lang":"de"}`)
		case r.URL.Query().Get("list") == "search":
			_, _ = fmt.Fprint(w, `{"query":{"search":[{"title":"München","pageid":1,"snippet":"-"}]}}`)
		default:
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"pageid":1,"title":"München","extract":""}]}}`)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(
		WithHTTPClient(srv.Client()),
		WithAPIHostOverride(srv.URL),
		WithLang("de"),
	)
	outcome, err := p.Fetch(context.Background(), "wikipedia: München")
	if err != nil {
		t.Fatalf("Fetch(shorthand non-ASCII): %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("outcome.Article: want non-nil")
	}
	if got, want := outcome.Article.Data["url"], "https://de.wikipedia.org/wiki/M%C3%BCnchen"; got != want {
		t.Errorf("data.url (non-ASCII): want %q, got %q", want, got)
	}
}

func Test_resolveURL(t *testing.T) {
	t.Parallel()

	p := New(WithLang("en"))

	cases := []struct {
		input string
		want string
	}{
		{"https://en.wikipedia.org/wiki/Iran", "https://en.wikipedia.org/wiki/Iran"}, // pass-through
		{"https://de.wikipedia.org/wiki/Klingonisch", "https://de.wikipedia.org/wiki/Klingonisch"},
		{"wikipedia: Iran", "https://en.wikipedia.org/wiki/Iran"},
		{"wikipedia:Iran", "https://en.wikipedia.org/wiki/Iran"},
		{"WIKIPEDIA: Iran", "https://en.wikipedia.org/wiki/Iran"},
		{"wikipedia: Go (programming language)", "https://en.wikipedia.org/wiki/Go_(programming_language)"},
	}
	for _, c := range cases {
		got, err := p.resolveURL(c.input)
		if err != nil {
			t.Errorf("resolveURL(%q): unexpected err: %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveURL(%q): want %q, got %q", c.input, c.want, got)
		}
	}

	// `wikipedia: ` (whitespace-only topic) doesn't match the
	// shorthand regex (the `\S` anchor requires a non-whitespace
	// character). resolveURL returns it unchanged, and Match() rejects
	// it — so yaad-index never dispatches such an input here. Asserted
	// in TestPlugin_MatchAcceptsWikipediaURLsAndShorthand alongside
	// the rest of the negative cases.
}

// TestPlugin_FetchShorthandWithParensReturnsArticleDirectly pins
// the the source issue fix: shorthand inputs whose topic is already the
// canonical Wikipedia title (parens-disambiguator form) MUST go
// straight through the URL fetch path — no search-first interception.
//
// Legacy, search-first ran on every shorthand and intercepted
// `wikipedia: Martin Wallace (game designer)` by surfacing
// Options for the same person + his games, even though the topic
// was already a fully-qualified title. Agents looped on the
// disambig response and fell back to URL anyway.
func TestPlugin_FetchShorthandWithParensReturnsArticleDirectly(t *testing.T) {
	t.Parallel()

	var summaryHits, searchHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			summaryHits++
			_, _ = fmt.Fprint(w, `{"title":"Martin Wallace (game designer)","lang":"en","extract":"British boardgame designer"}`)
		case r.URL.Path == "/w/api.php" && r.URL.Query().Get("list") == "search":
			searchHits++
			t.Errorf("search must NOT run on parens-form shorthand; URL=%q", r.URL.String())
		case r.URL.Path == "/w/api.php":
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"extract":"body"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))

	outcome, err := p.Fetch(context.Background(), "wikipedia: Martin Wallace (game designer)")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("Article: want non-nil for canonical parens-form shorthand, got %+v", outcome)
	}
	// Per ADR-0021: source-side Name retains parens-disambig
	// (round-trips to the Wikipedia URL); daemon's slug.Slug
	// then derives `wikipedia-article:martin-wallace-game-designer`.
	if outcome.Article.Name != "Martin Wallace (game designer)" {
		t.Errorf("Article.Name: want %q, got %q",
			"Martin Wallace (game designer)", outcome.Article.Name)
	}
	if summaryHits != 1 {
		t.Errorf("summary endpoint hit count: want 1, got %d", summaryHits)
	}
	if searchHits != 0 {
		t.Errorf("search endpoint hit count: want 0 (no search-first on shorthand post-), got %d", searchHits)
	}
}

// TestPlugin_FetchShorthandBareTopicViaDisambigPage pins the
// shorthand-with-bare-topic path. `wikipedia: Martin Wallace`
// resolves to `/wiki/Martin_Wallace` which IS Wikipedia's
// disambiguation page — Wikipedia's own `summary.type ==
// "disambiguation"` signal triggers the existing URL-side
// disambig fallback, which calls `searchArticles` to surface
// candidates. The disambig signal comes from Wikipedia, NOT
// from a pre-emptive search-first heuristic.
func TestPlugin_FetchShorthandBareTopicViaDisambigPage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"Martin Wallace","lang":"en","type":"disambiguation"}`)
		case r.URL.Path == "/w/api.php" && r.URL.Query().Get("list") == "search":
			_, _ = fmt.Fprint(w, `{"query":{"search":[
				{"title":"Martin Wallace (game designer)","pageid":1,"snippet":"British board-game designer"},
				{"title":"Martin Wallace (American football)","pageid":2,"snippet":"American football quarterback"}
			]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))

	outcome, err := p.Fetch(context.Background(), "wikipedia: Martin Wallace")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article != nil {
		t.Fatalf("Article: want nil on disambig-page fallback, got %+v", outcome.Article)
	}
	if len(outcome.Options) != 2 {
		t.Fatalf("Options: want 2 from the disambig-page fallback search, got %d (%+v)",
			len(outcome.Options), outcome.Options)
	}
	want := []string{"Martin Wallace (game designer)", "Martin Wallace (American football)"}
	for i, w := range want {
		if outcome.Options[i].ID != w {
			t.Errorf("Options[%d].ID: want %q, got %q", i, w, outcome.Options[i].ID)
		}
	}
}

// TestPlugin_FetchShorthandNonexistentTopicReturnsNotFound pins the
// post- contract for a shorthand whose topic doesn't resolve to
// any Wikipedia article: the URL summary endpoint returns 404 and
// the plugin propagates ErrNotFoundUpstream. Before, the
// shorthand-only search-first path returned empty Options for this
// shape; the search-first removal flips it to a hard not-found,
// which is the cleaner shape (the agent now sees an explicit miss
// instead of a fake disambig that's actually empty).
func TestPlugin_FetchShorthandNonexistentTopicReturnsNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/") {
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))
	outcome, err := p.Fetch(context.Background(), "wikipedia: ZxqyNoSuchTopic")
	if !errors.Is(err, ErrNotFoundUpstream) {
		t.Fatalf("err: want ErrNotFoundUpstream, got %v", err)
	}
	if outcome != nil {
		t.Errorf("outcome: want nil on not-found, got %+v", outcome)
	}
}

// TestPlugin_FetchDisambiguationPageURLFallback covers the URL-input
// path where the URL itself resolves to a Wikipedia disambiguation
// page (summary.type == "disambiguation"). The plugin falls back to
// search using the article title to surface candidates rather than
// materializing the disambig page as an article.
func TestPlugin_FetchDisambiguationPageURLFallback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"Martin Wallace","lang":"en","type":"disambiguation"}`)
		case r.URL.Query().Get("list") == "search":
			_, _ = fmt.Fprint(w, `{"query":{"search":[
				{"title":"Martin Wallace (game designer)","pageid":1,"snippet":"English board game designer"},
				{"title":"Martin Wallace (American football)","pageid":2,"snippet":"American football quarterback"}
			]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))

	outcome, err := p.Fetch(context.Background(), "https://en.wikipedia.org/wiki/Martin_Wallace")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article != nil {
		t.Errorf("Article: want nil for disambig-page URL, got %+v", outcome.Article)
	}
	if len(outcome.Options) != 2 {
		t.Fatalf("Options: want 2, got %d", len(outcome.Options))
	}
	if outcome.Options[0].ID != "Martin Wallace (game designer)" {
		t.Errorf("Options[0].ID: %q", outcome.Options[0].ID)
	}
	if len(outcome.Provenance) != 1 || outcome.Provenance[0].Source != "wikipedia:disambiguation" {
		t.Errorf("Provenance: want one wikipedia:disambiguation entry, got %+v", outcome.Provenance)
	}
}

// TestStripSnippetMarkup pins the search-snippet HTML cleanup. A
// regression that breaks the `<span class="searchmatch">` strip
// would surface raw HTML to the agent.
func TestStripSnippetMarkup(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in string
		want string
	}{
		{`English board game <span class="searchmatch">designer</span>`, "English board game designer"},
		{`<span class="searchmatch">Tehran</span> is the capital`, "Tehran is the capital"},
		{`No tags at all`, "No tags at all"},
		{``, ``},
		{`<span class="searchmatch">a</span> <span class="searchmatch">b</span> c`, "a b c"},
	}
	for _, c := range cases {
		got := stripSnippetMarkup(c.in)
		if got != c.want {
			t.Errorf("stripSnippetMarkup(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}

// TestMediaWikiHeadingsToMarkdown pins the conversion at all common
// section levels (1-4 — Wikipedia rarely nests deeper) plus the
// defensive case where a non-heading line contains `==` text that
// must NOT be rewritten. Closes the source issue.
func TestMediaWikiHeadingsToMarkdown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in string
		want string
	}{
		{
			name: "level 1",
			in: "Some intro paragraph.\n= Top section =\nBody under the top section.",
			want: "Some intro paragraph.\n# Top section\nBody under the top section.",
		},
		{
			name: "level 2",
			in: "Intro.\n== History ==\nHistorical context.",
			want: "Intro.\n## History\nHistorical context.",
		},
		{
			name: "level 3",
			in: "Intro.\n=== Specific topic ===\nDetail.",
			want: "Intro.\n### Specific topic\nDetail.",
		},
		{
			name: "level 4",
			in: "Intro.\n==== Sub-detail ====\nFurther text.",
			want: "Intro.\n#### Sub-detail\nFurther text.",
		},
		{
			name: "all four levels in one document",
			in: "= L1 =\n== L2 ==\n=== L3 ===\n==== L4 ====",
			want: "# L1\n## L2\n### L3\n#### L4",
		},
		{
			name: "level 7+ caps at markdown h6",
			in: "======= Deep nest =======",
			want: "###### Deep nest",
		},
		{
			name: "non-heading line with `==` not rewritten",
			// e.g. an article paragraph mentioning an equation; not
			// at start-of-line + balanced ends, so the regex doesn't
			// fire.
			in: "The equation x == y holds.",
			want: "The equation x == y holds.",
		},
		{
			name: "heading-shaped line embedded mid-paragraph stays untouched",
			// `==Foo==` appearing inside prose (no leading newline +
			// no trailing newline framing it) — the mid-paragraph
			// shape isn't a wiki heading, regex anchors to ^/$ per
			// line so this is preserved.
			in: "Some prose with ==inline equals== mid-line.",
			want: "Some prose with ==inline equals== mid-line.",
		},
		{
			name: "title with multiple words preserved",
			in: "== Founding and early years ==",
			want: "## Founding and early years",
		},
		{
			name: "empty input returns empty",
			in: "",
			want: "",
		},
		{
			name: "no headings round-trip unchanged",
			in: "Plain article content with\nmultiple paragraphs but no\nheadings whatsoever.",
			want: "Plain article content with\nmultiple paragraphs but no\nheadings whatsoever.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := mediaWikiHeadingsToMarkdown(c.in)
			if got != c.want {
				t.Errorf("input:\n%q\nwant:\n%q\ngot:\n%q", c.in, c.want, got)
			}
		})
	}
}

// canonicalAxisFixture builds a single httptest.Server that responds
// to all three upstream endpoints yaad-wikipedia hits when canonical
// detection is in play: REST summary, action API extract, Wikidata
// EntityData. Each call accepts pre-canned per-title data so a
// table-driven test exercises the kind-detection path for several
// shapes without re-spelling the fixture.
type canonicalFixtureRow struct {
	title string
	wantSummary string // JSON for /api/rest_v1/page/summary/<title>
	wantWikidata string // JSON for /wiki/Special:EntityData/<Qid>.json (empty → 404)
	qid string // wikibase_item to claim from summary; "" when not set
	wikidataFail bool // when true, the Wikidata route returns 500
}

func canonicalAxisFixture(t *testing.T, rows []canonicalFixtureRow) *httptest.Server {
	t.Helper()
	byTitle := make(map[string]canonicalFixtureRow, len(rows))
	byQID := make(map[string]canonicalFixtureRow, len(rows))
	for _, r := range rows {
		byTitle[r.title] = r
		if r.qid != "" {
			byQID[r.qid] = r
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			title := strings.TrimPrefix(r.URL.Path, "/api/rest_v1/page/summary/")
			if row, ok := byTitle[title]; ok {
				_, _ = fmt.Fprint(w, row.wantSummary)
				return
			}
			http.NotFound(w, r)
		case r.URL.Path == "/w/api.php":
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"pageid":1,"title":"x","extract":"body"}]}}`)
		case strings.HasPrefix(r.URL.Path, "/wiki/Special:EntityData/"):
			qid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/wiki/Special:EntityData/"), ".json")
			row, ok := byQID[qid]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if row.wikidataFail {
				http.Error(w, `{"error":"upstream broke"}`, http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprint(w, row.wantWikidata)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPlugin_FetchEmitsCanonicalStub_TableDriven exercises the
// canonical-axis acceptance criteria from a prior PR:
// Tehran → city, Martin Wallace → person, Caverna → boardgame.
// Plus three negative cases: no wikibase_item → no canonical;
// unknown P31 Q-id → no canonical; Wikidata API 500 → no
// canonical (source article still lands).
func TestPlugin_FetchEmitsCanonicalStub_TableDriven(t *testing.T) {
	t.Parallel()

	// Per ADR-0021 +: the plugin emits the
	// `is_about` canonical-edge with a descriptive Name (parens-
	// disambig stripped) + Kind. Daemon's slug.Slug derives the
	// canonical-label slug (`person:martin-wallace` etc.) — the
	// daemon-side derivation is pinned in yaad-index's
	// internal/slug tests, not here.
	cases := []struct {
		name string
		title string
		fixture canonicalFixtureRow
		wantStubKind string // "" when no canonical edge expected
		wantStubName string // descriptive name on the edge target (parens stripped)
		wantArticleOK bool // article itself always succeeds
	}{
		{
			name: "tehran → city",
			title: "Tehran",
			fixture: canonicalFixtureRow{
				title: "Tehran",
				qid: "Q3692",
				wantSummary: `{"title":"Tehran","lang":"en","wikibase_item":"Q3692"}`,
				wantWikidata: `{"entities":{"Q3692":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q515"}}}}]}}}}`,
			},
			wantStubKind: "city",
			wantStubName: "Tehran",
			wantArticleOK: true,
		},
		{
			name: "martin wallace → person",
			title: "Martin_Wallace",
			fixture: canonicalFixtureRow{
				title: "Martin_Wallace",
				qid: "Q956036",
				wantSummary: `{"title":"Martin Wallace","lang":"en","wikibase_item":"Q956036"}`,
				wantWikidata: `{"entities":{"Q956036":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q5"}}}}]}}}}`,
			},
			wantStubKind: "person",
			wantStubName: "Martin Wallace",
			wantArticleOK: true,
		},
		{
			name: "caverna → boardgame",
			title: "Caverna",
			fixture: canonicalFixtureRow{
				title: "Caverna",
				qid: "Q12480556",
				wantSummary: `{"title":"Caverna","lang":"en","wikibase_item":"Q12480556"}`,
				wantWikidata: `{"entities":{"Q12480556":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q131436"}}}}]}}}}`,
			},
			wantStubKind: "boardgame",
			wantStubName: "Caverna",
			wantArticleOK: true,
		},
		{
			//: source `wikipedia:martin-wallace-game-designer`
			// keeps the parens-derived suffix (the Wikipedia URL
			// round-trips through it), but canonical `person:` ID
			// strips the trailing `(game designer)` so cross-plugin
			// merge with bgg's designed_by-emitted `person:martin-wallace`
			// works.
			name: "person with parens-disambig → canonical strips parens",
			title: "Martin_Wallace_(game_designer)",
			fixture: canonicalFixtureRow{
				title: "Martin_Wallace_(game_designer)",
				qid: "Q956036",
				wantSummary: `{"title":"Martin Wallace (game designer)","lang":"en","wikibase_item":"Q956036"}`,
				wantWikidata: `{"entities":{"Q956036":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q5"}}}}]}}}}`,
			},
			wantStubKind: "person",
			wantStubName: "Martin Wallace",
			wantArticleOK: true,
		},
		{
			//: `Brass (board game)` → canonical
			// `boardgame:brass` (matches `bgg:` numeric ingest of
			// the same game once both PRs land — bgg drops
			// year suffix in parallel).
			name: "boardgame with parens-disambig → canonical strips parens",
			title: "Brass_(board_game)",
			fixture: canonicalFixtureRow{
				title: "Brass_(board_game)",
				qid: "Q1573594",
				wantSummary: `{"title":"Brass (board game)","lang":"en","wikibase_item":"Q1573594"}`,
				wantWikidata: `{"entities":{"Q1573594":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q131436"}}}}]}}}}`,
			},
			wantStubKind: "boardgame",
			wantStubName: "Brass",
			wantArticleOK: true,
		},
		{
			// acceptance §4: anime mapping (Q1107)
			// — defensive given the original spec confused Q1107 with
			// Q202444 (which is "given name" on Wikidata, not anime).
			// This test pins Q1107 → `anime` so a regression to
			// Q202444 fails loudly.
			name: "neon genesis evangelion → anime",
			title: "Neon_Genesis_Evangelion",
			fixture: canonicalFixtureRow{
				title: "Neon_Genesis_Evangelion",
				qid: "Q189898",
				wantSummary: `{"title":"Neon Genesis Evangelion","lang":"en","wikibase_item":"Q189898"}`,
				wantWikidata: `{"entities":{"Q189898":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q1107"}}}}]}}}}`,
			},
			wantStubKind: "anime",
			wantStubName: "Neon Genesis Evangelion",
			wantArticleOK: true,
		},
		{
			// acceptance §4: film-series mapping
			// (Q24856) — film-series stays distinct from `movie`
			// (Q11424) per the verified mapping (no consolidation).
			name: "lord of the rings → film-series",
			title: "The_Lord_of_the_Rings_(film_series)",
			fixture: canonicalFixtureRow{
				title: "The_Lord_of_the_Rings_(film_series)",
				qid: "Q170564",
				wantSummary: `{"title":"The Lord of the Rings (film series)","lang":"en","wikibase_item":"Q170564"}`,
				wantWikidata: `{"entities":{"Q170564":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q24856"}}}}]}}}}`,
			},
			wantStubKind: "film-series",
			wantStubName: "The Lord of the Rings",
			wantArticleOK: true,
		},
		{
			// acceptance §4: album mapping (Q482994).
			name: "ok computer → album",
			title: "OK_Computer",
			fixture: canonicalFixtureRow{
				title: "OK_Computer",
				qid: "Q482912",
				wantSummary: `{"title":"OK Computer","lang":"en","wikibase_item":"Q482912"}`,
				wantWikidata: `{"entities":{"Q482912":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q482994"}}}}]}}}}`,
			},
			wantStubKind: "album",
			wantStubName: "OK Computer",
			wantArticleOK: true,
		},
		{
			// acceptance §4: podcast mapping
			// (Q24634210) — newest mapping in the table; pin to
			// catch any silent removal.
			name: "serial → podcast",
			title: "Serial_(podcast)",
			fixture: canonicalFixtureRow{
				title: "Serial_(podcast)",
				qid: "Q18141161",
				wantSummary: `{"title":"Serial (podcast)","lang":"en","wikibase_item":"Q18141161"}`,
				wantWikidata: `{"entities":{"Q18141161":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q24634210"}}}}]}}}}`,
			},
			wantStubKind: "podcast",
			wantStubName: "Serial",
			wantArticleOK: true,
		},
		{
			// acceptance §4 +1 (the implementer's pick):
			// video-game mapping (Q7889) — broadly-applicable
			// category that operators in the gaming domain will
			// frequently enable.
			name: "half-life → video-game",
			title: "Half-Life_(video_game)",
			fixture: canonicalFixtureRow{
				title: "Half-Life_(video_game)",
				qid: "Q193581",
				wantSummary: `{"title":"Half-Life (video game)","lang":"en","wikibase_item":"Q193581"}`,
				wantWikidata: `{"entities":{"Q193581":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q7889"}}}}]}}}}`,
			},
			wantStubKind: "video-game",
			wantStubName: "Half-Life",
			wantArticleOK: true,
		},
		{
			name: "no wikibase_item → no canonical",
			title: "RandomNoQID",
			fixture: canonicalFixtureRow{
				title: "RandomNoQID",
				wantSummary: `{"title":"RandomNoQID","lang":"en"}`,
			},
			wantStubKind: "",
			wantArticleOK: true,
		},
		{
			name: "unknown P31 Q-id → no canonical",
			title: "UnknownInstanceOf",
			fixture: canonicalFixtureRow{
				title: "UnknownInstanceOf",
				qid: "Q9999999",
				wantSummary: `{"title":"UnknownInstanceOf","lang":"en","wikibase_item":"Q9999999"}`,
				wantWikidata: `{"entities":{"Q9999999":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q424242"}}}}]}}}}`,
			},
			wantStubKind: "",
			wantArticleOK: true,
		},
		{
			name: "wikidata API 500 → no canonical, article succeeds",
			title: "WikidataDown",
			fixture: canonicalFixtureRow{
				title: "WikidataDown",
				qid: "Q42",
				wantSummary: `{"title":"WikidataDown","lang":"en","wikibase_item":"Q42"}`,
				wikidataFail: true,
			},
			wantStubKind: "",
			wantArticleOK: true,
		},
		{
			// Closes the source issue: Wikidata responses for real entities
			// (Susanna Clarke, Tehran, etc.) carry many properties
			// alongside P31 — image filenames as bare strings (P18),
			// dates as time-objects (P569), coordinates as
			// globecoordinate objects (P625), monolingualtext on
			// alternate-name properties. The previous decoder ate
			// claims map-wide into a strict {id} struct and crashed
			// on the first non-entity-shaped value before reaching
			// P31. This row pins the now-isolated P31-only decode:
			// foreign-shape neighbors must not break extraction.
			name: "mixed claim value types → P31 still resolves",
			title: "SusannaClarke",
			fixture: canonicalFixtureRow{
				title: "SusannaClarke",
				qid: "Q232772",
				wantSummary: `{"title":"Susanna Clarke","lang":"en",` +
					`"wikibase_item":"Q232772"}`,
				wantWikidata: `{"entities":{"Q232772":{"claims":{` +
					`"P18":[{"mainsnak":{"datavalue":{"value":"Susanna_Clarke.jpg","type":"string"}}}],` +
					`"P569":[{"mainsnak":{"datavalue":{"value":{"time":"+1959-11-01T00:00:00Z","precision":11,"calendarmodel":"http://www.wikidata.org/entity/Q1985727"},"type":"time"}}}],` +
					`"P625":[{"mainsnak":{"datavalue":{"value":{"latitude":52.5,"longitude":-1.9,"globe":"http://www.wikidata.org/entity/Q2"},"type":"globecoordinate"}}}],` +
					`"P1559":[{"mainsnak":{"datavalue":{"value":{"text":"Susanna Clarke","language":"en"},"type":"monolingualtext"}}}],` +
					`"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q5","entity-type":"item","numeric-id":5},"type":"wikibase-entityid"}}}]` +
					`}}}}`,
			},
			wantStubKind: "person",
			wantStubName: "Susanna Clarke",
			wantArticleOK: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := canonicalAxisFixture(t, []canonicalFixtureRow{c.fixture})
			p := New(
				WithHTTPClient(srv.Client()),
				WithAPIHostOverride(srv.URL),
				WithWikidataHostOverride(srv.URL),
			)

			outcome, err := p.Fetch(context.Background(),
				"https://en.wikipedia.org/wiki/"+c.title)
			if !c.wantArticleOK {
				if err == nil {
					t.Fatalf("Fetch: want error, got outcome %+v", outcome)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch: want success, got err: %v", err)
			}
			if outcome.Article == nil {
				t.Fatalf("outcome.Article: want non-nil for URL input, got Options=%+v", outcome.Options)
			}
			article := outcome.Article
			// Per ADR-0021: every source-shape emission carries
			// the universal `is_a` edge to source-type, so the
			// edges block is never fully empty. Canonical
			// `is_about` may or may not appear depending on
			// wikidata resolution.
			if _, hasIsA := article.Edges[SourceTypeEdgeType]; !hasIsA {
				t.Errorf("Edges[%q]: missing universal source-type edge", SourceTypeEdgeType)
			}
			if c.wantStubKind == "" {
				if got, ok := article.Edges[CanonicalEdgeType]; ok {
					t.Errorf("Edges[%q]: want empty (no wikidata kind), got %+v",
						CanonicalEdgeType, got)
				}
				return
			}
			isAbout, ok := article.Edges[CanonicalEdgeType]
			if !ok {
				t.Fatalf("Edges[%q]: missing for resolved kind %q",
					CanonicalEdgeType, c.wantStubKind)
			}
			if len(isAbout) != 1 {
				t.Fatalf("Edges[%q]: want 1 target, got %d",
					CanonicalEdgeType, len(isAbout))
			}
			if got := isAbout[0].Kind; got != c.wantStubKind {
				t.Errorf("Edges[%q][0].Kind: want %q, got %q",
					CanonicalEdgeType, c.wantStubKind, got)
			}
			if got := isAbout[0].Name; got != c.wantStubName {
				t.Errorf("Edges[%q][0].Name: want %q, got %q (parens-disambig should be stripped Per the prior design,)",
					CanonicalEdgeType, c.wantStubName, got)
			}
		})
	}
}

// TestStripTrailingParens covers the canonical-side title-cleaner
// per. The function is the load-bearing piece of
// the cross-plugin dedup story (`person:martin-wallace` matches
// bgg's designed_by-emitted stub once both and
// land).
func TestStripTrailingParens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in string
		want string
	}{
		// Issue spec examples.
		{"Martin Wallace (game designer)", "Martin Wallace"},
		{"Brass (board game)", "Brass"},
		{"London (board game)", "London"},
		{"Age of Steam (board game)", "Age of Steam"},
		// No parens → no-op.
		{"Susanna Clarke", "Susanna Clarke"},
		{"Caverna", "Caverna"},
		// Other parens shapes that should still strip end-anchored.
		{"The Goldfinch (novel)", "The Goldfinch"},
		{"Inception (2010 film)", "Inception"},
		// Non-trailing parens stay (Wikipedia's disambig is end-only).
		{"Foo (Bar) Baz", "Foo (Bar) Baz"},
		// Multiple trailing parens — single strip per v1 trade-off
		// noted in the doc comment. Outer-most trailing parens go;
		// inner-trailing remains.
		{"Foo (a) (b)", "Foo (a)"},
		// Whitespace handling.
		{"Trailing spaces (x) ", "Trailing spaces"},
		{"Brass(no-leading-space)", "Brass"}, // regex tolerates missing leading space
		// Edge: empty input → empty output.
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			if got := stripTrailingParens(c.in); got != c.want {
				t.Errorf("stripTrailingParens(%q): want %q, got %q", c.in, c.want, got)
			}
		})
	}
}

// TestPlugin_FetchCanonicalSourceVsCanonicalIDDivergence pins the
// load-bearing contract: SOURCE entity ID retains
// the parens-derived slug so the Wikipedia URL round-trips, but the
// CANONICAL entity ID strips parens so cross-plugin dedup with bgg's
// designed_by-emitted stub works. The article.ID and the canonical
// stub ID MUST diverge on a parens-disambig title.
func TestPlugin_FetchCanonicalSourceVsCanonicalIDDivergence(t *testing.T) {
	t.Parallel()
	srv := canonicalAxisFixture(t, []canonicalFixtureRow{{
		title: "Martin_Wallace_(game_designer)",
		qid: "Q956036",
		wantSummary: `{"title":"Martin Wallace (game designer)","lang":"en","wikibase_item":"Q956036"}`,
		wantWikidata: `{"entities":{"Q956036":{"claims":{"P31":[{"mainsnak":{"datavalue":{"value":{"id":"Q5"}}}}]}}}}`,
	}})
	p := New(
		WithHTTPClient(srv.Client()),
		WithAPIHostOverride(srv.URL),
		WithWikidataHostOverride(srv.URL),
	)

	outcome, err := p.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/Martin_Wallace_(game_designer)")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article == nil {
		t.Fatal("Article: want non-nil")
	}
	article := outcome.Article

	// Per ADR-0021: source-side Name retains parens-disambig
	// (round-trips to Wikipedia URL); canonical-edge target Name
	// is parens-stripped per so cross-plugin
	// dedup with BGG's `designed_by` edge target converges to the
	// same daemon-derived slug.
	if article.Name != "Martin Wallace (game designer)" {
		t.Errorf("article.Name (source): want %q, got %q",
			"Martin Wallace (game designer)", article.Name)
	}
	isAbout, ok := article.Edges[CanonicalEdgeType]
	if !ok || len(isAbout) != 1 {
		t.Fatalf("Edges[%q]: want 1 target, got %+v", CanonicalEdgeType, isAbout)
	}
	if got := isAbout[0].Name; got != "Martin Wallace" {
		t.Errorf("Edges[%q][0].Name (canonical): want %q, got %q",
			CanonicalEdgeType, "Martin Wallace", got)
	}
	if got := isAbout[0].Kind; got != "person" {
		t.Errorf("Edges[%q][0].Kind: want %q, got %q",
			CanonicalEdgeType, "person", got)
	}
	// Source Name MUST diverge from canonical-edge target Name on
	// parens-disambig titles — that's the load-bearing property of
	//.
	if article.Name == isAbout[0].Name {
		t.Errorf("source Name MUST diverge from canonical-edge target Name on parens-disambig title; got both %q", article.Name)
	}
}
