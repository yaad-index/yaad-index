package gmail

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// helper for raw RFC-822 fixtures. CRLF line endings — net/mail
// requires them.
func msg(headers map[string]string, body string) []byte {
	var sb strings.Builder
	for k, v := range headers {
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(v)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return []byte(sb.String())
}

func TestParseMessage_HappyPath(t *testing.T) {
	t.Parallel()

	raw := msg(map[string]string{
		"Message-ID": "<CABx12@gmail.com>",
		"Subject": "Re: Job Application",
		"From": "Hiring Manager <hr@company.com>",
		"To": "Eli <eli@example.com>",
		"Cc": "Manager <mgr@company.com>, Bob <bob@company.com>",
		"Date": "Mon, 10 May 2026 12:00:00 +0000",
	}, "Hello, thanks for applying.\n")

	pm, err := ParseMessage(raw, []string{"INBOX", "Job Search/Active"}, false)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if pm.MessageID != "CABx12@gmail.com" {
		t.Errorf("MessageID: got %q, want %q", pm.MessageID, "CABx12@gmail.com")
	}
	if pm.Subject != "Re: Job Application" {
		t.Errorf("Subject: got %q", pm.Subject)
	}
	if pm.From != "hr@company.com" {
		t.Errorf("From: got %q, want %q", pm.From, "hr@company.com")
	}
	if len(pm.To) != 1 || pm.To[0] != "eli@example.com" {
		t.Errorf("To: got %v, want [eli@example.com]", pm.To)
	}
	if len(pm.Cc) != 2 {
		t.Errorf("Cc: want 2 entries, got %v", pm.Cc)
	}
	if pm.IsSentFolder {
		t.Errorf("IsSentFolder: want false (inbox parse)")
	}
	if len(pm.Bcc) != 0 {
		t.Errorf("Bcc: want empty on inbound, got %v", pm.Bcc)
	}
	if len(pm.Labels) != 2 {
		t.Errorf("Labels: want 2 (INBOX + Job Search/Active), got %v", pm.Labels)
	}
	if !pm.Date.Equal(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("Date: got %v, want 2026-05-10T12:00:00Z", pm.Date)
	}
	if !strings.Contains(string(pm.Body), "thanks for applying") {
		t.Errorf("Body: missing expected content; got %q", string(pm.Body))
	}
}

// TestParseMessage_SentFolderSurfacesBcc pins the spec's BCC rule:
// Bcc edges only emit on messages from the sent folder. When
// IsSentFolder is true, the Bcc header parses; when false, Bcc
// stays empty regardless of header content.
func TestParseMessage_SentFolderSurfacesBcc(t *testing.T) {
	t.Parallel()

	raw := msg(map[string]string{
		"Message-ID": "<sent-001@gmail.com>",
		"Subject": "Outbound",
		"From": "Eli <eli@example.com>",
		"To": "Recipient <r1@example.com>",
		"Bcc": "Quiet <bcc1@example.com>, Hidden <bcc2@example.com>",
	}, "")

	sent, err := ParseMessage(raw, nil, true)
	if err != nil {
		t.Fatalf("ParseMessage (sent): %v", err)
	}
	if len(sent.Bcc) != 2 {
		t.Errorf("sent Bcc: want 2 entries, got %v", sent.Bcc)
	}

	inbound, err := ParseMessage(raw, nil, false)
	if err != nil {
		t.Fatalf("ParseMessage (inbound): %v", err)
	}
	if len(inbound.Bcc) != 0 {
		t.Errorf("inbound Bcc: want empty (sent-folder-only rule), got %v", inbound.Bcc)
	}
}

// TestParseMessage_MissingMessageIDRejects: a message with no
// Message-ID header returns ErrMissingMessageID — caller's
// responsibility to skip.
func TestParseMessage_MissingMessageIDRejects(t *testing.T) {
	t.Parallel()

	raw := msg(map[string]string{
		"Subject": "no message id",
		"From": "x@y.com",
	}, "body")

	_, err := ParseMessage(raw, nil, false)
	if !errors.Is(err, ErrMissingMessageID) {
		t.Errorf("ParseMessage: want ErrMissingMessageID, got %v", err)
	}
}

// TestParseMessage_MalformedAddressHeaders_DoNotFail: malformed
// To/Cc headers leave the corresponding slice empty rather than
// failing the parse — graceful degradation per the spec's parser
// contract.
func TestParseMessage_MalformedAddressHeaders_DoNotFail(t *testing.T) {
	t.Parallel()

	raw := msg(map[string]string{
		"Message-ID": "<malformed-cc@gmail.com>",
		"Subject": "edge case",
		"From": "Fine <fine@example.com>",
		"To": "garbage no @ at all",
	}, "")

	pm, err := ParseMessage(raw, nil, false)
	if err != nil {
		t.Fatalf("ParseMessage: malformed To shouldn't fail; got %v", err)
	}
	if pm.From != "fine@example.com" {
		t.Errorf("From: got %q, want %q", pm.From, "fine@example.com")
	}
	if len(pm.To) != 0 {
		t.Errorf("To: malformed should yield empty, got %v", pm.To)
	}
}
