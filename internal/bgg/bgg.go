// Package bgg implements the Match + Fetch logic for the yaad-bgg
// plugin. The wire-shape concerns (capabilities doc, fetch envelope,
// notations, attachments) live in main.go on purpose — that split
// keeps THIS package straightforwardly unit-testable against
// fixture XML without spawning subprocesses.
//
// Built on top of github.com/fzerorubigd/bggo (the operator's BGG API
// client lib). Every upstream call goes through *bggo.Client; we
// don't roll our own HTTP. Per yaad-index/+ the operator's
// 2026-05-06 spec, scope is `boardgame` kind only — expansions /
// publishers / designers / play-logs are separate follow-ups.
package bgg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fzerorubigd/bggo"

	"github.com/yaad-index/yaad-index/internal/buildinfo"
)

// PluginName is the stable identifier surfaced as the capabilities
// document's `name` and on every provenance entry. Matches the slug
// in entity ids (`boardgame:<bgg_id>`) so log lines pivot cleanly.
const PluginName = "bgg"

// PluginVersion is the version string the plugin's `--version`
// handler emits and that lands in the Capabilities document's
// `version` field. Read directly from `internal/buildinfo.Version`,
// which the Makefile's LDFLAGS rewrite at link time to
// `<git-tag>+<short-hash>`. Build paths that don't run through the
// Makefile (`go test`, IDE-driven `go build`, `go install`) see
// the package's initial sentinel `"unknown"` and emit that
// verbatim — explicit "no build identity" signal rather than a
// stale-looking semver fallback.
//
// Per yaad-index/yaad-index: the daemon's plugin-cache key
// strips the `+<hash>` suffix before comparing, so build-metadata
// rebuilds at the same tag don't invalidate the cache. `unknown`
// stays `unknown` after the strip — distinct from any real semver
// tag, never collides with a tagged release.
var PluginVersion = buildinfo.Version

// SourceNamespace is the vault-path prefix and entity-ID namespace
// declared in capabilities under ADR-0021. The daemon uses this
// value as the per-plugin namespace when deriving source-node IDs
// (`bgg:<slug>`) and routing the corresponding vault file
// (`<vault>/bgg/<slug>.md`).
const SourceNamespace = "bgg"

// UniversalSourceKind is the wire-shape value yaad-bgg emits in
// `structured.kind` per ADR-0021: every node a plugin emits is
// `kind: source`. The daemon translates this at the storage
// layer to the per-plugin SourceNamespace, so multi-plugin
// source-shape emissions don't collide in a single `source/`
// directory.
const UniversalSourceKind = "source"

// SourceTypeEdgeType is the universal source-type edge yaad-bgg
// always emits per ADR-0021: from the source node to
// `source-type:bgg-record`. Pairs with the operator's
// `canonical_edge_types:` config — declared in
// CanonicalEdgeTypesEmitted so the operator can gate it.
const SourceTypeEdgeType = "is_a"

// SourceTypeName is the descriptive name of yaad-bgg's source-
// type label — the target of the universal `is_a` edge. Daemon's
// slug.Slug derives the canonical-label slug
// (`source-type:bgg-record`).
const SourceTypeName = "bgg-record"

// SourceTypeKind is the system-reserved canonical-kind for
// source-type labels per ADR-0021. Bypasses the operator's
// canonical_kinds gate at the daemon's thin-row materialize
// step.
const SourceTypeKind = "source-type"

// CanonicalEdgeType is the canonical-kind edge yaad-bgg emits
// from a BGG source node to its boardgame canonical label.
const CanonicalEdgeType = "is_about"

// CanonicalKind is the canonical kind a BGG `boardgame` source
// resolves to. Used as the Kind on the `is_about` edge target so
// the daemon can derive the canonical label
// `boardgame:<slug.Slug(name)>`.
const CanonicalKind = "boardgame"

// ExpansionCanonicalKind is the canonical kind a BGG
// `boardgameexpansion` source resolves to per #334 Cut 1.
// Separate-axis decision: the operator's mental model treats
// expansions as a different kind of thing from base games even
// though both are "things you can ingest from BGG" — sharing the
// canonical kind would conflate the type axis and force every
// downstream consumer to disambiguate via metadata.
const ExpansionCanonicalKind = "boardgame-expansion"

// ExpansionEdgeType ties a `boardgame-expansion` canonical to its
// base `boardgame` canonical. yaad-bgg emits one `expansion_of`
// edge per parent boardgame the BGG XMLAPI lists under the
// expansion's `<link type="boardgameexpansion"/>` entries (which,
// for an expansion thing, are the base games it expands).
const ExpansionEdgeType = "expansion_of"

// EdgeTarget is one entry in the ADR-0021 source-shape edges
// block — a descriptive `{name, kind}` reference. Daemon
// resolves to a canonical-label endpoint via slug.Slug.
type EdgeTarget struct {
	Name string
	Kind string
}

// DefaultTTLDays is the LEGACY freshness budget surfaced on
// `entity_kinds[].default_ttl_days`. Kept on the wire for backward
// compat with older yaad-index builds that pre-date's three-
// level cache TTL hierarchy. Modern yaad-index reads
// DefaultCacheTTLSeconds; the dual emission lets operators on the
// upgrade path see consistent values until they fully migrate.
//
// Same 365d value as DefaultCacheTTLSeconds (just expressed in the
// older days unit).
const DefaultTTLDays = 365

// DefaultCacheTTLSeconds is the post- plugin-level cache TTL in
// seconds. 31536000s = 365 days. BGG metadata (designer/publisher/
// year/mechanics) is essentially static once a game ships, so a
// yearly hands-off contract is the right default. Operators wanting
// fresher data override per-entry / globally via yaad-index's
// three-level hierarchy.
//
// Surfaced through `Capabilities.CacheTTLSeconds` (--init top-level
// field) AND `FetchResult.CacheTTLSeconds` (per-fetch override; same
// value as the plugin default for now). Matches the contract
// established by /.
const DefaultCacheTTLSeconds = 31536000

// URLPattern is the regex yaad-bgg advertises for canonical
// `https://boardgamegeek.com/boardgame/<id>[/<slug>]` URLs. yaad-index
// pre-compiles every plugin's url_patterns and dispatches in
// registration order; we keep the pattern conservative (anchored host
// + /boardgame/ + numeric id) so we don't claim search/list/forum
// URLs.
//
// Scope per yaad-index/is `boardgame` only. Expansion
// (`/boardgameexpansion/<id>`) and other BGG kinds (publisher,
// designer, person) are separate follow-ups; their URL patterns
// will land alongside their fetchers.
const URLPattern = `^https?://(www\.)?boardgamegeek\.com/boardgame/[0-9]+(/.*)?$`

// ShorthandPattern is the regex yaad-bgg advertises for the shorthand
// input form `bgg: <id>` (e.g. `bgg: 224517`). Case-insensitive on the
// prefix; allows but doesn't require whitespace after the colon. The
// captured suffix must start with a non-whitespace character —
// guards against `bgg: ` matching and resolving to an empty id.
//
// PR-A accepts numeric ids only; name-based shorthand (`bgg: Brass
// Birmingham`) requires a search call and lands in PR-B.
const ShorthandPattern = `(?i)^bgg:\s*(\S.*)$`

// shorthandRegex is ShorthandPattern compiled once for in-package use.
// The advertised pattern (ShorthandPattern) is what yaad-index
// dispatches against; this in-process copy lets Match recognise the
// shape without a per-call regex compile.
var shorthandRegex = regexp.MustCompile(ShorthandPattern)

// urlRegex is URLPattern compiled once for in-package use. Same
// rationale as shorthandRegex.
var urlRegex = regexp.MustCompile(URLPattern)

// Match returns true for any input this plugin can handle: a canonical
// boardgamegeek.com /boardgame/<id> URL OR the shorthand `bgg: <id>`
// form. Mirrors yaad-wikipedia's Match shape so callers (incl. unit
// tests) can dispatch without the regex round-trip yaad-index does.
func Match(input string) bool {
	if _, ok := matchShorthand(input); ok {
		return true
	}
	return urlRegex.MatchString(input)
}

// matchShorthand returns the suffix captured from a shorthand input,
// or "", false if input is not the shorthand form. PR-A doesn't yet
// parse the suffix into a BGG id (numeric vs name lookup); PR-B will
// add that translation alongside the BGG client.
func matchShorthand(input string) (suffix string, ok bool) {
	m := shorthandRegex.FindStringSubmatch(input)
	if len(m) < 2 {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// CanonicalURL returns the canonical desktop URL for a numeric BGG id.
// Follows BGG's own canonical form (no trailing slug — the id alone
// is the load-bearing identifier).
func CanonicalURL(bggID string) string {
	return "https://boardgamegeek.com/boardgame/" + url.PathEscape(bggID)
}

// urlIDRegex extracts the numeric id from a canonical
// `https://boardgamegeek.com/boardgame/<id>[/<slug>]` URL.
var urlIDRegex = regexp.MustCompile(`(?i)^https?://(?:www\.)?boardgamegeek\.com/boardgame/(\d+)(?:/.*)?$`)

// numericIDRegex matches the numeric-id form of the shorthand
// (`bgg: 224517`). Name-based shorthand (`bgg: Brass Birmingham`)
// is rejected by ResolveID — name-search is a follow-up issue.
var numericIDRegex = regexp.MustCompile(`^\d+$`)

// ErrNotFoundUpstream is returned when BGG replies with no items
// for the requested id. yaad-index's subprocess wrapper translates
// the non-zero plugin exit into a fetch_failed envelope.
var ErrNotFoundUpstream = errors.New("bgg: thing not found")

// errNameShorthand is the internal sentinel returned by ResolveID
// for shorthand inputs whose suffix isn't all-digits (e.g. `bgg:
// Brass Birmingham` or `bgg: brass-birmingham`). Fetch routes these
// to BGG's search API for name-resolution + disambiguation per
//. Not exported because no caller outside this package
// needs to branch on the case — name-resolution is internal control
// flow inside Fetch.
var errNameShorthand = errors.New("bgg: shorthand suffix is a name, not a numeric id")

// ResolveID parses an input (URL or shorthand) into the numeric BGG
// id. Returns the id as int64 plus the canonical desktop URL the
// caller should use for both data.url and the originating notation.
//
// Returns errNameShorthand (internal sentinel) for shorthand inputs
// whose suffix isn't all-digits — Fetch handles those by falling
// through to BGG search per.
func ResolveID(input string) (id int64, canonicalURL string, err error) {
	if m := urlIDRegex.FindStringSubmatch(input); len(m) == 2 {
		v, perr := strconv.ParseInt(m[1], 10, 64)
		if perr != nil {
			return 0, "", fmt.Errorf("%s: parse id from URL: %w", PluginName, perr)
		}
		return v, CanonicalURL(m[1]), nil
	}
	if suffix, ok := matchShorthand(input); ok {
		if !numericIDRegex.MatchString(suffix) {
			return 0, "", errNameShorthand
		}
		v, perr := strconv.ParseInt(suffix, 10, 64)
		if perr != nil {
			return 0, "", fmt.Errorf("%s: parse shorthand id: %w", PluginName, perr)
		}
		return v, CanonicalURL(suffix), nil
	}
	return 0, "", fmt.Errorf("%s: input %q is not a recognised BGG URL or shorthand", PluginName, input)
}

// Plugin holds the runtime configuration for Fetch. Construct via
// New so the bggo.Client is wired with the operator's API key.
type Plugin struct {
	client *bggo.Client
	apiKey string
	// extraClientOpts are bggo.Option values forwarded into the
	// Plugin's internal Client construction when WithClient is
	// NOT supplied. Tests use WithBggClientOptions to point the
	// internal client at an httptest.Server (instead of supplying
	// a fully-built client via WithClient, which would skip the
	// Plugin's own cookie-jar restore path).
	extraClientOpts []bggo.Option
	// username + password are the optional BGG credentials for
	// per-game collection enrichment per #282. When both are
	// non-empty AND dataDir is set, fetchByID also calls
	// /xmlapi2/collection?showprivate=1 + merges the operator's
	// per-game fields (status flags, personal rating, num plays,
	// comment, privateinfo) into the canonical row. When either
	// is empty, enrichment is silently off and the plugin
	// behaves as the /thing-only legacy path.
	username string
	password string
	// dataDir is the daemon-managed per-instance persistent
	// state directory (YAAD_PLUGIN_DATA_DIR, see #284). The
	// plugin persists its session-cookie jar to
	// `<dataDir>/session.json` so subsequent subprocess
	// invocations skip the Login round-trip when the jar's
	// session is still valid. Empty disables persistence —
	// each subprocess re-logins from scratch.
	dataDir string
	// warnf is the side-channel logger for non-fatal events
	// (collection-enrichment fall-through, cookie-jar write
	// errors, etc.). main.go injects a stderr-backed function;
	// tests inject a buffer-backed function. nil silences.
	warnf func(format string, args ...any)
	// enrichmentSession holds the active credentials' username
	// after a successful Login OR successful cookie-jar
	// restore. Empty means the credentials have not yet been
	// exercised in this subprocess lifetime.
	enrichmentSession string
}

// Option configures a Plugin at construction.
type Option func(*Plugin)

// WithClient overrides the default bggo.Client. Tests inject a
// client wired to an httptest.Server-backed bggo.WithHost +
// WithScheme when they don't care about the Plugin's cookie-jar
// restore flow; otherwise prefer WithBggClientOptions so the
// Plugin can construct its own client + run the jar restore.
func WithClient(c *bggo.Client) Option {
	return func(p *Plugin) { p.client = c }
}

// WithBggClientOptions forwards extra bggo.Option values into the
// Plugin's internal client construction. Tests pass
// WithHost / WithScheme here so the Plugin builds its own client
// pointed at an httptest.Server — the cookie-jar restore path
// runs unchanged. Production code (cmd/yaad-bgg/main.go) doesn't
// need this and uses the default bggo host.
func WithBggClientOptions(opts ...bggo.Option) Option {
	return func(p *Plugin) {
		p.extraClientOpts = append(p.extraClientOpts, opts...)
	}
}

// WithCredentials enables per-game collection enrichment by
// wiring the BGG username + password. Both empty → enrichment
// off (legacy /thing-only behavior). Both non-empty + dataDir
// set → enrichment on. Either empty → enrichment off (silent;
// per #282 acceptance "if either is missing, enrichment is
// silently off").
func WithCredentials(username, password string) Option {
	return func(p *Plugin) {
		p.username = username
		p.password = password
	}
}

// WithDataDir wires the daemon-managed persistent-state
// directory (YAAD_PLUGIN_DATA_DIR, #284) so the plugin can
// persist its session-cookie jar across subprocess invocations.
// Empty string disables persistence — each subprocess starts
// from a fresh login.
func WithDataDir(dir string) Option {
	return func(p *Plugin) { p.dataDir = dir }
}

// WithWarnLogger wires the side-channel logger for non-fatal
// events (collection enrichment fall-through, cookie-jar write
// errors, etc.). main.go injects a stderr-backed function;
// tests inject a buffer-backed function. nil silences (the
// zero-value default — Plugin code checks for nil before
// calling).
func WithWarnLogger(f func(format string, args ...any)) Option {
	return func(p *Plugin) { p.warnf = f }
}

// New constructs a Plugin with a bggo.Client built from apiKey.
// apiKey is required (BGG returns rate-limited / fields-limited
// data on anonymous calls); empty string is rejected per the operator's
// fail-closed spec.
//
// Options apply BEFORE the cookie-jar restore + WithCookies
// wiring so a test that supplies WithClient skips the cookie
// jar entirely (the supplied client owns its own state).
func New(apiKey string, opts ...Option) (*Plugin, error) {
	if apiKey == "" {
		return nil, errors.New("bgg.New: BGG_API_KEY is required (yaad-bgg fails closed; no anonymous fallback)")
	}
	p := &Plugin{
		apiKey: apiKey,
	}
	for _, o := range opts {
		o(p)
	}
	if p.client == nil {
		clientOpts := append([]bggo.Option{}, p.extraClientOpts...)
		// #282: try to restore a persisted session cookie jar so
		// the first authenticated fetch in this subprocess
		// lifetime can skip the Login round-trip. Absent /
		// expired / unparseable jars resolve to a fresh login on
		// first authed fetch.
		if p.enrichmentEnabled() {
			username, cookies, err := loadCookieJar(p.dataDir)
			switch {
			case err == nil:
				if username == p.username {
					clientOpts = append(clientOpts, bggo.WithCookies(username, cookies))
					p.enrichmentSession = username
				}
			case errors.Is(err, errCookieJarAbsent):
				// Expected on first run; nothing to log.
			default:
				p.warn("bgg: cookie jar load failed (will re-login on next authed fetch): %v", err)
			}
		}
		p.client = bggo.NewClient(apiKey, clientOpts...)
	}
	return p, nil
}

// enrichmentEnabled reports whether per-game collection
// enrichment should run for this Plugin per #282. Requires
// both creds AND the daemon-managed data dir — without a dir
// the plugin can't persist cookies across subprocess
// invocations + would re-login on every dispatch, which
// hammers BGG and slows every fetch. The conservative gate
// matches the issue's "if either is missing, enrichment is
// silently off" rule.
func (p *Plugin) enrichmentEnabled() bool {
	return p.username != "" && p.password != "" && p.dataDir != ""
}

// warn calls the side-channel logger if set, ignoring otherwise.
func (p *Plugin) warn(format string, args ...any) {
	if p.warnf != nil {
		p.warnf(format, args...)
	}
}

// FetchOutcome carries the result of a successful Fetch. Mutually-
// exclusive `Boardgame` / `Options` shape, mirroring yaad-wikipedia
// per ADR-0006:
//
// - `Boardgame` populated → standard fetched-entity case. main.go
// emits `structured` / `raw_content` from this.
// - `Options` populated → BGG search returned multi-match (name-
// shorthand path per. main.go emits `options` and
// yaad-index's subprocess wrapper translates that to
// `state: disambiguation`. Agent picks one and re-invokes ingest
// via the `bgg: <id>` numeric shorthand.
//
// Provenance is shared — both shapes carry the upstream attempt
// record (search call for the disambiguation case; thing+search for
// the resolved case).
type FetchOutcome struct {
	Boardgame *Boardgame
	Options []DisambiguationOption
	Provenance []ProvenanceEntry
}

// DisambiguationOption is one candidate when BGG search resolved a
// name-shorthand to multiple matches (per. `ID` is the
// numeric BGG id as a string — the agent re-ingests via
// `bgg: <ID>`. `Label` is the human-readable display text (`<name>
// (<year>)` when the year is known, bare name otherwise). `Summary`
// is intentionally empty in v1: BGG's search API returns no
// description field — the rich data lives behind the per-id
// xmlapi2/thing call (which is what re-ingest pays for). Carrying
// the empty field on the wire keeps the disambiguation envelope
// shape identical to yaad-wikipedia's (mcp surface treats it the
// same).
type DisambiguationOption struct {
	ID string
	Label string
	Summary string
}

// Boardgame is the parsed result of a successful Fetch. The wire
// shape (kind=source + name + data + edges + aliases + attachments)
// is constructed from this in main.go per ADR-0021.
type Boardgame struct {
	// Name is the descriptive BGG title (e.g. "Brass: Birmingham
	// (2018)"). Daemon's slug.Slug derives the source-node ID
	// `bgg:<slug.Slug(Name)>` — annotations participate in the
	// source slug so multiple games sharing a base name
	// disambiguate cleanly.
	Name string

	// BGGID is the numeric BGG identifier (e.g. 224517 for "Brass:
	// Birmingham (2018)"). Used to build the `bgg: <id>` notation
	// shorthand and to label the staged thumbnail attachment.
	// Source-node entity ID derivation does NOT use this (per
	// ADR-0021 the daemon slugifies Name); BGGID stays around for
	// notation + attachment naming where a numeric handle is more
	// useful than a slugified title.
	BGGID string

	// Edges is the ADR-0021 source-shape edges block. Always
	// carries:
	//
	// - is_a → [{Name: "bgg-record", Kind: "source-type"}]
	// - is_about → [{Name: <canonical name>, Kind: "boardgame"}]
	//
	// Conditionally carries (when BGG returns the relation):
	//
	// - designed_by → [{Name, Kind: "person"}, ...]
	// - artist_by → [{Name, Kind: "person"}, ...]
	// - published_by → [{Name, Kind: "company"}, ...]
	//
	// Canonical-edge target Names are stripped of BGG's domain-
	// specific annotations (year-suffix, parens-disambig, mid-name
	// `: ` series-separator → single space) per the plugin-side
	// canonical-name-production responsibility split. Both feed
	// daemon's slug.Slug; cross-plugin convergence with yaad-
	// wikipedia's `is_about` edge target on the same boardgame /
	// person works because both sides emit the same descriptive
	// canonical name.
	Edges map[string][]EdgeTarget

	// Data is the entity.data map persisted to vault frontmatter.
	// Ordering matches the canonical field set the operator's spec calls
	// out (publisher / designed_by / artist_by) plus standard
	// boardgame metadata. Map (not struct) because vault writes
	// preserve insertion order and a typed struct would force a
	// schema migration on every field add.
	Data map[string]any

	// Aliases is the alternative-label list (per ADR-0011 +
	//. Populated from BGG's `alternate name` entries
	// — foreign-language titles, original publisher's title, etc.
	// First element is always the primary name (matches the
	// title-synthesis surface yaad-index runs on the canonical-
	// kinds path). yaad-index dedupes against ADR-0011's title-
	// synthesized alias at vault-write time.
	Aliases []string

	// ThumbnailURL is the upstream-canonical thumbnail URL emitted
	// by BGG. main.go downloads this to staging and emits an
	// attachment via pkg/plugin/attach. Empty when BGG returns no
	// thumbnail for this game (rare; main.go skips with WARN).
	ThumbnailURL string

	// Notations is the set of input forms that resolve to this
	// boardgame — canonical URL + `bgg: <id>` shorthand. Used by
	// yaad-index's lookup-first cache to short-circuit re-ingest.
	Notations []string
}

// ProvenanceEntry records the upstream attempt for one fetch.
// Mirrors store.ProvenanceEntry without the import — same shape
// rationale as yaad-wikipedia's local copy.
type ProvenanceEntry struct {
	Source string
	FetchedAt time.Time
	OK bool
}

// Fetch resolves an input (URL, numeric shorthand, or name shorthand)
// to a BGG boardgame and returns it. Three terminal states:
//
// - `Boardgame` populated → resolved cleanly (URL / numeric / single-
// match name search).
// - `Options` populated → name search returned multi-match; agent
// re-ingests via the chosen `bgg: <id>` shorthand.
// - error → ErrNotFoundUpstream (BGG knows the id/name but it's
// non-boardgame, or search returned zero results), or a wrapped
// transport / parse error.
//
// Context cancellation is honoured (passed through to bggo.Client).
func (p *Plugin) Fetch(ctx context.Context, input string) (*FetchOutcome, error) {
	id, canonicalURL, err := ResolveID(input)
	if errors.Is(err, errNameShorthand) {
		// Name-shorthand path: extract the suffix and resolve via BGG
		// search. matchShorthand can't fail here — ResolveID already
		// matched the prefix and returned errNameShorthand only when
		// the suffix existed but wasn't numeric.
		suffix, _ := matchShorthand(input)
		return p.fetchByName(ctx, suffix)
	}
	if err != nil {
		return nil, err
	}
	return p.fetchByID(ctx, id, canonicalURL, input)
}

// fetchByID hits BGG's xmlapi2/thing endpoint for a resolved id and
// constructs the *FetchOutcome. Shared between the URL / numeric-
// shorthand path and the post-search single-match path.
func (p *Plugin) fetchByID(ctx context.Context, id int64, canonicalURL, originalInput string) (*FetchOutcome, error) {
	results, err := p.client.GetThings(ctx, bggo.GetThingsRequest{IDs: []int64{id}})
	if err != nil {
		return nil, fmt.Errorf("%s: GetThings(%d): %w", PluginName, id, err)
	}
	if len(results) == 0 {
		return nil, ErrNotFoundUpstream
	}

	// BGG's `thing` endpoint can return any item type for an id —
	// boardgameexpansion, boardgameaccessory, rpgitem, videogame.
	// #334 Cut 1 accepts boardgameexpansion in addition to
	// boardgame; other types still surface as not-found pending
	// Cut 2 (boardgameaccessory + boardgamefamily) and Cut 3
	// (videogame + rpg / rpgitem).
	t := results[0]
	if t.Type != bggo.BoardGameType && t.Type != bggo.BoardGameExpansionType {
		return nil, fmt.Errorf("%w: id %d is type %q (yaad-bgg supports boardgame + boardgameexpansion only; other thing types pending later cuts)",
			ErrNotFoundUpstream, id, t.Type)
	}

	now := operatorNow()
	bggIDStr := strconv.FormatInt(t.ID, 10)

	// ADR-0021: the plugin emits the descriptive title (with
	// year-suffix / parens-disambig / BGG series-separator
	// retained); the daemon's slug.Slug derives the source-node
	// ID at translation time. Canonical-edge target names are
	// stripped of those annotations via canonicalizeBGGName below
	// so the daemon-derived canonical-label slug converges with
	// other plugins emitting the same descriptive name (e.g.
	// yaad-wikipedia's `is_about` edge for the same boardgame).
	bg := &Boardgame{
		Name: t.Name,
		BGGID: bggIDStr,
		Data: buildData(canonicalURL, t),
		Aliases: buildAliases(t),
		ThumbnailURL: t.Thumbnail,
		Notations: []string{
			originalInput,
			canonicalURL,
			"bgg: " + bggIDStr,
		},
		Edges: buildEdges(t),
	}
	bg.Notations = dedupeStrings(bg.Notations)

	provenance := []ProvenanceEntry{
		{
			Source: canonicalURL,
			FetchedAt: now,
			OK: true,
		},
	}

	// #282: per-game collection enrichment. When credentials +
	// data dir are configured AND the game is in the operator's
	// BGG collection, merge personal fields onto bg.Data.
	// Anything that goes wrong here is non-fatal — the /thing
	// result still lands; the operator sees a stderr WARN.
	if p.enrichmentEnabled() {
		entry, source, enrichErr := p.fetchCollectionEntry(ctx, id)
		if enrichErr != nil {
			p.warn("bgg: collection enrichment failed for id %d: %v (falling back to /thing-only)", id, enrichErr)
		} else if entry != nil {
			mergeOperatorFields(bg.Data, entry)
		}
		if source != "" {
			provenance = append(provenance, ProvenanceEntry{
				Source: source,
				FetchedAt: now,
				OK: enrichErr == nil,
			})
		}
	}

	return &FetchOutcome{
		Boardgame: bg,
		Provenance: provenance,
	}, nil
}

// buildEdges composes the ADR-0021 source-shape edges block for a
// fetched boardgame. Always carries:
//
// - is_a → bgg-record (source-type label)
// - is_about → <canonicalized title> (boardgame canonical kind)
//
// Conditionally carries (when BGG returns the relation):
//
// - designed_by → person target per designer
// - artist_by → person target per artist
// - published_by → company target (single — first publisher only)
//
// Canonical-edge target Names are passed through canonicalizeBGGName
// for the boardgame `is_about` target (BGG titles carry year-suffix
// / parens-disambig / series-separator). Person + company names
// flow through as-is — BGG's link names for individuals/companies
// don't carry the same annotations.
func buildEdges(t bggo.ThingResult) map[string][]EdgeTarget {
	// #334 Cut 1: for a boardgameexpansion thing, the canonical
	// `is_about` target is the boardgame-expansion kind rather
	// than boardgame, AND each `boardgameexpansion`-typed link
	// becomes an `expansion_of` edge to the parent boardgame
	// canonical. BGG's xmlapi2 carries these as inbound links
	// from the expansion to the base game(s) — the daemon's
	// slug.Slug derives the canonical-label id from the link
	// Name. (Outbound expansion links from a base game's thing
	// page would be the reverse direction; v1 doesn't emit those
	// — operators consume the relationship from the expansion
	// side only, which is the side that knows what it expands.)
	canonicalKind := CanonicalKind
	if t.Type == bggo.BoardGameExpansionType {
		canonicalKind = ExpansionCanonicalKind
	}
	edges := map[string][]EdgeTarget{
		SourceTypeEdgeType: {{Name: SourceTypeName, Kind: SourceTypeKind}},
		CanonicalEdgeType: {{
			Name: canonicalizeBGGName(t.Name),
			Kind: canonicalKind,
		}},
	}
	if t.Type == bggo.BoardGameExpansionType {
		if parents := t.Links[string(bggo.BoardGameExpansionType)]; len(parents) > 0 {
			row := make([]EdgeTarget, len(parents))
			for i, p := range parents {
				row[i] = EdgeTarget{
					Name: canonicalizeBGGName(p.Name),
					Kind: CanonicalKind,
				}
			}
			edges[ExpansionEdgeType] = row
		}
	}
	if designers := t.Designers(); len(designers) > 0 {
		row := make([]EdgeTarget, len(designers))
		for i, d := range designers {
			row[i] = EdgeTarget{Name: d.Name, Kind: "person"}
		}
		edges["designed_by"] = row
	}
	if artists := t.Artists(); len(artists) > 0 {
		row := make([]EdgeTarget, len(artists))
		for i, a := range artists {
			row[i] = EdgeTarget{Name: a.Name, Kind: "person"}
		}
		edges["artist_by"] = row
	}
	if publishers := t.Publishers(); len(publishers) > 0 {
		// Single publisher per the operator's 2026-05-06 spec — rest are
		// localized variants v1 doesn't surface.
		edges["published_by"] = []EdgeTarget{
			{Name: publishers[0].Name, Kind: "company"},
		}
	}
	return edges
}

// trailingYearRE matches BGG's year-suffix annotation: `(YYYY)` at
// the end of the title with optional leading whitespace.
// "Brass: Birmingham (2018)" → match.
var trailingYearRE = regexp.MustCompile(`\s*\(\d{4}\)\s*$`)

// trailingParensRE matches BGG's rare non-numeric parens-disambig
// at the end of the title (e.g. a fictional "Foo (special edition)"
// case). Runs after trailingYearRE so a 4-digit year is consumed
// by the more-specific regex first; this catches everything else.
var trailingParensRE = regexp.MustCompile(`\s*\([^()]+\)\s*$`)

// midNameSeriesColonRE matches BGG's series-separator: `: `
// (colon-space) appearing mid-name (NOT trailing). Example:
// "Brass: Birmingham" → "Brass Birmingham" (both halves survive,
// joined by a single space). The `:` MUST be followed by a non-
// whitespace word character to qualify as mid-name; trailing
// `Foo:` doesn't match (preserves rare titles ending in a colon).
var midNameSeriesColonRE = regexp.MustCompile(`:\s+(\S)`)

// canonicalizeBGGName strips BGG's domain-specific annotations
// from a title, producing the descriptive name yaad-bgg emits as
// the canonical-edge target Name per ADR-0021. The daemon's
// slug.Slug then derives `boardgame:<slug>`.
//
// Order:
//
// 1. Strip trailing `(\d{4})` year-suffix.
// 2. Strip trailing `(...)` non-numeric parens-disambig.
// 3. Replace mid-name `: ` (colon-space) with a single space —
// "Brass: Birmingham" → "Brass Birmingham". Both halves
// survive; the colon is mid-name, not trailing.
// 4. Trim surrounding whitespace.
//
// Source-node Name (Boardgame.Name) keeps all annotations — the
// source slug round-trips back to BGG. canonicalizeBGGName runs
// only on the canonical-edge target side.
func canonicalizeBGGName(name string) string {
	s := strings.TrimSpace(name)
	s = trailingYearRE.ReplaceAllString(s, "")
	s = trailingParensRE.ReplaceAllString(s, "")
	s = midNameSeriesColonRE.ReplaceAllString(s, " $1")
	return strings.TrimSpace(s)
}

// fetchByName resolves a name-shorthand suffix via BGG's xmlapi2
// search endpoint. Three result paths:
//
// - 0 matches → ErrNotFoundUpstream wrapped with the query so the
// subprocess wrapper surfaces a clean fetch_failed envelope.
// - 1 match → recurse into fetchByID (transparent ingest). The
// agent sees no disambiguation; the resolved entity lands as if
// they'd typed the canonical URL.
// - ≥2 matches → return *FetchOutcome with Options populated.
//
// `query` is the trimmed shorthand suffix (e.g. `Brass Birmingham`,
// `brass-birmingham`, `"Brass: Birmingham"`). Pass through to BGG
// search verbatim — BGG's tokenizer handles hyphens / quotes
// reasonably; if it doesn't, the agent gets `not_found` and re-tries
// with a different form. No client-side normalization in v1.
//
// Provenance on the disambiguation path records the search call.
// Provenance on the single-match path comes from the subsequent
// fetchByID call.
func (p *Plugin) fetchByName(ctx context.Context, query string) (*FetchOutcome, error) {
	if query == "" {
		return nil, fmt.Errorf("%s: empty name shorthand", PluginName)
	}
	// #334 Cut 1: include boardgameexpansion in the search types
	// so name-shorthand resolution surfaces expansions alongside
	// base games. Disambiguation candidates now correctly include
	// expansion entries (which the fetchByID path then accepts
	// per the boardgame || boardgameexpansion gate above).
	results, err := p.client.Search(ctx, bggo.SearchRequest{
		Query: query,
		Types: []bggo.ItemType{bggo.BoardGameType, bggo.BoardGameExpansionType},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: Search(%q): %w", PluginName, query, err)
	}
	switch len(results) {
	case 0:
		return nil, fmt.Errorf("%w: bgg search for %q returned no boardgame matches", ErrNotFoundUpstream, query)
	case 1:
		// Single-match transparent resolve. Re-use the canonical-URL
		// shape so the resulting entity's Notations carry the same
		// triple as a URL-or-numeric ingest would.
		bggIDStr := strconv.FormatInt(results[0].ID, 10)
		return p.fetchByID(ctx, results[0].ID, CanonicalURL(bggIDStr), "bgg: "+query)
	default:
		now := operatorNow()
		options := make([]DisambiguationOption, 0, len(results))
		for _, r := range results {
			label := r.Name
			if r.YearPublished > 0 {
				label = fmt.Sprintf("%s (%d)", r.Name, r.YearPublished)
			}
			options = append(options, DisambiguationOption{
				ID: strconv.FormatInt(r.ID, 10),
				Label: label,
				// Summary intentionally empty: BGG search has no
				// description field; the rich data lives behind
				// xmlapi2/thing.
			})
		}
		return &FetchOutcome{
			Options: options,
			Provenance: []ProvenanceEntry{
				{
					Source: fmt.Sprintf("bgg:search?q=%s", query),
					FetchedAt: now,
					OK: true,
				},
			},
		}, nil
	}
}

// buildData composes the entity.data map per the operator's 2026-05-06
// frontmatter spec:
//
// - title — primary BGG name
// - url — canonical desktop URL
// - year — year_published (omit when 0)
// - publisher — first publisher name only (rest are localized
// variants v1 doesn't surface)
// - designed_by — list of designer names
// - artist_by — list of artist names (BGG splits these from
// designers explicitly)
// - description — BGG's description text (HTML-decoded by bggo)
// - bgg_rating — average rating (0..10 scale)
// - bgg_weight — average weight (1..5 complexity scale)
// - min/max_players — player count range
// - playing_time — minutes
//
// yaad-index's ingest layer translates `publisher: <name>`,
// `designed_by: [...]`, `artist_by: [...]` into typed canonical
// edges (`published_by company:<name>`, `designed_by person:<name>`,
// etc.) per the operator's 2026-05-06 spec — the plugin only emits the
// frontmatter; the daemon resolves edges.
func buildData(canonicalURL string, t bggo.ThingResult) map[string]any {
	data := map[string]any{
		"title": t.Name,
		"url": canonicalURL,
		"bgg_id": t.ID,
	}
	if t.YearPublished != 0 {
		data["year"] = t.YearPublished
	}
	publishers := t.Publishers()
	if len(publishers) > 0 {
		data["publisher"] = publishers[0].Name
	}
	if designers := t.Designers(); len(designers) > 0 {
		data["designed_by"] = linkNames(designers)
	}
	if artists := t.Artists(); len(artists) > 0 {
		data["artist_by"] = linkNames(artists)
	}
	if t.Description != "" {
		data["description"] = t.Description
	}
	if t.AverageRate != 0 {
		data["bgg_rating"] = t.AverageRate
	}
	if t.AverageWeight != 0 {
		data["bgg_weight"] = t.AverageWeight
	}
	if t.MinPlayers != 0 {
		data["min_players"] = t.MinPlayers
	}
	if t.MaxPlayers != 0 {
		data["max_players"] = t.MaxPlayers
	}
	if t.PlayingTime != 0 {
		data["playing_time"] = t.PlayingTime
	}
	return data
}

// buildAliases returns the alias list per ADR-0011: primary BGG
// name first (matches what yaad-index's title-synthesis would
// produce), followed by every BGG alternate name (foreign-language
// titles, original-publisher's title). Dedupes inline.
func buildAliases(t bggo.ThingResult) []string {
	aliases := make([]string, 0, len(t.AlternateNames)+1)
	if t.Name != "" {
		aliases = append(aliases, t.Name)
	}
	aliases = append(aliases, t.AlternateNames...)
	return dedupeStrings(aliases)
}

// linkNames extracts just the names from a slice of bggo.Link.
func linkNames(links []bggo.Link) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.Name
	}
	return out
}

// dedupeStrings returns s with duplicates dropped, preserving input
// order. Skips empty strings.
func dedupeStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// operatorNow returns time.Now() in the operator-configured TZ
// (per yaad-index PR-D's YAAD_TIMEZONE env). Falls back to
// UTC when the env var is unset or malformed — same defensive
// posture as yaad-index's own clock.Location() default.
func operatorNow() time.Time {
	if name := strings.TrimSpace(os.Getenv("YAAD_TIMEZONE")); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return time.Now().In(loc)
		}
	}
	return time.Now().UTC()
}
