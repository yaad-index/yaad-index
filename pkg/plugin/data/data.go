// Package data is the plugin-author-facing helper for accessing
// the per-(plugin,instance) persistent data directory provided by
// yaad-index per #284. Plugins import this from yaad-index (no
// internal/ dependency) and call DataDir() to learn where they
// should write durable state (cookie jars, refresh tokens, plugin-
// managed caches, etc.).
//
// Why a public package under pkg/ alongside pkg/plugin/attach: the
// data dir is part of the plugin runtime contract surfaced through
// the subprocess env shape. Co-locating it with the other public
// SDK helper keeps the plugin-author import surface coherent
// (`github.com/yaad-index/yaad-index/pkg/plugin/{attach,data}`).
//
// Contract per #284:
//
// - Daemon creates a per-(plugin,instance) directory before
// spawning the plugin subprocess.
// - Daemon stamps the absolute path on YAAD_PLUGIN_DATA_DIR.
// - Plugin owns file structure under that dir.
// - Permissions: 0700 — secrets-grade file-perms model.
// - Path is stable across plugin restarts (same plugin+instance
// → same path). Multi-instance plugins receive different paths.
//
// Plugins predating this surface (or running outside yaad-index)
// see DefaultDataDir as the fallback.
package data

import "os"

// EnvDataDir is the environment variable yaad-index sets when
// spawning a plugin subprocess. The value is the absolute path
// of the per-(plugin,instance) persistent-state directory the
// daemon owns + creates. Plugins read it via DataDir() rather
// than hand-rolling.
const EnvDataDir = "YAAD_PLUGIN_DATA_DIR"

// DefaultDataDir is the fallback returned by DataDir() when the
// environment variable is unset (the binary isn't running under
// yaad-index, or the daemon predates #284). Empty by design —
// callers MUST check for this case and decline to write durable
// state when no daemon-provided dir is available, rather than
// silently falling back to a path the operator hasn't sanctioned.
const DefaultDataDir = ""

// DataDir returns the daemon-provided per-(plugin,instance)
// persistent-state directory from EnvDataDir, or DefaultDataDir
// (empty string) when the env var is unset.
//
// Callers SHOULD check for an empty return value before writing
// durable files — an empty path signals "no daemon-managed
// directory available; refuse to persist." This is the
// conservative shape: a plugin writing to a hardcoded fallback
// (e.g. /tmp or ~/.cache) would bypass operator config and break
// the per-instance isolation contract.
func DataDir() string {
	return os.Getenv(EnvDataDir)
}
