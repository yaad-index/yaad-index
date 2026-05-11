package wikipedia

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlugin_FetchArticleEmitsAliases pins the the source issue a prior PR
// contract: a successful Fetch produces an Article with the title
// in `Aliases`. yaad-index Marshal merges this with its own
// ADR-0011-synthesized alias and dedupes; emitting it here is
// cheap-redundant but keeps the plugin-side intent explicit.
func TestPlugin_FetchArticleEmitsAliases(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"Susanna Clarke","lang":"en"}`)
		case r.URL.Path == "/w/api.php":
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"extract":"body"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	plugin := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))
	outcome, err := plugin.Fetch(context.Background(),
		"https://en.wikipedia.org/wiki/Susanna_Clarke")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article == nil {
		t.Fatalf("Article: want non-nil")
	}
	if len(outcome.Article.Aliases) != 1 {
		t.Fatalf("Article.Aliases: want 1 entry, got %d (%v)",
			len(outcome.Article.Aliases), outcome.Article.Aliases)
	}
	if outcome.Article.Aliases[0] != "Susanna Clarke" {
		t.Errorf("Article.Aliases[0]: want %q, got %q",
			"Susanna Clarke", outcome.Article.Aliases[0])
	}
}

// TestPlugin_FetchDisambigPathEmitsNoAliases pins the no-Article-
// no-Aliases path: when Fetch returns Options instead of Article
// (disambiguation page resolved via search-fallback), there's no
// Article to carry an alias slice. The wire-side `aliases:` field
// is absent on the disambig response shape.
func TestPlugin_FetchDisambigPathEmitsNoAliases(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"Martin Wallace","lang":"en","type":"disambiguation"}`)
		case r.URL.Path == "/w/api.php" && r.URL.Query().Get("list") == "search":
			_, _ = fmt.Fprint(w, `{"query":{"search":[
				{"title":"Martin Wallace (game designer)","pageid":1,"snippet":"Designer"},
				{"title":"Martin Wallace (American football)","pageid":2,"snippet":"QB"}
			]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	plugin := New(WithHTTPClient(srv.Client()), WithAPIHostOverride(srv.URL))
	outcome, err := plugin.Fetch(context.Background(), "wikipedia: Martin Wallace")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if outcome.Article != nil {
		t.Fatalf("Article: want nil on disambig path, got %+v", outcome.Article)
	}
	// No Article means no Article.Aliases — there's nothing for the
	// plugin to claim aliases against until the agent picks an
	// option and re-ingests.
	if len(outcome.Options) == 0 {
		t.Errorf("Options: want non-empty disambig set")
	}
}
