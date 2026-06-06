package wikipedia

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// kindByQID maps Wikidata "instance of" (P31) Q-ids to canonical
// entity kinds yaad-wikipedia emits. The map is plugin-authored
// from Wikidata's P31 hierarchy and ships comprehensive coverage
// — adding more Q-ids here is encouraged. The operator's
// `canonical_kinds:` config gate filters at the daemon's
// thin-row materialize step: only kinds the operator enabled
// produce canonical-label rows, so a richer plugin map widens
// the menu without forcing every operator to consume every kind.
//
// The Q-id is the language-agnostic + deterministic signal: each
// Wikipedia article has at most one wikibase_item, and Wikidata's
// P31 is the closest thing to a typed identity claim. Picking
// from this allowlist (vs. consuming every P31 value) keeps the
// surface bounded — the operator's gate then narrows further per
// their interests.
//
// Multi-Q-id consolidation: several Q-ids can map to the same
// canonical kind (e.g. Q3624078 "sovereign state" → country).
// The dedup happens implicitly via the map's value set
// (KnownCanonicalKinds below).
//
// Adding a Q-id = add a row here; the value lands in
// KnownCanonicalKinds via the deduper. Kind-specific gap prompts
// now live in the daemon's canonical-kind registry — the
// operator's `canonical_kinds:` config supplies the
// per-gap Description. Universal gaps (summary + tags) ride
// regardless of kind detection.
var kindByQID = map[string]string{
	// People (existing).
	"Q5": "person",

	// Places (existing).
	"Q515": "city",
	"Q6256": "country",

	// Written / printed (existing + comic).
	"Q571": "book",
	"Q1004": "comic",

	// Film / TV / animation.
	"Q11424": "movie",
	"Q24856": "film-series",
	"Q5398426": "tv-show",
	"Q1107": "anime",

	// Audio.
	"Q482994": "album",
	"Q24634210": "podcast",

	// Games.
	"Q7889": "video-game",
	"Q131436": "boardgame",

	// Visual art.
	"Q838948": "artwork",

	// Organizations.
	"Q43229": "organization",
	"Q4830453": "business",
	"Q3914": "school",

	// Software.
	"Q7397": "software",
}

// KnownCanonicalKinds is the set of canonical kinds yaad-wikipedia
// declares it MAY emit, surfaced through `Capabilities.
// CanonicalKindsEmitted` per ADR-0008. yaad-index startup warns
// operators when a plugin declares a canonical kind they haven't
// enabled in their `canonical_kinds:` config, so this list shapes
// the discovery message.
//
// The list deduplicates kindByQID's values; ordered alphabetically
// for stable diffs when entries land. The 18 entries below match
// the verified mapping table from.
var KnownCanonicalKinds = []string{
	"album",
	"anime",
	"artwork",
	"boardgame",
	"book",
	"business",
	"city",
	"comic",
	"country",
	"film-series",
	"movie",
	"organization",
	"person",
	"podcast",
	"school",
	"software",
	"tv-show",
	"video-game",
}

// CanonicalEdgeType is the canonical-edge type yaad-wikipedia emits
// from a source-shape Wikipedia article to its inferred canonical
// label. Surfaced through `Capabilities.CanonicalEdgeTypesEmitted`.
const CanonicalEdgeType = "is_about"

// SourceTypeEdgeType is the universal source-type edge yaad-
// wikipedia always emits per ADR-0021: from the source node to
// `source-type:wikipedia-article`. Surfaces source-type info as
// a label-edge (rather than as the source node's own kind);
// pairs with `Capabilities.CanonicalEdgeTypesEmitted` so the
// operator can gate it through the standard edge-type config.
const SourceTypeEdgeType = "is_a"

// SourceTypeName is the descriptive name of yaad-wikipedia's
// source-type label — the target of the universal `is_a` edge.
// Daemon's slug.Slug derives the canonical-label slug
// (`source-type:wikipedia-article`).
const SourceTypeName = "wikipedia-article"

// SourceTypeKind is the system-reserved canonical-kind for
// source-type labels per ADR-0021. Bypasses the operator's
// canonical_kinds gate at the daemon's thin-row materialize
// step.
const SourceTypeKind = "source-type"

// EdgeTarget is one entry in the ADR-0021 source-shape edges
// block — a descriptive `{name, kind}` reference. Daemon
// resolves to a canonical-label endpoint via slug.Slug.
type EdgeTarget struct {
	Name string
	Kind string
}

// fetchKindByQID resolves a Wikidata Q-id to a canonical kind via
// the EntityData JSON endpoint:
//
//	GET https://www.wikidata.org/wiki/Special:EntityData/<Qid>.json
//
// Reads `entities.<Qid>.claims.P31[*].mainsnak.datavalue.value.id`
// and matches against kindByQID. Returns the matched kind on the
// first hit; "" + nil error if no claim matches a known kind.
//
// All failure paths (network, non-2xx, decode, missing claim
// structure) return ("", err) — the caller treats any err as
// non-fatal and emits no canonical stub. ADR-0008 partial-
// degradation: the source-shape Wikipedia article still lands.
func (p *Plugin) fetchKindByQID(ctx context.Context, qid string) (string, error) {
	if qid == "" {
		return "", nil
	}
	apiURL := buildWikidataEntityURL(p.wikidataHostOverride, qid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("%s: build wikidata request: %w", PluginName, err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: fetch wikidata %s: %w", PluginName, apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s: wikidata upstream returned %d", PluginName, resp.StatusCode)
	}

	var doc wikidataEntityResponse
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("%s: decode wikidata response: %w", PluginName, err)
	}

	entity, ok := doc.Entities[qid]
	if !ok {
		return "", nil
	}
	p31Raw, ok := entity.Claims["P31"]
	if !ok {
		return "", nil
	}
	// Decode only P31 claims into the strict entity-id-shape struct.
	// Other Wikidata properties on the same entity (P18 image filename,
	// P569 birth-date time-object, P625 globe coordinates, etc.) carry
	// foreign datavalue shapes that would crash a single-pass decode of
	// the entire claims map. Keeping the outer claims as
	// json.RawMessage isolates the strict struct to the one path
	// fetchKindByQID actually walks.
	var p31Claims []wikidataClaim
	if err := json.Unmarshal(p31Raw, &p31Claims); err != nil {
		return "", fmt.Errorf("%s: decode P31 claims: %w", PluginName, err)
	}
	for _, claim := range p31Claims {
		matchedQID := claim.Mainsnak.DataValue.Value.ID
		if kind, ok := kindByQID[matchedQID]; ok {
			return kind, nil
		}
	}
	return "", nil
}

// buildWikidataEntityURL composes the Wikidata EntityData URL.
// hostOverride accepts either a bare host or full http(s) base URL,
// mirroring buildAPIURL's two-shape parse so tests can point at an
// httptest.Server instead of wikidata.org.
func buildWikidataEntityURL(hostOverride, qid string) string {
	const path = "/wiki/Special:EntityData/"
	if strings.HasPrefix(hostOverride, "http://") || strings.HasPrefix(hostOverride, "https://") {
		return strings.TrimRight(hostOverride, "/") + path + qid + ".json"
	}
	host := "www.wikidata.org"
	if hostOverride != "" {
		host = hostOverride
	}
	return "https://" + host + path + qid + ".json"
}

// wikidataEntityResponse mirrors the subset of the EntityData JSON
// response this plugin reads — Q-id-keyed map of entities, each
// carrying P31 instance-of claims. Many other claim properties
// (P19 birthplace, P569 birth-date, P17 country, etc.) live in the
// same shape and could be read by future PRs for kind-specific
// data extraction.
type wikidataEntityResponse struct {
	Entities map[string]wikidataEntity `json:"entities"`
}

// wikidataEntity carries claims as a property-name → opaque-bytes
// map; only P31 is unmarshaled into the strict claim shape inside
// fetchKindByQID. Other properties stay opaque so their foreign
// datavalue shapes (string for P18, time-object for P569, etc.)
// don't crash the decoder.
type wikidataEntity struct {
	Claims map[string]json.RawMessage `json:"claims"`
}

type wikidataClaim struct {
	Mainsnak wikidataSnak `json:"mainsnak"`
}

type wikidataSnak struct {
	DataValue wikidataDataValue `json:"datavalue"`
}

type wikidataDataValue struct {
	Value wikidataValue `json:"value"`
}

type wikidataValue struct {
	ID string `json:"id"`
}
