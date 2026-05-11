package attachments

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// roleRegex enforces ADR-0014 §5a: role MUST match
// `^[a-z0-9][a-z0-9-]{0,31}$`. Lowercase ASCII alphanumeric + dash,
// 1-32 chars, must start with alphanumeric. Blocks path-traversal via
// the role field (e.g. `role: '../../etc/cron.d/evil'`), shell
// metacharacters, whitespace, Unicode, and uppercase shenanigans.
var roleRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// extensionRegex enforces ADR-0014 §5b: extension MUST match
// `^[a-z0-9]{1,10}$`. 10-char cap covers all real-world extensions
// (jpeg, webm, flac, mp3, png, …); longer is suspicious. Note the
// stricter shape than role — no dashes, no underscores.
var extensionRegex = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// localIDRegex enforces ADR-0014 §3: the local-id (the part of the
// entity ID after the `<kind>:` namespace prefix) MUST match
// `^[a-z0-9_-]+$`. Underscore is allowed (existing entities use it,
// e.g. `wikipedia:susanna-clarke` → `susanna-clarke`); dot/slash/colon
// are excluded so the destination path can't contain path separators.
//
// Existing entities should already conform; the regex here is the
// defense-in-depth check at attachment-write time, in case some
// future entity-id shape sneaks past the kind-name regex elsewhere.
var localIDRegex = regexp.MustCompile(`^[a-z0-9_-]+$`)

// kindRegex matches the operator-canonicalized kind name. Same shape
// as localID but the ADR uses it under §3 to constrain the on-disk
// directory name. Mirrors the kind validator in config.go.
var kindRegex = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validateRole enforces roleRegex. Public-shaped errors so the caller
// can log without re-formatting.
func validateRole(role string) error {
	if !roleRegex.MatchString(role) {
		return fmt.Errorf("role %q must match %s", role, roleRegex)
	}
	return nil
}

// validateExtension enforces extensionRegex.
func validateExtension(ext string) error {
	if !extensionRegex.MatchString(ext) {
		return fmt.Errorf("extension %q must match %s", ext, extensionRegex)
	}
	return nil
}

// validateLocalID enforces localIDRegex.
func validateLocalID(localID string) error {
	if localID == "" {
		return errors.New("local-id is empty")
	}
	if !localIDRegex.MatchString(localID) {
		return fmt.Errorf("local-id %q must match %s", localID, localIDRegex)
	}
	return nil
}

// validateKind enforces the kind name regex.
func validateKind(kind string) error {
	if kind == "" {
		return errors.New("kind is empty")
	}
	if !kindRegex.MatchString(kind) {
		return fmt.Errorf("kind %q must match %s", kind, kindRegex)
	}
	return nil
}

// destPath constructs the canonical on-disk attachment path per
// ADR-0018 §Attachments and ownership cascade:
//
//	<vaultRoot>/<kind>/<localID>/attachments/<role>.<extension>
//
// The aggregate-root layout puts every attachment under the entity's
// own subdir so archive / restore / destroy can move-or-remove the
// whole subtree atomically alongside the .md file ( part 3).
//
// Pre-ADR-0018 layout was the flat `<localID>.<role>.<ext>` shape at
// the kind directory level — this PR is the v1 break. Existing
// vaults need a one-time migration (move the flat thumb files into
// the nested subdir + reindex); operators can drop the old files
// or move them with a small script — the daemon does NOT auto-
// migrate today.
//
// AND defense-in-depth re-validates that the resolved path is a
// strict descendant of <vaultRoot>/<kind>/<localID>/attachments.
// Path is NOT cleaned with filepath.Clean for the descendant check
// — Clean would resolve `..` components and could mask a traversal
// attempt. Instead we check the parent-of-file equals the
// attachments-subdir.
func destPath(vaultRoot, kind, localID, role, ext string) (string, error) {
	// Validate inputs again — this is the single load-bearing entry
	// point so a caller that bypassed the higher-level checks still
	// can't write outside the vault.
	if err := validateKind(kind); err != nil {
		return "", err
	}
	if err := validateLocalID(localID); err != nil {
		return "", err
	}
	if err := validateRole(role); err != nil {
		return "", err
	}
	if err := validateExtension(ext); err != nil {
		return "", err
	}

	kindDir := filepath.Join(vaultRoot, kind)
	entityDir := filepath.Join(kindDir, localID)
	attachDir := filepath.Join(entityDir, "attachments")
	filename := role + "." + ext
	full := filepath.Join(attachDir, filename)

	// Defense-in-depth: the constructed path's directory MUST be
	// the attachments subdir (not the entity dir, not the kind dir,
	// not somewhere else). Equality with attachDir means the role-
	// or-localID-derived components didn't sneak in a separator.
	if filepath.Dir(full) != attachDir {
		return "", fmt.Errorf("destination %q escapes attachments dir %q", full, attachDir)
	}
	// And the kindDir itself must descend from vaultRoot — guards
	// against a kind name like `..` (kindRegex blocks that already,
	// but the redundant check costs us nothing).
	if !strings.HasPrefix(filepath.Clean(kindDir)+string(filepath.Separator),
		filepath.Clean(vaultRoot)+string(filepath.Separator)) {
		return "", fmt.Errorf("kind dir %q escapes vault root %q", kindDir, vaultRoot)
	}
	return full, nil
}
