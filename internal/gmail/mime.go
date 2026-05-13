package gmail

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"path/filepath"
	"strings"
)

// MIMEAttachment is a per-message MIME part the Poller surfaces to
// the wire layer for ADR-0014 staging + emission. Includes the part's
// payload bytes (decoded from Content-Transfer-Encoding), its
// suggested filename / extension, and a Role hint mirroring the
// ADR-0014 attach.Attachment.Role field — "html-body" for the
// alternative HTML body, "attachment" for disposition=attachment +
// large inline parts per the #12 spec.
//
// Role values:
//
//   - "html-body" — the message's HTML alternative body. At most one
//     per envelope (the walker picks the first text/html part it
//     finds in multipart/alternative or at the top level).
//   - "attachment" — a binary or text part whose Content-Disposition
//     is "attachment", OR an inline part whose size exceeds
//     InlineSizeThreshold.
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
// A small inline part (e.g. a signature image, an embedded tracking
// pixel) stays out of the attachment surface; a large inline payload
// (e.g. an inline-displayed photo the sender embedded vs attached) is
// staged so the operator can read it.
//
// 8 KiB is the threshold — empirically matches the inline/genuine-
// attachment split on representative mail in the yaad-index org. Bump
// if false-positive small-attachments surface; lower if signature
// images leak through.
const InlineSizeThreshold = 8 * 1024

// WalkMIMEParts parses raw (the full RFC-822 message bytes from
// IMAP) and returns the HTML body (if any) plus the per-message
// attachment list. Pure function: no I/O, no env var reads — the
// caller decides whether to stage the returned bytes to disk.
//
// The walk is depth-first over nested multipart structures. The
// first text/html part encountered (in either a top-level
// multipart/alternative or a nested multipart/mixed > alternative
// branch) becomes the html-body; subsequent text/html parts are
// surfaced as attachments. text/plain parts are NOT surfaced as
// attachments — the existing clean_content path already covers
// plain-text body extraction.
//
// Returns (nil, nil, nil) when raw is empty / unparseable as RFC-822
// — the caller falls back to the legacy body-bytes-only emission
// path. Per-part decode failures are logged via the walker's
// non-fatal path (the part is skipped, the walk continues); the
// public signature stays simple so the caller doesn't have to thread
// a logger through.
//
// The returned htmlBody is decoded text — Content-Transfer-Encoding
// resolution (quoted-printable, base64) handled by Go's stdlib
// mime/multipart reader. Same for each attachment's Data.
func WalkMIMEParts(raw []byte) (htmlBody []byte, attachments []MIMEAttachment, err error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("gmail: walk mime: parse rfc-822: %w", err)
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		// No Content-Type header → plain text body, nothing to walk.
		return nil, nil, nil
	}
	mediaType, params, parseErr := mime.ParseMediaType(ct)
	if parseErr != nil {
		// Malformed Content-Type → treat as opaque single-part body,
		// not an error: legacy gmail clients sometimes emit broken
		// headers; we don't want one bad message to drop the cycle.
		return nil, nil, nil
	}

	// Single-part top-level: only meaningful when it's text/html (the
	// html-body candidate). text/plain is already covered by
	// clean_content; binary single-part at the top level is rare and
	// gets reclassified to an attachment.
	if !strings.HasPrefix(mediaType, "multipart/") {
		w := &mimeWalker{}
		w.handleSinglePart(msg.Header, msg.Body, mediaType, 0)
		return w.htmlBody, w.attachments, nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, nil
	}
	w := &mimeWalker{}
	w.walkMultipart(msg.Body, boundary, mediaType)
	return w.htmlBody, w.attachments, nil
}

// mimeWalker carries the depth-first walk state. PartIndex is
// monotonic across the whole walk so staged filenames are unique
// even for deeply-nested attachments.
type mimeWalker struct {
	htmlBody    []byte
	attachments []MIMEAttachment
	partIndex   int
}

// walkMultipart iterates the parts of a multipart container, dispatching
// each leaf to handleSinglePart and recursing into nested multiparts.
// parentMediaType controls a small semantic difference: html-body
// candidates from multipart/alternative win over candidates from
// multipart/mixed (the alternative branch is the "rendered body"
// channel; the mixed branch's html parts are usually attached
// rendered emails).
func (w *mimeWalker) walkMultipart(r io.Reader, boundary, parentMediaType string) {
	mr := multipart.NewReader(r, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			// Either io.EOF (clean end) or a malformed boundary —
			// either way, stop walking. Already-collected parts
			// survive.
			return
		}
		w.handlePart(part, parentMediaType)
		_ = part.Close()
	}
}

// handlePart classifies a part as leaf vs nested-multipart and routes.
func (w *mimeWalker) handlePart(part *multipart.Part, parentMediaType string) {
	ct := part.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		// Parts without a Content-Type default to text/plain per
		// RFC-2045 — treat as such, but skip the html-body /
		// attachment assignment (text/plain isn't surfaced here).
		return
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary != "" {
			w.walkMultipart(part, boundary, mediaType)
		}
		return
	}
	// Leaf part. Read body fully; if the read errors, skip silently.
	w.partIndex++
	idx := w.partIndex
	data, err := io.ReadAll(part)
	if err != nil {
		return
	}
	w.classifyAndStash(part.Header, mediaType, data, idx, parentMediaType)
}

// handleSinglePart is the top-level non-multipart entry point. The
// `header` is a textproto-equivalent mail header; the body bytes are
// read once.
func (w *mimeWalker) handleSinglePart(header mail.Header, body io.Reader, mediaType string, _ int) {
	data, err := io.ReadAll(body)
	if err != nil {
		return
	}
	w.partIndex++
	idx := w.partIndex
	// Adapt mail.Header → the same get-shape classifyAndStash uses.
	w.classifyAndStash(headerAdapter{h: header}, mediaType, data, idx, "")
}

// partHeader is the minimal interface classifyAndStash needs — both
// multipart.Part and mail.Header (via headerAdapter) implement it.
type partHeader interface {
	Get(key string) string
}

// headerAdapter lifts a mail.Header to the same Get-shape multipart.Part
// uses, so classifyAndStash can take either.
type headerAdapter struct {
	h mail.Header
}

func (a headerAdapter) Get(key string) string { return a.h.Get(key) }

// classifyAndStash decides whether a leaf MIME part is the html-body,
// an attachment, or neither (text/plain / unrecognized), and updates
// the walker state accordingly.
func (w *mimeWalker) classifyAndStash(header partHeader, mediaType string, data []byte, partIndex int, parentMediaType string) {
	disposition, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := dispParams["filename"]

	switch {
	case mediaType == "text/html" && disposition != "attachment":
		// First html in an alternative branch wins. Subsequent ones
		// (including htmls from multipart/mixed sub-branches) become
		// attachments — the operator may want to read embedded
		// forwarded htmls.
		if w.htmlBody == nil && (parentMediaType == "multipart/alternative" || parentMediaType == "" || parentMediaType == "multipart/related") {
			w.htmlBody = data
			return
		}
		// Reclassify as attachment.
		w.attachments = append(w.attachments, MIMEAttachment{
			Role:        "attachment",
			Filename:    filename,
			Extension:   extensionForPart(filename, mediaType),
			ContentType: mediaType,
			PartIndex:   partIndex,
			Data:        data,
		})

	case mediaType == "text/plain":
		// Plain-text body is already surfaced via clean_content. Skip.

	case disposition == "attachment":
		w.attachments = append(w.attachments, MIMEAttachment{
			Role:        "attachment",
			Filename:    filename,
			Extension:   extensionForPart(filename, mediaType),
			ContentType: mediaType,
			PartIndex:   partIndex,
			Data:        data,
		})

	case len(data) > InlineSizeThreshold:
		// Inline-disposition above the threshold. Per #12 spec these
		// reclassify to attachment — large inline images are
		// indistinguishable from genuine attachments at the operator-
		// surface level.
		w.attachments = append(w.attachments, MIMEAttachment{
			Role:        "attachment",
			Filename:    filename,
			Extension:   extensionForPart(filename, mediaType),
			ContentType: mediaType,
			PartIndex:   partIndex,
			Data:        data,
		})
	}
}

// extensionForPart picks the best file extension for a MIME part:
// prefer the filename's extension when present (operator wants to
// see the sender's chosen extension), fall back to a Content-Type
// → extension mapping for the common types. Empty when neither is
// resolvable — the caller decides whether to stage extension-less
// (yaad-bgg's pattern: empty extension produces a no-extension
// staged file, daemon-side dispatcher handles it).
func extensionForPart(filename, mediaType string) string {
	if filename != "" {
		if ext := strings.TrimPrefix(filepath.Ext(filename), "."); ext != "" {
			return strings.ToLower(ext)
		}
	}
	// Content-Type fallback for common types. Not exhaustive — the
	// long tail (application/vnd.*) gets empty, which is fine.
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
