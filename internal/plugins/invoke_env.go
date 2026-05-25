package plugins

import "context"

// extraEnvKey is the unexported context key for the per-call
// subprocess env override per ADR-0028 §3 + §4 (Cut 4). The
// dispatch layer (URL routing in /v1/ingest + command fan-out
// in handleCommandFanOut) stamps the active instance's
// YAAD_PLUGIN_CONFIG + InstanceEntry.Env entries into the
// invocation ctx so each subprocess spawn runs with the right
// per-instance env — distinct from the registry-build-time
// configEnv that pre-Cut-4 stamped at construction.
type extraEnvKey struct{}

// WithExtraEnv returns a derived context carrying the given
// `KEY=VALUE` entries that subprocess.Plugin.env() will splice
// on top of the plugin's registered configEnv at spawn time.
// Per-call entries land LAST so they override registered
// values with the same key (matches the operator-yaml-wins
// precedence the plugin-level configEnv has over shell env).
//
// nil or empty env is a no-op clone — the returned ctx is the
// same parent for callers that uniformly invoke WithExtraEnv
// regardless of whether they have anything to splice.
func WithExtraEnv(ctx context.Context, env []string) context.Context {
	if len(env) == 0 {
		return ctx
	}
	return context.WithValue(ctx, extraEnvKey{}, append([]string(nil), env...))
}

// ExtraEnvFromContext returns the per-call env entries the
// dispatch layer stamped via WithExtraEnv, or nil when the
// context carries none. Callers (subprocess.Plugin spawn sites)
// append these to cmd.Env after their per-plugin configEnv so
// the per-call values take precedence on duplicate keys.
func ExtraEnvFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(extraEnvKey{}).([]string); ok {
		return v
	}
	return nil
}
