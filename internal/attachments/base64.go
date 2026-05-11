package attachments

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// handleBase64 implements the `base64://<encoded>` scheme per
// ADR-0014 §2.base64. The plugin includes the bytes inline. We
// decode (RFC 4648 standard alphabet, padding optional) and write
// to dest.
//
// Padding optional: callers can drop trailing `=` per the
// padded/unpadded variants RFC 4648 allows. We try padded first
// (most common encoder default), fall through to RawStdEncoding
// (no padding) on padding-error.
//
// No URL-safe variant. Plugins that pre-encoded with the URL-safe
// alphabet should re-encode with the standard alphabet — ADR-0014
// §2.base64 is explicit about this to avoid the dispatch table
// growing to 4 schemes.
func (d *Dispatcher) handleBase64(a Attachment, dest string) error {
	if !strings.HasPrefix(a.URI, "base64://") {
		return fmt.Errorf("expected base64:// URI, got %q", a.URI)
	}
	encoded := strings.TrimPrefix(a.URI, "base64://")
	if encoded == "" {
		return fmt.Errorf("empty base64 payload")
	}

	bytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Padding-error fallback: try the no-padding variant.
		// RawStdEncoding accepts strings without trailing `=`.
		if rb, rerr := base64.RawStdEncoding.DecodeString(encoded); rerr == nil {
			bytes = rb
		} else {
			return fmt.Errorf("decode base64 (tried padded + unpadded): %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir kind dir for %q: %w", dest, err)
	}

	if err := os.WriteFile(dest, bytes, 0o644); err != nil {
		return fmt.Errorf("write base64 payload to %q: %w", dest, err)
	}
	return nil
}
