package bgg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fzerorubigd/bggo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalThingXML is a single-item boardgame /xmlapi2/thing
// response used by the enrichment tests. Keeps the XML small —
// each enrichment test cares about whether the operator_* fields
// got merged onto bg.Data, not the boardgame metadata.
const minimalThingXML = `<?xml version="1.0" encoding="utf-8"?>
<items>
 <item type="boardgame" id="42">
  <thumbnail>https://example.invalid/thumb.jpg</thumbnail>
  <name type="primary" sortindex="1" value="Placeholder Game"/>
  <yearpublished value="2099"/>
 </item>
</items>`

// enrichmentFixture serves a multi-endpoint BGG mock: /xmlapi2/thing,
// /xmlapi2/collection, /login/api/v1. Tests configure per-endpoint
// status + body via the lookup maps. lastReq surfaces the most-
// recent path + query string for each endpoint so tests can pin
// the auth-cookie + query-param contract.
type enrichmentFixture struct {
	mu       sync.Mutex
	server   *httptest.Server
	thingXML string

	// Collection response programmed per call attempt — index 0
	// returns first, index 1 returns second, etc. After exhaust
	// the last entry repeats. Lets tests model 401-then-200
	// retry sequences.
	collectionResponses []fixtureResponse
	collectionCalls     int

	// Login response — typically StatusOK + a Set-Cookie header
	// with the SessionID; tests can override for bad-creds
	// scenarios.
	loginStatus int
	loginCookie string

	// Captured requests for assertions.
	lastCollectionQuery string
	lastCollectionAuth  string
	loginCount          int
}

type fixtureResponse struct {
	status int
	body   string
}

func newEnrichmentFixture(t *testing.T) *enrichmentFixture {
	t.Helper()
	f := &enrichmentFixture{
		thingXML:    minimalThingXML,
		loginStatus: http.StatusOK,
		loginCookie: "SessionID=fixture-session-id; Path=/; Domain=127.0.0.1",
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/xmlapi2/thing"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(f.thingXML))
		case strings.HasPrefix(r.URL.Path, "/xmlapi2/collection"):
			f.collectionCalls++
			f.lastCollectionQuery = r.URL.RawQuery
			cookies := r.Cookies()
			names := make([]string, 0, len(cookies))
			for _, c := range cookies {
				names = append(names, c.Name+"="+c.Value)
			}
			f.lastCollectionAuth = strings.Join(names, "; ")
			resp := f.pickCollectionResponse()
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(resp.status)
			if resp.body != "" {
				_, _ = w.Write([]byte(resp.body))
			}
		case strings.HasPrefix(r.URL.Path, "/login/api/v1"):
			f.loginCount++
			if f.loginCookie != "" {
				w.Header().Set("Set-Cookie", f.loginCookie)
			}
			w.WriteHeader(f.loginStatus)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *enrichmentFixture) pickCollectionResponse() fixtureResponse {
	if len(f.collectionResponses) == 0 {
		return fixtureResponse{status: http.StatusOK, body: emptyCollectionXML}
	}
	idx := f.collectionCalls - 1
	if idx >= len(f.collectionResponses) {
		idx = len(f.collectionResponses) - 1
	}
	return f.collectionResponses[idx]
}

func (f *enrichmentFixture) bggClient(t *testing.T) *bggo.Client {
	t.Helper()
	u, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	return bggo.NewClient("test-key", bggo.WithHost(u.Host), bggo.WithScheme(u.Scheme))
}

// hostOpts returns the bggo.Option values that point a Plugin's
// internal client at this fixture's httptest.Server. Tests pass
// it via WithBggClientOptions so the Plugin can construct its
// own client + still exercise the cookie-jar restore flow.
func (f *enrichmentFixture) hostOpts(t *testing.T) []bggo.Option {
	t.Helper()
	u, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	return []bggo.Option{bggo.WithHost(u.Host), bggo.WithScheme(u.Scheme)}
}

const emptyCollectionXML = `<?xml version="1.0" encoding="utf-8"?>
<items totalitems="0"></items>`

const ownedCollectionXML = `<?xml version="1.0" encoding="utf-8"?>
<items totalitems="1">
  <item objectid="42" subtype="boardgame" collid="100">
    <name>Placeholder Game</name>
    <yearpublished>2099</yearpublished>
    <status own="1" want="0" wishlist="0" />
    <numplays>5</numplays>
    <comment>Public comment here.</comment>
    <stats><rating value="8.5"/></stats>
    <privateinfo pricepaid="49.99" pp_currency="USD" acquisitiondate="2099-01-15" acquiredfrom="local-shop" inventorylocation="shelf-A">
      <privatecomment>Private comment here.</privatecomment>
    </privateinfo>
  </item>
</items>`

// TestFetch_NoCredentials_BehavesAsLegacy pins the back-compat
// path: a Plugin without credentials hits /xmlapi2/thing only,
// emits no operator_* fields, and skips the collection-fetch
// provenance entry.
func TestFetch_NoCredentials_BehavesAsLegacy(t *testing.T) {
	t.Parallel()
	f := newEnrichmentFixture(t)
	bggClient := f.bggClient(t)

	p, err := New("test-key", WithClient(bggClient))
	require.NoError(t, err)

	out, err := p.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err)
	require.NotNil(t, out.Boardgame)
	for k := range out.Boardgame.Data {
		assert.False(t, strings.HasPrefix(k, "operator_"),
			"no operator_ keys when creds absent (got %q)", k)
	}
	assert.Equal(t, 0, f.collectionCalls, "/xmlapi2/collection must not be hit when creds absent")
	require.Len(t, out.Provenance, 1, "no provenance entry for collection when creds absent")
}

// TestFetch_WithCredentials_MergesOperatorFields pins the happy
// path: a Plugin with creds + a game in the operator's
// collection emits the full operator_* field set + provenance.
func TestFetch_WithCredentials_MergesOperatorFields(t *testing.T) {
	t.Parallel()
	f := newEnrichmentFixture(t)
	f.collectionResponses = []fixtureResponse{{status: http.StatusOK, body: ownedCollectionXML}}
	bggClient := f.bggClient(t)

	dir := t.TempDir()
	p, err := New("test-key",
		WithClient(bggClient),
		WithCredentials("operator", "secret"),
		WithDataDir(dir),
	)
	require.NoError(t, err)

	out, err := p.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err)
	require.NotNil(t, out.Boardgame)

	d := out.Boardgame.Data
	assert.ElementsMatch(t, []string{"own", "played"}, d["operator_status"],
		"bggo derives 'played' from numplays>0; both flags expected on the merged status list")
	assert.Equal(t, 9, d["operator_rating"], "8.5 rating rounds to 9")
	assert.Equal(t, 5, d["operator_num_plays"])
	assert.Equal(t, "Public comment here.", d["operator_comment"])
	assert.Equal(t, "49.99", d["operator_price_paid"])
	assert.Equal(t, "USD", d["operator_price_currency"])
	assert.Equal(t, "2099-01-15", d["operator_acquisition_date"])
	assert.Equal(t, "local-shop", d["operator_acquired_from"])
	assert.Equal(t, "shelf-A", d["operator_inventory_location"])
	assert.Equal(t, "Private comment here.", d["operator_private_comment"])

	assert.Contains(t, f.lastCollectionQuery, "stats=1")
	assert.Contains(t, f.lastCollectionQuery, "showprivate=1")
	assert.Equal(t, 1, f.loginCount, "fresh subprocess → one Login")

	// Cookie jar must be persisted at <dir>/session.json.
	_, statErr := os.Stat(filepath.Join(dir, cookieJarFileName))
	require.NoError(t, statErr, "cookie jar must persist for next subprocess")

	require.Len(t, out.Provenance, 2)
	assert.Equal(t, collectionEndpointForProvenance, out.Provenance[1].Source)
	assert.True(t, out.Provenance[1].OK)
}

// TestFetch_WithCredentials_GameNotInCollection pins the negative
// path: BGG returns totalitems="0" → /thing result lands
// unchanged, no operator_* fields. Provenance still records the
// collection round-trip happened.
func TestFetch_WithCredentials_GameNotInCollection(t *testing.T) {
	t.Parallel()
	f := newEnrichmentFixture(t)
	bggClient := f.bggClient(t)

	dir := t.TempDir()
	p, err := New("test-key",
		WithClient(bggClient),
		WithCredentials("operator", "secret"),
		WithDataDir(dir),
	)
	require.NoError(t, err)

	out, err := p.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err)
	require.NotNil(t, out.Boardgame)

	for k := range out.Boardgame.Data {
		assert.False(t, strings.HasPrefix(k, "operator_"),
			"no operator_ keys for game-not-in-collection (got %q)", k)
	}
	require.Len(t, out.Provenance, 2,
		"second provenance entry MUST land even when game is not in collection")
}

// TestFetch_MidSession401_RelogsAndRetries pins #282's acceptance:
// a 401 on the GetCollection call triggers one re-login + one
// retry. On retry success the operator_* fields land.
func TestFetch_MidSession401_RelogsAndRetries(t *testing.T) {
	t.Parallel()
	f := newEnrichmentFixture(t)
	f.collectionResponses = []fixtureResponse{
		{status: http.StatusUnauthorized},
		{status: http.StatusOK, body: ownedCollectionXML},
	}
	bggClient := f.bggClient(t)

	dir := t.TempDir()
	var warnings []string
	p, err := New("test-key",
		WithClient(bggClient),
		WithCredentials("operator", "secret"),
		WithDataDir(dir),
		WithWarnLogger(func(format string, args ...any) {
			warnings = append(warnings, format)
		}),
	)
	require.NoError(t, err)

	out, err := p.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err)
	require.NotNil(t, out.Boardgame)
	assert.Equal(t, 9, out.Boardgame.Data["operator_rating"],
		"retry must succeed + populate operator_rating")
	assert.Equal(t, 2, f.loginCount, "401 triggers exactly one re-login")
	assert.Equal(t, 2, f.collectionCalls, "401 triggers exactly one retry")

	// Per #282 acceptance: one warning log line on the retry path.
	foundWarn := false
	for _, w := range warnings {
		if strings.Contains(w, "401") {
			foundWarn = true
			break
		}
	}
	assert.True(t, foundWarn, "401 must surface a WARN log line per #282 acceptance")
}

// TestFetch_BadCredentialsAtLogin_FallsBackToThing pins the
// hard-failure path: Login returns non-2xx → enrichment fails
// over to /thing-only result with a WARN, fetch overall succeeds.
func TestFetch_BadCredentialsAtLogin_FallsBackToThing(t *testing.T) {
	t.Parallel()
	f := newEnrichmentFixture(t)
	f.loginStatus = http.StatusUnauthorized
	f.loginCookie = ""
	bggClient := f.bggClient(t)

	dir := t.TempDir()
	var warnings []string
	p, err := New("test-key",
		WithClient(bggClient),
		WithCredentials("operator", "wrong-password"),
		WithDataDir(dir),
		WithWarnLogger(func(format string, args ...any) {
			warnings = append(warnings, format)
		}),
	)
	require.NoError(t, err)

	out, err := p.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err, "fetch overall MUST succeed even when auth fails")
	require.NotNil(t, out.Boardgame)
	for k := range out.Boardgame.Data {
		assert.False(t, strings.HasPrefix(k, "operator_"),
			"no operator_ keys when login fails (got %q)", k)
	}
	assert.NotEmpty(t, warnings, "bad-creds login MUST surface a WARN")

	// Cookie jar must NOT be persisted for a failed login.
	_, statErr := os.Stat(filepath.Join(dir, cookieJarFileName))
	assert.True(t, os.IsNotExist(statErr),
		"cookie jar must not persist after failed login")
}

// TestFetch_CookieJarRestoresSession pins the lazy-login skip:
// a second subprocess starts with the persisted jar + skips the
// Login round-trip on first authed fetch. Built by running two
// Plugins back-to-back against the same fixture + data dir.
func TestFetch_CookieJarRestoresSession(t *testing.T) {
	t.Parallel()
	f := newEnrichmentFixture(t)
	f.collectionResponses = []fixtureResponse{{status: http.StatusOK, body: ownedCollectionXML}}

	dir := t.TempDir()

	// First subprocess — logs in, persists jar. Use
	// WithBggClientOptions so the Plugin builds its own client +
	// runs the cookie-jar restore flow.
	p1, err := New("test-key",
		WithBggClientOptions(f.hostOpts(t)...),
		WithCredentials("operator", "secret"),
		WithDataDir(dir),
	)
	require.NoError(t, err)
	_, err = p1.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err)
	loginsAfterFirst := f.loginCount
	require.Equal(t, 1, loginsAfterFirst)

	// Second subprocess — should skip Login because jar exists.
	// Same data dir; fresh Plugin builds its own client.
	p2, err := New("test-key",
		WithBggClientOptions(f.hostOpts(t)...),
		WithCredentials("operator", "secret"),
		WithDataDir(dir),
	)
	require.NoError(t, err)
	_, err = p2.Fetch(context.Background(), "https://boardgamegeek.com/boardgame/42/x")
	require.NoError(t, err)
	assert.Equal(t, loginsAfterFirst, f.loginCount,
		"second subprocess MUST skip Login when cookie jar exists")
}
