package gmail

import (
	"testing"
)

func TestAssembleEdges_HappyPath(t *testing.T) {
	t.Parallel()

	pm := &ParsedMessage{
		MessageID: "msg-001@example.com",
		From: "from@example.com",
		To: []string{"to1@example.com", "to2@example.com"},
		Cc: []string{"cc1@example.com"},
		Labels: []string{"INBOX", "Job Search/Active", "alice2-ingested", "alice2-skip"},
		IsSentFolder: false,
	}
	edges := AssembleEdges(pm, "alice2-ingested", "alice2-skip")

	// Expected:
	// - 1 is_about → email:gmail-msg-001-example-com
	// - 1 from
	// - 2 to
	// - 1 cc
	// - 0 bcc (not sent folder)
	// - 2 tagged_as (INBOX + Job Search/Active; alice2-ingested + alice2-skip filtered)
	// Total: 7
	if len(edges) != 7 {
		t.Errorf("edge count: got %d, want 7; edges=%+v", len(edges), edges)
	}

	counts := map[string]int{}
	for _, e := range edges {
		counts[e.Type]++
	}
	if counts[EdgeTypeIsAbout] != 1 {
		t.Errorf("is_about count: got %d, want 1", counts[EdgeTypeIsAbout])
	}
	if counts[EdgeTypeFrom] != 1 {
		t.Errorf("from count: got %d, want 1", counts[EdgeTypeFrom])
	}
	if counts[EdgeTypeTo] != 2 {
		t.Errorf("to count: got %d, want 2", counts[EdgeTypeTo])
	}
	if counts[EdgeTypeCc] != 1 {
		t.Errorf("cc count: got %d, want 1", counts[EdgeTypeCc])
	}
	if counts[EdgeTypeBcc] != 0 {
		t.Errorf("bcc count: got %d, want 0 (inbound)", counts[EdgeTypeBcc])
	}
	if counts[EdgeTypeTaggedAs] != 2 {
		t.Errorf("tagged_as count: got %d, want 2 (INBOX + Job Search/Active; control labels filtered)", counts[EdgeTypeTaggedAs])
	}
}

// TestAssembleEdges_BccOnlyOnSentFolder pins the spec's BCC rule:
// when IsSentFolder is true AND Bcc list is non-empty, bcc edges
// emit; otherwise zero.
func TestAssembleEdges_BccOnlyOnSentFolder(t *testing.T) {
	t.Parallel()

	pm := &ParsedMessage{
		MessageID: "sent@example.com",
		From: "me@example.com",
		To: []string{"r1@example.com"},
		Bcc: []string{"bcc1@example.com", "bcc2@example.com"},
		IsSentFolder: true,
	}
	sent := AssembleEdges(pm, "", "")
	bccCount := 0
	for _, e := range sent {
		if e.Type == EdgeTypeBcc {
			bccCount++
		}
	}
	if bccCount != 2 {
		t.Errorf("sent-folder BCC count: got %d, want 2", bccCount)
	}

	// Same message, IsSentFolder=false (e.g. somehow received with
	// Bcc still in headers — shouldn't surface).
	pm.IsSentFolder = false
	inbound := AssembleEdges(pm, "", "")
	for _, e := range inbound {
		if e.Type == EdgeTypeBcc {
			t.Errorf("inbound BCC edge surfaced: %+v", e)
		}
	}
}

// TestAssembleEdges_LabelControlPlaneFilter pins that ingested_label
// + skip_label are NEVER emitted as tagged_as edges, regardless of
// presence on the message. This is the load-bearing filter for the
// label-as-control-plane semantic.
func TestAssembleEdges_LabelControlPlaneFilter(t *testing.T) {
	t.Parallel()

	pm := &ParsedMessage{
		MessageID: "labels@example.com",
		Labels: []string{"alice2-ingested", "alice2-skip", "INBOX", "Personal"},
	}
	edges := AssembleEdges(pm, "alice2-ingested", "alice2-skip")
	for _, e := range edges {
		if e.Type != EdgeTypeTaggedAs {
			continue
		}
		if e.Name == "alice2-ingested" || e.Name == "alice2-skip" {
			t.Errorf("control-plane label leaked to tagged_as: %+v", e)
		}
	}
	count := 0
	for _, e := range edges {
		if e.Type == EdgeTypeTaggedAs {
			count++
		}
	}
	if count != 2 {
		t.Errorf("tagged_as count: got %d, want 2 (INBOX + Personal; control labels filtered)", count)
	}
}

// TestAssembleEdges_EmptyControlLabels_DontFilterAnything: when
// the operator opted out (IngestedLabel="" or SkipLabel=""), the
// filter is bypassed for that slot — every label that's not the
// other slot's value passes through.
func TestAssembleEdges_EmptyControlLabels_DontFilterAnything(t *testing.T) {
	t.Parallel()

	pm := &ParsedMessage{
		MessageID: "x@example.com",
		Labels: []string{"alice2-ingested", "alice2-skip", "INBOX"},
	}
	edges := AssembleEdges(pm, "", "")
	count := 0
	for _, e := range edges {
		if e.Type == EdgeTypeTaggedAs {
			count++
		}
	}
	if count != 3 {
		t.Errorf("tagged_as count with disabled control: got %d, want 3 (no filter applied)", count)
	}
}

// TestAssembleEdges_NilMessage returns nil rather than panicking.
func TestAssembleEdges_NilMessage(t *testing.T) {
	t.Parallel()
	edges := AssembleEdges(nil, "alice2-ingested", "alice2-skip")
	if edges != nil {
		t.Errorf("nil message: got %v, want nil", edges)
	}
}
