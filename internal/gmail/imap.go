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
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
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
type FetchedMessage struct {
	UID uint32
	Body []byte
	Labels []string
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

// SearchUningested issues a SEARCH with the X-GM-RAW negative-
// label predicate. Gmail's IMAP search recognises `-label:<name>`
// as the absence-of-label predicate; combining two with whitespace
// AND-joins them.
//
// When BOTH labels are empty the function returns every UID in the
// folder via a bare ALL search — the operator opted out of all
// label-based gating and the poll loop re-emits the whole folder
// every cycle (per the spec's empty-string-disables note on the
// IngestedLabel knob).
func (c *realClient) SearchUningested(_ context.Context, ingestedLabel, skipLabel string) ([]uint32, error) {
	criteria := imap.NewSearchCriteria()
	predicate := buildSearchPredicate(ingestedLabel, skipLabel)
	if predicate != "" {
		// Gmail's X-GM-RAW search criterion accepts the
		// search-syntax string operators recognise on the
		// gmail.com web UI ("-label:foo bar" etc.). v1's
		// SearchCriteria carries arbitrary header criteria via
		// the Header map; X-GM-RAW is keyed there.
		criteria.Header = map[string][]string{"X-GM-RAW": {predicate}}
	}
	uids, err := c.conn.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("gmail: imap search: %w", err)
	}
	return uids, nil
}

// buildSearchPredicate composes the X-GM-RAW search string from
// the configured label slot pair.
func buildSearchPredicate(ingestedLabel, skipLabel string) string {
	parts := []string{}
	if ingestedLabel != "" {
		parts = append(parts, "-label:"+ingestedLabel)
	}
	if skipLabel != "" {
		parts = append(parts, "-label:"+skipLabel)
	}
	return strings.Join(parts, " ")
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

	msgCh := make(chan *imap.Message, len(uids))
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.conn.UidFetch(seqSet, items, msgCh)
	}()

	out := make([]FetchedMessage, 0, len(uids))
	for msg := range msgCh {
		fm := FetchedMessage{UID: msg.Uid}
		// Body bytes from the BODY[] section. v1's Body map keys
		// are the requested *BodySectionName values; reading any
		// non-empty section into bytes is enough.
		for _, lit := range msg.Body {
			if lit == nil {
				continue
			}
			b, _ := io.ReadAll(lit)
			fm.Body = b
			break
		}
		// X-GM-LABELS extension data lives in Items under the
		// non-standard FetchItem key. The value is a slice of
		// imap.RawString (or interface{} after generic parse) —
		// parseLabels normalizes both encodings into []string.
		if raw, ok := msg.Items[gmailLabelsItem]; ok {
			fm.Labels = parseLabels(raw)
		}
		out = append(out, fm)
	}
	if err := <-errCh; err != nil {
		return nil, fmt.Errorf("gmail: imap fetch: %w", err)
	}
	return out, nil
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
