// Package attach is the plugin-author-facing helper for emitting
// ADR-0014 attachments. Plugins import this from alice2-index (no
// internal/ dependency) and call File / URL / Bytes to construct
// the right wire shape; the daemon's dispatcher (see
// internal/attachments) handles the rest.
//
// Why a public package under pkg/ and not a standalone repo:
// ADR-0014 references a future "alice2-plugin-sdk" — that's the
// long-term home. For now the SDK lives here so PR-B can ship
// without bootstrapping a new repo + go.mod + CI. Plugins import
// `github.com/yaad-index/yaad-index/pkg/plugin/attach`; a future
// migration just moves the package and updates the import path.
//
// Wire shape contract (per ADR-0014 §1):
//
//	{"role": "...", "uri": "file:// | https:// | base64://...", "extension": "..."}
//
// Plugins emit a slice of these on the top-level `attachments` field
// of their FetchResult JSON. The daemon dispatches by URI scheme.
//
// **Plugins MUST stage `file://` payloads under StagingDir().**
// The daemon's path-traversal guard rejects file URIs whose
// resolved path isn't a strict descendant of the operator-
// configured staging dir; an absolute path outside that root is
// silently dropped at the daemon side.
package attach

import (
	"encoding/base64"
	"os"
)

// EnvStagingDir is the environment variable alice2-index sets when
// spawning a plugin subprocess. The value is the operator-
// configured `plugin_staging_dir` (default `/tmp`). Plugins read
// it via StagingDir() rather than hand-rolling.
const EnvStagingDir = "YAAD_PLUGIN_STAGING_DIR"

// DefaultStagingDir is the fallback returned by StagingDir() when
// the environment variable is unset (the binary isn't running
// under alice2-index, or the daemon predates ADR-0014's PR-B).
// `/tmp` matches the daemon-side default and POSIX expectations.
const DefaultStagingDir = "/tmp"

// Attachment is the wire shape the daemon decodes per ADR-0014 §1.
// Field tags pin the JSON keys; plugins SHOULD only construct
// instances via File / URL / Bytes — direct struct-literal use
// risks emitting a malformed scheme, which the daemon will reject.
type Attachment struct {
	Role string `json:"role"`
	URI string `json:"uri"`
	Extension string `json:"extension"`
}

// StagingDir returns the operator-configured plugin staging
// directory (from EnvStagingDir), or DefaultStagingDir when the
// env var is unset. Always returns a non-empty string so callers
// can use it directly with os.CreateTemp / filepath.Join without
// a nil check.
//
// Plugins use this as the parent directory for any tmp files they
// stage before emitting `file://` attachments — the daemon's path-
// traversal guard rejects file URIs outside this root.
func StagingDir() string {
	if v := os.Getenv(EnvStagingDir); v != "" {
		return v
	}
	return DefaultStagingDir
}

// File constructs an Attachment whose URI is `file://<absPath>`.
// absPath MUST be absolute and MUST live under StagingDir() — the
// daemon's path-traversal guard rejects file URIs outside the
// operator-configured staging dir.
//
// The plugin owns the staged file; the daemon copies (or
// hardlinks) into the vault and DELETES the staged source on
// success per ADR-0014 §2.file. Plugins should NOT depend on the
// staged file existing after returning the Attachment to alice2-
// index — its lifetime is one Fetch call.
func File(role, absPath, extension string) Attachment {
	return Attachment{
		Role: role,
		URI: "file://" + absPath,
		Extension: extension,
	}
}

// URL constructs an Attachment whose URI is the upstream URL
// verbatim. The daemon GETs the URL and stores the response body
// per ADR-0014 §2.https. Use this only when:
//
// - The URL is canonical and stable (operator-readable, no
// auth, no redirects to plugin-private surfaces).
// - The daemon's standard HTTP behavior (60s timeout, max 5
// redirects, default UA) suffices.
//
// Plugins authenticating against private endpoints (BGG_API_KEY,
// GitHub PATs, etc.) MUST NOT use this scheme — the daemon
// doesn't carry plugin credentials. Use File() with a staged tmp
// file instead.
//
// The url MUST start with `https://` or `http://`. The helper
// doesn't pre-validate (the daemon validates per attachment); a
// non-URL string here is a plugin bug that surfaces as a daemon-
// side WARN + skip on the first ingest.
func URL(role, url, extension string) Attachment {
	return Attachment{
		Role: role,
		URI: url,
		Extension: extension,
	}
}

// Bytes constructs an Attachment whose URI is `base64://<encoded>`
// with data inline (RFC 4648 standard alphabet, padded). Use only
// for small payloads (recommended < 100 KB raw; not enforced) —
// the encoded form bloats the JSON ~33%, and a 1 MB binary becomes
// a 1.33 MB JSON field that strains the subprocess pipe + the
// daemon's decoder.
//
// Prefer File() for anything larger. Bytes() is the right choice
// when the plugin runs in a sandboxed context with no filesystem
// access (future WASM runtimes), or when a single-roundtrip
// stdio-only emission matters.
func Bytes(role string, data []byte, extension string) Attachment {
	return Attachment{
		Role: role,
		URI: "base64://" + base64.StdEncoding.EncodeToString(data),
		Extension: extension,
	}
}
