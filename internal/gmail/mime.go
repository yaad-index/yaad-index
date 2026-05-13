package gmail

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jhillyerd/enmime"
)

// MIMEAttachment is a per-message MIME part the Poller surfaces to
// the wire layer for ADR-0014 staging + emission. Carries the
// part's decoded payload bytes (CTE + charset resolved by enmime),
// suggested filename / extension, and a Role hint mirroring
// ADR-0014 attach.Attachment.Role — "html-body" for the alternative
// HTML body, "attachment" for disposition=attachment + large inline
// parts per the #12 spec.
//
// PartIndex is a stable per-message ordinal across the depth-first
// MIME walk, used by the wire layer to name staged files
// ("<part-index>.<ext>") so the operator can cross-reference the
// staged blob with the part it represents.
type MIMEAttachment struct {
	Role        string
	Filename    string
	Extension   string
	ContentType string
	PartIndex   int
	Data        []byte
}

// InlineSizeThreshold is the byte cap above which an inline-disposition
// MIME part is reclassified as an "attachment" role per the #12 spec.
// A small inline part (e.g. a signature image, embedded tracking
// pixel) stays out of the attachment surface; a large inline payload
// (e.g. an inline-displayed photo) is staged so the operator can
// read it.
//
// 8 KiB empirically matches the inline/genuine-attachment split on
// representative mail in the yaad-index org. Bump if false-positive
// small-attachments surface; lower if signature images leak through.
const InlineSizeThreshold = 8 * 1024

// WalkMIMEParts parses raw (the full RFC-822 message bytes from
// IMAP) via enmime and returns the HTML body (when the message has
// one) plus the per-message attachment list. Returns (nil, nil, nil)
// when raw is empty.
//
// enmime handles the bulk of MIME parsing: nested multipart walks,
// Content-Transfer-Encoding decoding (base64, quoted-printable),
// charset conversion, RFC-2047 header decoding for filenames. The
// thin layer on top applies two yaad-index-specific rules:
//
//  1. **html-in-multipart/mixed → attachment.** An HTML part that
//     enmime would surface as the body but whose parent in the MIME
//     tree is multipart/mixed (no multipart/alternative wrapper) is
//     a forwarded HTML email body, not the rendered body. Operators
//     may want to read it, so it surfaces as a role:attachment
//     instead of role:html-body.
//
//  2. **Inline parts above InlineSizeThreshold → attachment.** Small
//     inline parts (signatures, tracking pixels) stay hidden; large
//     inline parts (genuine embedded images) reclassify so the
//     operator gets them.
func WalkMIMEParts(raw []byte) (htmlBody []byte, attachments []MIMEAttachment, err error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("gmail: walk mime: %w", err)
	}

	var atts []MIMEAttachment
	partIndex := 0
	nextIndex := func() int {
		partIndex++
		return partIndex
	}

	// HTML body assignment with the html-in-mixed reclassification.
	// enmime fills env.HTML from the first text/html part it finds
	// regardless of parent; we walk to confirm the parent's wrap.
	htmlAsBody := env.HTML
	htmlPart := findHTMLBodyPart(env.Root)
	if htmlPart != nil && parentIsMultipartMixed(htmlPart) {
		// Reclassify as attachment.
		htmlAsBody = ""
		atts = append(atts, partToAttachment(htmlPart, "attachment", nextIndex()))
	}

	// Real attachments (Content-Disposition: attachment).
	for _, p := range env.Attachments {
		atts = append(atts, partToAttachment(p, "attachment", nextIndex()))
	}

	// Inline parts: reclassify above the threshold.
	for _, p := range env.Inlines {
		if len(p.Content) > InlineSizeThreshold {
			atts = append(atts, partToAttachment(p, "attachment", nextIndex()))
		}
	}

	// OtherParts: enmime parks things that don't fit Inline /
	// Attachment / body channels here (e.g. multipart/related
	// sub-parts that aren't pure decoration). Surface as
	// attachments so the operator can see them.
	for _, p := range env.OtherParts {
		atts = append(atts, partToAttachment(p, "attachment", nextIndex()))
	}

	if htmlAsBody != "" {
		htmlBody = []byte(htmlAsBody)
	}
	return htmlBody, atts, nil
}

// findHTMLBodyPart walks the part tree depth-first and returns the
// first text/html leaf — matches enmime's env.HTML selection so the
// parent-of-html check below stays aligned.
func findHTMLBodyPart(root *enmime.Part) *enmime.Part {
	if root == nil {
		return nil
	}
	if root.ContentType == "text/html" && root.FirstChild == nil {
		return root
	}
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if got := findHTMLBodyPart(c); got != nil {
			return got
		}
	}
	return nil
}

// parentIsMultipartMixed reports whether the part's immediate
// parent is multipart/mixed — the project rule for treating the
// part as an embedded / forwarded body rather than the rendered
// body.
func parentIsMultipartMixed(p *enmime.Part) bool {
	if p == nil || p.Parent == nil {
		return false
	}
	return p.Parent.ContentType == "multipart/mixed"
}

// partToAttachment lifts an enmime.Part into the project's
// MIMEAttachment shape with the role + part-index resolved.
func partToAttachment(p *enmime.Part, role string, idx int) MIMEAttachment {
	return MIMEAttachment{
		Role:        role,
		Filename:    p.FileName,
		Extension:   extensionForPart(p.FileName, p.ContentType),
		ContentType: p.ContentType,
		PartIndex:   idx,
		Data:        p.Content,
	}
}

// extensionForPart picks the best file extension for a MIME part:
// prefer the filename's extension when present (operator wants to
// see the sender's chosen extension), fall back to a Content-Type
// → extension mapping for the common types. Empty when neither is
// resolvable.
func extensionForPart(filename, mediaType string) string {
	if filename != "" {
		if ext := strings.TrimPrefix(filepath.Ext(filename), "."); ext != "" {
			return strings.ToLower(ext)
		}
	}
	switch mediaType {
	case "text/html":
		return "html"
	case "text/plain":
		return "txt"
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "application/pdf":
		return "pdf"
	case "application/zip":
		return "zip"
	}
	return ""
}
