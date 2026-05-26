// Package datadir resolves + provisions the per-(plugin,instance)
// persistent-state directory yaad-index surfaces on
// YAAD_PLUGIN_DATA_DIR per #284 + #287. The daemon calls Resolve at
// config-load time to compute the path and Ensure at startup to
// create it with the secrets-grade 0700 perm before the plugin
// subprocess spawns.
//
// Path shape precedence (per #287, highest priority first):
//
//  1. `instances[*].data_dir` absolute path — passed verbatim.
//     Validated absolute at config-load (validateInstances).
//  2. `plugin_data_root` from operator config — joined with
//     `yaad-<plugin>/<instance>/`. Wired via SetRoot from
//     cmd/yaad-index/main.go at startup.
//  3. `$STATE_DIRECTORY` env (systemd `StateDirectory=`-aware) —
//     joined with `plugin-data/yaad-<plugin>/<instance>/`. The
//     `plugin-data/` segment keeps the plugin tree distinct from
//     any other state the unit writes under the same root.
//  4. `os.UserCacheDir()` — joined with `yaad-<plugin>/<instance>/`
//     (XDG default, the original #284 fallback). Suitable for
//     dev hosts running the daemon by hand; production systemd
//     units typically have one of (2) or (3) set.
//
// The (3) systemd integration unblocks the deploy regression in
// #287: hardened units with `ProtectHome=read-only` can't write
// under `os.UserCacheDir()`, but they can write under whatever
// path `StateDirectory=` allocates.
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// pluginDataRoot stores the operator-configured top-level base
// directory per #287. Empty means "no operator override; fall
// through the env-driven precedence chain." Mutated only at
// daemon startup via SetRoot; reads use atomic.Value's load
// semantics so the per-call Resolve path is lock-free.
var pluginDataRoot atomic.Value // string

// SetRoot pins the operator-configured `plugin_data_root` for the
// per-(plugin,instance) data dir resolver. Called once at server
// startup from cmd/yaad-index/main.go after loading + validating
// the config; idempotent across re-calls. Empty string is
// treated as "reset to env-driven chain" — the next Resolve call
// falls through to `$STATE_DIRECTORY` or `os.UserCacheDir()`.
func SetRoot(root string) {
	pluginDataRoot.Store(root)
}

// envStateDirectory is the systemd-injected env var name for
// units that declare `StateDirectory=`. systemd creates the dir +
// exports the absolute path here; the daemon picks it up
// transparently per #287's option (B).
const envStateDirectory = "STATE_DIRECTORY"

// Resolve computes the absolute path for this (plugin, instance)
// pair. Walks the precedence chain:
//
//  1. `override` — `instances[*].data_dir` from validated config.
//     Passed verbatim (validateInstances guarantees absolute).
//  2. `pluginDataRoot` — `plugin_data_root` from validated config,
//     stamped via SetRoot at boot. Joined with
//     `yaad-<plugin>/<instance>/`.
//  3. `$STATE_DIRECTORY` — systemd-allocated state path.
//     Joined with `plugin-data/yaad-<plugin>/<instance>/`.
//  4. `os.UserCacheDir()` — XDG fallback for dev hosts.
//     Joined with `yaad-<plugin>/<instance>/`.
//
// Plugin + instance names are joined as path segments; the
// validateInstances regex (`[a-z0-9]+([_-][a-z0-9]+)*`) guarantees
// they're filesystem-safe (no slashes, no traversal).
func Resolve(pluginName, instanceName, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if root, ok := pluginDataRoot.Load().(string); ok && root != "" {
		return filepath.Join(root, "yaad-"+pluginName, instanceName), nil
	}
	if stateDir := os.Getenv(envStateDirectory); stateDir != "" {
		return filepath.Join(stateDir, "plugin-data", "yaad-"+pluginName, instanceName), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir for plugin %q instance %q: %w",
			pluginName, instanceName, err)
	}
	return filepath.Join(cache, "yaad-"+pluginName, instanceName), nil
}

// Ensure creates the directory at the given absolute path with
// 0700 perm if absent. Idempotent: an existing dir at the path
// is left in place (the daemon never deletes operator-owned state
// per the #284 contract). Returns an error when the path exists
// but is not a directory, or when the parent chain can't be
// created with 0700.
//
// The 0700 perm matches the secrets-grade file-perms model #256
// established for env-file secrets — file contents under the dir
// are treated as credential-equivalent because typical contents
// are session cookies / refresh tokens.
func Ensure(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("data dir %q exists but is not a directory", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat data dir %q: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data dir %q: %w", path, err)
	}
	return nil
}
