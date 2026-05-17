package gmail

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// Slug-length budget so the composed `<source_namespace>/<slug>.md`
// vault path + write-as-you-go `.tmp-NNNNNNNNNN` suffix stays under
// the 255-byte filesystem name limit per #146:
//
//   - ext4 / xfs / apfs cap individual path components at 255 bytes.
//   - The vault writer appends `.md.tmp-NNNNNNNNNN` (18 chars) when
//     staging an atomic temp file.
//   - The daemon's source-namespace prefix (`gmail`) is added as a
//     directory component (not part of the slug-length budget here)
//     but the leading dot for the dotfile + extension does eat into
//     the byte budget on rename.
//
// 200 bytes for the bare slug leaves ~55 bytes for `.md.tmp-NNNN...`
// + safety margin. Within that, individual components are capped so
// the message-id (the identity-bearing piece) is never sacrificed
// for a long subject.
const (
	// sourceSlugCap is the hard ceiling on the composed
	// `<subject-slug>-<message-id-slug>` length. Anything over
	// triggers the hash-on-overflow tail.
	sourceSlugCap = 200

	// sourceSubjectCap caps the subject slug before composition
	// so a 500-char encoded MIME subject doesn't push past the
	// ceiling on its own. 120 chars preserves enough subject for
	// the slug to remain operator-readable in the typical case.
	sourceSubjectCap = 120

	// sourceMessageIDCap caps the message-id slug. RFC-822
	// Message-IDs are bounded in practice but github
	// notification envelopes can carry 100+ char ids. 80 chars
	// is the practical ceiling that still keeps the
	// identity-bearing tail unique.
	sourceMessageIDCap = 80

	// sourceHashTailLen is the length of the SHA-1-hex tail
	// appended when the composed slug still exceeds the cap
	// after per-component truncation (rare; degenerate
	// subjects with no whitespace). 10 hex chars = 40 bits =
	// ~4.6e-7 collision probability across 1M emails — fine
	// for the rare path.
	sourceHashTailLen = 10
)

// slugifyClean is the daemon's clean-slug rule applied locally to
// header-derived strings (subject, label, address-local-parts, etc.).
// Lowercases ASCII, replaces any non-[a-z0-9] character with a single
// hyphen, collapses adjacent hyphens, trims leading/trailing hyphens.
// The output is always safe to embed in a `<kind>:<slug>` canonical
// id without further escaping.
//
// Intentionally string-input, string-output: callers compose the
// final id by joining slugified pieces with `-`. Empty input returns
// the empty string — caller's responsibility to provide a fallback.
func slugifyClean(in string) string {
	in = strings.ToLower(in)
	var b strings.Builder
	b.Grow(len(in))
	prevDash := true // suppress leading hyphens
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	return strings.TrimRight(out, "-")
}

// MessageIDSlug strips angle brackets from an RFC-822 Message-ID
// header value and clean-slugifies the inner identifier. Used to
// build both the source slug suffix and the `email` canonical
// kind's id.
//
// Empty / unparsable inputs return the empty string; callers that
// require non-empty fall back via their own sentinel logic.
func MessageIDSlug(rawMessageID string) string {
	v := strings.TrimSpace(rawMessageID)
	v = strings.TrimPrefix(v, "<")
	v = strings.TrimSuffix(v, ">")
	return slugifyClean(v)
}

// SourceSlug builds `<subject-slug>-<message-id-slug>` for the
// gmail source-shape entity id. Both pieces use the daemon's
// clean-slug rule. When subject is empty (no Subject header), the
// shape collapses to `<message-id-slug>` alone.
//
// Length-capping per #146: long encoded-MIME subjects + long
// notification message-ids can compose to >255 bytes, which the
// vault writer's `<slug>.md.tmp-NNNNNNNNNN` atomic-temp pattern
// then can't write (FS name limit). The fix caps each component
// before composition (subject ≤ 120, message-id ≤ 80) and falls
// back to a stable SHA-1-hex tail when the composed slug still
// exceeds the overall cap (degenerate subjects without
// whitespace). The hash input is the raw (subject, rawMessageID)
// pair so the same email always produces the same slug across
// runs — important for the daemon's slug-as-idempotency-key
// contract.
func SourceSlug(subject, rawMessageID string) string {
	subj := capSlug(slugifyClean(subject), sourceSubjectCap)
	mid := capSlug(MessageIDSlug(rawMessageID), sourceMessageIDCap)
	var composed string
	switch {
	case subj == "" && mid == "":
		return ""
	case subj == "":
		composed = mid
	case mid == "":
		composed = subj
	default:
		composed = subj + "-" + mid
	}
	if len(composed) <= sourceSlugCap {
		return composed
	}
	// Pathological case: the cap-applied composition still
	// exceeds the ceiling (degenerate subject without
	// hyphenable whitespace). Truncate + append a deterministic
	// hash of the raw input so the slug stays under the cap
	// AND the same email re-fetches to the same id.
	tail := hashTail(subject, rawMessageID)
	keep := sourceSlugCap - sourceHashTailLen - 1 // -1 for the joining `-`
	if keep < 1 {
		keep = 1
	}
	if keep > len(composed) {
		keep = len(composed)
	}
	return strings.TrimRight(composed[:keep], "-") + "-" + tail
}

// capSlug truncates s to at most max bytes, trimming any
// trailing hyphen that the cut produced so the result remains
// a well-formed clean-slug. The input must already be
// clean-slugged (no multi-byte runes; ASCII only).
func capSlug(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimRight(s[:max], "-")
}

// hashTail returns a short hex tail derived from the raw
// (subject, rawMessageID) input — used as the
// hash-on-overflow disambiguator in SourceSlug. SHA-1 is fine
// here: this is a uniqueness tag, not a security primitive.
func hashTail(subject, rawMessageID string) string {
	sum := sha1.Sum([]byte(subject + "\x00" + rawMessageID))
	return hex.EncodeToString(sum[:])[:sourceHashTailLen]
}

// EmailCanonicalSlug builds `gmail-<message-id-slug>` for the
// `email` canonical kind. Provider-prefix + sanitized RFC-822
// Message-ID per the spec — provider-prefix lets a future yaad-fastmail
// or yaad-outlook plugin emit non-colliding `email:` ids on the
// same canonical-graph axis.
func EmailCanonicalSlug(rawMessageID string) string {
	mid := MessageIDSlug(rawMessageID)
	if mid == "" {
		return ""
	}
	return "gmail-" + mid
}

// EmailAddressSlug encodes an email address into a canonical-label
// slug per the spec: lowercase, `@` → `_at_`, `.` → `_dot_`. Other
// special characters survive `slugifyClean`'s normalization on the
// local-part + domain after the explicit `_at_`/`_dot_`
// substitution.
//
// Encoding chosen so the slug round-trips back to the address
// unambiguously by reversing the substitutions; collisions between
// distinct addresses are avoided by the explicit `_at_`/`_dot_`
// markers.
func EmailAddressSlug(address string) string {
	a := strings.ToLower(strings.TrimSpace(address))
	if a == "" {
		return ""
	}
	a = strings.ReplaceAll(a, "@", "_at_")
	a = strings.ReplaceAll(a, ".", "_dot_")
	// Run clean-slug on each underscore-bounded segment so non-
	// ASCII / special characters in the local part don't leak —
	// preserves the `_at_` / `_dot_` markers.
	parts := strings.Split(a, "_")
	for i, p := range parts {
		parts[i] = slugifyClean(p)
	}
	return strings.Join(parts, "_")
}

// LabelSlug encodes a Gmail label name into a canonical-label slug
// per the spec: lowercase, `/` → `_slash_` (preserves hierarchy
// boundary), other characters via clean-slug (whitespace + special
// chars collapse to `-`). Distinct from `EmailAddressSlug` because
// labels carry hierarchy (`Job Search/Active`) and the
// hierarchy-preserving marker (`_slash_`) must visually distinguish
// from word-boundary hyphens.
//
// Examples:
//
// - `Job Search/Active` → `job-search_slash_active`
// - `Job Search Active` → `job-search-active` (distinct from the nested form above)
// - `INBOX` → `inbox`
// - `Personal/Family` → `personal_slash_family`
func LabelSlug(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" {
		return ""
	}
	// Substitute the hierarchy marker BEFORE the clean-slug pass
	// so it survives as `_slash_` rather than being collapsed to a
	// hyphen by the non-alphanumeric rule.
	l = strings.ReplaceAll(l, "/", "_slash_")
	parts := strings.Split(l, "_")
	for i, p := range parts {
		parts[i] = slugifyClean(p)
	}
	return strings.Join(parts, "_")
}
