package gmail

import (
	"bufio"
	"bytes"
	"net/mail"
	"regexp"
	"strings"
)

// Forward detection (#323). Gmail's "Forward" button rewrites the
// envelope `From:` to the forwarder, masking the original sender — which
// breaks workflow predicates conditioned on `entity.data.from`. We surface
// the original sender (and the un-prefixed subject) as ADDITIVE
// `forwarded_from` / `forwarded_subject` fields rather than overriding
// `from`, so both identities are preserved and workflows opt in.

// forwardSubjectPrefixes are the case-insensitive Subject markers that
// flag a forwarded message. Matched as a prefix of the trimmed subject.
var forwardSubjectPrefixes = []string{"fwd:", "fw:", "fwd "}

// gmailForwardBlock matches Gmail's forwarded-message separator line,
// tolerating dash-count variance around the standard
// `---------- Forwarded message ---------`.
var gmailForwardBlock = regexp.MustCompile(`(?i)-{3,}\s*forwarded message\s*-{3,}`)

// parseForwarded inspects a parsed message's Subject + raw body for
// Gmail-forward shape. When a forward is detected it returns the original
// sender's address (parsed from the embedded `From:` line of the first
// forward block, empty when that header is absent/unparseable — e.g. an
// HTML-only body) and the subject with its forward prefix stripped. When
// no forward is detected it returns two empty strings.
//
// For nested forwards (a forward-of-a-forward), the parser intentionally
// surfaces only the OUTERMOST forwarded sender — the first (most-recent)
// forward block's `From:`. The deeper (older) forward blocks are
// intentionally not walked: their senders are not the most-recent hop,
// and surfacing them would mis-attribute the forward. The bounded scan
// in embeddedFromAddress enforces this first-block boundary (#458).
func parseForwarded(subject string, body []byte) (forwardedFrom, forwardedSubject string) {
	isForward := hasForwardSubjectPrefix(subject) || gmailForwardBlock.Match(body)
	if !isForward {
		return "", ""
	}
	return embeddedFromAddress(body), stripForwardPrefix(subject)
}

// hasForwardSubjectPrefix reports whether the trimmed subject begins with
// a case-insensitive forward marker (`Fwd:`, `FW:`, `Fwd `).
func hasForwardSubjectPrefix(subject string) bool {
	low := strings.ToLower(strings.TrimSpace(subject))
	for _, p := range forwardSubjectPrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// stripForwardPrefix removes a single leading forward prefix from the
// subject (case-insensitive). A subject with no prefix is returned
// trimmed but otherwise unchanged.
func stripForwardPrefix(subject string) string {
	s := strings.TrimSpace(subject)
	low := strings.ToLower(s)
	for _, p := range forwardSubjectPrefixes {
		if strings.HasPrefix(low, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}

// embeddedFromAddress finds the FIRST Gmail forward block in body and
// returns the address from its `From:` header line. Empty when there is
// no forward block, no `From:` line in that first block, or the address is
// unparseable.
//
// Nested-forward boundary: the scan is bounded to the first block's header
// region and MUST stop there — it must never fall through into a deeper
// (older) forward block's `From:`. The scan terminates on the first of:
// the header-block's blank-line terminator, a non-header line (the
// forwarded body), or a second gmailForwardBlock separator line. That last
// guard covers the pathological case where the first block has no `From:`
// and no blank line precedes the next separator: without it the scan could
// otherwise leak the deeper block's sender. When the first block has no
// parseable `From:`, this returns "" rather than the deeper sender.
func embeddedFromAddress(body []byte) string {
	loc := gmailForwardBlock.FindIndex(body)
	if loc == nil {
		return ""
	}
	// Scan the header lines that follow the separator. Gmail emits a
	// compact From/Date/Subject/To block; bound the scan to that block so
	// a stray "From:" deeper in the quoted body isn't mistaken for it.
	sc := bufio.NewScanner(bytes.NewReader(body[loc[1]:]))
	started := false
	for sc.Scan() {
		raw := sc.Text()
		// A second forward separator marks the start of a deeper (older)
		// forward block. Stop here so the outermost block's absent From:
		// can't leak the nested block's sender.
		if gmailForwardBlock.MatchString(raw) {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			if started {
				break // blank line terminates the header block
			}
			continue // leading blank(s) between separator and headers
		}
		if !looksLikeHeader(line) {
			break // past the header block (into the forwarded body)
		}
		started = true
		if rest, ok := cutHeaderPrefix(line, "from:"); ok {
			if addr, err := mail.ParseAddress(rest); err == nil {
				return addr.Address
			}
			return ""
		}
	}
	return ""
}

// looksLikeHeader reports whether line has a short `Name:` header shape,
// used to bound the embedded-header scan. The colon must fall within the
// first 40 bytes — a generous bound on an RFC-5322 field name (the longest
// standard names are ~25 chars), so a body line that merely contains a
// colon isn't mistaken for a header.
func looksLikeHeader(line string) bool {
	c := strings.IndexByte(line, ':')
	return c > 0 && c <= 40
}

// cutHeaderPrefix splits a `Header: value` line when its (case-
// insensitive) header name matches prefix (which must include the
// trailing colon). Returns the trimmed value and true on match.
func cutHeaderPrefix(line, prefix string) (string, bool) {
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(line[len(prefix):]), true
}
