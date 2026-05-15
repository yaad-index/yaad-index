// Tests for dedupFetchedByUID — the per-UID merge layer that
// collapses Gmail's duplicate FETCH responses (yaad-index #60).
// The helper is a pure function over []FetchedMessage so the
// merge contract (body first-non-empty, labels set-union,
// request-order output) is testable without a real IMAP server.

package gmail

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDedupFetchedByUID_GmailDoubleEmit pins the observed-in-prod
// pattern: one response with BODY[] + labels, one response with only
// labels, same UID. The collapsed FetchedMessage must carry the
// body bytes + the union of both responses' labels + no ReadErr.
func TestDedupFetchedByUID_GmailDoubleEmit(t *testing.T) {
	t.Parallel()
	body := []byte("Message-ID: <a@x>\r\n\r\nhello\r\n")
	raws := []FetchedMessage{
		{UID: 42, Body: body, Labels: []string{"INBOX"}},
		{UID: 42, Body: nil, Labels: []string{"yaad-ingested"}},
	}
	out := dedupFetchedByUID([]uint32{42}, raws)

	require.Len(t, out, 1, "duplicate UID must collapse to one entry")
	assert.Equal(t, uint32(42), out[0].UID)
	assert.Equal(t, body, out[0].Body, "body from first non-empty response wins")
	assert.Nil(t, out[0].ReadErr)
	assert.Equal(t, []string{"INBOX", "yaad-ingested"}, out[0].Labels,
		"labels are union of both responses, sorted")
}

// TestDedupFetchedByUID_PhantomFirst covers the arrival-order edge:
// the empty-body phantom arrives BEFORE the real BODY[] response.
// First-non-empty-wins must still pick up the body from the second
// response and emit it.
func TestDedupFetchedByUID_PhantomFirst(t *testing.T) {
	t.Parallel()
	body := []byte("Message-ID: <b@x>\r\n\r\nworld\r\n")
	raws := []FetchedMessage{
		{UID: 7, Body: nil, Labels: []string{"\\Important"}},
		{UID: 7, Body: body, Labels: []string{"INBOX"}},
	}
	out := dedupFetchedByUID([]uint32{7}, raws)

	require.Len(t, out, 1)
	assert.Equal(t, body, out[0].Body, "body picked up from second response")
	assert.Equal(t, []string{"INBOX", "\\Important"}, out[0].Labels,
		"label union preserves both responses regardless of arrival order")
}

// TestDedupFetchedByUID_LabelUnionDropsDuplicates: when the same
// label appears in both responses, the set-union emits it once.
func TestDedupFetchedByUID_LabelUnionDropsDuplicates(t *testing.T) {
	t.Parallel()
	raws := []FetchedMessage{
		{UID: 1, Body: []byte("Message-ID: <a@x>\r\n\r\n"), Labels: []string{"INBOX", "yaad-ingested"}},
		{UID: 1, Body: nil, Labels: []string{"yaad-ingested", "Personal"}},
	}
	out := dedupFetchedByUID([]uint32{1}, raws)

	require.Len(t, out, 1)
	assert.Equal(t, []string{"INBOX", "Personal", "yaad-ingested"}, out[0].Labels,
		"duplicate labels collapse, output sorted")
}

// TestDedupFetchedByUID_PhantomOnly: a UID that appears ONLY as
// empty-body responses (no companion BODY[] response in the cycle —
// genuinely empty message or pathological Gmail state) emits one
// entry with empty Body + nil ReadErr. Downstream ParseMessage will
// fail with EOF, which is the right behavior — the de-dup layer
// surfaces the empty state once instead of N times.
func TestDedupFetchedByUID_PhantomOnly(t *testing.T) {
	t.Parallel()
	raws := []FetchedMessage{
		{UID: 99, Body: nil, Labels: []string{"INBOX"}},
		{UID: 99, Body: nil, Labels: []string{"yaad-ingested"}},
	}
	out := dedupFetchedByUID([]uint32{99}, raws)

	require.Len(t, out, 1, "phantom-only UID still emits one entry; downstream handles empty-body")
	assert.Empty(t, out[0].Body)
	assert.Nil(t, out[0].ReadErr)
	assert.Equal(t, []string{"INBOX", "yaad-ingested"}, out[0].Labels)
}

// TestDedupFetchedByUID_ReadErrPreservedThenCleared: ReadErr on
// the first response is preserved while body stays empty; a later
// response with non-empty Body clears ReadErr (transient-then-
// recovery in the same cycle).
func TestDedupFetchedByUID_ReadErrPreservedThenCleared(t *testing.T) {
	t.Parallel()
	readErr := errors.New("transient stream EOF")
	body := []byte("Message-ID: <c@x>\r\n\r\ndone\r\n")

	// Case A: ReadErr only — no recovery in this cycle.
	outA := dedupFetchedByUID([]uint32{1}, []FetchedMessage{
		{UID: 1, Body: nil, ReadErr: readErr},
	})
	require.Len(t, outA, 1)
	assert.Empty(t, outA[0].Body)
	assert.ErrorIs(t, outA[0].ReadErr, readErr, "single-response ReadErr preserved")

	// Case B: ReadErr first, then a clean body — ReadErr cleared.
	outB := dedupFetchedByUID([]uint32{1}, []FetchedMessage{
		{UID: 1, Body: nil, ReadErr: readErr},
		{UID: 1, Body: body, Labels: []string{"INBOX"}},
	})
	require.Len(t, outB, 1)
	assert.Equal(t, body, outB[0].Body)
	assert.Nil(t, outB[0].ReadErr, "successor body clears earlier ReadErr — same-cycle recovery")
}

// TestDedupFetchedByUID_RequestOrder pins that output follows the
// input `uids` slice order, not msgCh arrival order. Tests +
// poll-loop log lines depend on this for stability.
func TestDedupFetchedByUID_RequestOrder(t *testing.T) {
	t.Parallel()
	raws := []FetchedMessage{
		// Arrival order intentionally scrambled vs. request order.
		{UID: 3, Body: []byte("Message-ID: <3@x>\r\n\r\n"), Labels: []string{"INBOX"}},
		{UID: 1, Body: []byte("Message-ID: <1@x>\r\n\r\n"), Labels: []string{"INBOX"}},
		{UID: 2, Body: []byte("Message-ID: <2@x>\r\n\r\n"), Labels: []string{"INBOX"}},
	}
	out := dedupFetchedByUID([]uint32{1, 2, 3}, raws)

	require.Len(t, out, 3)
	assert.Equal(t, uint32(1), out[0].UID)
	assert.Equal(t, uint32(2), out[1].UID)
	assert.Equal(t, uint32(3), out[2].UID)
}

// TestDedupFetchedByUID_ServerSilent: a requested UID with no
// matching response (e.g. mid-cycle deletion) is skipped from
// output — caller correlates by UID on the returned slice.
func TestDedupFetchedByUID_ServerSilent(t *testing.T) {
	t.Parallel()
	raws := []FetchedMessage{
		{UID: 1, Body: []byte("Message-ID: <1@x>\r\n\r\n"), Labels: []string{"INBOX"}},
	}
	out := dedupFetchedByUID([]uint32{1, 2, 3}, raws)

	require.Len(t, out, 1, "only the responded UID emits; missing UIDs silently skipped")
	assert.Equal(t, uint32(1), out[0].UID)
}

// TestDedupFetchedByUID_DuplicateInputUIDs: a defensive case where
// the caller passes a `uids` slice with duplicates. Output emits
// each UID once, in first-appearance order.
func TestDedupFetchedByUID_DuplicateInputUIDs(t *testing.T) {
	t.Parallel()
	raws := []FetchedMessage{
		{UID: 1, Body: []byte("Message-ID: <1@x>\r\n\r\n"), Labels: []string{"INBOX"}},
	}
	out := dedupFetchedByUID([]uint32{1, 1, 1}, raws)

	require.Len(t, out, 1, "duplicate input UIDs collapse to a single output entry")
	assert.Equal(t, uint32(1), out[0].UID)
}

// TestDedupFetchedByUID_EmptyInputs: zero-message inputs and a
// nil raws slice both produce an empty output slice without
// panicking.
func TestDedupFetchedByUID_EmptyInputs(t *testing.T) {
	t.Parallel()
	assert.Empty(t, dedupFetchedByUID(nil, nil))
	assert.Empty(t, dedupFetchedByUID([]uint32{1, 2}, nil))
	assert.Empty(t, dedupFetchedByUID(nil, []FetchedMessage{{UID: 1, Body: []byte("x")}}))
}

// TestDedupFetchedByUID_NoLabelsResponse: when no response carries
// X-GM-LABELS, the output entry's Labels stays nil (vs. empty
// slice). ParseMessage tolerates both shapes via `append([]string{},
// labels...)`, so the choice is internal — pinning nil here
// guards against accidental allocation of empty []string{}.
func TestDedupFetchedByUID_NoLabelsResponse(t *testing.T) {
	t.Parallel()
	raws := []FetchedMessage{
		{UID: 1, Body: []byte("Message-ID: <1@x>\r\n\r\n"), Labels: nil},
	}
	out := dedupFetchedByUID([]uint32{1}, raws)

	require.Len(t, out, 1)
	assert.Nil(t, out[0].Labels, "no labels in any response → nil Labels on output (not [] slice)")
}
