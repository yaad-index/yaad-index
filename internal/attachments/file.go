package attachments

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// handleFile implements the `file://<absolute-path>` scheme per
// ADR-0014 §2.file. The plugin has staged the binary at the named
// path; the daemon copies (hardlinks when same fs) into the vault
// destination, then deletes the staged source on success.
//
// Path-traversal guard: the resolved staged path MUST be a strict
// descendant of the operator-configured stagingDir (after symlink
// resolution). Paths outside the staging dir are rejected per §5c.
//
// On copy success: the staged source is deleted. On copy failure:
// the staged source is left intact for operator inspection (so the
// plugin's work isn't lost when a transient destination problem like
// disk-full occurs).
func (d *Dispatcher) handleFile(a Attachment, dest string) error {
	srcPath, err := parseFileURI(a.URI)
	if err != nil {
		return err
	}

	// Path-traversal guard: resolve symlinks then check descendant.
	// EvalSymlinks errors on non-existent paths — that's the
	// missing-source case from the §5 test matrix; surface as the
	// canonical error so the caller's WARN log carries a useful
	// message.
	resolved, err := filepath.EvalSymlinks(srcPath)
	if err != nil {
		return fmt.Errorf("resolve staged source %q: %w", srcPath, err)
	}

	// Strict descendant of stagingDir. The trailing separator on
	// both sides is what makes the comparison "descendant" rather
	// than "starts with the same prefix" (which would let
	// `/tmpfoo` masquerade as under `/tmp`).
	stagingPrefix := strings.TrimRight(d.stagingDir, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(resolved+string(filepath.Separator), stagingPrefix) && resolved != d.stagingDir {
		return fmt.Errorf("staged source %q is not a descendant of stagingDir %q",
			resolved, d.stagingDir)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat staged source %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged source %q is not a regular file", resolved)
	}

	// Ensure the destination's parent (kind dir) exists. The vault
	// writer creates the kind dir during the .md write, but
	// attachments may be dispatched before/independent of that path
	// — be defensive.
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir kind dir for %q: %w", dest, err)
	}

	// Hardlink-when-same-fs, copy-when-different. os.Link is the
	// cheap path; on failure (cross-device, source-and-dest on
	// different mounts, sandbox restrictions, or dest-exists from
	// a prior fetch) we fall back to a copy.
	linkErr := os.Link(resolved, dest)
	if linkErr == nil {
		// Hardlink success → delete the staged source. The dest
		// file owns the inode now; the staging-dir name is just a
		// second link we drop.
		_ = os.Remove(resolved)
		return nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		// dest exists from a prior fetch (re-fetch with a
		// different uri per ADR-0014 §4 overwrites). Remove + retry.
		if err := os.Remove(dest); err != nil {
			return fmt.Errorf("remove existing dest %q for hardlink retry: %w", dest, err)
		}
		if err := os.Link(resolved, dest); err == nil {
			_ = os.Remove(resolved)
			return nil
		}
		// Second link also failed — fall through to copy.
	}

	// Copy fallback. Open source + dest in the safe order: source
	// for read, dest with O_TRUNC|O_CREATE so an existing file is
	// overwritten in place.
	if err := copyFile(resolved, dest); err != nil {
		return err
	}
	// Delete staged source on copy success.
	if err := os.Remove(resolved); err != nil {
		// Source-delete failure on otherwise-successful copy is a
		// debug-grade signal — the dest landed correctly so the
		// entity write isn't broken. Operator may want to know to
		// clean up tmp.
		d.logger.Warn("attachments: copied but staged source remove failed",
			"src", resolved, "dest", dest, "err", err)
	}
	return nil
}

// parseFileURI extracts the absolute filesystem path from a
// `file://<absolute-path>` URI. Constraints per ADR-0014 §2.file:
// path MUST be absolute. Empty path or a non-absolute path post-parse
// is an error.
//
// The standard library's url.Parse handles `file://` correctly when
// the path starts with `/` after the scheme://, which is the only
// shape we accept. Stricter than RFC 8089 — we don't grok
// `file:relative-path` or `file:///some/host`.
func parseFileURI(uri string) (string, error) {
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("expected file:// URI, got %q", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("parse file URI: %w", err)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("file URI host must be empty or localhost, got %q", u.Host)
	}
	if u.Path == "" {
		return "", errors.New("file URI has empty path")
	}
	if !filepath.IsAbs(u.Path) {
		return "", fmt.Errorf("file URI path %q is not absolute", u.Path)
	}
	return u.Path, nil
}

// copyFile is the cross-device copy fallback for the `file://`
// handler. Open source, create dest with O_TRUNC, io.Copy, fsync.
// Permissions on dest are 0644 (vault files are operator-readable).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open staged source %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create dest %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy %q → %q: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("fsync dest %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dest %q: %w", dst, err)
	}
	return nil
}
