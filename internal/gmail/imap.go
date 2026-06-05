// IMAP client interface + the go-imap v1 + X-GM-LABELS extension
// adapter. The interface (Client below) is the seam tests use: the
// poll loop calls into Client; production wires it to a v1
// `*client.Client` from emersion's go-imap; tests wire it to a mock
// that records calls + replays canned responses.
//
// **Why v1, not v2.** go-imap/v2-beta's typed FetchOptions /
// FetchItemData closed-discriminated-union surface has no extension
// hook for X-GM-LABELS — unknown FETCH attributes get silently
// discarded by the response parser. v1's `Message.Items
// map[FetchItem]interface{}` is the open-extension surface
// upstream documented for exactly this case ("This map's values
// should not be used directly, they must only be used by libraries
// implementing extensions of the IMAP protocol"). Per
// a prior PR review: shipping without a working X-GM-LABELS round-trip
// delivers a known-broken plugin since the label flow is the
// load-bearing feature; v1 is the path that delivers it without
// forking upstream.

package gmail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/responses"
)

// gmailLabelsItem is the FETCH attribute key for Gmail's
// X-GM-LABELS extension. Used as both the request item (passed in
// the FetchItem slice on UidFetch) and the response key under
// `Message.Items` after the response parses.
const gmailLabelsItem imap.FetchItem = "X-GM-LABELS"

// gmailLabelsStoreItem is the STORE flag-add operator for the
// X-GM-LABELS extension. `+X-GM-LABELS` adds labels; `-X-GM-LABELS`
// removes; bare `X-GM-LABELS` replaces. yaad-gmail only uses the
// add form (writing `ingested_label` after a successful ingest).
const gmailLabelsStoreItem imap.StoreItem = "+X-GM-LABELS"

// FetchedMessage is the per-message bundle the IMAP client returns
// to the poll loop. Carries the raw RFC-822 body bytes (for parser
// + vault clean_content), the per-message Gmail label list
// (parsed out of the X-GM-LABELS response attribute), and the
// IMAP UID (used for the post-ingest STORE +X-GM-LABELS write).
//
// ReadErr, when non-nil, names a body-stream read failure
// (typically io.ReadAll on the FETCH BODY[] reader returning
// an err). The poll loop checks this BEFORE attempting to parse;
// downstream parse would just see empty Body and fail with a
// less informative error class. Recovery is implicit via the
// polling cycle — the message still doesn't carry ingested_label,
// so the next SearchUningested returns its UID + the next fetch
// re-issues against a (hopefully) recovered stream.
type FetchedMessage struct {
	UID     uint32
	Body    []byte
	Labels  []string
	ReadErr error
}

// Client is the surface the poll loop talks to. Production wires
// it to a real go-imap v1 connection; tests wire it to an
// in-process fake.
type Client interface {
	// SelectFolder opens the named IMAP folder for subsequent
	// search/fetch/store operations. Implementations switch via
	// IMAP SELECT; consecutive calls with the same name are
	// idempotent (v1's Select is idempotent server-side).
	SelectFolder(ctx context.Context, folder string) error

	// SearchUningested returns the UIDs of messages in the
	// currently-selected folder that don't carry `ingestedLabel`
	// AND don't carry `skipLabel`. Empty `ingestedLabel` skips the
	// negative-label half (so EVERY message matches the predicate
	// — every poll re-fetches the whole folder, see operator-opt-out
	// note in the spec); empty `skipLabel` skips the skip-check.
	//
	// Both empty → the search returns every message in the folder.
	// Implementation routes through Gmail's `X-GM-RAW` search
	// criterion.
	SearchUningested(ctx context.Context, ingestedLabel, skipLabel string) ([]uint32, error)

	// FetchMessages retrieves the raw RFC-822 body + X-GM-LABELS
	// list for each UID in the selected folder. Order is not
	// guaranteed; caller correlates by UID on the returned slice.
	// Empty `uids` returns an empty slice + nil error.
	FetchMessages(ctx context.Context, uids []uint32) ([]FetchedMessage, error)

	// MarkIngested writes `ingestedLabel` onto the message via
	// STORE +X-GM-LABELS. Empty `ingestedLabel` is a no-op (the
	// operator opted out of the auto-write per the spec). Returns
	// nil when the STORE succeeded; non-nil on transport error.
	MarkIngested(ctx context.Context, uid uint32, ingestedLabel string) error

	// Close shuts the IMAP connection. Idempotent (a second close
	// is a no-op).
	Close() error
}

// IMAPConfig carries the operator config the IMAP client needs at
// dial time. Host + Port default to imap.gmail.com:993 (Defaults
// applied at the constructor); AccountEmail + AppPassword are
// required.
type IMAPConfig struct {
	Host string
	Port int
	AccountEmail string
	AppPassword string
}

// Validate returns nil when the config carries enough data to dial
// + auth. Per the spec, AccountEmail + AppPassword have no defaults;
// missing either is a startup-fatal error.
func (c IMAPConfig) Validate() error {
	if c.AccountEmail == "" {
		return errors.New("gmail: IMAP config missing account_email")
	}
	if c.AppPassword == "" {
		return errors.New("gmail: IMAP config missing app_password")
	}
	return nil
}

// realClient is the v1 + X-GM-LABELS adapter that satisfies the
// Client interface against a live Gmail connection. Tests do NOT
// exercise this path — they substitute a fake at the Client
// interface boundary.
type realClient struct {
	conn *client.Client
}

// Dial opens a TLS IMAP connection to host:port and authenticates
// via LOGIN. Returns the live Client wrapper.
func Dial(_ context.Context, cfg IMAPConfig) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	host := cfg.Host
	if host == "" {
		host = DefaultIMAPHost
	}
	port := cfg.Port
	if port == 0 {
		port = DefaultIMAPPort
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	conn, err := client.DialTLS(addr, nil)
	if err != nil {
		return nil, fmt.Errorf("gmail: imap dial %s: %w", addr, err)
	}
	if err := conn.Login(cfg.AccountEmail, cfg.AppPassword); err != nil {
		_ = conn.Logout()
		return nil, fmt.Errorf("gmail: imap login %s: %w", cfg.AccountEmail, err)
	}
	return &realClient{conn: conn}, nil
}

func (c *realClient) SelectFolder(_ context.Context, folder string) error {
	if _, err := c.conn.Select(folder, false); err != nil {
		return fmt.Errorf("gmail: imap select %s: %w", folder, err)
	}
	return nil
}

// SearchUningested issues a UID SEARCH with the X-GM-RAW negative-
// label predicate. Gmail's IMAP search recognises `-label:<name>`
// as the absence-of-label predicate; combining two with whitespace
// AND-joins them.
//
// When BOTH labels are empty the function returns every UID in the
// folder via a bare ALL search — the operator opted out of all
// label-based gating and the poll loop re-emits the whole folder
// every cycle (per the spec's empty-string-disables note on the
// IngestedLabel knob).
//
// Why not SearchCriteria.Header (per #56): go-imap v1's
// SearchCriteria.Header serializes as `HEADER "<key>" "<value>"`
// for non-canonical headers, which is the RFC 3501 header-search
// criterion — Gmail reads it as "find messages whose X-GM-RAW
// header matches" (no such header) and returns 0. The correct
// wire shape for the vendor extension is the top-level
// `X-GM-RAW "<predicate>"` criterion, which we emit via a custom
// commander rather than fighting the SearchCriteria abstraction.
func (c *realClient) SearchUningested(_ context.Context, ingestedLabel, skipLabel string) ([]uint32, error) {
	predicate := buildSearchPredicate(ingestedLabel, skipLabel)
	if predicate == "" {
		// Both labels disabled: bare ALL search through the
		// standard path. No vendor extension needed.
		uids, err := c.conn.UidSearch(imap.NewSearchCriteria())
		if err != nil {
			return nil, fmt.Errorf("gmail: imap search: %w", err)
		}
		return uids, nil
	}

	cmd := &xGMRawSearchCommand{Predicate: predicate}
	res := &responses.Search{}
	status, err := c.conn.Execute(cmd, res)
	if err != nil {
		return nil, fmt.Errorf("gmail: imap search: %w", err)
	}
	if err := status.Err(); err != nil {
		return nil, fmt.Errorf("gmail: imap search status: %w", err)
	}
	return res.Ids, nil
}

// xGMRawSearchCommand emits `UID SEARCH X-GM-RAW "<predicate>"`
// directly on the wire. go-imap v1's SearchCriteria has no escape
// hatch for vendor criteria — Format() walks RFC-shaped fields
// only — so we bypass it with a custom Commander per #56.
//
// The Predicate is a string (not RawString): IMAP serialization
// quotes it (or switches to literal form for embedded specials),
// matching Python imaplib's `mail.uid('search', None, 'X-GM-RAW',
// '"<predicate>"')` wire shape that Gmail accepts.
type xGMRawSearchCommand struct {
	Predicate string
}

// Command implements imap.Commander.
func (cmd *xGMRawSearchCommand) Command() *imap.Command {
	return &imap.Command{
		Name: "UID",
		Arguments: []interface{}{
			imap.RawString("SEARCH"),
			imap.RawString("X-GM-RAW"),
			cmd.Predicate,
		},
	}
}

// buildSearchPredicate composes the X-GM-RAW search string from
// the configured label slot pair. Each label value is quoted so a
// label containing whitespace (e.g. `Job Search/Active`, a nested
// label, or any space-bearing name) doesn't silently split into
// separate search terms and shift the result set (#450).
func buildSearchPredicate(ingestedLabel, skipLabel string) string {
	parts := []string{}
	if ingestedLabel != "" {
		parts = append(parts, "-label:"+quoteLabelValue(ingestedLabel))
	}
	if skipLabel != "" {
		parts = append(parts, "-label:"+quoteLabelValue(skipLabel))
	}
	return strings.Join(parts, " ")
}

// quoteLabelValue renders a label value safe for the Gmail X-GM-RAW
// search syntax. A bare value whose tokens contain whitespace splits
// into separate query terms (`-label:My Label` parses as `-label:My`
// AND a bare `Label`), and an embedded quote breaks the phrase. Wrap
// the value in double quotes and backslash-escape any embedded
// backslash or quote so the Gmail parser reads it as one quoted label.
// Quoting unconditionally (even single-token values) is accepted by
// Gmail and keeps the predicate uniform. The whole predicate is then
// IMAP-quoted again by go-imap on the wire; the two encodings compose.
func quoteLabelValue(label string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(label)
	return `"` + escaped + `"`
}

// FetchMessages issues a UID FETCH for each UID requesting BODY[]
// + UID + X-GM-LABELS. Returns one FetchedMessage per message; the
// slice may be shorter than `uids` if the server returned no data
// for some UIDs (e.g. mid-poll deletion).
//
// X-GM-LABELS is requested as a non-standard FetchItem; v1's
// `Message.Items` map carries arbitrary FetchItem→raw-data pairs
// (the open-extension surface upstream documented for exactly
// this case — quoting v1's Message struct: "This map's values
// should not be used directly, they must only be used by libraries
// implementing extensions of the IMAP protocol"). parseLabels
// normalizes the response value into a flat []string.
func (c *realClient) FetchMessages(_ context.Context, uids []uint32) ([]FetchedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)

	bodySection := &imap.BodySectionName{} // BODY[] — full message
	items := []imap.FetchItem{
		imap.FetchUid,
		bodySection.FetchItem(),
		gmailLabelsItem,
	}

	// Buffered for >1 response per UID (Gmail double-emit, see
	// dedupFetchedByUID note below). Headroom (2×) absorbs the
	// observed 11-responses-for-8-UIDs ratio without blocking the
	// UidFetch goroutine.
	msgCh := make(chan *imap.Message, len(uids)*2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.conn.UidFetch(seqSet, items, msgCh)
	}()

	raws := make([]FetchedMessage, 0, len(uids)*2)
	for msg := range msgCh {
		fm := FetchedMessage{UID: msg.Uid}
		// Body bytes from the BODY[] section. v1's Body map keys
		// are the requested *BodySectionName values; reading any
		// non-empty section into bytes is enough.
		//
		// io.ReadAll errors used to be discarded (pre-#58), which
		// silently produced empty-body messages on transient stream
		// failures. The bytes are now captured + the err parked on
		// FetchedMessage.ReadErr; the poll loop reads it BEFORE
		// invoking ParseMessage so the operator sees the actual
		// failure class instead of a downstream "missing Message-ID"
		// from the empty-body fallout.
		for _, lit := range msg.Body {
			if lit == nil {
				continue
			}
			b, err := io.ReadAll(lit)
			if err != nil {
				fm.ReadErr = err
			} else {
				fm.Body = b
			}
			break
		}
		// X-GM-LABELS extension data lives in Items under the
		// non-standard FetchItem key. The value is a slice of
		// imap.RawString (or interface{} after generic parse) —
		// parseLabels normalizes both encodings into []string.
		if raw, ok := msg.Items[gmailLabelsItem]; ok {
			fm.Labels = parseLabels(raw)
		}
		raws = append(raws, fm)
	}
	if err := <-errCh; err != nil {
		return nil, fmt.Errorf("gmail: imap fetch: %w", err)
	}
	return dedupFetchedByUID(uids, raws), nil
}

// dedupFetchedByUID collapses per-UID duplicate FETCH responses into
// one FetchedMessage per UID, preserving the input `uids` slice's
// order on output.
//
// **Why this exists.** Gmail's IMAP server emits MORE THAN ONE FETCH
// response per UID when X-GM-LABELS is requested alongside BODY[] —
// typically one response carrying BODY[] (+ labels), one carrying
// only labels (no BODY[] section). The naive 1-FetchedMessage-per-
// msgCh-msg shape produced phantom empty-body entries that then
// failed downstream ParseMessage with EOF (yaad-index #60).
//
// Merge rules:
//   - Body: first non-empty wins. Phantom responses with empty Body
//     don't overwrite a previously captured body.
//   - ReadErr: cleared when a later response yields non-empty Body
//     (transient-then-recovery in the same cycle); otherwise the
//     first observed ReadErr is preserved.
//   - Labels: set-union across all responses. Defensive — Gmail's
//     second response may carry the full label snapshot OR only a
//     subset; set-merge tolerates both without dropping labels on
//     a split-snapshot edge case. Sorted for deterministic output.
//
// UIDs the server returned nothing for are skipped. Duplicates
// within the input `uids` slice are also de-duplicated on output.
func dedupFetchedByUID(uids []uint32, raws []FetchedMessage) []FetchedMessage {
	type accumulator struct {
		body    []byte
		readErr error
		labels  map[string]struct{}
	}
	byUID := make(map[uint32]*accumulator, len(uids))
	for _, r := range raws {
		acc, ok := byUID[r.UID]
		if !ok {
			acc = &accumulator{labels: map[string]struct{}{}}
			byUID[r.UID] = acc
		}
		if len(acc.body) == 0 {
			if len(r.Body) > 0 {
				acc.body = r.Body
				acc.readErr = nil
			} else if r.ReadErr != nil && acc.readErr == nil {
				acc.readErr = r.ReadErr
			}
		}
		for _, l := range r.Labels {
			acc.labels[l] = struct{}{}
		}
	}

	out := make([]FetchedMessage, 0, len(byUID))
	seen := make(map[uint32]struct{}, len(byUID))
	for _, uid := range uids {
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		acc, ok := byUID[uid]
		if !ok {
			continue
		}
		fm := FetchedMessage{UID: uid, Body: acc.body, ReadErr: acc.readErr}
		if len(acc.labels) > 0 {
			labels := make([]string, 0, len(acc.labels))
			for l := range acc.labels {
				labels = append(labels, l)
			}
			sort.Strings(labels)
			fm.Labels = labels
		}
		out = append(out, fm)
	}
	return out
}

// parseLabels normalizes the X-GM-LABELS response value into a
// flat []string. v1's response decoder lands the parenthesized
// list value as either `[]interface{}` (each element a string or
// imap.RawString) or a bare slice — handle both shapes plus a
// defensive single-string fallback.
//
// Gmail's X-GM-LABELS response uses IMAP atom or quoted-string
// per label. Special label names (system folders like
// `\Important`, `\Sent`, `\Inbox`) start with a backslash and
// arrive verbatim — the assembler treats them like any other
// label string (the LabelSlug encoder handles the backslash via
// clean-slug normalization).
func parseLabels(raw any) []string {
	switch v := raw.(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s := stringifyLabelToken(it); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		// Defensive: a bare-string response (rare; shouldn't
		// happen for parenthesized list responses but the
		// decoder might land a single-label case here).
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

// stringifyLabelToken extracts the string form from one X-GM-LABELS
// list element. v1's response decoder may land each element as
// `string`, `imap.RawString`, or a typed variant — the type
// assertions below cover the observed shapes; unknown types log a
// silent skip rather than panic.
func stringifyLabelToken(token any) string {
	switch v := token.(type) {
	case string:
		return v
	case imap.RawString:
		return string(v)
	}
	return ""
}

// MarkIngested issues UID STORE +X-GM-LABELS (label) for the
// given UID. Empty `ingestedLabel` is a no-op (operator opted out
// of the auto-write).
func (c *realClient) MarkIngested(_ context.Context, uid uint32, ingestedLabel string) error {
	if ingestedLabel == "" {
		return nil
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	// v1's UidStore takes a value as `interface{}` because the
	// extension shape is open. For X-GM-LABELS the canonical
	// encoding is a parenthesized list of label names — passing
	// a `[]interface{}` of label strings serializes correctly.
	value := []interface{}{imap.RawString(ingestedLabel)}
	if err := c.conn.UidStore(seqSet, gmailLabelsStoreItem, value, nil); err != nil {
		return fmt.Errorf("gmail: imap store +X-GM-LABELS uid=%d: %w", uid, err)
	}
	return nil
}

func (c *realClient) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Logout()
	c.conn = nil
	return err
}
