package config

// ReservedInstanceEnvKeys names the env keys yaad-index reserves
// for daemon-stamped values on every plugin subprocess invocation
// per #286. Operator `instances[*].env` containing any of these
// keys would shadow the daemon stamp via exec.Cmd's last-wins
// duplicate-key semantics — handing the plugin a value the daemon
// didn't provision. validateInstances rejects them at config-load
// with a pointed error message that names the correct structured
// knob (when one exists) so the operator can re-route the override.
//
// The map value is the operator-facing hint text appended to the
// rejection error. Empty string means "no structured override
// today; file a follow-up if you need one."
//
// **`YAAD_PLUGIN_CONFIG` is intentionally NOT in this set.** Per
// the existing comment at internal/api/instance_routing.go
// buildInstanceEnv, operator env wins over the daemon-stamped
// YAAD_PLUGIN_CONFIG on duplicate keys to support the env-only-
// instance use case (gmail-style: env: { ... } with no config:
// block). Don't change that semantic without a separate ADR.
var ReservedInstanceEnvKeys = map[string]string{
	// #284: per-(plugin,instance) persistent-state directory.
	// Operator override → `instances[*].data_dir`.
	"YAAD_PLUGIN_DATA_DIR": "use `instances[*].data_dir` for an operator override path instead",

	// ADR-0014 + subprocess.go: per-plugin attachment staging
	// dir. No per-instance override surface today; an operator
	// who needs to relocate it should set the daemon-level
	// `plugin_staging_dir` (see docs/configs.md §3) which the
	// daemon stamps uniformly for every instance.
	"YAAD_PLUGIN_STAGING_DIR": "use the daemon-level `plugin_staging_dir` to relocate; per-instance override is not currently supported",

	// subprocess.go pluginEnv: operator-configured IANA
	// timezone (used by plugins for provenance timestamp
	// formatting). No per-instance override today — timezone
	// is a daemon-global property.
	"YAAD_TIMEZONE": "this is a daemon-global setting; per-instance override is not supported",
}
