// Package datadir resolves + provisions the per-(plugin,instance)
// persistent-state directory yaad-index surfaces on
// YAAD_PLUGIN_DATA_DIR per #284. The daemon calls Resolve at config-
// load time to compute the path and Ensure at startup to create it
// with the secrets-grade 0700 perm before the plugin subprocess
// spawns.
//
// Path shape (per the issue's daemon contract):
//
//   - default: `<userCacheDir>/yaad-<plugin>/<instance>/`
//     (UserCacheDir maps to `$XDG_CACHE_HOME` or `~/.cache` on
//     Linux per XDG semantics).
//   - operator override: `instances[*].data_dir` absolute path
//     passed through verbatim (already enforced absolute at
//     config-load time per validateInstances).
//
// The default uses os.UserCacheDir() so the daemon picks up
// XDG_CACHE_HOME when set (CI / containerized deployments) and
// falls back to ~/.cache only when XDG isn't configured.
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
)

// Resolve computes the absolute path for this (plugin, instance)
// pair. When override is non-empty, it MUST already be absolute
// (validateInstances enforces this at config-load); Resolve passes
// it through verbatim. Otherwise the default is
// `<userCacheDir>/yaad-<plugin>/<instance>/`.
//
// Plugin + instance names are joined as path segments; the
// validateInstances regex (`[a-z0-9]+([_-][a-z0-9]+)*`) guarantees
// they're filesystem-safe (no slashes, no traversal).
func Resolve(pluginName, instanceName, override string) (string, error) {
	if override != "" {
		return override, nil
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
