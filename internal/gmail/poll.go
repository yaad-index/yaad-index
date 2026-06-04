// Polling pipeline + per-message ingest assembly. The Poller drives
// the Client through one fetch/ingest cycle (Tick) or runs an
// indefinite ticker loop (Run); production wires it to a real
// IMAP-backed Client via Dial, tests substitute a fake Client.

package gmail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// IngestEnvelope is the per-message bundle the Poller emits to the
// Emit hook for hand-off to the daemon's plugin protocol layer.
// Carries everything main.go needs to serialize the source-shape
// entity + edges JSON the daemon expects.
type IngestEnvelope struct {
	// SourceID is the full source-shape id `gmail:<source-slug>`.
	SourceID string
	// MessageID is the RFC-822 Message-ID header (angle brackets
	// stripped). Used by the wire layer as the per-message subdir
	// name under attach.StagingDir() when staging MIME-walked
	// attachments to disk.
	MessageID string
	// Subject + Date come from the parsed RFC-822 headers.
	Subject string
	Date time.Time
	// From / To / Cc / Bcc carry the parsed RFC-822 address
	// headers per #272. Each entry is a bare email address
	// (display-name stripped by ParseMessage's net/mail parse).
	// From is single-valued (RFC-5322 multi-from collapses to
	// the first address per ParseMessage's contract); empty
	// when the source carries no From header. To / Cc are
	// inbound on both folders; Bcc is populated only when the
	// source message came from the sent folder (inbound BCC
	// headers don't reach the recipient reliably).
	//
	// Surfaced through the wire layer into entity.data so
	// workflow CEL predicates can read
	// `entity.data.from == "noreply@example.com"` directly
	// without regexing the clean_content body.
	From string
	To   []string
	Cc   []string
	Bcc  []string
	// ForwardedFrom / ForwardedSubject carry the original sender +
	// un-prefixed subject for Gmail-forwarded messages (#323), surfaced
	// into entity.data as additive `forwarded_from` / `forwarded_subject`
	// fields. Empty for non-forwards (and ForwardedFrom empty when the
	// embedded `From:` isn't recoverable). The envelope `From:`
	// (data.from) still carries the forwarder — both identities preserved.
	ForwardedFrom string
	ForwardedSubject string
	// Body is the raw RFC-822 body bytes for vault clean_content.
	Body []byte
	// Edges is the cross-canonical edge list (is_about + from/to/
	// cc/bcc + tagged_as) per AssembleEdges. is_a / source-type is
	// applied at the wire-emission layer (main.go), not here.
	Edges []Edge
	// HTMLBody is the text/html alternative body extracted from the
	// MIME tree per yaad-index #12. Empty when the message has no
	// HTML alternative. The wire layer stages this under
	// attach.StagingDir()/<message-id>/body.html and emits a
	// `role: html-body` ADR-0014 attachment when non-empty.
	HTMLBody []byte
	// Attachments lists the per-message binary / non-text MIME parts
	// (Content-Disposition attachment, inline-above-threshold) the
	// MIME walker surfaced. The wire layer stages each under
	// attach.StagingDir()/<message-id>/<part-index>.<ext> and emits
	// `role: attachment` ADR-0014 entries.
	Attachments []MIMEAttachment
}

// EmitFunc is the per-envelope hand-off the Poller calls. Returns
// nil on successful emission (Poller then writes the
// `ingestedLabel` via Client.MarkIngested); non-nil halts the
// label-write so the next polling cycle re-attempts the same
// message.
type EmitFunc func(ctx context.Context, env IngestEnvelope) error

// Poller orchestrates one full fetch-and-ingest cycle. Driven by
// Tick (single cycle) or Run (ticker-based loop). Stateless across
// cycles — the IMAP-side `ingestedLabel` is the durable seen-set,
// per the spec's restart-safety property.
type Poller struct {
	Client Client
	Folders []string
	IngestedLabel string
	SkipLabel string
	SentFolder string
	Emit EmitFunc
	Logger *slog.Logger
}

// NewPoller constructs a Poller with the standard folder list
// (INBOX + [Gmail]/Sent Mail), the configured label slots, and the
// provided emit hook. The Logger defaults to a discarding handler
// when nil.
func NewPoller(client Client, ingestedLabel, skipLabel string, emit EmitFunc, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Poller{
		Client: client,
		Folders: []string{InboxFolderName, SentFolderName},
		IngestedLabel: ingestedLabel,
		SkipLabel: skipLabel,
		SentFolder: SentFolderName,
		Emit: emit,
		Logger: logger,
	}
}

// Tick runs one polling cycle: for each folder in the configured
// list, search the un-ingested set, fetch each message, parse,
// emit, and mark-ingested on success. Returns the count of
// messages successfully ingested across all folders + any errors
// encountered during the cycle (errors are logged + accumulated;
// the cycle continues past per-message failures so one bad
// message doesn't block the rest of the queue).
func (p *Poller) Tick(ctx context.Context) (ingested int, errs []error) {
	for _, folder := range p.Folders {
		if err := p.Client.SelectFolder(ctx, folder); err != nil {
			errs = append(errs, fmt.Errorf("select %s: %w", folder, err))
			continue
		}

		uids, err := p.Client.SearchUningested(ctx, p.IngestedLabel, p.SkipLabel)
		if err != nil {
			errs = append(errs, fmt.Errorf("search %s: %w", folder, err))
			continue
		}
		p.Logger.Debug("gmail poll: search result",
			"folder", folder, "uid_count", len(uids))
		if len(uids) == 0 {
			continue
		}

		fetched, err := p.Client.FetchMessages(ctx, uids)
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch %s: %w", folder, err))
			continue
		}
		p.Logger.Debug("gmail poll: fetch result",
			"folder", folder, "requested_count", len(uids), "fetched_count", len(fetched))

		isSent := folder == p.SentFolder
		for _, fm := range fetched {
			// Body-stream read failure from FetchMessages (io.ReadAll
			// on the FETCH BODY[] reader returning an err). Pre-#58
			// the err was discarded and the message fell through with
			// an empty Body, producing a downstream "missing
			// Message-ID" that hid the real cause. Surface it WARN-
			// level so the operator sees the actual class; skip parse
			// + emit + mark — the message stays uningested, so the
			// next polling cycle re-issues a fetch (implicit retry).
			if fm.ReadErr != nil {
				p.Logger.Warn("gmail poll: body read failed; skipping message",
					"folder", folder, "uid", fm.UID, "err", fm.ReadErr)
				errs = append(errs, fmt.Errorf("read %s uid=%d: %w", folder, fm.UID, fm.ReadErr))
				continue
			}
			pm, err := ParseMessage(fm.Body, fm.Labels, isSent)
			if err != nil {
				if errors.Is(err, ErrMissingMessageID) {
					p.Logger.Debug("gmail poll: skipping message with no Message-ID",
						"folder", folder, "uid", fm.UID,
						"body_preview", bodyPreview(fm.Body))
					continue
				}
				p.Logger.Debug("gmail poll: parse failed",
					"folder", folder, "uid", fm.UID, "err", err,
					"body_preview", bodyPreview(fm.Body))
				errs = append(errs, fmt.Errorf("parse %s uid=%d: %w", folder, fm.UID, err))
				continue
			}
			p.Logger.Debug("gmail poll: parsed message",
				"folder", folder, "uid", fm.UID,
				"message_id", pm.MessageID,
				"subject_len", len(pm.Subject))

			// MIME walk for ADR-0014 attachment emission per #12. Errors
			// here are non-fatal: a message whose MIME tree the walker
			// can't parse still emits its source-shape + edges; the
			// attachment surface stays empty for that envelope. The
			// walker handles malformed Content-Type by returning empty
			// results, so the only error path is a fundamental
			// rfc-822 parse failure — log it + continue.
			htmlBody, attachments, mimeErr := WalkMIMEParts(fm.Body)
			if mimeErr != nil {
				p.Logger.Debug("gmail poll: mime walk failed; continuing without attachments",
					"folder", folder, "uid", fm.UID, "err", mimeErr)
			}

			env := IngestEnvelope{
				SourceID:    SourceNamespace + ":" + SourceSlug(pm.Subject, pm.MessageID),
				MessageID:   pm.MessageID,
				Subject:     pm.Subject,
				Date:        pm.Date,
				From:        pm.From,
				To:          pm.To,
				Cc:          pm.Cc,
				Bcc:         pm.Bcc,
				ForwardedFrom: pm.ForwardedFrom,
				ForwardedSubject: pm.ForwardedSubject,
				Body:        pm.Body,
				Edges:       AssembleEdges(pm, p.IngestedLabel, p.SkipLabel),
				HTMLBody:    htmlBody,
				Attachments: attachments,
			}

			if err := p.Emit(ctx, env); err != nil {
				p.Logger.Debug("gmail poll: emit failed",
					"folder", folder, "uid", fm.UID, "err", err)
				errs = append(errs, fmt.Errorf("emit %s uid=%d: %w", folder, fm.UID, err))
				continue
			}
			p.Logger.Debug("gmail poll: emitted envelope",
				"folder", folder, "uid", fm.UID, "source_id", env.SourceID)

			if err := p.Client.MarkIngested(ctx, fm.UID, p.IngestedLabel); err != nil {
				// Mark-ingested failure is non-fatal — the
				// message ingested successfully, but the
				// label-write didn't land. Next cycle re-attempts
				// (the search predicate still matches). Log + keep
				// going.
				p.Logger.Warn("gmail poll: mark-ingested label-write failed; will re-attempt next cycle",
					"folder", folder, "uid", fm.UID, "err", err)
				errs = append(errs, fmt.Errorf("mark-ingested %s uid=%d: %w", folder, fm.UID, err))
				continue
			}
			p.Logger.Debug("gmail poll: marked ingested",
				"folder", folder, "uid", fm.UID)
			ingested++
		}
	}
	return ingested, errs
}

// Run drives Tick on a ticker until the context cancels. Returns
// the context's error on cancel; per-cycle errors are logged via
// the Poller's logger but don't halt the loop.
func (p *Poller) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("gmail: polling interval must be > 0")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run an initial Tick immediately so the first poll happens
	// at startup rather than after one full interval.
	if err := p.runOneTick(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.runOneTick(ctx); err != nil {
				return err
			}
		}
	}
}

// bodyPreview returns a bounded, %q-quoted slice of body for debug
// logs on parse-error branches. The cap (96 bytes) keeps the line
// short enough to scan while still surfacing typical RFC-822 header
// prefixes like `Message-ID:` / `From:` that diagnose body-content
// shape. Empty input renders as `""`.
func bodyPreview(body []byte) string {
	const cap = 96
	if len(body) <= cap {
		return fmt.Sprintf("%q", body)
	}
	return fmt.Sprintf("%q…", body[:cap])
}

func (p *Poller) runOneTick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	count, errs := p.Tick(ctx)
	if len(errs) > 0 {
		for _, err := range errs {
			p.Logger.Warn("gmail poll: cycle error",
				"err", err)
		}
	}
	if count > 0 {
		p.Logger.Info("gmail poll: ingested",
			"count", count, "errors", len(errs))
	}
	return nil
}
