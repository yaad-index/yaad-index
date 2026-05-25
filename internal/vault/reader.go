package vault

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidAttachmentName is returned by attachment-resolution paths
// when the supplied name contains a path separator, traversal segment,
// or hidden-file leading dot. The HTTP layer surfaces this as 400; the
// daemon's URL parsing already rejects most of these via the URL spec
// — this is the defense-in-depth gate.
var ErrInvalidAttachmentName = errors.New("invalid attachment name")

// ErrAttachmentNotInManifest is returned when the resolved entity has
// no manifest entry matching the requested attachment name. ADR-0018
// step 6: only manifest-listed files are reachable; the filesystem
// is not the contract surface.
var ErrAttachmentNotInManifest = errors.New("attachment not in manifest")

// Reader loads Entity values from a vault root directory using the
// folder-per-kind layout. Mirror shape of Writer; together they form
// the round-trip API. Reader is safe for concurrent use.
type Reader struct {
	root string
}

// NewReader constructs a Reader rooted at vaultRoot. Same root-path
// rules as NewWriter (absolute, exists, is a directory).
func NewReader(vaultRoot string) (*Reader, error) {
	if !filepath.IsAbs(vaultRoot) {
		return nil, fmt.Errorf("vault root must be absolute, got %q", vaultRoot)
	}
	info, err := os.Stat(vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("stat vault root %q: %w", vaultRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("vault root %q is not a directory", vaultRoot)
	}
	return &Reader{root: vaultRoot}, nil
}

// ReadByID loads the entity whose vault file lives under one of
// three layouts:
//
// - active source-shape: `<root>/<kind>/<slug>.md`
// - canonical-label (per ADR-0021 amendment / yaad-index
// phase D): `<root>/ct/<kind>/<slug>.md`
// - archived (per ADR-0018 step 2): `<root>/_archive/<kind>/<slug>.md`
//
// Probe order: active → canonical-label → archive. A given id
// resolves to exactly one of these paths in practice — source-
// shape entities live under the active layout; canonical-label
// metadata files live under `ct/`; archived entities of either
// shape live under `_archive/`. The fallback chain lets callers
// (GET /v1/entities/{id}, operator-fill, etc.) pull any kind of
// vault file with a single call.
//
// Returns os.ErrNotExist via the wrapped error when none of the
// three paths exists.
func (r *Reader) ReadByID(kind, id string) (*Entity, error) {
	path, err := r.pathFor(kind, id)
	if err != nil {
		return nil, err
	}
	if e, err := r.ReadFile(path); err == nil {
		return e, nil
	} else if !IsNotExist(err) {
		return nil, err
	}
	// Canonical-label layout per ADR-0021 — auto-materialized by
	// operator-fill at `<root>/ct/<kind>/<slug>.md`.
	canonicalPath, err := r.canonicalLabelPathFor(kind, id)
	if err != nil {
		return nil, err
	}
	if e, err := r.ReadFile(canonicalPath); err == nil {
		return e, nil
	} else if !IsNotExist(err) {
		return nil, err
	}
	// Archive layout per ADR-0018 — same slug, same kind, but
	// rooted under `_archive/`.
	archivePath, err := r.archivePathFor(kind, id)
	if err != nil {
		return nil, err
	}
	return r.ReadFile(archivePath)
}

// ReadFile loads the entity at the given file path. Path is taken as-is
// (no root containment check beyond the read-only filesystem boundary —
// callers can construct Reader with any root, and reading outside it is
// a deliberate use case for testing and reindex operating on a manually
// supplied path).
func (r *Reader) ReadFile(path string) (*Entity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	e, err := Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	return e, nil
}

func (r *Reader) pathFor(kind, id string) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.root, KindDir(kind), slug+".md"), nil
}

// canonicalLabelPathFor mirrors pathFor for the
// `ct/<kind>/<slug>.md` layout introduced by ADR-0021's
// amendment (yaad-index phase D): operator-fill auto-
// materialize against a canonical-label entity writes to this
// path rather than the per-kind default. Used by ReadByID's
// chained fallback so subsequent reads find the file regardless
// of which layout produced it.
func (r *Reader) canonicalLabelPathFor(kind, id string) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.root, "ct", kind, slug+".md"), nil
}

// archivePathFor mirrors pathFor for the `_archive/<kind>/<slug>.md`
// layout introduced by ADR-0018 step 2. Used by ReadByID's fallback
// when the active path is absent.
func (r *Reader) archivePathFor(kind, id string) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("%w: kind", ErrMissingRequiredField)
	}
	slug, err := slugFromID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.root, ArchiveDir, KindDir(kind), slug+".md"), nil
}

// ArchiveDir is the relative subdirectory under the vault root that
// holds archived entities per ADR-0018 step 2. Exposed as a package
// constant so both reader (fallback) and writer (move target) refer
// to the same name without scattering string literals.
const ArchiveDir = "_archive"

// IsNotExist is a small helper so callers can detect "no such entity"
// without unwrapping multiple layers of fmt.Errorf wrapping.
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }

// OpenAttachment opens the named attachment for streaming. ADR-0018
// step 6 §Attachments: the manifest IS the contract surface. The
// flow is:
//
// 1. Validate `name` — reject path separators, traversal segments,
// leading dots. Defense-in-depth on top of HTTP routing.
// 2. Load the entity's frontmatter (active path, falling back to
// `_archive/`). ErrNotExist bubbles when neither layout has the
// .md file.
// 3. Find the manifest entry whose `Name == name`. Missing →
// ErrAttachmentNotInManifest. Filesystem files that aren't in
// the manifest are NOT reachable — the manifest is the boundary.
// 4. Validate `attachment.Path`: filepath.Clean must not escape the
// entity subdir (no leading `..`, no absolute paths). Reject
// into ErrInvalidAttachmentName since a manifest poisoned with
// `../../etc/passwd` is the same threat shape as a malicious
// name URL segment.
// 5. Open the file at `<entity-subdir>/<attachment.Path>` (under
// the active or archive root, matching the .md fallback). Caller
// gets an io.ReadCloser and the resolved Attachment metadata
// (so the HTTP layer can pick Content-Type from manifest.Kind +
// stat for Content-Length / Last-Modified).
//
// Returns os.ErrNotExist when the manifest file is missing on disk
// despite the entry existing — a vault-DB / manifest-disk drift the
// HTTP layer surfaces as 404 with a "drift" hint.
func (r *Reader) OpenAttachment(kind, id, name string) (io.ReadCloser, *Attachment, os.FileInfo, error) {
	if err := validateAttachmentName(name); err != nil {
		return nil, nil, nil, err
	}

	entity, archived, err := r.readByIDWithArchiveFlag(kind, id)
	if err != nil {
		return nil, nil, nil, err
	}

	var manifest *Attachment
	for i := range entity.Attachments {
		if entity.Attachments[i].Name == name {
			manifest = &entity.Attachments[i]
			break
		}
	}
	if manifest == nil {
		return nil, nil, nil, fmt.Errorf("%w: name=%q", ErrAttachmentNotInManifest, name)
	}

	if err := validateAttachmentPath(manifest.Path); err != nil {
		return nil, nil, nil, err
	}

	slug, err := slugFromID(id)
	if err != nil {
		return nil, nil, nil, err
	}
	var entityDir string
	if archived {
		entityDir = filepath.Join(r.root, ArchiveDir, KindDir(kind), slug)
	} else {
		entityDir = filepath.Join(r.root, KindDir(kind), slug)
	}
	resolved := filepath.Join(entityDir, filepath.Clean(manifest.Path))

	// Belt-and-braces: after Clean+Join, confirm the resolved path is
	// still under the entity directory. If a future refactor weakens
	// validateAttachmentPath, this catches it.
	rel, err := filepath.Rel(entityDir, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, nil, nil, fmt.Errorf("%w: resolved path escapes entity dir", ErrInvalidAttachmentName)
	}

	f, err := os.Open(resolved)
	if err != nil {
		return nil, nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, nil, err
	}
	return f, manifest, info, nil
}

// readByIDWithArchiveFlag mirrors ReadByID's active-then-archive
// resolution but reports which layout served the file. The HTTP
// attachment handler needs the layout to anchor the manifest's
// relative Path correctly.
func (r *Reader) readByIDWithArchiveFlag(kind, id string) (*Entity, bool, error) {
	activePath, err := r.pathFor(kind, id)
	if err != nil {
		return nil, false, err
	}
	if e, err := r.ReadFile(activePath); err == nil {
		return e, false, nil
	} else if !IsNotExist(err) {
		return nil, false, err
	}
	archivePath, err := r.archivePathFor(kind, id)
	if err != nil {
		return nil, false, err
	}
	e, err := r.ReadFile(archivePath)
	if err != nil {
		return nil, false, err
	}
	return e, true, nil
}

// validateAttachmentName rejects names that could escape the
// attachment subdir or trigger filesystem ambiguity. Allow only
// non-empty names without path separators, traversal segments, or
// leading dots (no hidden files).
func validateAttachmentName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidAttachmentName)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("%w: contains path separator", ErrInvalidAttachmentName)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: leading dot or traversal", ErrInvalidAttachmentName)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("%w: name not in canonical form", ErrInvalidAttachmentName)
	}
	return nil
}

// validateAttachmentPath rejects manifest Path fields that escape
// the entity subdir. The cold-reviewer's a prior PR carry-over: a manifest poisoned
// with `../../etc/passwd` must reject, not silently round-trip
// through to the filesystem layer.
func validateAttachmentPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty manifest path", ErrInvalidAttachmentName)
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("%w: absolute manifest path", ErrInvalidAttachmentName)
	}
	clean := filepath.Clean(path)
	if clean == "." {
		// Resolves to the entity directory itself — not an escape,
		// but os.Open on a directory + http.ServeContent's seek
		// would fail at runtime. Reject upfront so the agent gets a
		// clean 400 instead of a confusing 500. (the cold-reviewer flag, a prior PR.)
		return fmt.Errorf("%w: manifest path resolves to entity dir, not a file", ErrInvalidAttachmentName)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "..\\") {
		return fmt.Errorf("%w: manifest path traverses outside entity dir", ErrInvalidAttachmentName)
	}
	return nil
}
