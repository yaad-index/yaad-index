package gmail

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWalkMIMEParts_PlainTextOnly pins the no-attachment baseline:
// a pure text/plain message with no multipart wrapper returns no
// html body + no attachments. The legacy clean_content path is the
// sole carrier of the plain-text body.
func TestWalkMIMEParts_PlainTextOnly(t *testing.T) {
	t.Parallel()

	raw := []byte(strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: Plain hello",
		"Message-ID: <plain-1@example.com>",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Hello plain world.",
	}, "\r\n"))

	html, atts, err := WalkMIMEParts(raw)
	require.NoError(t, err)
	assert.Nil(t, html, "no html alternative → htmlBody nil")
	assert.Empty(t, atts, "plain-text-only → no attachments")
}

// TestWalkMIMEParts_HTMLBodyAlternative pins the most common shape:
// multipart/alternative carrying both text/plain + text/html. The
// HTML part becomes htmlBody; plain part skipped.
func TestWalkMIMEParts_HTMLBodyAlternative(t *testing.T) {
	t.Parallel()

	raw := []byte(strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: Mixed body",
		"Message-ID: <alt-1@example.com>",
		`Content-Type: multipart/alternative; boundary="BOUND"`,
		"",
		"--BOUND",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Plain version.",
		"--BOUND",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<p>HTML version.</p>",
		"--BOUND--",
		"",
	}, "\r\n"))

	html, atts, err := WalkMIMEParts(raw)
	require.NoError(t, err)
	assert.Equal(t, "<p>HTML version.</p>", strings.TrimSpace(string(html)),
		"multipart/alternative html part becomes htmlBody")
	assert.Empty(t, atts, "alternative-only message → no attachments")
}

// TestWalkMIMEParts_MixedWithAttachment pins the multi-part-with-
// binary-attachment shape: top-level multipart/mixed containing a
// multipart/alternative (body) AND an application/pdf attachment.
func TestWalkMIMEParts_MixedWithAttachment(t *testing.T) {
	t.Parallel()

	raw := []byte(strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: With attachment",
		"Message-ID: <mixed-1@example.com>",
		`Content-Type: multipart/mixed; boundary="OUTER"`,
		"",
		"--OUTER",
		`Content-Type: multipart/alternative; boundary="INNER"`,
		"",
		"--INNER",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Plain body.",
		"--INNER",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<p>HTML body.</p>",
		"--INNER--",
		"--OUTER",
		"Content-Type: application/pdf",
		`Content-Disposition: attachment; filename="report.pdf"`,
		"",
		"%PDF-1.4 fake pdf bytes",
		"--OUTER--",
		"",
	}, "\r\n"))

	html, atts, err := WalkMIMEParts(raw)
	require.NoError(t, err)
	assert.Equal(t, "<p>HTML body.</p>", strings.TrimSpace(string(html)))
	require.Len(t, atts, 1, "expected 1 attachment (the PDF)")
	a := atts[0]
	assert.Equal(t, "attachment", a.Role)
	assert.Equal(t, "report.pdf", a.Filename)
	assert.Equal(t, "pdf", a.Extension)
	assert.Equal(t, "application/pdf", a.ContentType)
	assert.Contains(t, string(a.Data), "%PDF-1.4")
}

// TestWalkMIMEParts_InlineAboveThreshold pins the inline-reclassify
// rule: an inline-disposition image whose payload exceeds
// InlineSizeThreshold gets reclassified as an attachment per the
// #12 spec ("inline parts above a size threshold").
func TestWalkMIMEParts_InlineAboveThreshold(t *testing.T) {
	t.Parallel()

	// Build a payload one byte over the threshold so the boundary
	// condition is exercised exactly.
	bigPayload := strings.Repeat("x", InlineSizeThreshold+1)
	raw := []byte(strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: Inline image",
		"Message-ID: <inline-big@example.com>",
		`Content-Type: multipart/related; boundary="BOUND"`,
		"",
		"--BOUND",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<p>Body with inline image.</p>",
		"--BOUND",
		"Content-Type: image/png",
		"Content-Disposition: inline",
		"",
		bigPayload,
		"--BOUND--",
		"",
	}, "\r\n"))

	_, atts, err := WalkMIMEParts(raw)
	require.NoError(t, err)
	require.Len(t, atts, 1, "inline-above-threshold reclassifies to attachment")
	assert.Equal(t, "attachment", atts[0].Role)
	assert.Equal(t, "png", atts[0].Extension)
}

// TestWalkMIMEParts_InlineBelowThresholdSkipped pins the converse:
// a small inline part (signature image, tracking pixel) stays out
// of the attachment surface.
func TestWalkMIMEParts_InlineBelowThresholdSkipped(t *testing.T) {
	t.Parallel()

	smallPayload := strings.Repeat("x", 32)
	raw := []byte(strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: Tracking pixel",
		"Message-ID: <inline-tiny@example.com>",
		`Content-Type: multipart/related; boundary="BOUND"`,
		"",
		"--BOUND",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<p>Body.</p>",
		"--BOUND",
		"Content-Type: image/png",
		"Content-Disposition: inline",
		"",
		smallPayload,
		"--BOUND--",
		"",
	}, "\r\n"))

	_, atts, err := WalkMIMEParts(raw)
	require.NoError(t, err)
	assert.Empty(t, atts, "small inline part stays out of the attachment surface")
}

// TestWalkMIMEParts_NoBody pins the nil-safe path: empty input
// returns (nil, nil, nil) so callers can call unconditionally.
func TestWalkMIMEParts_NoBody(t *testing.T) {
	t.Parallel()

	html, atts, err := WalkMIMEParts(nil)
	require.NoError(t, err)
	assert.Nil(t, html)
	assert.Empty(t, atts)
}

// TestWalkMIMEParts_TopLevelHTML pins the single-part HTML shape:
// some senders emit text/html as the top-level Content-Type without
// a multipart wrapper. That html becomes the htmlBody.
func TestWalkMIMEParts_TopLevelHTML(t *testing.T) {
	t.Parallel()

	raw := []byte(strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: HTML only",
		"Message-ID: <html-only@example.com>",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<h1>Hi</h1>",
	}, "\r\n"))

	html, atts, err := WalkMIMEParts(raw)
	require.NoError(t, err)
	assert.Equal(t, "<h1>Hi</h1>", strings.TrimSpace(string(html)))
	assert.Empty(t, atts)
}
