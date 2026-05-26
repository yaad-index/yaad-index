package bgg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/fzerorubigd/bggo"
)

// collectionEndpointForProvenance is the operator-visible source
// label stamped on the second ProvenanceEntry when collection
// enrichment runs. The actual URL bggo hits carries the api key +
// session cookie via headers; this string is for the vault
// provenance trail.
const collectionEndpointForProvenance = "https://boardgamegeek.com/xmlapi2/collection?showprivate=1"

// fetchCollectionEntry runs the per-game authenticated collection
// fetch for #282. Returns:
//
//   - entry != nil — the game is in the operator's collection;
//     caller merges operator_* fields onto bg.Data.
//   - entry == nil, err == nil — the game is NOT in the operator's
//     collection (BGG returned <items totalitems="0">); caller
//     emits the /thing result unchanged.
//   - entry == nil, err != nil — enrichment failed (auth, network,
//     unexpected payload, etc.); caller WARNs + falls back to
//     /thing-only.
//
// Lazy-login: if no session cookie is loaded (Cookies() empty),
// performs Login first + persists the cookie jar to disk before
// the GetCollection call. On a mid-session 401 (server-side
// session invalidation while client-side cookie Expires is
// future), re-logins once + retries the GetCollection per the
// #282 acceptance criteria.
//
// The provenance source string is non-empty exactly when a
// collection round-trip was actually attempted (lazy-login may
// fail before that point, in which case the caller doesn't add a
// provenance entry for an endpoint that never got hit).
func (p *Plugin) fetchCollectionEntry(ctx context.Context, id int64) (*bggo.CollectionItem, string, error) {
	if !p.enrichmentEnabled() {
		return nil, "", nil
	}

	// Lazy login. If the cookie jar restored a session for the
	// configured username, skip; otherwise login + persist.
	if p.enrichmentSession != p.username {
		if err := p.loginAndPersist(ctx); err != nil {
			return nil, "", fmt.Errorf("lazy login: %w", err)
		}
	}

	req := bggo.GetCollectionRequest{
		Username:    p.username,
		IDs:         []int64{id},
		Stats:       true,
		ShowPrivate: true,
	}
	items, err := p.client.GetCollection(ctx, req)
	if isUnauthorized(err) {
		// Mid-session 401: server-invalidated the cookie even
		// though client-side Expires is future. Re-login once +
		// retry once per #282 acceptance.
		p.warn("bgg: collection fetch returned 401 (session invalidated mid-flight); re-authenticating once")
		if relogErr := p.loginAndPersist(ctx); relogErr != nil {
			return nil, collectionEndpointForProvenance, fmt.Errorf("re-login after 401: %w", relogErr)
		}
		items, err = p.client.GetCollection(ctx, req)
	}
	if err != nil {
		return nil, collectionEndpointForProvenance, err
	}
	if len(items) == 0 {
		// Game not in operator's collection — /thing result
		// stands unchanged. Provenance still records the
		// collection round-trip happened.
		return nil, collectionEndpointForProvenance, nil
	}
	return &items[0], collectionEndpointForProvenance, nil
}

// loginAndPersist runs bggo.Client.Login + saves the resulting
// cookie jar to disk so subsequent subprocess invocations skip
// the round-trip. A persist write failure is non-fatal — the
// session is still usable for the current subprocess lifetime,
// the next subprocess just re-logs in.
//
// Bad credentials at this site MUST fall through to /thing-only
// (per #282 acceptance) — the caller wraps the returned error
// with a WARN so the operator sees the auth failure without the
// daemon crashing.
func (p *Plugin) loginAndPersist(ctx context.Context) error {
	if err := p.client.Login(ctx, bggo.LoginRequest{
		Username: p.username,
		Password: p.password,
	}); err != nil {
		return err
	}
	p.enrichmentSession = p.client.Username()
	if persistErr := saveCookieJar(p.dataDir, p.client.Username(), p.client.Cookies()); persistErr != nil {
		p.warn("bgg: cookie jar persist failed (session usable for this dispatch but next subprocess will re-login): %v", persistErr)
	}
	return nil
}

// isUnauthorized reports whether err carries a 401 status code
// per the bggo v0.2.1 typed-error contract. Wraps `errors.As`
// against *bggo.HTTPStatusError; non-HTTP errors (network,
// context, etc.) return false.
func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *bggo.HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == http.StatusUnauthorized
}

// mergeOperatorFields stamps the operator-collection fields onto
// the entity.data map per #282 acceptance:
//
//   - operator_status — flat list of true status flags
//   - operator_rating — int 1-10 (or absent when unrated)
//   - operator_num_plays — int (absent when 0)
//   - operator_comment — string (absent when empty)
//   - operator_price_paid + operator_price_currency
//   - operator_acquisition_date / acquired_from / inventory_location
//   - operator_private_comment
//
// Absent fields are NOT written as nulls — the daemon's
// frontmatter writer skips keys not in the map, which is the
// cleaner shape (a frontmatter full of explicit-null operator_*
// fields would be visual noise in the markdown vault).
func mergeOperatorFields(data map[string]any, item *bggo.CollectionItem) {
	if item == nil {
		return
	}
	if len(item.Status) > 0 {
		flags := make([]string, 0, len(item.Status))
		for _, s := range item.Status {
			flags = append(flags, string(s))
		}
		data["operator_status"] = flags
	}
	if item.Rating != nil {
		// Per #282 acceptance: operator_rating is int 1-10.
		// BGG sends decimals; round to the nearest int.
		// math.Round handles the half-point case (8.5 → 9).
		data["operator_rating"] = int(math.Round(*item.Rating))
	}
	if item.NumPlays > 0 {
		data["operator_num_plays"] = item.NumPlays
	}
	if item.Comment != "" {
		data["operator_comment"] = item.Comment
	}
	if item.PrivateInfo != nil {
		if item.PrivateInfo.PricePaid != "" {
			data["operator_price_paid"] = item.PrivateInfo.PricePaid
		}
		if item.PrivateInfo.PriceCurrency != "" {
			data["operator_price_currency"] = item.PrivateInfo.PriceCurrency
		}
		if item.PrivateInfo.AcquisitionDate != "" {
			data["operator_acquisition_date"] = item.PrivateInfo.AcquisitionDate
		}
		if item.PrivateInfo.AcquiredFrom != "" {
			data["operator_acquired_from"] = item.PrivateInfo.AcquiredFrom
		}
		if item.PrivateInfo.InventoryLocation != "" {
			data["operator_inventory_location"] = item.PrivateInfo.InventoryLocation
		}
		if item.PrivateInfo.PrivateComment != "" {
			data["operator_private_comment"] = item.PrivateInfo.PrivateComment
		}
	}
}

