package bgg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fzerorubigd/bggo"
)

func TestMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		input string
		want bool
	}{
		{"canonical_url_with_slug", "https://boardgamegeek.com/boardgame/224517/brass-birmingham", true},
		{"canonical_url_no_slug", "https://boardgamegeek.com/boardgame/224517", true},
		{"www_subdomain", "https://www.boardgamegeek.com/boardgame/13", true},
		{"http_scheme", "http://boardgamegeek.com/boardgame/13", true},
		{"shorthand_numeric", "bgg: 224517", true},
		{"shorthand_capitalised", "BGG: 224517", true},
		{"shorthand_with_name", "bgg: Brass Birmingham", true},

		{"unrelated_host", "https://example.com/boardgame/1", false},
		{"bgg_forum_url", "https://boardgamegeek.com/forum/123", false},
		{"bgg_search_url", "https://boardgamegeek.com/search?q=brass", false},
		{"empty_shorthand", "bgg:", false},
		{"empty_string", "", false},
		{"plain_url_root", "https://boardgamegeek.com/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Match(tc.input)
			if got != tc.want {
				t.Errorf("Match(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestCanonicalURL(t *testing.T) {
	t.Parallel()
	got := CanonicalURL("224517")
	want := "https://boardgamegeek.com/boardgame/224517"
	if got != want {
		t.Errorf("CanonicalURL(224517) = %q, want %q", got, want)
	}
}

func TestDefaultCacheTTLSeconds_Is365Days(t *testing.T) {
	t.Parallel()
	const want = 365 * 24 * 60 * 60
	if DefaultCacheTTLSeconds != want {
		t.Errorf("DefaultCacheTTLSeconds = %d, want %d (365 days)", DefaultCacheTTLSeconds, want)
	}
}

func TestResolveID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		input string
		wantID int64
		wantURL string
		wantErrText string
	}{
		{"url-with-slug", "https://boardgamegeek.com/boardgame/224517/brass-birmingham", 224517, "https://boardgamegeek.com/boardgame/224517", ""},
		{"url-no-slug", "https://boardgamegeek.com/boardgame/13", 13, "https://boardgamegeek.com/boardgame/13", ""},
		{"www-subdomain", "https://www.boardgamegeek.com/boardgame/30549", 30549, "https://boardgamegeek.com/boardgame/30549", ""},
		{"shorthand-numeric", "bgg: 224517", 224517, "https://boardgamegeek.com/boardgame/224517", ""},
		{"shorthand-with-spaces", "bgg: 13", 13, "https://boardgamegeek.com/boardgame/13", ""},

		// Name-shorthand returns errNameShorthand (private sentinel)
		// per — Fetch routes name-shorthand to BGG search.
		// errors.Is is the canonical caller-side check; the message
		// substring here just keeps the table-driven shape.
		{"shorthand-with-name", "bgg: Brass Birmingham", 0, "", "shorthand suffix is a name"},
		{"unrelated", "https://example.com/x", 0, "", "not a recognised"},
		{"empty", "", 0, "", "not a recognised"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, url, err := ResolveID(tc.input)
			if tc.wantErrText != "" {
				if err == nil {
					t.Fatalf("ResolveID(%q): want error containing %q, got nil", tc.input, tc.wantErrText)
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("ResolveID(%q): want err containing %q, got %v", tc.input, tc.wantErrText, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveID(%q): unexpected err: %v", tc.input, err)
			}
			if id != tc.wantID {
				t.Errorf("id: want %d, got %d", tc.wantID, id)
			}
			if url != tc.wantURL {
				t.Errorf("url: want %q, got %q", tc.wantURL, url)
			}
		})
	}
}

func TestNew_RejectsEmptyAPIKey(t *testing.T) {
	t.Parallel()
	if _, err := New(""); err == nil {
		t.Errorf("New(\"\"): want error (fail-closed on missing API key), got nil")
	}
}

// TestPlugin_Fetch_HappyPath drives Fetch end-to-end against a fake
// BGG xmlapi2 endpoint. Exercises the full parse pipeline:
// frontmatter (publisher/designed_by/artist_by/year/rating/weight),
// aliases (primary name + foreign-language alternate), notations
// (input + canonical + shorthand), thumbnail URL passthrough.
func TestPlugin_Fetch_HappyPath(t *testing.T) {
	t.Parallel()

	const fakeXML = `<?xml version="1.0" encoding="utf-8"?>
<items termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="224517">
 <thumbnail>https://cf.geekdo-images.com/thumb_224517.jpg</thumbnail>
 <image>https://cf.geekdo-images.com/img_224517.jpg</image>
 <name type="primary" sortindex="1" value="Brass: Birmingham"/>
 <name type="alternate" value="Бирмингем"/>
 <name type="alternate" value="Brass: Birmingham"/>
 <description>Brass: Birmingham is an economic strategy game.</description>
 <yearpublished value="2018"/>
 <minplayers value="2"/>
 <maxplayers value="4"/>
 <playingtime value="120"/>
 <minplaytime value="60"/>
 <maxplaytime value="120"/>
 <minage value="14"/>
 <link type="boardgamedesigner" id="9714" value="Martin Wallace"/>
 <link type="boardgamedesigner" id="20415" value="Gavan Brown"/>
 <link type="boardgameartist" id="9714" value="Lina Cossette"/>
 <link type="boardgameartist" id="9715" value="David Forest"/>
 <link type="boardgamepublisher" id="33038" value="Roxley"/>
 <link type="boardgamepublisher" id="42088" value="Lacerta"/>
 <statistics page="1">
 <ratings>
 <usersrated value="40000"/>
 <average value="8.59"/>
 <bayesaverage value="8.41"/>
 <ranks>
 <rank type="subtype" id="1" name="boardgame" friendlyname="Board Game Rank" value="1" bayesaverage="8.41"/>
 </ranks>
 <averageweight value="3.93"/>
 </ratings>
 </statistics>
 </item>
</items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/xmlapi2/thing") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(fakeXML))
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)

	p, err := New("test-key", WithClient(bggClient))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := p.Fetch(context.Background(),
		"https://boardgamegeek.com/boardgame/224517/brass-birmingham")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.Boardgame == nil {
		t.Fatal("Boardgame: want non-nil")
	}
	bg := out.Boardgame

	// Per ADR-0021: plugin emits descriptive Name (with year-suffix
	// retained); daemon's slug.Slug derives the source-node ID. The
	// canonical-edge target Name is stripped via canonicalizeBGGName
	// (asserted below in the Edges block).
	if bg.Name != "Brass: Birmingham" {
		t.Errorf("Name: want %q, got %q", "Brass: Birmingham", bg.Name)
	}
	if bg.BGGID != "224517" {
		t.Errorf("BGGID: want %q, got %q", "224517", bg.BGGID)
	}
	// Per ADR-0021: canonical-edge `is_about` target Name is
	// stripped of BGG annotations — series-colon `: ` → single
	// space. Daemon's slug.Slug then derives `boardgame:<slug>`.
	isAbout, ok := bg.Edges[CanonicalEdgeType]
	if !ok || len(isAbout) != 1 {
		t.Fatalf("Edges[%q]: want 1 target, got %+v", CanonicalEdgeType, isAbout)
	}
	if isAbout[0].Name != "Brass Birmingham" || isAbout[0].Kind != CanonicalKind {
		t.Errorf("Edges[%q][0]: want {Name=%q, Kind=%q}, got %+v",
			CanonicalEdgeType, "Brass Birmingham", CanonicalKind, isAbout[0])
	}
	// Universal source-type edge: every emission carries it.
	isA, ok := bg.Edges[SourceTypeEdgeType]
	if !ok || len(isA) != 1 || isA[0].Name != SourceTypeName || isA[0].Kind != SourceTypeKind {
		t.Errorf("Edges[%q]: want [{Name=%q, Kind=%q}], got %+v",
			SourceTypeEdgeType, SourceTypeName, SourceTypeKind, isA)
	}
	if title, _ := bg.Data["title"].(string); title != "Brass: Birmingham" {
		t.Errorf("data.title: want %q, got %v", "Brass: Birmingham", bg.Data["title"])
	}
	if year := bg.Data["year"]; year != 2018 {
		t.Errorf("data.year: want 2018, got %v", year)
	}
	// data.bgg_id preserves upstream linkage even though the entity
	// ID is human-readable now .
	if id, _ := bg.Data["bgg_id"].(int64); id != 224517 {
		t.Errorf("data.bgg_id: want 224517, got %v (%T)", bg.Data["bgg_id"], bg.Data["bgg_id"])
	}
	if pub := bg.Data["publisher"]; pub != "Roxley" {
		t.Errorf("data.publisher: want Roxley (first only), got %v", pub)
	}
	if designers, _ := bg.Data["designed_by"].([]string); len(designers) != 2 {
		t.Fatalf("data.designed_by: want 2 entries, got %v", bg.Data["designed_by"])
	} else if designers[0] != "Martin Wallace" || designers[1] != "Gavan Brown" {
		t.Errorf("data.designed_by: want [Martin Wallace, Gavan Brown], got %v", designers)
	}
	if artists, _ := bg.Data["artist_by"].([]string); len(artists) != 2 {
		t.Fatalf("data.artist_by: want 2 entries, got %v", bg.Data["artist_by"])
	}
	if rating, _ := bg.Data["bgg_rating"].(float64); rating < 8.5 || rating > 8.7 {
		t.Errorf("data.bgg_rating: want ~8.59, got %v", bg.Data["bgg_rating"])
	}
	if weight, _ := bg.Data["bgg_weight"].(float64); weight < 3.8 || weight > 4.0 {
		t.Errorf("data.bgg_weight: want ~3.93, got %v", bg.Data["bgg_weight"])
	}

	// Aliases per ADR-0011: primary first, foreign-language
	// alternate second, dedupe drops the duplicate primary that
	// BGG also returns under <name type="alternate">.
	if len(bg.Aliases) != 2 {
		t.Fatalf("Aliases: want 2 (primary + foreign), got %v", bg.Aliases)
	}
	if bg.Aliases[0] != "Brass: Birmingham" {
		t.Errorf("Aliases[0]: want primary name first, got %q", bg.Aliases[0])
	}
	if bg.Aliases[1] != "Бирмингем" {
		t.Errorf("Aliases[1]: want foreign-language alternate, got %q", bg.Aliases[1])
	}

	// Notations: input first (per yaad-index lookup-first
	// invariant), then canonical, then shorthand. Dedupe on the
	// input matching one of the derived forms.
	wantNotations := []string{
		"https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"https://boardgamegeek.com/boardgame/224517",
		"bgg: 224517",
	}
	if len(bg.Notations) != len(wantNotations) {
		t.Fatalf("Notations: want %d entries, got %d (%v)", len(wantNotations), len(bg.Notations), bg.Notations)
	}
	for i, n := range wantNotations {
		if bg.Notations[i] != n {
			t.Errorf("Notations[%d]: want %q, got %q", i, n, bg.Notations[i])
		}
	}

	if bg.ThumbnailURL != "https://cf.geekdo-images.com/thumb_224517.jpg" {
		t.Errorf("ThumbnailURL: want geekdo URL, got %q", bg.ThumbnailURL)
	}
}

// TestCanonicalizeBGGName covers the BGG-specific canonical-name
// transformation Per the prior design,'s spec body: strip trailing year-suffix,
// strip trailing parens-disambig, replace mid-name `: ` series-
// separator with a single space (NOT a trailing-colon strip — both
// halves survive). Source-side Boardgame.Name retains all
// annotations; the `is_about` edge target Name is the
// canonicalized form.
func TestCanonicalizeBGGName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in string
		want string
	}{
		// Trailing year-suffix.
		{"trailing_year_with_space", "Brass: Birmingham (2018)", "Brass Birmingham"},
		{"trailing_year_no_space", "Foo(2020)", "Foo"},
		// Trailing parens-disambig (rare on BGG).
		{"trailing_parens_disambig", "Foo (special edition)", "Foo"},
		// BGG series-separator `: ` mid-name → single space, both
		// halves survive.
		{"series_colon_mid_name", "Brass: Birmingham", "Brass Birmingham"},
		{"series_colon_in_long_title", "Pandemic Legacy: Season 1", "Pandemic Legacy Season 1"},
		{"series_colon_with_year", "1830: Railways & Robber Barons", "1830 Railways & Robber Barons"},
		// Combined: series-colon + year-suffix.
		{"series_colon_plus_year", "Brass: Birmingham (2018)", "Brass Birmingham"},
		{"long_series_plus_year", "Through the Ages: A New Story of Civilization (2015)", "Through the Ages A New Story of Civilization"},
		// No annotation → no-op.
		{"no_annotation", "Caverna", "Caverna"},
		{"already_clean", "Spirit Island", "Spirit Island"},
		// Trailing colon (no space after) is NOT a series-separator
		// — preserved.
		{"trailing_colon_no_space", "Foo:", "Foo:"},
		// Whitespace.
		{"surrounding_whitespace", " Caverna ", "Caverna"},
		// Empty.
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := canonicalizeBGGName(tc.in); got != tc.want {
				t.Errorf("canonicalizeBGGName(%q) = %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}

// TestPlugin_Fetch_NotFound covers the empty-result path: BGG
// returns a well-formed XML doc with zero items (e.g., id doesn't
// exist). Plugin returns ErrNotFoundUpstream.
func TestPlugin_Fetch_NotFound(t *testing.T) {
	t.Parallel()

	const emptyXML = `<?xml version="1.0" encoding="utf-8"?>
<items termsofuse="https://boardgamegeek.com/xmlapi/termsofuse"></items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(emptyXML))
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	_, err := p.Fetch(context.Background(), "bgg: 999999")
	if err == nil {
		t.Fatal("Fetch: want ErrNotFoundUpstream, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("Fetch err: want 'not found' substring, got %v", err)
	}
}

// TestPlugin_Fetch_NameShorthand_SingleMatch covers the transparent-
// resolve path per: BGG search returns exactly one match,
// Fetch follows through to xmlapi2/thing and returns the Boardgame.
func TestPlugin_Fetch_NameShorthand_SingleMatch(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="1" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="224517">
 <name type="primary" value="Brass: Birmingham"/>
 <yearpublished value="2018"/>
 </item>
</items>`
	const thingXML = `<?xml version="1.0" encoding="utf-8"?>
<items termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="224517">
 <name type="primary" sortindex="1" value="Brass: Birmingham"/>
 <yearpublished value="2018"/>
 <minplayers value="2"/>
 <maxplayers value="4"/>
 </item>
</items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/search") {
			_, _ = w.Write([]byte(searchXML))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/thing") {
			_, _ = w.Write([]byte(thingXML))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	out, err := p.Fetch(context.Background(), "bgg: brass-birmingham")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.Boardgame == nil {
		t.Fatal("Boardgame: want non-nil (single-match transparent resolve)")
	}
	if len(out.Options) != 0 {
		t.Errorf("Options: want empty (single-match), got %d", len(out.Options))
	}
	// Per ADR-0021: plugin emits descriptive Name; daemon's slug.Slug
	// derives the source-node ID at translation time. The
	// canonical-edge target Name is asserted via the Edges block.
	if out.Boardgame.Name != "Brass: Birmingham" {
		t.Errorf("Boardgame.Name: want %q, got %q",
			"Brass: Birmingham", out.Boardgame.Name)
	}
	isAbout, ok := out.Boardgame.Edges[CanonicalEdgeType]
	if !ok || len(isAbout) != 1 {
		t.Fatalf("Edges[%q]: want 1 target, got %+v", CanonicalEdgeType, isAbout)
	}
	if isAbout[0].Name != "Brass Birmingham" {
		t.Errorf("Edges[%q][0].Name: want %q (series-colon stripped), got %q",
			CanonicalEdgeType, "Brass Birmingham", isAbout[0].Name)
	}
}

// TestPlugin_Fetch_NameShorthand_MultiMatch covers the disambiguation
// path per: BGG search returns multiple matches with NO single
// exact-name match, Fetch returns *FetchOutcome with Options
// populated (no Boardgame), so the agent picks one and re-ingests
// via `bgg: <numeric-id>`. Per #329 the search-result names are
// chosen so the query has no exact match (the original "Brass"
// entry is renamed; preserved query "brass" → all three are
// substring hits, none exact-match → disambig stays).
func TestPlugin_Fetch_NameShorthand_MultiMatch(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="3" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="28720">
 <name type="primary" value="Brass: Original"/>
 <yearpublished value="2007"/>
 </item>
 <item type="boardgame" id="220308">
 <name type="primary" value="Brass: Lancashire"/>
 <yearpublished value="2018"/>
 </item>
 <item type="boardgame" id="224517">
 <name type="primary" value="Brass: Birmingham"/>
 <yearpublished value="2018"/>
 </item>
</items>`

	var thingHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/search") {
			_, _ = w.Write([]byte(searchXML))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/thing") {
			thingHits++
			http.NotFound(w, r) // disambig path MUST NOT call thing
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	out, err := p.Fetch(context.Background(), "bgg: brass")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.Boardgame != nil {
		t.Error("Boardgame: want nil on disambiguation path")
	}
	if len(out.Options) != 3 {
		t.Fatalf("Options: want 3 candidates, got %d", len(out.Options))
	}
	if thingHits > 0 {
		t.Errorf("xmlapi2/thing was called %d times on disambig path; should be zero", thingHits)
	}

	// Each option's ID is the numeric BGG id (string-encoded for
	// re-ingest via `bgg: <id>`); Label is `<name> (<year>)` when
	// the year is known; Summary is empty (BGG search has no desc).
	wantByID := map[string]string{
		"28720": "Brass: Original (2007)",
		"220308": "Brass: Lancashire (2018)",
		"224517": "Brass: Birmingham (2018)",
	}
	gotByID := make(map[string]string, len(out.Options))
	for _, o := range out.Options {
		gotByID[o.ID] = o.Label
		if o.Summary != "" {
			t.Errorf("Option(%s).Summary: want empty (BGG search has no desc), got %q", o.ID, o.Summary)
		}
	}
	for id, wantLabel := range wantByID {
		if got := gotByID[id]; got != wantLabel {
			t.Errorf("Option(%s).Label: want %q, got %q", id, wantLabel, got)
		}
	}
}

// TestPlugin_Fetch_NameShorthand_ExactMatchAutoResolves pins the
// #329 contract: when the BGG search returns multiple results
// but exactly one has a name that matches the query exactly
// (case + whitespace normalized; year suffix excluded), Fetch
// auto-resolves to that single entry via fetchByID rather than
// returning the full set as disambiguation. The repro scenario
// is the original #329 trigger: a `bgg: The Lost Expedition`
// search returns ~10 substring hits but only one exact-name
// match.
func TestPlugin_Fetch_NameShorthand_ExactMatchAutoResolves(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="4" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="216459">
 <name type="primary" value="The Lost Expedition"/>
 <yearpublished value="2017"/>
 </item>
 <item type="boardgame" id="1572">
 <name type="primary" value="The Lost Expeditions"/>
 <yearpublished value="2016"/>
 </item>
 <item type="boardgame" id="999991">
 <name type="primary" value="The Lost Expedition: Promo Pack"/>
 <yearpublished value="2018"/>
 </item>
 <item type="boardgame" id="999992">
 <name type="primary" value="Lost Cities: Expedition 6 – The Lost Expedition"/>
 <yearpublished value="2019"/>
 </item>
</items>`

	const thingXML = `<?xml version="1.0" encoding="utf-8"?>
<items termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="216459">
 <name type="primary" sortindex="5" value="The Lost Expedition"/>
 <yearpublished value="2017"/>
 <description>A jungle survival deduction game.</description>
 <minplayers value="1"/>
 <maxplayers value="5"/>
 <playingtime value="45"/>
 </item>
</items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/search") {
			_, _ = w.Write([]byte(searchXML))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/thing") {
			_, _ = w.Write([]byte(thingXML))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	out, err := p.Fetch(context.Background(), "bgg: The Lost Expedition")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.Boardgame == nil {
		t.Fatal("Boardgame: want resolved boardgame, got nil (auto-resolve must fire on exact-name match)")
	}
	if got := out.Boardgame.BGGID; got != "216459" {
		t.Errorf("Boardgame.BGGID: want \"216459\" (the only exact-name match), got %q", got)
	}
	if len(out.Options) != 0 {
		t.Errorf("Options: want empty on single-exact-match auto-resolve, got %d", len(out.Options))
	}
}

// TestPlugin_Fetch_NameShorthand_MultiExactMatchStaysDisambig pins
// the carve-out: when 2+ results share an exact-name match (e.g.,
// Catan with multiple editions / republishings), the auto-resolve
// short-circuits and the full disambiguation list is returned so
// the operator picks the intended year.
func TestPlugin_Fetch_NameShorthand_MultiExactMatchStaysDisambig(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="3" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="13">
 <name type="primary" value="Catan"/>
 <yearpublished value="1995"/>
 </item>
 <item type="boardgame" id="40694">
 <name type="primary" value="Catan"/>
 <yearpublished value="2015"/>
 </item>
 <item type="boardgame" id="999998">
 <name type="primary" value="Catan: Cities &amp; Knights"/>
 <yearpublished value="1998"/>
 </item>
</items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/search") {
			_, _ = w.Write([]byte(searchXML))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	out, err := p.Fetch(context.Background(), "bgg: Catan")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.Boardgame != nil {
		t.Error("Boardgame: want nil on multi-exact-match (year disambig path)")
	}
	if len(out.Options) != 3 {
		t.Errorf("Options: want 3 (full disambig list), got %d", len(out.Options))
	}
}

// TestExactNameMatch_NormalizationCases pins the normalizer's
// case + whitespace handling so the helper's contract is
// independent of the Fetch integration tests.
func TestExactNameMatch_NormalizationCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		query   string
		results []bggo.SearchResult
		wantID  int64 // 0 = no match (fall through to disambig)
	}{
		{
			name: "case_insensitive_exact",
			query: "the lost expedition",
			results: []bggo.SearchResult{
				{ID: 1, Name: "Some Other"},
				{ID: 2, Name: "The Lost Expedition"},
			},
			wantID: 2,
		},
		{
			name: "whitespace_collapsed",
			query: "The  Lost   Expedition",
			results: []bggo.SearchResult{
				{ID: 7, Name: "The Lost Expedition"},
			},
			// Single-result path doesn't go through exactNameMatch
			// in fetchByName; the helper still returns the match.
			wantID: 7,
		},
		{
			name: "prefix_not_exact",
			query: "The Lost Expedition",
			results: []bggo.SearchResult{
				{ID: 3, Name: "The Lost Expedition: Promo Pack"},
			},
			wantID: 0,
		},
		{
			name: "two_exact_matches_no_resolve",
			query: "Catan",
			results: []bggo.SearchResult{
				{ID: 13, Name: "Catan", YearPublished: 1995},
				{ID: 40694, Name: "Catan", YearPublished: 2015},
			},
			wantID: 0,
		},
		{
			name: "empty_query_no_resolve",
			query: "",
			results: []bggo.SearchResult{
				{ID: 1, Name: ""},
			},
			wantID: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := exactNameMatch(c.query, c.results)
			if c.wantID == 0 {
				if got != nil {
					t.Errorf("exactNameMatch: want nil, got result id=%d", got.ID)
				}
				return
			}
			if got == nil {
				t.Fatalf("exactNameMatch: want result id=%d, got nil", c.wantID)
			}
			if got.ID != c.wantID {
				t.Errorf("exactNameMatch: want id=%d, got id=%d", c.wantID, got.ID)
			}
		})
	}
}

// TestPlugin_Fetch_NameShorthand_NoMatch covers the search-misses
// path: BGG search returns zero results for the query. Fetch wraps
// ErrNotFoundUpstream with the search query for operator clarity.
func TestPlugin_Fetch_NameShorthand_NoMatch(t *testing.T) {
	t.Parallel()

	const emptySearchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="0" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse"></items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/search") {
			_, _ = w.Write([]byte(emptySearchXML))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	_, err := p.Fetch(context.Background(), "bgg: nonexistent-game-name-xyz")
	if err == nil {
		t.Fatal("Fetch: want ErrNotFoundUpstream, got nil")
	}
	if !errors.Is(err, ErrNotFoundUpstream) {
		t.Errorf("Fetch err: want errors.Is(err, ErrNotFoundUpstream), got %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent-game-name-xyz") {
		t.Errorf("Fetch err: want query echoed, got %v", err)
	}
}

// TestPlugin_Fetch_RejectsNonBoardgame covers the type-filter:
// BGG returns a boardgameexpansion or rpgitem for the id; v1 is
// boardgame-only so we reject with not-found rather than emitting
// a confusingly-shaped entity.
func TestPlugin_Fetch_RejectsNonBoardgame(t *testing.T) {
	t.Parallel()

	const expansionXML = `<?xml version="1.0" encoding="utf-8"?>
<items termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgameexpansion" id="271247">
 <name type="primary" sortindex="1" value="Brass: Birmingham + Lancashire"/>
 </item>
</items>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(expansionXML))
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	bggClient := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, _ := New("test-key", WithClient(bggClient))

	_, err := p.Fetch(context.Background(), "bgg: 271247")
	if err == nil {
		t.Fatal("Fetch: want ErrNotFoundUpstream-wrapped, got nil")
	}
	if !strings.Contains(err.Error(), "boardgame-only") {
		t.Errorf("Fetch err: want 'boardgame-only' substring, got %v", err)
	}
}
