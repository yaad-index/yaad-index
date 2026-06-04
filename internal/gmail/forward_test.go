package gmail

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gmailForwardBody is a realistic Gmail "Forward" body: the separator
// line, a compact From/Date/Subject/To header block, a blank line, then
// the forwarded content (which itself contains a decoy `From:` that the
// bounded scan must NOT pick up).
const gmailForwardBody = "Sent from my phone.\n\n" +
	"---------- Forwarded message ---------\n" +
	"From: Original Sender <noreply@acme.com>\n" +
	"Date: Mon, 1 Jun 2026 09:00:00 +0000\n" +
	"Subject: Your receipt\n" +
	"To: Forwarder <me@gmail.com>\n" +
	"\n" +
	"From: this is body text, not a header\n" +
	"Thanks for your order.\n"

func TestParseForwarded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		subject     string
		body        string
		wantFrom    string
		wantSubject string
	}{
		{
			name:        "classic Fwd: with forward block",
			subject:     "Fwd: Your receipt",
			body:        gmailForwardBody,
			wantFrom:    "noreply@acme.com",
			wantSubject: "Your receipt",
		},
		{
			name:        "FW: prefix variant",
			subject:     "FW: Your receipt",
			body:        gmailForwardBody,
			wantFrom:    "noreply@acme.com",
			wantSubject: "Your receipt",
		},
		{
			name:        "Fwd (space, no colon) variant",
			subject:     "Fwd Your receipt",
			body:        gmailForwardBody,
			wantFrom:    "noreply@acme.com",
			wantSubject: "Your receipt",
		},
		{
			name:        "forward block but no subject prefix",
			subject:     "Your receipt",
			body:        gmailForwardBody,
			wantFrom:    "noreply@acme.com",
			wantSubject: "Your receipt",
		},
		{
			name:        "Fwd: subject but no parseable block (HTML body)",
			subject:     "Fwd: Your receipt",
			body:        "<html><body>opaque html</body></html>",
			wantFrom:    "", // embedded From not recoverable
			wantSubject: "Your receipt",
		},
		{
			name:        "not a forward",
			subject:     "Your receipt",
			body:        "Just a normal email.\n",
			wantFrom:    "",
			wantSubject: "",
		},
		{
			name:        "bare embedded From address",
			subject:     "Fwd: hi",
			body:        "---------- Forwarded message ---------\nFrom: bare@acme.com\nSubject: hi\n\nbody",
			wantFrom:    "bare@acme.com",
			wantSubject: "hi",
		},
		{
			name:        "dash-count variance in separator",
			subject:     "Fwd: hi",
			body:        "----- Forwarded message -----\nFrom: x@acme.com\n\nbody",
			wantFrom:    "x@acme.com",
			wantSubject: "hi",
		},
		{
			name:        "strips only one forward prefix",
			subject:     "Fwd: Fwd: nested",
			body:        gmailForwardBody,
			wantFrom:    "noreply@acme.com",
			wantSubject: "Fwd: nested",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotFrom, gotSubject := parseForwarded(tc.subject, []byte(tc.body))
			assert.Equal(t, tc.wantFrom, gotFrom, "forwarded_from")
			assert.Equal(t, tc.wantSubject, gotSubject, "forwarded_subject")
		})
	}
}

// TestParseMessage_ForwardedSurfacesOriginalSender pins the end-to-end
// path: a forwarded RFC-822 message carries the forwarder as From but
// surfaces the original sender as ForwardedFrom, leaving From intact.
func TestParseMessage_ForwardedSurfacesOriginalSender(t *testing.T) {
	t.Parallel()
	raw := msg(map[string]string{
		"Message-ID": "<fwd123@gmail.com>",
		"Subject":    "Fwd: Your receipt",
		"From":       "Forwarder <me@gmail.com>",
		"Date":       "Mon, 1 Jun 2026 10:00:00 +0000",
	}, gmailForwardBody)

	pm, err := ParseMessage(raw, []string{"INBOX"}, false)
	require.NoError(t, err)
	assert.Equal(t, "me@gmail.com", pm.From, "envelope From stays the forwarder")
	assert.Equal(t, "noreply@acme.com", pm.ForwardedFrom, "original sender surfaced")
	assert.Equal(t, "Your receipt", pm.ForwardedSubject, "un-prefixed subject surfaced")
}

// TestParseMessage_NonForwardHasNoForwardedFields pins that an ordinary
// message leaves the forwarded fields empty.
func TestParseMessage_NonForwardHasNoForwardedFields(t *testing.T) {
	t.Parallel()
	raw := msg(map[string]string{
		"Message-ID": "<plain1@gmail.com>",
		"Subject":    "Your receipt",
		"From":       "Shop <noreply@acme.com>",
	}, "Thanks for your order.\n")

	pm, err := ParseMessage(raw, []string{"INBOX"}, false)
	require.NoError(t, err)
	assert.Empty(t, pm.ForwardedFrom)
	assert.Empty(t, pm.ForwardedSubject)
}
