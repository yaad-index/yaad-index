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
// Only the most-recent (first) forward block is parsed; multi-hop
// forwards are out of scope per #323.
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

// embeddedFromAddress finds the first Gmail forward block in body and
// returns the address from its `From:` header line. Empty when there is
// no forward block, no `From:` line in that block, or the address is
// unparseable.
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
		line := strings.TrimSpace(sc.Text())
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
