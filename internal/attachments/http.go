package attachments

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// httpUserAgent is the default User-Agent the daemon sends on
// `https://` / `http://` attachment fetches. Plain string — no
// version pinning yet (yaad-index/<version> when buildinfo lands
// here).
const httpUserAgent = "yaad-index/0"

// httpMaxRedirects caps redirect chains per ADR-0014 §2.https.
// http.Client's default would follow up to 10 — we tighten to 5 for
// the same defense-in-depth reason as the 60s timeout.
const httpMaxRedirects = 5

// handleHTTP implements the `https://<url>` (and `http://<url>`)
// scheme per ADR-0014 §2.https. The plugin emits an upstream URL;
// the daemon GETs it, follows up to 5 redirects, applies a 60s
// timeout, and writes the response body to dest. 4xx/5xx → error
// (caller drops the attachment with a WARN). The default User-Agent
// is `yaad-index/<version>`; no auth headers — plugins authenticating
// against private endpoints SHOULD use the `file://` scheme.
//
// The 60s timeout is enforced by the http.Client constructed in
// New (Dispatcher.httpClient.Timeout). The redirect cap is wired
// here per-request via CheckRedirect because http.Client's Timeout
// applies to the whole chain anyway (so a single 60s budget across
// redirects + body read is what we want).
func (d *Dispatcher) handleHTTP(ctx context.Context, a Attachment, dest string) error {
	// Per-request CheckRedirect — http.Client.CheckRedirect can be
	// set globally on d.httpClient, but two concurrent dispatches
	// would race on the field. Setting it through a request-scoped
	// http.Client clone keeps the fix local.
	client := *d.httpClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= httpMaxRedirects {
			return fmt.Errorf("redirect cap reached (%d hops)", httpMaxRedirects)
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URI, nil)
	if err != nil {
		return fmt.Errorf("build request for %q: %w", a.URI, err)
	}
	req.Header.Set("User-Agent", httpUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %q: %w", a.URI, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %d for %q", resp.StatusCode, a.URI)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir kind dir for %q: %w", dest, err)
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create dest %q: %w", dest, err)
	}
	if err := copyCapped(out, resp.Body, d.maxAttachmentBytes); err != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("write response body to %q: %w", dest, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("fsync dest %q: %w", dest, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dest %q: %w", dest, err)
	}
	return nil
}

// copyCapped streams src to dst, refusing once src exceeds max bytes.
// io.CopyN writes at most max+1 bytes: an io.EOF return means the source
// fit within the cap; any other nil error means max+1 bytes were
// available, i.e. the source is over the cap. Callers remove the partial
// dest on the returned error — the same fail-soft path as any other copy
// failure (ADR-0014 §5). Shared by the file:// and https:// handlers.
func copyCapped(dst io.Writer, src io.Reader, max int64) error {
	n, err := io.CopyN(dst, src, max+1)
	if err != nil && err != io.EOF {
		return err
	}
	if n > max {
		return fmt.Errorf("%w: limit %d bytes", ErrAttachmentTooLarge, max)
	}
	return nil
}
