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
		{"both populated", "yaad-ingested", "yaad-skip", "-label:yaad-ingested -label:yaad-skip"},
		{"only ingested", "yaad-ingested", "", "-label:yaad-ingested"},
		{"only skip", "", "yaad-skip", "-label:yaad-skip"},
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

// TestXGMRawSearchCommand_WireShape pins the fix for #56:
// the command emits `UID SEARCH X-GM-RAW "<predicate>"` rather
// than the broken `UID SEARCH HEADER "X-GM-RAW" "<predicate>"`
// shape that go-imap v1's SearchCriteria.Header would produce.
// Validating at the Commander.Command() level avoids needing
// a live IMAP server while still pinning the over-the-wire
// argument vector that determines Gmail's response.
func TestXGMRawSearchCommand_WireShape(t *testing.T) {
	cmd := &xGMRawSearchCommand{Predicate: "-label:yaad-ingested -label:yaad-skip"}
	out := cmd.Command()

	if out.Name != "UID" {
		t.Fatalf("Command().Name: want %q, got %q", "UID", out.Name)
	}
	if len(out.Arguments) != 3 {
		t.Fatalf("Command().Arguments: want 3 fields, got %d (%v)", len(out.Arguments), out.Arguments)
	}

	// First two must be RawString so the IMAP writer emits them
	// unquoted as IMAP atoms (verbs / criteria keywords). Quoting
	// these would break Gmail's parser.
	if raw, ok := out.Arguments[0].(imap.RawString); !ok || raw != "SEARCH" {
		t.Errorf("Arguments[0]: want imap.RawString(%q), got %T %v", "SEARCH", out.Arguments[0], out.Arguments[0])
	}
	if raw, ok := out.Arguments[1].(imap.RawString); !ok || raw != "X-GM-RAW" {
		t.Errorf("Arguments[1]: want imap.RawString(%q), got %T %v", "X-GM-RAW", out.Arguments[1], out.Arguments[1])
	}
	// Predicate is a plain string so IMAP serialization quotes it
	// (matching Python imaplib's `'"<predicate>"'`). RawString
	// would emit it unquoted + Gmail's parser would choke on the
	// embedded space.
	if s, ok := out.Arguments[2].(string); !ok || s != "-label:yaad-ingested -label:yaad-skip" {
		t.Errorf("Arguments[2]: want plain string predicate, got %T %v", out.Arguments[2], out.Arguments[2])
	}
}

// TestXGMRawSearchCommand_EmptyPredicate_StillEmitsShape pins
// that the commander doesn't special-case empty predicates — the
// caller is responsible for branching to the standard ALL path
// when both labels are disabled. SearchUningested upstream owns
// that decision.
func TestXGMRawSearchCommand_EmptyPredicate_StillEmitsShape(t *testing.T) {
	cmd := &xGMRawSearchCommand{Predicate: ""}
	out := cmd.Command()
	if len(out.Arguments) != 3 {
		t.Fatalf("Command().Arguments: want 3 fields even for empty predicate, got %d", len(out.Arguments))
	}
	if s, ok := out.Arguments[2].(string); !ok || s != "" {
		t.Errorf("Arguments[2]: want empty string, got %T %v", out.Arguments[2], out.Arguments[2])
	}
}
