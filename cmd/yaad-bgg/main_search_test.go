package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fzerorubigd/bggo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/bgg"
)

// newSearchTestPlugin stands up a *bgg.Plugin whose bggo client is
// wired to an httptest server returning the given xmlapi2/search XML.
// Mirrors the bgg package's WithClient(httptest) pattern so runSearch
// can be exercised end-to-end without real upstream calls.
func newSearchTestPlugin(t *testing.T, searchXML string) *bgg.Plugin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasPrefix(r.URL.Path, "/xmlapi2/search") {
			_, _ = w.Write([]byte(searchXML))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := bggo.NewClient("test-key",
		bggo.WithHost(srvURL.Host),
		bggo.WithScheme("http"),
	)
	p, err := bgg.New("test-key", bgg.WithClient(client))
	require.NoError(t, err)
	return p
}

// TestRunSearch_HappyPath pins the #457 operation=search wire shape:
// runSearch decodes to a searchResponseDoc with ok:true and the
// expected candidates (id / label-with-year / type-as-summary).
// Fictional game names only (content-policy rule-9).
func TestRunSearch_HappyPath(t *testing.T) {
	t.Parallel()

	const searchXML = `<?xml version="1.0" encoding="utf-8"?>
<items total="2" termsofuse="https://boardgamegeek.com/xmlapi/termsofuse">
 <item type="boardgame" id="9001">
 <name type="primary" value="Widget Alpha Prime"/>
 <yearpublished value="2099"/>
 </item>
 <item type="boardgame" id="9003">
 <name type="primary" value="Sample Boardgame One"/>
 </item>
</items>`

	p := newSearchTestPlugin(t, searchXML)

	var out bytes.Buffer
	req := fetchRequest{Operation: "search", Query: "widget"}
	require.NoError(t, runSearch(context.Background(), p, req, &out))

	var doc searchResponseDoc
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	assert.True(t, doc.OK)
	assert.Empty(t, doc.ErrorMessage)
	assert.Equal(t, []bgg.SearchResultCandidate{
		{ID: "9001", Label: "Widget Alpha Prime (2099)", Summary: "boardgame"},
		{ID: "9003", Label: "Sample Boardgame One", Summary: "boardgame"},
	}, doc.Candidates)
}

// TestRunSearch_EmptyQuery confirms an empty/whitespace query yields
// ok:false with the missing-query error_message and never reaches the
// upstream client.
func TestRunSearch_EmptyQuery(t *testing.T) {
	t.Parallel()

	// Nil-client plugin: if runSearch reached p.Search it would panic,
	// proving the empty-query short-circuit fires before the call.
	p, err := bgg.New("test-key", bgg.WithClient(&bggo.Client{}))
	require.NoError(t, err)

	var out bytes.Buffer
	req := fetchRequest{Operation: "search", Query: "   "}
	require.NoError(t, runSearch(context.Background(), p, req, &out))

	var doc searchResponseDoc
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	assert.False(t, doc.OK)
	assert.Empty(t, doc.Candidates)
	assert.Contains(t, doc.ErrorMessage, "request missing `query`")
}

// TestRunFetch_UnsupportedOperationMessage pins that the new
// operation-dispatch default still carries the `unsupported
// operation` substring the existing TestRunFetch_RejectsMalformed
// asserts (#457 changed the message text).
func TestRunFetch_UnsupportedOperationMessage(t *testing.T) {
	t.Setenv(EnvAPIKey, "test-key")

	body := `{"operation":"meow","url":"https://boardgamegeek.com/boardgame/1"}`
	err := runFetch(context.Background(), strings.NewReader(body), discard{}, discard{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported operation")
}

// discard is a minimal io.Writer sink for the runFetch error-path
// test above (avoids importing io just for io.Discard).
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
