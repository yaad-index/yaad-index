// Cache TTL resolution per alice2-index (three-level hierarchy).
//
// resolveCacheTTL walks levels {entry-input > plugin-Capabilities >
// global-config} and returns the first non-zero value found. Each
// level uses identical sentinel rules:
//
// - 0 (or nil at the entry level) — no opinion; fall through.
// - positive N — expire after N seconds.
// - negative — never expire (cache forever).
//
// All-zero across all three levels resolves to 0, which the
// orchestrator interprets as "don't stamp the entity's
// CacheTTLSeconds field" — vault frontmatter omits the key, lookup
// treats absent-or-zero as cache-hits-forever (preserves legacy
// behavior where the global config alone determined freshness and
// `0` meant "infinite").
//
// Resolution runs ONCE at ingest time per the operator's 2026-05-06
// clarification: the resolved value is baked into vault frontmatter
// (`cache_ttl_seconds:`); the lookup path just compares
// `provenance[last].fetched_at + frontmatter.CacheTTLSeconds` vs
// now without re-walking. Operator hand-edits to the frontmatter
// slot do NOT persist across re-ingests — the next ingest cycle
// re-runs resolveCacheTTL from the live entry/plugin/global inputs
// and overwrites with the fresh value.

package api

import (
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// resolveCacheExpires runs the three-level resolution chain (entry,
// plugin, global) and translates the resulting duration into the
// absolute-date `cache_expires:` stamp per alice2-index.
//
// `fetchedAt` is the new ingest's freshest provenance.fetched_at —
// the anchor for the absolute expiry date. The returned pointer is:
//
// - nil — no opinion at any level (caller leaves the frontmatter
// field absent).
// - {Never: true} — negative TTL at some level (caller stamps the
// literal "never" sentinel in frontmatter).
// - {Time: fetchedAt + ttl} — positive TTL; caller stamps the
// absolute date string. Time is rendered via clock.In so the
// ISO output carries the operator-TZ offset .
//
// resolveCacheTTL's sentinel rules are unchanged from; this
// helper just wraps it with the date-translation step needs.
func resolveCacheExpires(entry *int, plugin, global int, fetchedAt time.Time) *vault.CacheExpires {
	ttl := resolveCacheTTL(entry, plugin, global)
	switch {
	case ttl == 0:
		return nil
	case ttl < 0:
		return vault.CacheExpiresNever()
	default:
		return vault.CacheExpiresAt(clock.In(fetchedAt.Add(time.Duration(ttl) * time.Second)))
	}
}

// resolveCacheTTL applies the three-level sentinel hierarchy.
// `entry` is a pointer so absent-vs-explicit-zero can both be
// distinguished from an explicit non-zero override (per
// FetchResult.CacheTTLSeconds wire shape). `plugin` and `global`
// are plain ints — their absent-vs-zero distinction is collapsed
// at the wire layer.
//
// Returns 0 when no level expressed an opinion; positive N for an
// N-second TTL; negative for "never expire". The orchestrator
// stamps the result into Entity.CacheTTLSeconds when it's non-zero
// and leaves the field nil (omits the frontmatter key) when it's 0.
func resolveCacheTTL(entry *int, plugin, global int) int {
	if entry != nil && *entry != 0 {
		return *entry
	}
	if plugin != 0 {
		return plugin
	}
	if global != 0 {
		return global
	}
	return 0
}
