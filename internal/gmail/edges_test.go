package gmail

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAssembleEdges_FiltersSystemLabels pins #449: Gmail system labels
// (the X-GM-LABELS `\`-prefixed special-use flags) must not produce
// tagged_as edges — only operator-applied labels do.
func TestAssembleEdges_FiltersSystemLabels(t *testing.T) {
	t.Parallel()

	t.Run("system labels only yields no tagged_as", func(t *testing.T) {
		t.Parallel()
		pm := &ParsedMessage{
			MessageID: "msg-sys@example.com",
			Labels:    []string{`\Inbox`, `\Unread`, `\Sent`, `\Important`, `\Starred`},
		}
		edges := AssembleEdges(pm, "yaad-ingested", "yaad-skip")
		for _, e := range edges {
			assert.NotEqualf(t, EdgeTypeTaggedAs, e.Type,
				"system label leaked to tagged_as: %+v", e)
		}
	})

	t.Run("operator label plus system labels tags only the operator label", func(t *testing.T) {
		t.Parallel()
		pm := &ParsedMessage{
			MessageID: "msg-mix@example.com",
			Labels:    []string{`\Inbox`, `\Important`, "Job Search/Active", `\Unread`},
		}
		edges := AssembleEdges(pm, "yaad-ingested", "yaad-skip")
		var tagged []string
		for _, e := range edges {
			if e.Type == EdgeTypeTaggedAs {
				tagged = append(tagged, e.Name)
			}
		}
		require.Len(t, tagged, 1, "only the operator label produces a tagged_as edge")
		assert.Equal(t, LabelSlug("Job Search/Active"), tagged[0])
	})
}

func TestAssembleEdges_HappyPath(t *testing.T) {
	t.Parallel()

	pm := &ParsedMessage{
		MessageID: "msg-001@example.com",
		From: "from@example.com",
		To: []string{"to1@example.com", "to2@example.com"},
		Cc: []string{"cc1@example.com"},
		Labels: []string{"INBOX", "Job Search/Active", "yaad-ingested", "yaad-skip"},
		IsSentFolder: false,
	}
	edges := AssembleEdges(pm, "yaad-ingested", "yaad-skip")

	// Expected:
	// - 1 is_about → email:gmail-msg-001-example-com
	// - 1 from
	// - 2 to
	// - 1 cc
	// - 0 bcc (not sent folder)
	// - 2 tagged_as (INBOX + Job Search/Active; yaad-ingested + yaad-skip filtered)
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
		Labels: []string{"yaad-ingested", "yaad-skip", "INBOX", "Personal"},
	}
	edges := AssembleEdges(pm, "yaad-ingested", "yaad-skip")
	for _, e := range edges {
		if e.Type != EdgeTypeTaggedAs {
			continue
		}
		if e.Name == "yaad-ingested" || e.Name == "yaad-skip" {
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
		Labels: []string{"yaad-ingested", "yaad-skip", "INBOX"},
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
	edges := AssembleEdges(nil, "yaad-ingested", "yaad-skip")
	if edges != nil {
		t.Errorf("nil message: got %v, want nil", edges)
	}
}
