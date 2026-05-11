// Package wikipedia implements the Match + Fetch logic for the Wikipedia
// extractor. It matches any URL whose host ends in `wikipedia.org` and
// fetches the article summary via the public REST API
// (https://<lang>.wikipedia.org/api/rest_v1/page/summary/<title>).
//
// The summary endpoint is chosen over the full-content endpoints
// because it returns a clean, well-typed JSON shape that maps directly
// onto an entity.data: title, extract (a few-paragraph summary), lang,
// canonical content URL. No HTML parsing required.
//
// This package is consumed by the top-level yaad-wikipedia binary,
// which translates Article + ProvenanceEntry into the JSON wire shape
// alice2-index's subprocess.Plugin expects (per ADR-0006). The wire
// translation lives in main.go on purpose — keeping wire concerns out
// of the parser keeps this package straightforwardly unit-testable.
package wikipedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/buildinfo"
)

// PluginName is the stable identifier surfaced as the capabilities
// document's `name` and on every provenance entry.
const PluginName = "wikipedia"

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
// (`wikipedia-article:<slug>`) and routing the corresponding vault
// file (`<vault>/wikipedia-article/<slug>.md`).
const SourceNamespace = "wikipedia-article"

// UniversalSourceKind is the wire-shape value yaad-wikipedia
// emits in `structured.kind` per ADR-0021: every node a plugin
// emits is `kind: source`. The daemon translates this at the
// storage layer to the per-plugin SourceNamespace, so multi-plugin
// source-shape emissions don't collide in a single `source/`
// directory.
const UniversalSourceKind = "source"

// DefaultTTLDays is the LEGACY freshness budget surfaced on
// `entity_kinds[].default_ttl_days` in the --init capabilities
// document. Predates alice2-index's three-level cache TTL
// hierarchy; kept on the wire for backward compat with older
// alice2-index builds. Modern alice2-index (post-) reads
// DefaultCacheTTLSeconds instead. Operators on the upgrade path
// see consistent values until they fully migrate.
const DefaultTTLDays = 7

// DefaultCacheTTLSeconds is the post- plugin-level cache TTL
// in seconds (per alice2-index/yaad-wikipedia). 31536000s = 365
// days. Wikipedia article cadence is slow enough that a yearly
// default is the right hands-off contract. Operators wanting
// fresher data override per-entry / per-plugin config via the
// alice2-index three-level hierarchy.
//
// Surfaced through `Capabilities.CacheTTLSeconds` (--init top-
// level field) AND `FetchResult.CacheTTLSeconds` (per-fetch
// override; same value as the plugin default for now, lets the
// plugin specialize per-article in a future PR if some pages
// merit different freshness).
const DefaultCacheTTLSeconds = 31536000

// DefaultUpstreamTimeout caps each upstream HTTP fetch. 5s matches the
// per-fetch budget on alice2-index's subprocess wrapper, so the
// subprocess timeout never trips before the in-binary timeout has a
// chance to surface a clean error.
const DefaultUpstreamTimeout = 5 * time.Second

// DefaultLang is the Wikipedia language code used when resolving the
// shorthand input form (`wikipedia: <topic>`) to a canonical URL.
// English is the most common case; operators with a different default
// audience override this via WithLang (yaad-wikipedia's
// YAAD_WIKIPEDIA_LANG env reads through to the option in main.go).
const DefaultLang = "en"

// ErrNotFoundUpstream is returned when Wikipedia replies 404 to the
// summary request. The yaad-wikipedia binary maps this to a non-zero
// exit + a stderr message naming the URL; alice2-index's subprocess
// wrapper translates the exit code into a fetch_failed envelope.
var ErrNotFoundUpstream = errors.New("wikipedia: article not found")

// FetchOutcome is what Fetch returns — a struct with mutually-
// exclusive Article and Options fields. Exactly one is populated:
//
// - Article: standard fetched-entity case. main.go emits
// `structured` / `raw_content` / `gaps` from this.
// - Options: search returned multi-match (or the URL was a
// disambiguation page that triggered the search-fallback per
// ADR-0006). main.go emits `options` from this; agent picks one
// and re-invokes ingest via the `wikipedia: <id>` shorthand.
//
// Provenance is shared — both shapes carry the upstream attempt
// record. Empty Options + nil Article is the degenerate
// no-results-from-search case (acceptable; agent re-tries with a
// more specific query).
type FetchOutcome struct {
	Article *Article
	Options []DisambiguationOption
	Provenance []ProvenanceEntry
}

// DisambiguationOption is one candidate when a search resolved to
// multiple matches. ID is the shorthand-resolvable handle the agent
// re-ingests via `wikipedia: <id>` — typically the Wikipedia
// article title with disambiguator parens (e.g. "Martin Wallace
// (game designer)"). Label is the human-readable display text;
// usually identical to ID. Summary is a one-sentence description
// (from the search snippet or the article's brief Wikidata
// description).
type DisambiguationOption struct {
	ID string
	Label string
	Summary string
}

// Article is the parsed result of a successful Fetch. The alice2-index
// wire shape (per ADR-0021: kind="source" + descriptive name + edges
// block under the `structured` key + top-level raw_content / gaps)
// is constructed from Article in main.go.
//
// Per ADR-0021, the plugin emits descriptive names + edge `{name,
// kind}` references only — no slugs. The daemon owns slug
// derivation via internal/slug.Slug. The plugin DOES strip its
// own domain-specific disambig (Wikipedia's parens-disambig) from
// the canonical-edge target name before emission, so the canonical
// label is clean (`person:martin-wallace`, not
// `person:martin-wallace-game-designer`); the source-node Name
// retains the parens-disambig (round-trips back to the Wikipedia
// URL).
type Article struct {
	// Name is the descriptive Wikipedia article title, retained
	// with parens-disambig (e.g. "Martin Wallace (game designer)").
	// The daemon's slug.Slug derives the source-node ID
	// `wikipedia-article:<slug.Slug(Name)>`. Source-side parens
	// preservation lets the slug round-trip back to the Wikipedia
	// URL: `martin-wallace-game-designer`.
	Name string
	Data map[string]any
	Provenance []ProvenanceEntry

	// RawContent is the article body in plaintext — the agent's AI
	// reads this for summary / tag generation via fill. Pulled via
	// MediaWiki action API (`/w/api.php?action=query&prop=extracts&
	// explaintext=1`) since the REST `/page/summary/` endpoint only
	// returns a few-paragraph intro. Wired into FetchResult.RawContent
	// per ADR-0008. Empty when the action API call failed; the
	// summary path still succeeded so the entity still lands.
	RawContent string

	// Edges is the ADR-0021 source-shape edges block, keyed by
	// edge type. Always carries:
	//
	// - `is_a` → [{Name: "wikipedia-article", Kind: "source-type"}]
	//
	// Conditionally carries (when the article's Wikidata Q-id
	// resolves to a known canonical kind):
	//
	// - `is_about` → [{Name: <stripTrailingParens(title)>, Kind: <kind>}]
	//
	// The `is_about` target Name is stripped of parens-disambig
	// per's cross-plugin-dedup rationale: the
	// canonical `person:martin-wallace` label needs to match BGG's
	// `designed_by` edge target by slug, so both plugins must
	// emit the same descriptive name (parens stripped) for the
	// daemon's slug.Slug to converge on `martin-wallace`.
	Edges map[string][]EdgeTarget

	// KindGaps is the kind-specific gap-name → AI-prompt map merged
	// into the wire-level `gaps` (alongside the universal summary +
	// tags). Set when a known canonical kind is detected; nil
	// otherwise. main.go's runFetch composes the final gap map.
	KindGaps map[string]string

	// Notations is every input form yaad-wikipedia knows resolves
	// to this article — canonical desktop URL, mobile subdomain URL,
	// shorthand `wikipedia: <human-title>`, and the original input
	// if it differs from the derived forms. alice2-index's
	// orchestrator (per alice2-index the source issue a prior PR) writes these to
	// the entity_notations cache after Fetch so subsequent ingests
	// of any equivalent form short-circuit on the cache without
	// re-invoking this plugin.
	//
	// Always includes the input notation (in whatever shape the
	// caller passed) so a self-roundtrip with the same input
	// registers a hit on the next call.
	Notations []string

	// Aliases is the alternative-label list emitted alongside the
	// article (per alice2-index the source issue a prior PR). Today, populated
	// with the article's human-readable title from the REST summary
	// — the same string the agent's natural Obsidian wikilink would
	// type. Supplements ADR-0011's title-synthesized alias on the
	// alice2-index side; the merge dedupes so emitting both is fine.
	//
	// Future expansion: multi-language aliases
	// pulled from Wikidata's `also_known_as` claims. v1 keeps it
	// to the title only.
	Aliases []string
}

// ProvenanceEntry records the upstream attempt for one article fetch.
// Mirrors the alice2-index store.ProvenanceEntry shape, but defined
// locally so this module has no alice2-index dependency.
type ProvenanceEntry struct {
	Source string
	FetchedAt time.Time
	OK bool
}

// Plugin holds the runtime configuration for Match + Fetch. Construct
// via New so the HTTP client + timeout defaults are wired sanely.
type Plugin struct {
	httpClient *http.Client

	// apiHostOverride forces upstream calls onto a different host
	// regardless of the request URL's own host. Default behaviour:
	// derive from the request URL (so `de.wikipedia.org` requests hit
	// `de.wikipedia.org`'s API). Tests use this to point the plugin
	// at an httptest.Server.
	apiHostOverride string

	// wikidataHostOverride targets the Wikidata EntityData endpoint
	// (default www.wikidata.org). Tests substitute an httptest.Server
	// URL so the wikidata Q-id → kind lookup resolves locally.
	wikidataHostOverride string

	// userAgent is the User-Agent header sent on every upstream call.
	// Wikimedia's API requires identifying ourselves; default value
	// names yaad-wikipedia, the version, and a contact URL per
	// https://meta.wikimedia.org/wiki/User-Agent_policy.
	userAgent string

	// lang is the Wikipedia language code used to resolve shorthand
	// input (`wikipedia: <topic>`) into a canonical URL. The full URL
	// form (`https://...wikipedia.org/wiki/...`) is unaffected — its
	// own host already names the language.
	lang string
}

// Option configures a Plugin at construction.
type Option func(*Plugin)

// WithHTTPClient overrides the default http.Client. Tests use this to
// inject a client wired to an httptest.Server.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Plugin) { p.httpClient = c }
}

// WithAPIHostOverride forces all upstream calls to use the given host
// (or full base URL if it begins with http:// or https://) regardless
// of the request URL's own host. Used by tests pointing at an
// httptest.Server.
func WithAPIHostOverride(host string) Option {
	return func(p *Plugin) { p.apiHostOverride = host }
}

// WithWikidataHostOverride targets the Wikidata EntityData endpoint
// at the given host or full http(s) base URL. Default points at
// www.wikidata.org; tests substitute an httptest.Server URL.
func WithWikidataHostOverride(host string) Option {
	return func(p *Plugin) { p.wikidataHostOverride = host }
}

// WithUserAgent overrides the default User-Agent header. Operators
// running yaad-wikipedia in a non-default deployment context (e.g.
// behind a known org email) can substitute a contact URL Wikimedia
// can route abuse triage to.
func WithUserAgent(ua string) Option {
	return func(p *Plugin) { p.userAgent = ua }
}

// WithLang overrides the Wikipedia language code used when resolving
// shorthand input (`wikipedia: <topic>`). Default is DefaultLang
// ("en"). Empty string is treated as "use the default."
func WithLang(lang string) Option {
	return func(p *Plugin) {
		if lang != "" {
			p.lang = lang
		}
	}
}

// New returns a Plugin with sensible defaults: 5s upstream timeout,
// a yaad-wikipedia-identifying User-Agent, and English as the
// shorthand-resolution language. Override via WithHTTPClient /
// WithUserAgent / WithLang.
func New(opts ...Option) *Plugin {
	p := &Plugin{
		httpClient: &http.Client{Timeout: DefaultUpstreamTimeout},
		userAgent: fmt.Sprintf(
			"yaad-wikipedia/%s (https://github.com/alice2-index/yaad-wikipedia; contact: alice2-index@alice2-index.invalid)",
			PluginVersion,
		),
		lang: DefaultLang,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// URLPattern is the regex yaad-wikipedia advertises for canonical
// `https://<lang>.wikipedia.org/wiki/<title>` URLs. alice2-index
// pre-compiles every plugin's url_patterns and dispatches in
// registration order; we keep the pattern conservative (anchored host
// + /wiki/ prefix) so we don't claim API or edit URLs.
const URLPattern = `^https?://[a-z]{2,3}(\.m)?\.wikipedia\.org/wiki/.+`

// ShorthandPattern is the regex yaad-wikipedia advertises for the
// shorthand input form `wikipedia: <topic>` (e.g. `wikipedia: Iran`).
// Case-insensitive on the prefix; allows but doesn't require
// whitespace after the colon. The captured topic must start with a
// non-whitespace character — guards against `wikipedia: ` matching
// and resolving to an empty title.
const ShorthandPattern = `(?i)^wikipedia:\s*(\S.*)$`

// shorthandRegex is ShorthandPattern compiled once for in-package use.
// The advertised pattern (ShorthandPattern) is what alice2-index
// dispatches against; this in-process copy lets Match and Fetch
// recognise the shape without a per-call regex compile.
var shorthandRegex = regexp.MustCompile(ShorthandPattern)

// Match returns true for any input this plugin can handle: a canonical
// Wikipedia URL OR the shorthand `wikipedia: <topic>` form. The
// function exists in addition to URLPattern + ShorthandPattern so
// callers (e.g. unit tests) can dispatch without the regex round-trip
// alice2-index does.
func (*Plugin) Match(input string) bool {
	if _, ok := matchShorthand(input); ok {
		return true
	}
	u, err := url.Parse(input)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, "wikipedia.org") {
		return false
	}
	return strings.HasPrefix(u.Path, "/wiki/")
}

// matchShorthand returns the topic captured from a shorthand input,
// or "", false if input is not the shorthand form.
func matchShorthand(input string) (topic string, ok bool) {
	m := shorthandRegex.FindStringSubmatch(input)
	if len(m) < 2 {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// resolveURL returns the canonical Wikipedia URL for input. A full
// `https://<lang>.wikipedia.org/wiki/...` URL is returned unchanged.
// The shorthand `wikipedia: <topic>` form is resolved against p.lang
// to `https://<lang>.wikipedia.org/wiki/<title>`. Spaces in the topic
// become underscores (Wikipedia's URL convention); non-ASCII bytes
// are percent-encoded; sub-delims like `(`/`)` stay literal so both
// shapes produce structurally identical canonical URLs (the URL-form
// input path preserves literal parens via url.URL.EscapedPath, so we
// match that here rather than letting url.PathEscape over-encode).
func (p *Plugin) resolveURL(input string) (string, error) {
	topic, ok := matchShorthand(input)
	if !ok {
		return input, nil
	}
	if topic == "" {
		return "", fmt.Errorf("%s: shorthand %q has empty topic", PluginName, input)
	}
	title := strings.ReplaceAll(topic, " ", "_")
	return fmt.Sprintf("https://%s.wikipedia.org/wiki/%s", p.lang, encodeWikipediaPathSegment(title)), nil
}

// encodeWikipediaPathSegment percent-encodes only what's strictly
// necessary for a Wikipedia URL path segment: ASCII alphanumerics,
// unreserved chars (`-._~`), and sub-delims (`!$&'()*+,;=`) plus `:`/
// `@` pass through; everything else (including UTF-8 non-ASCII bytes
// and reserved chars `?#%/`) is percent-encoded. Matches the literal-
// parens convention the URL-form input path already produces.
func encodeWikipediaPathSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '!', c == '$', c == '&', c == '\'', c == '(', c == ')',
			c == '*', c == '+', c == ',', c == ';', c == '=',
			c == ':', c == '@':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Fetch hits the Wikipedia REST summary API for the article identified
// by input, which can be either a full `https://...wikipedia.org/wiki/`
// URL or the shorthand `wikipedia: <topic>` form (resolved against
// p.lang). Returns ErrNotFoundUpstream on a 404; other non-2xx
// upstream statuses are surfaced as wrapped errors. Context
// cancellation is honoured.
//
// Returns *FetchOutcome with mutually-exclusive fields:
// - Article: standard fetched-entity case (structured fields,
// raw content, gaps, canonical entities).
// - Options: the URL resolved to a Wikipedia disambiguation page —
// the search-fallback path emits Options so the agent can pick
// a candidate (per ADR-0006). Caller emits `state: disambiguation`.
//
// Both shorthand and URL inputs go through the same URL fetch path:
// `resolveURL` builds the canonical URL form for shorthand, and the
// single disambiguation-page fallback (further down) handles the
// case where Wikipedia itself flags the page as ambiguous.
//
// Shorthand inputs do NOT run search-first — search
// heuristics intercepted canonical parens-form titles like
// `wikipedia: Martin Wallace (game designer)`, surfacing
// disambiguation Options for what was actually a fully-qualified
// title. The agent's natural URL-fallback then re-fetched anyway.
// Trusting Wikipedia's `summary.Type == "disambiguation"` signal is
// the cleaner shape: real ambiguity → real disambig, not search
// guess.
func (p *Plugin) Fetch(ctx context.Context, input string) (*FetchOutcome, error) {
	// resolveURL is a no-op for full URLs and a build-canonical-from-
	// shorthand for the shorthand form. After this call we have a
	// single URL shape to work with for the rest of Fetch.
	canonicalURL, err := p.resolveURL(input)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(canonicalURL)
	if err != nil {
		return nil, fmt.Errorf("%s: parse url: %w", PluginName, err)
	}

	// Title is the path segment after `/wiki/`. EscapedPath preserves
	// the URL-encoding the API expects.
	title := strings.TrimPrefix(u.EscapedPath(), "/wiki/")
	if title == "" {
		return nil, fmt.Errorf("%s: no article title in path %q", PluginName, u.Path)
	}

	apiURL := buildAPIURL(u.Host, p.apiHostOverride, title)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", PluginName, err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", PluginName, apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFoundUpstream
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a small slice of the body so the error message can
		// include upstream's diagnostic without unbounded memory.
		const peek = 512
		body, _ := io.ReadAll(io.LimitReader(resp.Body, peek))
		return nil, fmt.Errorf("%s: upstream returned %d: %s",
			PluginName, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var summary apiSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", PluginName, err)
	}
	if summary.Title == "" {
		return nil, fmt.Errorf("%s: response is missing `title`", PluginName)
	}

	now := time.Now().UTC()

	// Disambiguation-page fallback (per ADR-0006 +). When the
	// REST summary tells us this URL points at a disambiguation
	// page, run a search using the article's title and surface the
	// matches as Options — same code path as multi-match search
	// for shorthand inputs. Search-failure here is non-fatal;
	// empty Options is still better than materializing a bogus
	// article.
	if summary.Type == "disambiguation" {
		results, _ := p.searchArticles(ctx, u.Host, summary.Title)
		return &FetchOutcome{
			Options: searchResultsToOptions(results),
			Provenance: []ProvenanceEntry{
				{
					Source: "wikipedia:disambiguation",
					FetchedAt: now,
					OK: true,
				},
			},
		}, nil
	}

	// Per ADR-0021, plugins emit descriptive names + edge `{name,
	// kind}` references; the daemon's slug.Slug owns slug
	// derivation. Per (preserved rationale): the
	// post-redirect title from the REST summary is the source of
	// truth, so multiple equivalent input URLs that resolve to the
	// same article all converge on a single source-node ID at
	// daemon-derive time. The plugin name here keeps Wikipedia's
	// parens-disambig (round-trips to the article URL); the
	// canonical-edge target name strips the parens (per
	//, for cross-plugin dedup against e.g. BGG's
	// designed_by edge target).
	article := &Article{
		Name: summary.Title,
		Data: map[string]any{
			"title": summary.Title,
			"lang": summary.Lang,
			// data.url is the canonical resolved URL, not the
			// shorthand input — keeps the entity body shareable + the
			// `url` field shaped the same regardless of input form.
			"url": canonicalURL,
		},
		Provenance: []ProvenanceEntry{
			{
				Source: canonicalURL,
				FetchedAt: now,
				OK: true,
			},
		},
		Notations: notationsFor(input, canonicalURL, summary.Lang, summary.Title, title),
		// Aliases per alice2-index the source issue a prior PR — the article's
		// human-readable title is the primary wikilink target.
		// alice2-index merges this with its own ADR-0011-synthesized
		// alias and dedupes; emitting it here is cheap-redundant
		// but keeps the plugin-side intent explicit.
		Aliases: []string{summary.Title},
		// ADR-0021 universal source-type edge: every wikipedia-
		// emitted source node carries an `is_a` edge to
		// `source-type:wikipedia-article` (the daemon resolves the
		// label via slug.Slug).
		Edges: map[string][]EdgeTarget{
			SourceTypeEdgeType: {{Name: SourceTypeName, Kind: SourceTypeKind}},
		},
	}

	// Second upstream call: full plaintext article body for RawContent.
	// The summary endpoint above only returns a few-paragraph extract;
	// agent-fill needs the whole article to derive summary + tags. A
	// fetchExtract failure does NOT fail the whole Fetch — the entity
	// still lands without RawContent, the agent can re-ingest with
	// force_refetch later if needed.
	if extract, err := p.fetchExtract(ctx, u.Host, title); err == nil {
		article.RawContent = extract
	}

	// Third upstream call: Wikidata Q-id → canonical kind. Same
	// non-fatal pattern as fetchExtract — failure here just means
	// no canonical-edge gets emitted; source-shape article still
	// lands. Only fires when the summary returned a non-empty
	// wikibase_item (most articles have one).
	//
	// Stderr instrumentation per the source issue — three silent failure
	// modes (no wikibase_item, fetchKindByQID errored, no kind
	// matched). Stderr surfaces in `docker logs` via alice2-index's
	// subprocess wrapper, so each branch is observable without other
	// tooling. The success branch logs the chosen kind so a positive
	// trace also appears alongside negatives.
	if summary.WikibaseItem == "" {
		fmt.Fprintf(os.Stderr,
			"%s: canonical-axis: no wikibase_item on summary for %q; skipping kind detection\n",
			PluginName, summary.Title)
	} else {
		kind, err := p.fetchKindByQID(ctx, summary.WikibaseItem)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr,
				"%s: canonical-axis: fetchKindByQID(%s) for %q failed: %v\n",
				PluginName, summary.WikibaseItem, summary.Title, err)
		case kind == "":
			fmt.Fprintf(os.Stderr,
				"%s: canonical-axis: fetchKindByQID(%s) for %q found no matching kind in lookup table\n",
				PluginName, summary.WikibaseItem, summary.Title)
		default:
			// Canonical-edge target Name is stripped of Wikipedia's
			// parens-disambig per: the source
			// node's slug rounds-trips to the specific Wikipedia
			// article (URL stability), but the canonical
			// `person:martin-wallace` label needs to dedupe across
			// plugins (BGG emits `designed_by` to the same name).
			// Both sides feed daemon's slug.Slug; identical input
			// → identical canonical-label slug. When the title has
			// no trailing parens (e.g. "Susanna Clarke"),
			// stripTrailingParens is a no-op.
			canonicalName := stripTrailingParens(summary.Title)
			fmt.Fprintf(os.Stderr,
				"%s: canonical-axis: %q → %s:%q (via wikibase_item %s)\n",
				PluginName, summary.Title, kind, canonicalName, summary.WikibaseItem)
			article.Edges[CanonicalEdgeType] = []EdgeTarget{
				{Name: canonicalName, Kind: kind},
			}
			article.KindGaps = kindGaps(kind)
		}
	}

	return &FetchOutcome{Article: article}, nil
}

// fetchExtract calls MediaWiki's action API for the full plaintext
// body of an article. Returns the extract string on success; empty
// string + error on any failure (network, non-2xx, missing page,
// missing extract field). The Fetch caller treats the error as
// non-fatal: an article with no RawContent still flows through the
// rest of the pipeline; the agent's fill call can be re-issued with
// force_refetch when the action API is back up.
//
// The action API endpoint shape is:
//
//	GET https://<lang>.wikipedia.org/w/api.php
//	 ?action=query&format=json&formatversion=2
//	 &titles=<Title>&prop=extracts&explaintext=1
//
// Response:
//
//	{"query":{"pages":[{"pageid":N,"title":"...","extract":"<full body>"}]}}
//
// formatversion=2 makes pages an ordered array (vs a map keyed by
// stringified pageid in the v1 default), which is friendlier to
// decode without a stringly-keyed map.
func (p *Plugin) fetchExtract(ctx context.Context, requestHost, escapedTitle string) (string, error) {
	apiURL := buildActionAPIURL(requestHost, p.apiHostOverride, escapedTitle)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("%s: build extract request: %w", PluginName, err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: fetch extract %s: %w", PluginName, apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s: extract upstream returned %d", PluginName, resp.StatusCode)
	}

	var doc actionExtractResponse
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("%s: decode extract response: %w", PluginName, err)
	}
	if len(doc.Query.Pages) == 0 {
		return "", fmt.Errorf("%s: extract response has no pages", PluginName)
	}
	return mediaWikiHeadingsToMarkdown(doc.Query.Pages[0].Extract), nil
}

// searchArticles queries Wikipedia's action API for candidate
// articles matching the input query. Used as the disambiguation
// surface (per ADR-0006 +): when a query is ambiguous, search
// returns multiple matches and we surface them as Options for the
// agent to pick.
//
//	GET /w/api.php?action=query&list=search&format=json
//	 &formatversion=2&srlimit=10&srsearch=<query>
//
// Response shape:
//
//	{"query":{"search":[{"title":"...","snippet":"...","pageid":N}, ...]}}
//
// Returns up to 10 results. Empty result list is a non-error
// outcome — caller treats as "no candidates" + surfaces empty
// Options (degenerate but acceptable; agent re-tries with a more
// specific query). Network / non-2xx / decode failures propagate
// as errors — search is on the primary ingest path now, not
// best-effort like fetchExtract.
func (p *Plugin) searchArticles(ctx context.Context, requestHost, query string) ([]searchResult, error) {
	if query == "" {
		return nil, nil
	}
	apiURL := buildSearchAPIURL(requestHost, p.apiHostOverride, p.lang, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build search request: %w", PluginName, err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: fetch search %s: %w", PluginName, apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: search upstream returned %d", PluginName, resp.StatusCode)
	}

	var doc actionSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: decode search response: %w", PluginName, err)
	}
	return doc.Query.Search, nil
}

// buildSearchAPIURL composes the action API search URL with the
// query in a query-encoded `srsearch` parameter (path-shape /
// query-shape rules differ per the bug the cold-reviewer caught on a prior PR; we
// use url.QueryEscape directly here since the input is already a
// plain string, not a path-encoded title).
//
// langFallback is used when neither requestHost nor apiHostOverride
// names a host — i.e. shorthand searches with no per-request URL
// host hint. Caller passes p.lang.
func buildSearchAPIURL(requestHost, apiHostOverride, langFallback, query string) string {
	const path = "/w/api.php?action=query&format=json&formatversion=2&list=search&srlimit=10&srsearch="
	encoded := url.QueryEscape(query)
	if strings.HasPrefix(apiHostOverride, "http://") || strings.HasPrefix(apiHostOverride, "https://") {
		return strings.TrimRight(apiHostOverride, "/") + path + encoded
	}
	host := requestHost
	if apiHostOverride != "" {
		host = apiHostOverride
	}
	if host == "" {
		host = langFallback + ".wikipedia.org"
	}
	return "https://" + host + path + encoded
}

// actionSearchResponse mirrors the shape of the action API
// `?list=search` response we read.
type actionSearchResponse struct {
	Query struct {
		Search []searchResult `json:"search"`
	} `json:"query"`
}

// searchResult is one result row from the action search API.
type searchResult struct {
	Title string `json:"title"`
	Snippet string `json:"snippet"`
	PageID int `json:"pageid"`
}

// searchResultsToOptions converts search hits into DisambiguationOption
// entries. The option ID is the article title (Wikipedia titles like
// "Martin Wallace (game designer)" round-trip through the shorthand
// resolver — `wikipedia: Martin Wallace (game designer)` → URL with
// underscores + parens preserved). Label = title; Summary = snippet
// stripped of HTML tags (snippets carry `<span class="searchmatch">`
// emphasis markup).
func searchResultsToOptions(results []searchResult) []DisambiguationOption {
	options := make([]DisambiguationOption, 0, len(results))
	for _, r := range results {
		options = append(options, DisambiguationOption{
			ID: r.Title,
			Label: r.Title,
			Summary: stripSnippetMarkup(r.Snippet),
		})
	}
	return options
}

// stripSnippetMarkup removes the `<span class="searchmatch">...</span>`
// emphasis tags Wikipedia wraps around matched terms in search
// snippets. We don't surface HTML to agents — strip the tags + keep
// the inner text. Ad-hoc string-replace; HTML parsing would be
// heavy for this single tag pattern.
func stripSnippetMarkup(s string) string {
	s = strings.ReplaceAll(s, `<span class="searchmatch">`, "")
	s = strings.ReplaceAll(s, "</span>", "")
	return strings.TrimSpace(s)
}

// mwHeadingRe matches a single line containing a MediaWiki heading:
// leading run of `=`, one-or-more spaces, the title, one-or-more
// spaces, a trailing run of `=`, optional trailing whitespace. The
// opening and closing runs are NOT cross-checked for balance —
// MediaWiki emits them balanced in practice, and the looser shape
// keeps the rule from surprising on edge cases. (?m) flips ^/$ to
// per-line.
var mwHeadingRe = regexp.MustCompile(`(?m)^(=+) +(.+?) +=+\s*$`)

// mediaWikiHeadingsToMarkdown rewrites MediaWiki section markers in
// the action-API plaintext extract into markdown heading syntax —
// `== Foo ==` → `## Foo`. The action API's `explaintext=1` strips
// most wikitext (italics, links, templates) but leaves section
// markers in MediaWiki shape; markdown renderers like Obsidian show
// the literal `==` characters instead of formatted headings.
//
// Markdown caps at h6 (`######`); deeper MediaWiki nesting collapses
// to h6 rather than emitting non-heading text. Wikipedia in practice
// rarely goes past h4, so the cap is a defensive guard.
func mediaWikiHeadingsToMarkdown(s string) string {
	return mwHeadingRe.ReplaceAllStringFunc(s, func(line string) string {
		m := mwHeadingRe.FindStringSubmatch(line)
		if len(m) != 3 {
			return line
		}
		level := len(m[1])
		if level > 6 {
			level = 6
		}
		return strings.Repeat("#", level) + " " + m[2]
	})
}

// buildAPIURL composes the REST summary URL. apiHostOverride accepts
// either a bare host:port (used with HTTPS) or a full http://host:port
// base URL — the latter is what httptest.Server.URL gives us, which
// is why the override is parsed two ways.
func buildAPIURL(requestHost, apiHostOverride, escapedTitle string) string {
	if strings.HasPrefix(apiHostOverride, "http://") || strings.HasPrefix(apiHostOverride, "https://") {
		return strings.TrimRight(apiHostOverride, "/") + "/api/rest_v1/page/summary/" + escapedTitle
	}
	host := requestHost
	if apiHostOverride != "" {
		host = apiHostOverride
	}
	return "https://" + host + "/api/rest_v1/page/summary/" + escapedTitle
}

// buildActionAPIURL composes the MediaWiki action API URL for the
// plaintext-extract query. Same apiHostOverride two-shape parse as
// buildAPIURL. The action API lives at /w/api.php (different from
// the REST API's /api/rest_v1/), so tests targeting an httptest
// server need to handle both paths.
//
// escapedTitle arrives in path-encoded form (from u.EscapedPath +
// encodeWikipediaPathSegment); the action API expects the title in
// a query-string value, where the encoding rules differ — a literal
// `+` in a path is interpreted as space in a query string. We
// path-decode then query-encode so the round-trip matches what the
// upstream API expects regardless of which characters the title has
// (the cold-reviewer's catch on a prior PR).
func buildActionAPIURL(requestHost, apiHostOverride, escapedTitle string) string {
	decoded, err := url.PathUnescape(escapedTitle)
	if err != nil {
		decoded = escapedTitle
	}
	titleParam := url.QueryEscape(decoded)
	const query = "/w/api.php?action=query&format=json&formatversion=2&prop=extracts&explaintext=1&redirects=1&titles="
	if strings.HasPrefix(apiHostOverride, "http://") || strings.HasPrefix(apiHostOverride, "https://") {
		return strings.TrimRight(apiHostOverride, "/") + query + titleParam
	}
	host := requestHost
	if apiHostOverride != "" {
		host = apiHostOverride
	}
	return "https://" + host + query + titleParam
}

// apiSummary mirrors the subset of the Wikipedia REST summary response
// this plugin reads. The full response carries many more fields
// (thumbnails, dates, type tags, etc.); ignored fields are silently
// dropped by the decoder. We no longer surface `extract` from this
// path — the action API's full plaintext extract is what populates
// RawContent (the summary's few-paragraph extract is too short for
// agent-fill's summary/tag derivation).
//
// `wikibase_item` carries the Wikidata Q-id when the article has a
// linked Wikidata entity (most articles do). Used as the input to
// the canonical-kind lookup — present + non-empty triggers the
// fetchKindByQID call.
//
// `type` distinguishes regular articles ("standard" / absent) from
// disambiguation pages ("disambiguation" — per ADR-0006, ambiguous
// titles surface candidates for the agent to pick rather than
// materializing as an article).
type apiSummary struct {
	Title string `json:"title"`
	Lang string `json:"lang"`
	WikibaseItem string `json:"wikibase_item,omitempty"`
	Type string `json:"type,omitempty"`
}

// actionExtractResponse mirrors the MediaWiki action API's
// `?action=query&prop=extracts&format=json&formatversion=2` reply
// shape. formatversion=2 makes Pages an ordered array — easier to
// decode than v1's stringly-keyed map indexed by pageid.
type actionExtractResponse struct {
	Query struct {
		Pages []struct {
			PageID int `json:"pageid"`
			Title string `json:"title"`
			Extract string `json:"extract"`
		} `json:"pages"`
	} `json:"query"`
}

// notationsFor returns every input form yaad-wikipedia knows resolves
// to a given article — for the orchestrator's entity_notations cache
// (per alice2-index the source issue a prior PR). The list always includes the
// originating notation (in whatever shape the caller passed) so a
// self-roundtrip with the same input registers a hit on the next
// call. Duplicates are deduped while order is preserved (input
// notation first).
//
// Args:
//
// - input — the raw notation that triggered this Fetch.
// - canonicalURL — the resolved canonical desktop URL (post
// resolveURL/search-disambig).
// - lang — the article's wiki language code (from
// summary.Lang). Used to derive the mobile-subdomain URL form.
// - humanTitle — the human-readable title (from summary.Title,
// e.g. `"Susanna Clarke"`). Drives the shorthand form.
// - escapedTitle — the URL-encoded title segment (post path
// extract). Used to build the desktop/mobile URL forms when the
// canonicalURL doesn't already supply one.
func notationsFor(input, canonicalURL, lang, humanTitle, escapedTitle string) []string {
	if lang == "" {
		lang = DefaultLang
	}
	out := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(n string) {
		if n == "" {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}

	// 1. Originating input — always first so the lookup-first cache
	// registers a hit on a self-roundtrip with the exact same form.
	add(input)

	// 2. Canonical desktop URL.
	add(canonicalURL)

	// 3. Mobile subdomain URL — `<lang>.m.wikipedia.org`. Reuses the
	// URL-encoded title segment so percent-encoding survives.
	if escapedTitle != "" {
		add(fmt.Sprintf("https://%s.m.wikipedia.org/wiki/%s", lang, escapedTitle))
	}

	// 4. Shorthand `wikipedia: <human-title>`. The human title is
	// space-separated (post URL-decode), matching what an operator
	// typing `wikipedia: Susanna Clarke` would pass.
	if humanTitle != "" {
		add(fmt.Sprintf("wikipedia: %s", humanTitle))
	}
	return out
}

// trailingParensRE matches Wikipedia's disambiguation-parens
// suffix on a human-readable title — `(game designer)`,
// `(board game)`, `(2018 film)`, etc. Used by stripTrailingParens
// to compute the CANONICAL-side slug per; the
// source-side slug keeps the parens-derived chars (since the
// Wikipedia URL round-trips them).
var trailingParensRE = regexp.MustCompile(`\s*\([^()]+\)\s*$`)

// stripTrailingParens removes Wikipedia's disambiguation-parens
// suffix from a human-readable title. Used only when computing the
// canonical-entity-ID slug per:
//
//	Martin Wallace (game designer) → Martin Wallace
//	Brass (board game) → Brass
//	London (board game) → London
//	Susanna Clarke → Susanna Clarke (no-op)
//
// Non-trailing parens stay (the regex is anchored at end-of-string),
// so titles like `Foo (Bar) Baz` keep `(Bar)` — Wikipedia's
// disambig convention is end-only. Multiple trailing parens (rare;
// observed on a handful of historical-figure articles) are stripped
// one-pass-at-a-time: the regex's first match is removed and the
// remaining tail isn't re-stripped. v1 trade-off; the
// double-disambig case is uncommon enough that a single strip wins
// the dedup case it covers without overcomplicating the regex.
func stripTrailingParens(title string) string {
	return strings.TrimSpace(trailingParensRE.ReplaceAllString(title, ""))
}
