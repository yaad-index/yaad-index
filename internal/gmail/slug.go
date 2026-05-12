package gmail

import "strings"

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
func SourceSlug(subject, rawMessageID string) string {
	subj := slugifyClean(subject)
	mid := MessageIDSlug(rawMessageID)
	switch {
	case subj == "" && mid == "":
		return ""
	case subj == "":
		return mid
	case mid == "":
		return subj
	default:
		return subj + "-" + mid
	}
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
