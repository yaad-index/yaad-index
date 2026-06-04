package gmail

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// ParsedMessage is the in-memory shape after RFC-822 header parse.
// Carries everything the source-shape entity + edge-list assembly
// need: identity (Message-ID + Subject), date, sender, recipient
// lists (To/Cc/Bcc), and the per-message Gmail labels read off the
// IMAP X-GM-LABELS extension. Body is opaque bytes for vault-write
// (clean_content); MIME body parsing is deferred until a follow-up
// PR if richer body extraction is needed.
type ParsedMessage struct {
	// MessageID is the RFC-822 Message-ID header value with angle
	// brackets stripped. Empty when the source mail has no
	// Message-ID — caller decides whether to skip the message
	// (recommended) or fabricate a sentinel.
	MessageID string
	Subject string
	Date time.Time
	From string
	To []string
	Cc []string
	// Bcc is populated only on messages parsed from the sent
	// folder per the spec. Inbound messages with synthetic BCC
	// headers (uncommon; usually a sender's own log copy) are not
	// surfaced as edges by the assembler.
	Bcc []string
	// Labels carries the X-GM-LABELS extension's per-message label
	// list verbatim from the IMAP fetch — the assembler filters
	// the control-plane labels (ingested_label + skip_label) out
	// before producing `tagged_as` edges.
	Labels []string
	// Body is the raw RFC-822 body bytes for vault clean_content.
	// MIME / multipart parsing deferred.
	Body []byte
	// IsSentFolder marks whether the message came from the sent
	// folder; the assembler only emits BCC edges when this is true.
	IsSentFolder bool
	// ForwardedFrom / ForwardedSubject carry the original sender's
	// address + un-prefixed subject when this message is a Gmail forward
	// (#323). Both empty for non-forwards. ForwardedFrom is empty when the
	// forward is detected (by Subject prefix) but the embedded `From:`
	// header isn't recoverable from the body. Surfaced into entity.data as
	// the additive `forwarded_from` / `forwarded_subject` fields so
	// workflow predicates can match the original sender even though the
	// envelope `From:` (entity.data.from) carries the forwarder.
	ForwardedFrom string
	ForwardedSubject string
}

// ErrMissingMessageID signals an RFC-822 message with no
// `Message-ID:` header. Callers should skip these — without a
// stable Message-ID, the source slug + canonical email slug have
// no anchor.
var ErrMissingMessageID = errors.New("gmail: rfc-822 message missing Message-ID header")

// ParseMessage reads RFC-822 headers from `raw` (the BODY[] bytes
// returned by an IMAP fetch) and returns a ParsedMessage. The
// `labels` slice is the X-GM-LABELS extension result for this
// message (passed through to ParsedMessage.Labels verbatim;
// control-plane filtering happens at edge-assembly time, not here).
// `isSent` is the caller's IsSentFolder discriminant — true when
// the message came from `[Gmail]/Sent Mail`.
//
// Address-header parsing uses `net/mail.AddressList`, which handles
// both bare `addr@host` and quoted `"Name" <addr@host>` shapes.
// Empty / unparseable address headers don't fail the call — the
// corresponding slice is left empty and the rest of the message
// parses cleanly. The only failure path that returns a non-nil
// error is the missing-Message-ID case (ErrMissingMessageID), which
// callers MUST treat as skip-not-fail.
func ParseMessage(raw []byte, labels []string, isSent bool) (*ParsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gmail: parse rfc-822: %w", err)
	}

	out := &ParsedMessage{
		Subject: msg.Header.Get("Subject"),
		Labels: append([]string{}, labels...),
		IsSentFolder: isSent,
	}

	mid := strings.TrimSpace(msg.Header.Get("Message-ID"))
	mid = strings.TrimPrefix(mid, "<")
	mid = strings.TrimSuffix(mid, ">")
	if mid == "" {
		return nil, ErrMissingMessageID
	}
	out.MessageID = mid

	if dateRaw := msg.Header.Get("Date"); dateRaw != "" {
		if t, err := mail.ParseDate(dateRaw); err == nil {
			out.Date = t
		}
	}

	if fromList := parseAddressList(msg.Header.Get("From")); len(fromList) > 0 {
		// RFC-5322 allows multi-address From headers but the
		// canonical mail-client convention is single-sender; the
		// edge assembler emits a single `from` edge per message.
		// We keep only the first address.
		out.From = fromList[0]
	}
	out.To = parseAddressList(msg.Header.Get("To"))
	out.Cc = parseAddressList(msg.Header.Get("Cc"))
	if isSent {
		out.Bcc = parseAddressList(msg.Header.Get("Bcc"))
	}

	// Body bytes for clean_content. Read once; if the message has
	// no body (e.g. headers-only fetch) the read returns empty and
	// that's acceptable.
	bodyBuf := new(bytes.Buffer)
	if msg.Body != nil {
		_, _ = bodyBuf.ReadFrom(msg.Body)
	}
	out.Body = bodyBuf.Bytes()

	// #323: detect Gmail-forward shape and surface the original sender +
	// un-prefixed subject. Runs after Subject + Body are populated.
	out.ForwardedFrom, out.ForwardedSubject = parseForwarded(out.Subject, out.Body)

	return out, nil
}

// parseAddressList wraps `net/mail.ParseAddressList` with a
// non-failing lift: malformed / empty input returns nil rather than
// an error. The plugin treats address-header malformation as
// graceful degradation (the corresponding edge type just doesn't
// emit) rather than a hard parse failure that drops the whole
// message.
func parseAddressList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(raw)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Address == "" {
			continue
		}
		out = append(out, a.Address)
	}
	return out
}
