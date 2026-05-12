// Poll-loop tests using an in-process fake Client. Exercises the
// bidirectional label flow (read → tagged_as edges; write
// ingested_label after emit; skip messages with skip_label;
// refetch on label-removal), the BCC-only-on-sent rule, and the
// restart-safety property (state lives entirely on the fake's
// label-side, no client-side state).

package gmail

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMessage models one IMAP message in the fake server: identity
// (UID + Message-ID + Subject), parsed headers (From + To + Cc +
// Bcc), folder placement (which folders return it via search),
// and the per-message Gmail label set (mutable so tests can
// simulate label add/remove).
type fakeMessage struct {
	UID uint32
	MessageID string
	Subject string
	From string
	To, Cc, Bcc []string
	Labels map[string]struct{}
	// Folder placement: which folder name returns this message
	// via SearchUningested. A message can live in multiple folders
	// (e.g. INBOX + [Gmail]/All Mail) but for this fake we keep
	// it single-placement to keep tests simple.
	Folder string
}

// fakeClient is an in-process IMAP client substitute for the
// poll-loop tests. Records call sequences so tests assert flow
// shape (search → fetch → store ordering); maintains per-message
// label state so the bidirectional label flow exercises end-to-end
// without a real IMAP server.
type fakeClient struct {
	mu sync.Mutex
	messages []*fakeMessage
	selectedFolder string
	closed bool
	markIngestedLog []uint32
	searchCount int
	fetchCount int
	failMarkIngested bool // when true, MarkIngested returns an error (test injection)
}

func newFakeClient(messages ...*fakeMessage) *fakeClient {
	for _, m := range messages {
		if m.Labels == nil {
			m.Labels = map[string]struct{}{}
		}
	}
	return &fakeClient{messages: messages}
}

func (f *fakeClient) SelectFolder(_ context.Context, folder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectedFolder = folder
	return nil
}

func (f *fakeClient) SearchUningested(_ context.Context, ingestedLabel, skipLabel string) ([]uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCount++
	out := []uint32{}
	for _, m := range f.messages {
		if m.Folder != f.selectedFolder {
			continue
		}
		if ingestedLabel != "" {
			if _, has := m.Labels[ingestedLabel]; has {
				continue
			}
		}
		if skipLabel != "" {
			if _, has := m.Labels[skipLabel]; has {
				continue
			}
		}
		out = append(out, m.UID)
	}
	return out, nil
}

func (f *fakeClient) FetchMessages(_ context.Context, uids []uint32) ([]FetchedMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCount++
	out := make([]FetchedMessage, 0, len(uids))
	for _, uid := range uids {
		var msg *fakeMessage
		for _, m := range f.messages {
			if m.UID == uid && m.Folder == f.selectedFolder {
				msg = m
				break
			}
		}
		if msg == nil {
			continue
		}
		out = append(out, FetchedMessage{
			UID: uid,
			Body: msg.rawRFC822(),
			Labels: msg.labelSlice(),
		})
	}
	return out, nil
}

func (f *fakeClient) MarkIngested(_ context.Context, uid uint32, ingestedLabel string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMarkIngested {
		return errors.New("fake: MarkIngested injected failure")
	}
	if ingestedLabel == "" {
		return nil
	}
	for _, m := range f.messages {
		if m.UID == uid && m.Folder == f.selectedFolder {
			m.Labels[ingestedLabel] = struct{}{}
			f.markIngestedLog = append(f.markIngestedLog, uid)
			return nil
		}
	}
	return errors.New("fake: MarkIngested for unknown UID")
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// rawRFC822 builds an RFC-822 byte stream the parser will accept
// from the fakeMessage's structured fields. CRLF line endings.
func (m *fakeMessage) rawRFC822() []byte {
	var sb strings.Builder
	if m.MessageID != "" {
		sb.WriteString("Message-ID: <" + m.MessageID + ">\r\n")
	}
	if m.Subject != "" {
		sb.WriteString("Subject: " + m.Subject + "\r\n")
	}
	if m.From != "" {
		sb.WriteString("From: " + m.From + "\r\n")
	}
	if len(m.To) > 0 {
		sb.WriteString("To: " + strings.Join(m.To, ", ") + "\r\n")
	}
	if len(m.Cc) > 0 {
		sb.WriteString("Cc: " + strings.Join(m.Cc, ", ") + "\r\n")
	}
	if len(m.Bcc) > 0 {
		sb.WriteString("Bcc: " + strings.Join(m.Bcc, ", ") + "\r\n")
	}
	sb.WriteString("\r\n")
	sb.WriteString("body for " + m.MessageID + "\r\n")
	return []byte(sb.String())
}

func (m *fakeMessage) labelSlice() []string {
	out := make([]string, 0, len(m.Labels))
	for l := range m.Labels {
		out = append(out, l)
	}
	return out
}

// recordingEmit collects emitted envelopes for assertions.
type recordingEmit struct {
	mu sync.Mutex
	envelopes []IngestEnvelope
}

func (r *recordingEmit) emit(_ context.Context, env IngestEnvelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envelopes = append(r.envelopes, env)
	return nil
}

// TestPoller_Tick_HappyPath_IngestsAllUningested: the load-bearing
// integration: messages without ingested_label + without
// skip_label are searched, fetched, parsed, emitted, and
// MarkIngested is called for each successful emit.
func TestPoller_Tick_HappyPath_IngestsAllUningested(t *testing.T) {
	t.Parallel()
	fc := newFakeClient(
		&fakeMessage{UID: 1, MessageID: "a@x", Subject: "first", From: "alice@x.com", Folder: InboxFolderName},
		&fakeMessage{UID: 2, MessageID: "b@x", Subject: "second", From: "bob@x.com", Folder: InboxFolderName},
	)
	rec := &recordingEmit{}
	p := NewPoller(fc, "yaad-ingested", "yaad-skip", rec.emit, nil)

	count, errs := p.Tick(context.Background())
	require.Empty(t, errs, "no per-cycle errors on happy path")
	assert.Equal(t, 2, count, "both messages ingested")

	require.Len(t, rec.envelopes, 2)
	assert.Equal(t, "gmail:first-a-x", rec.envelopes[0].SourceID)
	assert.Equal(t, "gmail:second-b-x", rec.envelopes[1].SourceID)

	// Both messages got the ingested label written.
	assert.ElementsMatch(t, []uint32{1, 2}, fc.markIngestedLog)
	assert.Contains(t, fc.messages[0].Labels, "yaad-ingested")
	assert.Contains(t, fc.messages[1].Labels, "yaad-ingested")
}

// TestPoller_Tick_SkipLabel_BlocksIngest: messages carrying
// skip_label aren't returned by SearchUningested, so the poll
// cycle never sees them.
func TestPoller_Tick_SkipLabel_BlocksIngest(t *testing.T) {
	t.Parallel()
	skipped := &fakeMessage{
		UID: 1, MessageID: "skip@x", Subject: "skipped", From: "x@y.com",
		Folder: InboxFolderName,
		Labels: map[string]struct{}{"yaad-skip": {}},
	}
	allowed := &fakeMessage{UID: 2, MessageID: "ok@x", Subject: "allowed", From: "x@y.com", Folder: InboxFolderName}
	fc := newFakeClient(skipped, allowed)
	rec := &recordingEmit{}
	p := NewPoller(fc, "yaad-ingested", "yaad-skip", rec.emit, nil)

	count, errs := p.Tick(context.Background())
	require.Empty(t, errs)
	assert.Equal(t, 1, count, "only the non-skipped message ingested")
	require.Len(t, rec.envelopes, 1)
	assert.Equal(t, "gmail:allowed-ok-x", rec.envelopes[0].SourceID)
}

// TestPoller_Tick_RefetchOnLabelRemoval: simulate operator removing
// the ingested label between cycles. Cycle 1 ingests + marks;
// operator removes the label; cycle 2 finds the message in the
// fetch set again and re-ingests.
func TestPoller_Tick_RefetchOnLabelRemoval(t *testing.T) {
	t.Parallel()
	m := &fakeMessage{UID: 1, MessageID: "refetch@x", Subject: "subject", From: "x@y.com", Folder: InboxFolderName}
	fc := newFakeClient(m)
	rec := &recordingEmit{}
	p := NewPoller(fc, "yaad-ingested", "yaad-skip", rec.emit, nil)

	// Cycle 1: ingest + mark.
	count1, errs := p.Tick(context.Background())
	require.Empty(t, errs)
	assert.Equal(t, 1, count1, "cycle 1 ingests")
	assert.Contains(t, m.Labels, "yaad-ingested", "cycle 1 marks")

	// Cycle 2 (no label removal): nothing to do.
	count2, errs := p.Tick(context.Background())
	require.Empty(t, errs)
	assert.Equal(t, 0, count2, "cycle 2 no-op when label still present")

	// Operator removes the label on Gmail-side (test simulation).
	delete(m.Labels, "yaad-ingested")

	// Cycle 3: refetch re-runs the same search predicate, finds the
	// message in the fetch set, re-ingests.
	count3, errs := p.Tick(context.Background())
	require.Empty(t, errs)
	assert.Equal(t, 1, count3, "cycle 3 re-ingests on label removal")
	require.Len(t, rec.envelopes, 2, "two emissions total (cycle 1 + cycle 3)")
}

// TestPoller_Tick_RestartSafe_NoClientStateNeeded: run two pollers
// against the same fake client (simulating restart). The first
// poller ingests + marks; the second poller starts fresh (no
// inherited state) and the search predicate naturally excludes the
// already-marked messages — proves state lives on Gmail-side.
func TestPoller_Tick_RestartSafe_NoClientStateNeeded(t *testing.T) {
	t.Parallel()
	fc := newFakeClient(
		&fakeMessage{UID: 1, MessageID: "a@x", Subject: "first", From: "x@y.com", Folder: InboxFolderName},
		&fakeMessage{UID: 2, MessageID: "b@x", Subject: "second", From: "x@y.com", Folder: InboxFolderName},
	)
	rec1 := &recordingEmit{}
	p1 := NewPoller(fc, "yaad-ingested", "yaad-skip", rec1.emit, nil)
	count1, _ := p1.Tick(context.Background())
	assert.Equal(t, 2, count1, "poller-1 ingests both")

	// Simulate restart: brand new poller against the same client.
	rec2 := &recordingEmit{}
	p2 := NewPoller(fc, "yaad-ingested", "yaad-skip", rec2.emit, nil)
	count2, _ := p2.Tick(context.Background())
	assert.Equal(t, 0, count2, "poller-2 finds nothing (state on Gmail)")
	assert.Empty(t, rec2.envelopes, "poller-2 emits zero envelopes — restart-safe")
}

// TestPoller_Tick_BccOnSentFolderOnly: a message carrying Bcc
// headers in the sent folder emits bcc edges; the same message
// shape in INBOX does not.
func TestPoller_Tick_BccOnSentFolderOnly(t *testing.T) {
	t.Parallel()
	fc := newFakeClient(
		&fakeMessage{
			UID: 1, MessageID: "out@x", Subject: "outbound",
			From: "me@x.com", To: []string{"r@x.com"},
			Bcc: []string{"hidden@x.com"},
			Folder: SentFolderName,
		},
	)
	rec := &recordingEmit{}
	p := NewPoller(fc, "yaad-ingested", "yaad-skip", rec.emit, nil)
	count, errs := p.Tick(context.Background())
	require.Empty(t, errs)
	assert.Equal(t, 1, count)

	require.Len(t, rec.envelopes, 1)
	bccCount := 0
	for _, e := range rec.envelopes[0].Edges {
		if e.Type == EdgeTypeBcc {
			bccCount++
		}
	}
	assert.Equal(t, 1, bccCount, "sent-folder BCC surfaces edge")
}

// TestPoller_Tick_MarkIngestedFailure_DoesNotLoseMessage: when
// MarkIngested errors out, the message has already emitted but the
// label-write didn't land — the next cycle re-attempts (the search
// predicate still matches). Tick records the error but continues.
func TestPoller_Tick_MarkIngestedFailure_DoesNotLoseMessage(t *testing.T) {
	t.Parallel()
	fc := newFakeClient(
		&fakeMessage{UID: 1, MessageID: "x@x", Subject: "subject", From: "x@y.com", Folder: InboxFolderName},
	)
	fc.failMarkIngested = true
	rec := &recordingEmit{}
	p := NewPoller(fc, "yaad-ingested", "yaad-skip", rec.emit, nil)

	count, errs := p.Tick(context.Background())
	assert.Equal(t, 0, count, "ingest count excludes mark-failed messages")
	assert.NotEmpty(t, errs, "mark-ingested failure recorded")
	require.Len(t, rec.envelopes, 1, "envelope still emitted (message visible to daemon)")

	// Recover: clear the injected failure, re-poll.
	fc.failMarkIngested = false
	count2, errs2 := p.Tick(context.Background())
	assert.Empty(t, errs2)
	assert.Equal(t, 1, count2, "next cycle re-ingests on recovery")
	require.Len(t, rec.envelopes, 2, "two emissions total (failure + recovery)")
}

// TestPoller_Tick_EmptyIngestedLabel_DisablesAutoWrite: empty
// ingested_label means MarkIngested is a no-op (per the spec) AND
// the search predicate skips the negative-label half. Net effect:
// every cycle re-ingests every message.
func TestPoller_Tick_EmptyIngestedLabel_DisablesAutoWrite(t *testing.T) {
	t.Parallel()
	fc := newFakeClient(
		&fakeMessage{UID: 1, MessageID: "a@x", Subject: "first", From: "x@y.com", Folder: InboxFolderName},
	)
	rec := &recordingEmit{}
	p := NewPoller(fc, "", "yaad-skip", rec.emit, nil)

	count1, _ := p.Tick(context.Background())
	count2, _ := p.Tick(context.Background())
	assert.Equal(t, 1, count1)
	assert.Equal(t, 1, count2, "every cycle re-ingests when ingested_label disabled")
	assert.Empty(t, fc.markIngestedLog, "MarkIngested no-op when label disabled")
}
