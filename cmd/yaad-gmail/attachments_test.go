package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/gmail"
)

// discardLogger returns a slog.Logger writing nowhere — keeps
// stageAttachments warn lines out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestStageAttachments_MultiPartHTMLAndBinary pins the canonical
// shape per #12: a message with both an HTML body and a binary
// attachment produces both ADR-0014 entries with `file://` URIs +
// the staged files exist on disk under <stagingRoot>/<message-id>/.
func TestStageAttachments_MultiPartHTMLAndBinary(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	env := gmail.IngestEnvelope{
		SourceID:  "gmail:abc",
		MessageID: "abc@example.com",
		HTMLBody:  []byte("<p>Hi</p>"),
		Attachments: []gmail.MIMEAttachment{
			{
				Role:        "attachment",
				Filename:    "report.pdf",
				Extension:   "pdf",
				ContentType: "application/pdf",
				PartIndex:   2,
				Data:        []byte("%PDF-1.4 fake"),
			},
		},
	}

	got := stageAttachments(stagingRoot, env, discardLogger())
	require.Len(t, got, 2, "want 2 attachments (html-body + pdf)")

	// Order is deterministic: html-body first, then attachments in
	// walk order. Assert by Role to make the test resilient to a
	// future ordering refactor.
	var htmlAtt, pdfAtt int = -1, -1
	for i, a := range got {
		switch a.Role {
		case "html-body":
			htmlAtt = i
		case "attachment":
			pdfAtt = i
		}
	}
	require.NotEqual(t, -1, htmlAtt, "html-body attachment missing")
	require.NotEqual(t, -1, pdfAtt, "attachment role missing")

	// HTML body: URI + staged file content.
	htmlA := got[htmlAtt]
	assert.Equal(t, "html", htmlA.Extension)
	assert.True(t, strings.HasPrefix(htmlA.URI, "file://"+stagingRoot),
		"html URI must start with file://<stagingRoot>; got %q", htmlA.URI)
	htmlPath := strings.TrimPrefix(htmlA.URI, "file://")
	htmlContent, err := os.ReadFile(htmlPath)
	require.NoError(t, err, "html body must be staged on disk")
	assert.Equal(t, "<p>Hi</p>", string(htmlContent))
	assert.Equal(t, "body.html", filepath.Base(htmlPath),
		"html body must land at body.html under the per-message subdir")

	// PDF attachment.
	pdfA := got[pdfAtt]
	assert.Equal(t, "pdf", pdfA.Extension)
	pdfPath := strings.TrimPrefix(pdfA.URI, "file://")
	pdfContent, err := os.ReadFile(pdfPath)
	require.NoError(t, err, "pdf attachment must be staged on disk")
	assert.Equal(t, "%PDF-1.4 fake", string(pdfContent))
	assert.Equal(t, "2.pdf", filepath.Base(pdfPath),
		"attachment must use <part-index>.<ext> naming")

	// Subdir name comes from sanitized Message-ID.
	subdir := filepath.Dir(htmlPath)
	assert.Equal(t, "abc@example.com", filepath.Base(subdir),
		"per-message subdir name comes from message-id")
}

// TestStageAttachments_HTMLOnly pins the no-binary path: a message
// with only an HTML alternative produces exactly one attach.Attachment
// (the html-body); the staged file exists.
func TestStageAttachments_HTMLOnly(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	env := gmail.IngestEnvelope{
		MessageID: "html-only@example.com",
		HTMLBody:  []byte("<h1>Hello</h1>"),
	}

	got := stageAttachments(stagingRoot, env, discardLogger())
	require.Len(t, got, 1, "want 1 attachment (html-body only)")
	assert.Equal(t, "html-body", got[0].Role)
	assert.Equal(t, "html", got[0].Extension)
}

// TestStageAttachments_PlainTextEmptyEnvelope pins the plain-text
// fallback: an envelope with no HTMLBody + no attachments → nil
// attachment list, no subdir created.
func TestStageAttachments_PlainTextEmptyEnvelope(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	env := gmail.IngestEnvelope{
		MessageID: "plain@example.com",
	}

	got := stageAttachments(stagingRoot, env, discardLogger())
	assert.Nil(t, got, "plain-text envelope → no attachments")

	// Subdir must NOT exist — we don't create the dir until we
	// have something to write.
	subdir := filepath.Join(stagingRoot, "plain@example.com")
	_, err := os.Stat(subdir)
	assert.True(t, os.IsNotExist(err),
		"empty envelope must not create the per-message subdir; stat err=%v", err)
}

// TestStageAttachments_BinaryOnlyNoHTMLBody pins the
// attachments-without-html path: a message with binary attachments
// but no HTML alternative produces exactly the binary list (no
// stub html entry).
func TestStageAttachments_BinaryOnlyNoHTMLBody(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	env := gmail.IngestEnvelope{
		MessageID: "binary-only@example.com",
		Attachments: []gmail.MIMEAttachment{
			{Role: "attachment", Extension: "zip", PartIndex: 1, Data: []byte("PK\x03\x04 zip bytes")},
		},
	}

	got := stageAttachments(stagingRoot, env, discardLogger())
	require.Len(t, got, 1)
	assert.Equal(t, "attachment", got[0].Role)
	assert.Equal(t, "zip", got[0].Extension)
}

// TestSanitizeMessageID_StripsUnsafeChars pins the message-id
// sanitization for filesystem-safe per-message subdir names.
// Path-segment-unsafe characters (`/`, `\`, `:`, control bytes)
// become `_`; leading dots strip to prevent `..` traversal; empty
// input falls back to the sentinel.
func TestSanitizeMessageID_StripsUnsafeChars(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"abc@example.com", "abc@example.com"},
		{"slash/in/id", "slash_in_id"},
		{"colon:in:id", "colon_in_id"},
		{"..traversal..attempt", "traversal..attempt"},
		{"", "no-message-id"},
		{"......", "no-message-id"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, sanitizeMessageID(tc.in))
		})
	}
}
