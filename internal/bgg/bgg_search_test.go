package bgg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fzerorubigd/bggo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlugin_Search_MapsResults pins the #457 federated-search
// mapping: every BGG search result becomes a SearchResultCandidate
// with ID=numeric-id-string, Label=`<name> (<year>)` when the year
// is known (bare name otherwise), and Summary=the BGG item-type
// string. Unlike fetchByName, Search returns the FULL list verbatim
// — no single-match auto-resolve, no exact-name collapse — so the
// `/xmlapi2/thing` endpoint MUST NOT be hit.
//
// All game names here are fictional (content-policy rule-9).
func TestPlugin_Search_MapsResults(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="3" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="9001">
 <name type="primary" value="Widget Alpha Prime"/>
 <yearpublished value="2099"/>
 </item>
 <item type="boardgameexpansion" id="9002">
 <name type="primary" value="Widget Alpha Prime: Expansion Pack"/>
 <yearpublished value="2100"/>
 </item>
 <item type="boardgame" id="9003">
 <name type="primary" value="Sample Boardgame One"/>
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
			http.NotFound(w, r) // search path MUST NOT resolve via /thing
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
	p, err := New("test-key", WithClient(bggClient))
	require.NoError(t, err)

	got, err := p.Search(context.Background(), "  widget  ", 0)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Zero(t, thingHits, "Search must not call /xmlapi2/thing")

	want := []SearchResultCandidate{
		{ID: "9001", Label: "Widget Alpha Prime (2099)", Summary: "boardgame"},
		{ID: "9002", Label: "Widget Alpha Prime: Expansion Pack (2100)", Summary: "boardgameexpansion"},
		{ID: "9003", Label: "Sample Boardgame One", Summary: "boardgame"},
	}
	assert.Equal(t, want, got)
}

// TestPlugin_Search_LimitTrims confirms the limit parameter trims
// the candidate list to the requested count (limit>0), preserving
// upstream order. Fictional names only.
func TestPlugin_Search_LimitTrims(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="3" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="100">
 <name type="primary" value="Test Game 2099"/>
 <yearpublished value="2099"/>
 </item>
 <item type="boardgame" id="101">
 <name type="primary" value="Test Game 2100"/>
 <yearpublished value="2100"/>
 </item>
 <item type="boardgame" id="102">
 <name type="primary" value="Test Game 2101"/>
 <yearpublished value="2101"/>
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
	p, err := New("test-key", WithClient(bggClient))
	require.NoError(t, err)

	got, err := p.Search(context.Background(), "test game", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "100", got[0].ID)
	assert.Equal(t, "101", got[1].ID)
}
