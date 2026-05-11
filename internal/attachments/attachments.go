// Package attachments dispatches plugin-emitted binary attachments to
// the vault per ADR-0014 (plugin attachment contract).
//
// A plugin emits an `attachments[]` array on its FetchResult, where
// each entry is a `{role, uri, extension}` triple. The daemon
// validates the shape (role + extension allowlists, path-traversal
// guard for `file://` URIs), dispatches on URI scheme to one of three
// handlers, and writes the resolved bytes to
// `<vault>/<kind>/<local-id>.<role>.<extension>`.
//
// **Fail-soft per attachment.** A bad role, a path-traversal attempt,
// a 4xx fetch — these log at WARN and the offending attachment is
// skipped; the rest of the entity still lands. The whole-entity
// failure modes (the entity .md write, the DB UpsertEntity) stay
// gated separately upstream; attachments are best-effort decorations
// on top.
//
// **Re-fetch hygiene.** The Dispatcher is given the previous fetch's
// `(role, uri)` set. A `(role, uri)` that matches a prior emission
// short-circuits the fetch and leaves the existing on-disk file
// untouched (per ADR-0014 §4). force-refetch (per alice2-index)
// bypasses this comparison and unconditionally refetches.
//
// **UGC preservation.** When a plugin emits no attachments on a
// re-ingest (or the array is missing entirely), existing on-disk
// attachments for the entity are PRESERVED — same shape as ADR-0012's
// user-content preservation contract. The dispatcher is only invoked
// when attachments are non-empty; the silence-preserves rule is
// enforced by the caller's invocation guard, not by walking the
// vault here.
package attachments

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// osStat is overridable for tests that want to bypass the real
// filesystem when synthesizing a Dispatch result.
var osStat = func(name string) (fs.FileInfo, error) { return os.Stat(name) }

// mimeForExt resolves a file extension (no leading dot) to a MIME
// type via the standard library's mime package. Returns "" when
// nothing matches — the read endpoint falls back to content-sniffing.
func mimeForExt(ext string) string {
	if ext == "" {
		return ""
	}
	return mime.TypeByExtension("." + ext)
}

// Attachment is the validated, scheme-decoded form of one
// `{role, uri, extension}` entry. Construct via Parse to enforce the
// regex + scheme allowlist; never zero-value-construct.
type Attachment struct {
	// Role is the plugin-defined semantic identifier — `thumb`,
	// `cover`, `rules`, etc. Becomes part of the on-disk filename.
	// Validated against roleRegex (see validate.go).
	Role string

	// URI is the source. One of three schemes: `file://`, `https://`
	// (or `http://`), `base64://`. Validated against schemeRegex.
	URI string

	// Extension is the on-disk file extension (no leading dot,
	// lowercase). Validated against extensionRegex.
	Extension string
}

// PreviousAttachment is the (role, uri) shape stored on the freshest
// provenance row's `fetch_attachments` field. The dispatcher uses this
// to decide whether to skip the re-fetch (per ADR-0014 §4).
type PreviousAttachment struct {
	Role string
	URI string
}

// Dispatcher is the configured entry point. Construct via New so the
// http.Client + 60s default timeout are wired sanely; never
// zero-value-construct.
type Dispatcher struct {
	httpClient *http.Client

	// stagingDir is the absolute, symlink-resolved path that every
	// `file://` URI MUST resolve under. Operator-configured via
	// `plugin_staging_dir` (default `/tmp`). Validated at New time
	// (must be absolute + exist + be a directory).
	stagingDir string

	logger *slog.Logger
}

// Option configures a Dispatcher at construction.
type Option func(*Dispatcher)

// WithHTTPClient overrides the default HTTP client. Tests inject a
// client wired to an httptest.Server.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Dispatcher) { d.httpClient = c }
}

// WithLogger sets the slog.Logger used for per-attachment WARNs.
func WithLogger(l *slog.Logger) Option {
	return func(d *Dispatcher) {
		if l != nil {
			d.logger = l
		}
	}
}

// New constructs a Dispatcher rooted at stagingDir. Returns an error
// when stagingDir is empty, relative, or doesn't resolve to an
// existing directory — fail-fast at server start matches ADR-0006's
// "operators must notice broken configs" rule.
func New(stagingDir string, opts ...Option) (*Dispatcher, error) {
	if stagingDir == "" {
		return nil, errors.New("attachments.New: stagingDir is empty")
	}
	if !filepath.IsAbs(stagingDir) {
		return nil, fmt.Errorf("attachments.New: stagingDir %q is not absolute", stagingDir)
	}
	resolved, err := filepath.EvalSymlinks(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("attachments.New: resolve stagingDir %q: %w", stagingDir, err)
	}
	d := &Dispatcher{
		// 60s timeout per ADR-0014 §2.https. Redirects + UA are wired
		// in the per-request scheme handler (http.go).
		httpClient: &http.Client{Timeout: 60 * time.Second},
		stagingDir: resolved,
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(d)
	}
	return d, nil
}

// StagingDir returns the resolved absolute staging directory. Used by
// the env-propagation path (subprocess pluginEnv) so plugins read
// the same value the daemon validates against.
func (d *Dispatcher) StagingDir() string { return d.stagingDir }

// DispatchInput is the request shape per ingest invocation. The
// dispatcher walks Attachments in order; for each attachment it:
//
// 1. Validates role / extension shape (rejects + logs on failure).
// 2. Compares (role, uri) against Previous; skips when ForceRefetch
// is false AND the pair matches a previous emission.
// 3. Routes by URI scheme to the matching handler (file://, https://,
// base64://) which writes the bytes to <VaultRoot>/<Kind>/<LocalID>.
// <role>.<extension>.
//
// Returns the list of attachments that actually landed on disk
// (including those skipped by the re-fetch contract — they're
// considered "still present"). The caller stamps these onto the
// fresh provenance row's `fetch_attachments` field.
type DispatchInput struct {
	Attachments []Attachment
	Previous []PreviousAttachment
	ForceRefetch bool

	VaultRoot string
	Kind string
	LocalID string
}

// DispatchResult names the (role, uri) pairs that ARE present on disk
// after this dispatch — the union of (a) skipped-by-rematch pairs and
// (b) freshly-written pairs. Stamped onto the fresh provenance row
// so the next ingest can re-do the comparison.
//
// FetchAttachments is order-stable across invocations of dispatcher
// against the same input set — the dispatcher walks the input
// Attachments in order. This stability matters for the YAML round-
// trip in vault frontmatter.
type DispatchResult struct {
	// FetchAttachments is the per-fetch (role, uri) provenance row per
	// ADR-0014 §4. Used for re-fetch comparison on the next ingest.
	FetchAttachments []PreviousAttachment

	// Manifest is the entity's attachment manifest per ADR-0018
	// §Attachments. The ingest tracker copies this into the entity's
	// `vault.Entity.Attachments` field before vault.Write so the
	// frontmatter `attachments:` list reflects what's actually on
	// disk. One entry per successfully-dispatched attachment in this
	// FetchResult.
	Manifest []ManifestEntry
}

// ManifestEntry mirrors `vault.Attachment` shape but lives in this
// package so internal/attachments doesn't import internal/vault
// (preserves the layering separation between dispatch + frontmatter
// serialization). The ingest tracker bridges by copying field-for-
// field — same names, same types.
type ManifestEntry struct {
	Name string // file basename, e.g. "thumb.jpg"
	Kind string // MIME type, e.g. "image/jpeg"; "" when undetectable
	Path string // relative to entity subdir, e.g. "attachments/thumb.jpg"
	Bytes int64 // size of the on-disk file in bytes
}

// Dispatch resolves every Attachment in input, writing each to its
// canonical vault location. Per-attachment failures log at WARN and
// the attachment is dropped from the result; whole-input failures
// (validation of shared inputs like LocalID / Kind / VaultRoot)
// return an error so the caller can decide whether to abort the
// whole entity write.
//
// Dispatch is concurrency-safe insofar as the underlying filesystem
// + HTTP client are: each attachment writes a unique destination
// path (role-keyed), so two concurrent ingests of disjoint entities
// don't collide. Two concurrent ingests of the SAME entity racing on
// the same role IS unsafe — same race shape as the read-merge-write
// vault flow flagged in a prior PR's body.
func (d *Dispatcher) Dispatch(ctx context.Context, in DispatchInput) (*DispatchResult, error) {
	if err := validateLocalID(in.LocalID); err != nil {
		return nil, fmt.Errorf("attachments.Dispatch: %w", err)
	}
	if err := validateKind(in.Kind); err != nil {
		return nil, fmt.Errorf("attachments.Dispatch: %w", err)
	}
	if in.VaultRoot == "" {
		return nil, errors.New("attachments.Dispatch: empty VaultRoot")
	}
	if !filepath.IsAbs(in.VaultRoot) {
		return nil, fmt.Errorf("attachments.Dispatch: VaultRoot %q is not absolute", in.VaultRoot)
	}

	prevByRole := make(map[string]string, len(in.Previous))
	for _, p := range in.Previous {
		prevByRole[p.Role] = p.URI
	}

	out := &DispatchResult{
		FetchAttachments: make([]PreviousAttachment, 0, len(in.Attachments)),
		Manifest: make([]ManifestEntry, 0, len(in.Attachments)),
	}
	seenRole := make(map[string]struct{}, len(in.Attachments))

	for _, a := range in.Attachments {
		if err := validateRole(a.Role); err != nil {
			d.logger.Warn("attachments: skipping — invalid role",
				"role", a.Role, "err", err, "entity", in.Kind+":"+in.LocalID)
			continue
		}
		if err := validateExtension(a.Extension); err != nil {
			d.logger.Warn("attachments: skipping — invalid extension",
				"role", a.Role, "extension", a.Extension, "err", err,
				"entity", in.Kind+":"+in.LocalID)
			continue
		}
		// Per ADR-0014 §1: a single attachment may appear at most
		// once per role per entity per FetchResult. Two emissions
		// with the same role on one fetch is a plugin bug; the
		// daemon SHOULD log + use the last one. Implement "use
		// last" by overwriting the previous out entry for this role.
		if _, dup := seenRole[a.Role]; dup {
			d.logger.Warn("attachments: duplicate role on single FetchResult — using last",
				"role", a.Role, "entity", in.Kind+":"+in.LocalID)
			out.FetchAttachments = removeRole(out.FetchAttachments, a.Role)
		}
		seenRole[a.Role] = struct{}{}

		dest, err := destPath(in.VaultRoot, in.Kind, in.LocalID, a.Role, a.Extension)
		if err != nil {
			d.logger.Warn("attachments: skipping — destination path rejected",
				"role", a.Role, "err", err, "entity", in.Kind+":"+in.LocalID)
			continue
		}

		// Re-fetch comparison (ADR-0014 §4). String-compare
		// (role, uri) against the freshest provenance's
		// fetch_attachments. Match → leave on-disk file in place.
		if !in.ForceRefetch {
			if prev, ok := prevByRole[a.Role]; ok && prev == a.URI {
				d.logger.Debug("attachments: re-fetch skipped — uri unchanged",
					"role", a.Role, "uri", a.URI,
					"entity", in.Kind+":"+in.LocalID)
				out.FetchAttachments = append(out.FetchAttachments, PreviousAttachment{
					Role: a.Role,
					URI: a.URI,
				})
				if entry, mErr := manifestEntryFor(dest, a.Role, a.Extension); mErr == nil {
					out.Manifest = append(out.Manifest, entry)
				} else {
					d.logger.Warn("attachments: manifest stat failed on re-fetch skip; entry omitted",
						"role", a.Role, "err", mErr, "entity", in.Kind+":"+in.LocalID)
				}
				continue
			}
		}

		scheme := schemeOf(a.URI)
		var fetchErr error
		switch scheme {
		case "file":
			fetchErr = d.handleFile(a, dest)
		case "https", "http":
			if scheme == "http" {
				d.logger.Warn("attachments: http:// scheme allowed but discouraged — prefer https",
					"role", a.Role, "uri", a.URI,
					"entity", in.Kind+":"+in.LocalID)
			}
			fetchErr = d.handleHTTP(ctx, a, dest)
		case "base64":
			fetchErr = d.handleBase64(a, dest)
		default:
			fetchErr = fmt.Errorf("unsupported scheme %q (allowed: file, https, http, base64)", scheme)
		}
		if fetchErr != nil {
			d.logger.Warn("attachments: skipping — handler failed",
				"role", a.Role, "uri", a.URI, "scheme", scheme,
				"err", fetchErr, "entity", in.Kind+":"+in.LocalID)
			continue
		}

		out.FetchAttachments = append(out.FetchAttachments, PreviousAttachment{
			Role: a.Role,
			URI: a.URI,
		})
		if entry, mErr := manifestEntryFor(dest, a.Role, a.Extension); mErr == nil {
			out.Manifest = append(out.Manifest, entry)
		} else {
			d.logger.Warn("attachments: manifest stat failed; entry omitted",
				"role", a.Role, "err", mErr, "entity", in.Kind+":"+in.LocalID)
		}
	}

	return out, nil
}

// manifestEntryFor stat()s the dispatched-on-disk attachment and
// builds the ADR-0018 §Attachments manifest entry for it. The
// frontmatter `path:` is relative to the entity's own subdir (so
// `attachments/<role>.<ext>`); MIME type is detected from the file
// extension via mime.TypeByExtension, falling back to empty when
// the OS has no entry for the type (the read endpoint then uses
// http.ServeContent's content-sniffing).
func manifestEntryFor(dest, role, ext string) (ManifestEntry, error) {
	info, err := osStat(dest)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("stat %s: %w", dest, err)
	}
	name := role + "." + ext
	return ManifestEntry{
		Name: name,
		Kind: mimeForExt(ext),
		Path: filepath.Join("attachments", name),
		Bytes: info.Size(),
	}, nil
}

// schemeOf extracts the scheme prefix (lowercase) from uri. Returns
// "" if uri has no scheme. Hand-rolled (rather than url.Parse) because
// `base64://` carries arbitrary alphabet bytes that may not pass
// url.Parse's stricter authority rules; we just need the prefix
// before the first `://` for dispatch.
func schemeOf(uri string) string {
	idx := strings.Index(uri, "://")
	if idx <= 0 {
		return ""
	}
	return strings.ToLower(uri[:idx])
}

// removeRole strips any entry with the given role from a
// PreviousAttachment slice. Used by Dispatch's duplicate-role guard.
func removeRole(s []PreviousAttachment, role string) []PreviousAttachment {
	out := s[:0]
	for _, p := range s {
		if p.Role == role {
			continue
		}
		out = append(out, p)
	}
	return out
}
