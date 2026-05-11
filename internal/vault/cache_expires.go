// CacheExpires is the absolute-date cache freshness stamp per
// alice2-index. Replaces's `cache_ttl_seconds:` (duration-
// based, opaque to a human reading the vault file) with an
// absolute date that's glanceable: "good until 2027-05-03" beats
// "31536000 seconds after a timestamp I have to find."
//
// Wire shape (vault frontmatter):
//
// cache_expires: 2027-05-03T08:00:00+02:00 # full ISO with operator-TZ offset
// cache_expires: never # infinite (replaces's *=-1 sentinel)
//
// Field absent → no opinion (replaces's nil/zero sentinel).
//
// Kept as a separate type (rather than *time.Time) to encode the
// "never" sentinel without leaking the magic-time-value pattern
// (e.g. Time.Date(9999, 12, 31, ...)) into every comparison.

package vault

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// neverSentinel is the literal vault-frontmatter token meaning
// "this entity never expires." Recognized at unmarshal; emitted
// at marshal when CacheExpires.Never is set.
const neverSentinel = "never"

// CacheExpires is the cache-freshness stamp on a vault entity per
// alice2-index. Two shapes:
//
// - Never == true: entity never expires (always cache hit).
// - Never == false, !Time.IsZero(): entity expires AT the
// stamped instant.
//
// The zero value (`Never == false, Time.IsZero()`) is reserved for
// internal "uninitialized" use; callers should never persist it.
// Use a nil *CacheExpires to mean "no opinion" — frontmatter
// emits no `cache_expires:` key when the field is nil.
type CacheExpires struct {
	Never bool
	Time time.Time
}

// CacheExpiresNever returns a CacheExpires marked as the "never"
// sentinel. Helper to avoid struct-literal verbosity at call sites.
func CacheExpiresNever() *CacheExpires {
	return &CacheExpires{Never: true}
}

// CacheExpiresAt returns a CacheExpires stamped at t. Helper to
// avoid struct-literal verbosity at call sites.
func CacheExpiresAt(t time.Time) *CacheExpires {
	return &CacheExpires{Time: t}
}

// Expired returns true when the entity is past its cache expiry
// AT now. Never-sentinel entities always return false (never
// expired). Callers handling the "no opinion" case (nil pointer)
// must check before calling.
func (c *CacheExpires) Expired(now time.Time) bool {
	if c == nil {
		return false // documented contract: nil = "no opinion"; cache forever
	}
	if c.Never {
		return false
	}
	return now.After(c.Time)
}

// MarshalYAML emits the "never" sentinel or an RFC3339-formatted
// time string. Time values keep their *time.Location, so a stamp
// with operator-TZ Location prints with that offset (per alice2-
// index PR-B's clock.Now() flow).
func (c *CacheExpires) MarshalYAML() (any, error) {
	if c == nil {
		return nil, nil
	}
	if c.Never {
		return neverSentinel, nil
	}
	if c.Time.IsZero() {
		return nil, fmt.Errorf("cache_expires: zero-value Time without Never sentinel")
	}
	return c.Time.Format(time.RFC3339), nil
}

// UnmarshalYAML accepts the "never" sentinel or an RFC3339 string
// (with or without Nano precision). Bad shapes fail loudly so a
// hand-edit typo doesn't silently degrade to "no opinion."
func (c *CacheExpires) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("cache_expires: expected scalar, got kind %d", node.Kind)
	}
	if node.Value == neverSentinel {
		c.Never = true
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, node.Value); err == nil {
		c.Time = t
		return nil
	}
	t, err := time.Parse(time.RFC3339, node.Value)
	if err != nil {
		return fmt.Errorf("cache_expires: %q is neither %q nor a valid RFC3339 timestamp: %w",
			node.Value, neverSentinel, err)
	}
	c.Time = t
	return nil
}
