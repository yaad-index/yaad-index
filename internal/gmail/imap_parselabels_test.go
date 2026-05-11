// Real-path X-GM-LABELS response-parser test. The full IMAP
// round-trip against a Gmail-extension-aware mock server is heavy
// (would need a custom IMAP responder running over a unix socket
// + the v1 client driving it); the load-bearing piece bob
// flagged is that production reads X-GM-LABELS off the response.
// `parseLabels` is the exact translator from raw IMAP-decoded
// response value → []string the rest of the pipeline consumes.
// Pinning the parser shape here covers the regression the cold-reviewer flagged
// (production never reaching the label data) at the right
// boundary.

package gmail

import (
	"testing"

	"github.com/emersion/go-imap"
	"github.com/stretchr/testify/assert"
)

// TestParseLabels_DecodedResponseShapes covers the response-value
// shapes v1's IMAP decoder lands at `Message.Items[X-GM-LABELS]`:
// `[]interface{}` of mixed `string` + `imap.RawString` elements
// (the canonical case for parenthesized list responses), the
// defensive single-string fallback, and the empty / unknown-type
// degrade-to-nil cases.
func TestParseLabels_DecodedResponseShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw any
		want []string
	}{
		{
			name: "nil",
			raw: nil,
			want: nil,
		},
		{
			name: "empty list",
			raw: []interface{}{},
			want: []string{},
		},
		{
			name: "list of strings (decoder canonical shape)",
			raw: []interface{}{"INBOX", "Job Search/Active", "Personal"},
			want: []string{"INBOX", "Job Search/Active", "Personal"},
		},
		{
			name: "list of imap.RawString (atom-decoded shape)",
			raw: []interface{}{imap.RawString("INBOX"), imap.RawString("\\Important")},
			want: []string{"INBOX", "\\Important"},
		},
		{
			name: "mixed strings + RawString",
			raw: []interface{}{"INBOX", imap.RawString("\\Important"), "Custom"},
			want: []string{"INBOX", "\\Important", "Custom"},
		},
		{
			name: "single-string defensive fallback",
			raw: "INBOX",
			want: []string{"INBOX"},
		},
		{
			name: "empty-string defensive fallback returns nil",
			raw: "",
			want: nil,
		},
		{
			name: "unknown type returns nil",
			raw: 42,
			want: nil,
		},
		{
			name: "list with empty/unknown elements skips them",
			raw: []interface{}{"INBOX", "", 42, imap.RawString("Personal")},
			want: []string{"INBOX", "Personal"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseLabels(tc.raw)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestBuildSearchPredicate covers the X-GM-RAW string composition
// that `realClient.SearchUningested` issues. Pinned here so the
// empty-string-disables semantic + the "both empty → ALL" path
// stay correct across refactors.
func TestBuildSearchPredicate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, ingested, skip, want string
	}{
		{"both populated", "alice2-ingested", "alice2-skip", "-label:alice2-ingested -label:alice2-skip"},
		{"only ingested", "alice2-ingested", "", "-label:alice2-ingested"},
		{"only skip", "", "alice2-skip", "-label:alice2-skip"},
		{"both empty (operator opted out)", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildSearchPredicate(tc.ingested, tc.skip)
			assert.Equal(t, tc.want, got)
		})
	}
}
