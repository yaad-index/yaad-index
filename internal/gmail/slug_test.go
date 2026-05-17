package gmail

import (
	"strings"
	"testing"
)

func TestSlugifyClean(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"hello", "hello"},
		{"Hello World", "hello-world"},
		{" trimmed ", "trimmed"},
		{"already-clean", "already-clean"},
		{"Mixed_Case 123", "mixed-case-123"},
		{"!!!special@@@chars###", "special-chars"},
	}
	for _, tc := range cases {
		got := slugifyClean(tc.in)
		if got != tc.want {
			t.Errorf("slugifyClean(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMessageIDSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"<>", ""},
		{"<CABx12@gmail.com>", "cabx12-gmail-com"},
		{" <Bare-id_001@host> ", "bare-id-001-host"},
		{"no-brackets@example.com", "no-brackets-example-com"},
	}
	for _, tc := range cases {
		got := MessageIDSlug(tc.in)
		if got != tc.want {
			t.Errorf("MessageIDSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSourceSlug pins the per-spec format `<subject-slug>-<message-id-slug>`
// from RFC-822 Message-ID. Includes the operator-helpful collapse
// shapes when one piece is empty.
func TestSourceSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, subject, mid, want string
	}{
		{"happy path", "Re: Job Application", "<CABx12@gmail.com>", "re-job-application-cabx12-gmail-com"},
		{"empty subject collapses", "", "<CABx12@gmail.com>", "cabx12-gmail-com"},
		{"empty mid collapses", "Hello World", "", "hello-world"},
		{"both empty", "", "", ""},
		{"special chars normalize", "Subject: Foo!", "<a.b@c.d>", "subject-foo-a-b-c-d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SourceSlug(tc.subject, tc.mid)
			if got != tc.want {
				t.Errorf("SourceSlug(%q, %q) = %q, want %q", tc.subject, tc.mid, got, tc.want)
			}
		})
	}
}

// TestSourceSlug_DistinctMessageIDsCollideOnSubjectAlone is the
// regression assertion the spec calls out: two distinct
// Message-IDs MUST produce distinct source slugs even when their
// subjects are identical. Pre-spec implementations that used IMAP
// UID (per-mailbox + UIDVALIDITY-resettable) failed this — we
// pin the Message-ID-based shape here.
func TestSourceSlug_DistinctMessageIDsCollideOnSubjectAlone(t *testing.T) {
	t.Parallel()
	a := SourceSlug("Re: Status", "<msg-001@gmail.com>")
	b := SourceSlug("Re: Status", "<msg-002@gmail.com>")
	if a == b {
		t.Errorf("distinct Message-IDs collided on subject: a=%q b=%q", a, b)
	}
}

// TestEmailCanonicalSlug pins `gmail-<message-id-slug>` provider-
// prefix shape. Empty Message-ID returns "" so the caller skips
// the message.
func TestEmailCanonicalSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"<>", ""},
		{"<CABx12@gmail.com>", "gmail-cabx12-gmail-com"},
		{"<msg-001@host>", "gmail-msg-001-host"},
	}
	for _, tc := range cases {
		got := EmailCanonicalSlug(tc.in)
		if got != tc.want {
			t.Errorf("EmailCanonicalSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEmailAddressSlug pins the `_at_`/`_dot_` encoding. The
// markers are explicit so the slug round-trips back to the
// address unambiguously, and distinct addresses produce distinct
// slugs (regression for the spec's collision assertion).
func TestEmailAddressSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{" ", ""},
		{"foo@bar.com", "foo_at_bar_dot_com"},
		{"Eli.Rubigd@Gmail.COM", "eli_dot_rubigd_at_gmail_dot_com"},
		{"first.last+tag@example.io", "first_dot_last-tag_at_example_dot_io"},
	}
	for _, tc := range cases {
		got := EmailAddressSlug(tc.in)
		if got != tc.want {
			t.Errorf("EmailAddressSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEmailAddressSlug_DistinctAddressesProduceDistinctSlugs:
// regression against the spec's collision assertion. Addresses
// like `eli.rubigd@gmail.com` and `eli-rubigd.at@gmail.com` MUST
// not collapse to the same slug.
func TestEmailAddressSlug_DistinctAddressesProduceDistinctSlugs(t *testing.T) {
	t.Parallel()
	a := EmailAddressSlug("eli.rubigd@gmail.com")
	b := EmailAddressSlug("eli-rubigd.at@gmail.com")
	if a == b {
		t.Errorf("distinct addresses collided: a=%q b=%q", a, b)
	}
}

// TestLabelSlug pins the `_slash_` hierarchy-marker encoding. The
// load-bearing collision regression: nested `Job Search/Active`
// MUST NOT collapse to flat `Job Search Active`'s slug.
func TestLabelSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"INBOX uppercase", "INBOX", "inbox"},
		{"flat label with spaces", "Job Search Active", "job-search-active"},
		{"nested label with slash", "Job Search/Active", "job-search_slash_active"},
		{"deeper nesting", "Personal/Family/Vacation", "personal_slash_family_slash_vacation"},
		{"Gmail folder", "[Gmail]/Sent Mail", "gmail_slash_sent-mail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := LabelSlug(tc.in)
			if got != tc.want {
				t.Errorf("LabelSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLabelSlug_NestedAndFlatProduceDistinctSlugs is the explicit
// regression the spec calls out: `Job Search/Active` (nested)
// MUST NOT slug-collide with `Job Search Active` (flat).
func TestLabelSlug_NestedAndFlatProduceDistinctSlugs(t *testing.T) {
	t.Parallel()
	nested := LabelSlug("Job Search/Active")
	flat := LabelSlug("Job Search Active")
	if nested == flat {
		t.Errorf("nested + flat label collided: nested=%q flat=%q", nested, flat)
	}
}

// TestSourceSlug_LongSubjectCapped pins #146: a long encoded-MIME
// subject + long github message-id compose to >255 bytes before
// the fix, which exceeds the ext4/xfs/apfs filename limit when
// the vault writer appends `.md.tmp-NNNNNNNNNN`. Cap per-
// component so the composed slug stays under the 200-byte
// budget.
func TestSourceSlug_LongSubjectCapped(t *testing.T) {
	t.Parallel()
	// Reconstruction of the failure shape from the bug report:
	// long github review email subject + long Message-ID.
	subject := "=?utf-8?Q?Re:_[owner/repo]_feat(?=" +
		"?utf-8?Q?fill)_canonical_5ftype_gap_entries_carry_free-form_data_=" +
		"e2=86=92_appended_as_=?utf-8?Q?dataview_paragraphs_on_target_(issue_119)?="
	mid := "<owner/repo/issue/119/issue/event/25624563815@github.com>"

	got := SourceSlug(subject, mid)
	if len(got) > sourceSlugCap {
		t.Errorf("SourceSlug exceeds cap: len=%d cap=%d got=%q", len(got), sourceSlugCap, got)
	}
	// The result still starts with the truncated subject prefix so
	// operators see something readable in the slug.
	if !strings.HasPrefix(got, "utf-8-q-re-owner-repo-feat") {
		t.Errorf("expected slug to start with truncated subject prefix; got=%q", got)
	}
}

// TestSourceSlug_DeterministicOnReFetch pins the idempotency
// contract under the cap: the same (subject, rawMessageID) pair
// must produce the same slug across calls. Required by the
// daemon's slug-as-idempotency-key contract per ADR-0023 §4.
func TestSourceSlug_DeterministicOnReFetch(t *testing.T) {
	t.Parallel()
	subject := strings.Repeat("a", 500) // forces the hash-on-overflow path
	mid := "<msg-id@gmail.com>"
	a := SourceSlug(subject, mid)
	b := SourceSlug(subject, mid)
	if a != b {
		t.Errorf("SourceSlug not deterministic: a=%q b=%q", a, b)
	}
	if len(a) > sourceSlugCap {
		t.Errorf("hash-on-overflow result exceeds cap: len=%d cap=%d got=%q", len(a), sourceSlugCap, a)
	}
}

// TestSourceSlug_LongSubjectDistinctMidsStaysDistinct pins the
// uniqueness contract through the cap: two distinct Message-IDs
// produce distinct slugs even when both subjects are long
// enough to trigger truncation. The message-id portion is what
// preserves identity.
func TestSourceSlug_LongSubjectDistinctMidsStaysDistinct(t *testing.T) {
	t.Parallel()
	subject := strings.Repeat("subject-fragment-", 30) // long subject
	a := SourceSlug(subject, "<msg-001@gmail.com>")
	b := SourceSlug(subject, "<msg-002@gmail.com>")
	if a == b {
		t.Errorf("distinct message-ids collided despite long subject: a=%q b=%q", a, b)
	}
}

// TestSourceSlug_HashTailOnOverflow pins the fallback path: a
// degenerate subject without hyphenable whitespace exceeds the
// cap even after per-component truncation. The slug appends a
// short SHA-1-hex tail so it stays under the limit.
func TestSourceSlug_HashTailOnOverflow(t *testing.T) {
	t.Parallel()
	// 500-char no-whitespace input forces the per-component
	// truncation to leave a long slug after composition; the
	// hash-tail fallback caps it.
	subject := strings.Repeat("a", 500)
	mid := strings.Repeat("b", 500) + "@x"
	got := SourceSlug(subject, mid)
	if len(got) > sourceSlugCap {
		t.Errorf("hash-on-overflow result exceeds cap: len=%d cap=%d got=%q",
			len(got), sourceSlugCap, got)
	}
	// The tail is sourceHashTailLen hex chars.
	if len(got) < sourceHashTailLen+1 {
		t.Fatalf("hash-on-overflow result too short to carry tail: got=%q", got)
	}
	tail := got[len(got)-sourceHashTailLen:]
	for _, r := range tail {
		isDigit := r >= '0' && r <= '9'
		isHexLower := r >= 'a' && r <= 'f'
		if !isDigit && !isHexLower {
			t.Errorf("hash tail not lowercase-hex: got=%q tail=%q", got, tail)
		}
	}
}
