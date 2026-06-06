// Package config loads the yaad-index server config (per ADR-0006).
//
// Today the only top-level key is `plugins:` — a map of plugin name to
// absolute binary path. Future PRs may add other keys (`store:`,
// `bind:`, …). The shape is intentionally narrow.
//
// Validation is fail-fast: a missing path, a relative path, a
// non-executable file, or a directory at the named path all produce
// an error at Load time and the server doesn't start. Per ADR-0006:
// "operators must notice broken configs."
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// kindOrFieldName is the shape rule for gap field names within a
// canonical kind. Lowercase ASCII + digits + underscore; must
// start with a letter. Field names are CEL-accessible identifiers
// so hyphens stay out — `entity.foo-bar` parses as a subtraction
// expression in CEL, not as a field access.
var kindOrFieldName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// canonicalKindName is the shape rule for canonical-kind names
// (e.g. `boardgame`, `tv-show`, `email-address`). Looser than
// kindOrFieldName: hyphens are permitted between alphanumeric
// groups so operator config can extend gaps on plugin-emitted
// kinds with hyphenated names (`tv-show`, `email-address`,
// `film-series`, `video-game`, …). The grouped-hyphen pattern
// `[a-z][a-z0-9_]*(-[a-z0-9_]+)*` forbids trailing hyphens
// (`foo-`) and consecutive hyphens (`foo--bar`) — the natural
// URL-slug shape.
var canonicalKindName = regexp.MustCompile(`^[a-z][a-z0-9_]*(-[a-z0-9_]+)*$`)

// instanceName is the shape rule for plugin-instance names per
// ADR-0028 §1: lowercase alphanumerics, hyphens, and underscores.
// No leading/trailing punctuation, no consecutive non-alphanum
// runs, no slashes (the slash is reserved for the
// `<plugin>/<instance>` invocation + `source:` field syntax).
var instanceName = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)

// defaultInstanceName is the name synthesized for plugins whose
// operator config omits the `instances:` block per ADR-0028 §1.
// The same name is also valid as an explicit operator-written
// instance name; the synthesis path produces a config equivalent
// to writing `instances: [{name: default}]` explicitly.
const defaultInstanceName = "default"

// DefaultPath is the location yaad-index reads its config from when
// the operator hasn't set --config / YAAD_INDEX_CONFIG.
const DefaultPath = "~/.config/yaad-index/config.yaml"

// Config is the parsed top-level config document.
type Config struct {
	// Plugins is the ordered list of plugins yaad-index will register
	// at startup. **Order matters**: ADR-0006's first-match-wins
	// dispatch rule means earlier entries claim ambiguous URLs before
	// later ones. The list shape (rather than a map) is deliberate —
	// Go map iteration is randomized, which would scramble dispatch
	// priority across server restarts.
	//
	// ADR-0006 explicitly rejects relative paths, PATH search, and
	// `~/` shell expansion on Path — operators type the full absolute
	// path or yaad-index doesn't run the plugin.
	Plugins []PluginEntry `yaml:"plugins"`

	// Vault names the markdown-vault root (per ADR-0008). When the key
	// is present, Path must be absolute, exist, and be a directory —
	// validated at Load time so a broken `vault.path` fails server
	// start. When absent, callers that need a vault (reindex, future
	// vault-first ingest) must surface a clear error themselves; this
	// package does not synthesize a default.
	Vault VaultEntry `yaml:"vault"`

	// CanonicalKinds is the operator's canonical-kind registry — per
	// ADR-0013 §1, each enabled kind owns its gap-set vocabulary +
	// fill instruction. The map keys ARE the enabled-kinds set
	// (semantically equivalent to ADR-0008's previous `[]string`
	// list shape; the schema migrated to a map per ADR-0013).
	//
	// Empty / missing → no canonical layer materializes; only
	// source-shape entities live. Operators opt-in explicitly to
	// each canonical kind they care about. There is no built-in
	// default; the system ships zero-canonical and the operator
	// names what they want.
	//
	// Per ADR-0013 §3: the registry is parsed + validated at
	// Load time and held in the Config struct. Wiring into the
	// `canonical_vocabulary` field on `needs_fill` responses lives
	// in the handler layer; this struct carries parsing + validation.
	//
	// Adding a kind requires a server restart (or POST /v1/reindex)
	// and re-ingest from sources where the now-enabled canonical
	// kind would have materialized; past ingests don't backfill.
	//
	// **Schema migration:** the
	// previous list-of-strings shape (`canonical_kinds: [person,
	// city]`) no longer parses. Operators migrate by giving each
	// enabled kind its per-kind config block (gaps + instruction);
	// see AGENTS.md §"Configuration" for the new shape.
	CanonicalKinds map[string]CanonicalKindConfig `yaml:"canonical_kinds"`

	// CanonicalKindsDefaults is the operator-config root-scoped
	// override block per ADR-0016 §3. Top-level sibling key (NOT
	// nested under `canonical_kinds:`) — keeps a kind named
	// "defaults" from colliding with the override scope-of-one.
	//
	// Both `Gaps` and `Instruction` apply to every canonical kind
	// in the merged effective registry, unless overridden by a
	// per-kind block under `canonical_kinds:`. Both fields are
	// optional; an empty / missing block is the common case.
	//
	// Example operator-config block:
	//
	//	canonical_kinds_defaults:
	//	 instruction:
	//	 enabled: true
	//	 text: "Fill carefully. Cite sources where possible."
	//	 gaps:
	//	 external_url:
	//	 type: string
	//	 description: "Authoritative URL for this entity."
	CanonicalKindsDefaults CanonicalKindConfig `yaml:"canonical_kinds_defaults"`

	// CanonicalEdgeTypes names the operator-enabled canonical edge
	// types (`is_about`, `same_as`, `lives_in`, …). Same gating
	// semantic as CanonicalKinds: edges of types not in this list
	// are silently dropped at ingest/fill time, even when both
	// endpoints exist as canonical entities.
	CanonicalEdgeTypes []string `yaml:"canonical_edge_types"`

	// UserContentFrontmatterEdges declares the operator-side mapping
	// from UGC frontmatter field → canonical edge
	// (ADR-0008 / ADR-0011 / ADR-0017 parity). Mirrors the
	// plugin-side `FrontmatterEdges` Capabilities declaration: the
	// daemon walks UGC `data.<field>` at create/edit time, slugifies
	// each value to `<target_kind>:<slug>`, ensures a canonical
	// stub exists, and materializes a typed edge from the UGC entity
	// to the stub.
	//
	// Why operator-config (not plugin-config): UGC has no plugin to
	// declare capabilities on. Per ADR-0016 the instruction-and-
	// mapping space is operator-only end-to-end; the UGC surface
	// inherits that policy.
	//
	// Subject to CanonicalGuard's enabled-kinds + edge-types: a
	// mapping referencing a kind not in `canonical_kinds:` or an
	// edge type not in `canonical_edge_types:` is silently dropped
	// at edge-creation time (same shape as the plugin path).
	//
	// Example operator-config block:
	//
	//	user_content_frontmatter_edges:
	//	 about:
	//	 edge_type: is_about
	//	 target_kind: boardgame
	//	 mentions:
	//	 edge_type: mentions
	//	 target_kind: person
	//	 designed_by:
	//	 edge_type: designed_by
	//	 target_kind: person
	//
	// Empty / missing → no derivation runs (the UGC create/edit
	// paths don't append canonical stubs or edges; legacy
	// behavior preserved). Operators opt-in explicitly per field.
	UserContentFrontmatterEdges map[string]UserContentFrontmatterEdgeMapping `yaml:"user_content_frontmatter_edges"`

	// Timezone is the operator-configured IANA TZ identifier
	// applied uniformly to every timestamp the binary renders
	// (vault frontmatter, provenance, logs, CLI output, plugin-
	// emitted fetched_at).
	// Examples: "America/Los_Angeles", "Asia/Tokyo", "America/New_York".
	//
	// Empty / missing → "UTC" (preserves legacy behavior). yaad-
	// index is a single-operator system so the configured TZ
	// applies everywhere — there is no per-request / per-route
	// override.
	//
	// Validate parses via time.LoadLocation; a malformed identifier
	// fails server start with the location's parse error.
	//
	// Config + binary boot plumbing live here; the .UTC() /
	// .In(time.UTC) call sites in vault writer / provenance / logs /
	// CLI / plugin SDK use clock.Now() / clock.In() helpers from
	// internal/clock.
	Timezone string `yaml:"timezone"`

	// Workflow bundles per-workflow-engine knobs. v1.x carries
	// just the graph-walk cap per ADR-0027 cut 3 (#232); future
	// workflow-related operator knobs land here too.
	Workflow WorkflowEntry `yaml:"workflow"`

	// LogLevel is the operator-controlled threshold the slog handler
	// filters against . One of "debug", "info" (default),
	// "warn", "error". Empty / missing → info, which matches the
	// historical hardcoded value in cmd/yaad-index — omitting the
	// key is observationally equivalent to legacy behavior.
	//
	// Parsing happens at Validate time via ParseLogLevel; an
	// unrecognized value fails server start with a helpful error
	// rather than silently degrading to info. Per ADR-0006:
	// operators must notice broken configs.
	LogLevel string `yaml:"log_level"`

	// CacheTTLSeconds is the operator's global TTL contribution to
	// the three-level resolution chain (originally a global-only
	// knob).
	// At ingest time, resolveCacheTTL walks {per-fetch
	// FetchResult.CacheTTLSeconds > plugin Capabilities.CacheTTLSeconds
	// > this global config} and stamps the first non-zero result into
	// the entity's vault frontmatter (`cache_ttl_seconds:`). Lookup
	// is then a simple `provenance[last].fetched_at +
	// frontmatter.cache_ttl_seconds vs now` comparison.
	//
	// Sentinel rules — identical at every level:
	//
	// - 0 (default) → no opinion; resolution falls through to the
	// next level. When ALL three levels are 0/absent, no TTL is
	// stamped and the entity caches forever (preserves legacy
	// behavior).
	// - positive N → entities expire after N seconds at this level.
	// - negative → entities never expire (cache forever).
	//
	// force_refetch=true skips the cache lookup entirely (orthogonal
	// to TTL).
	CacheTTLSeconds int `yaml:"cache_ttl_seconds"`

	// Auth carries the operational JWT-auth configuration.
	// Operational
	// config — sibling of port / log_level / vault.path — NOT
	// vault-readable. The private key must not live in the vault —
	// an agent could otherwise trick the index into returning it.
	//
	// The path precedence chain is locked: CLI flag >
	// `YAAD_INDEX_KEYS_DIR` env > `auth.keys_dir` config field.
	// Same precedence applies to default-ttl. The CLI / env
	// resolution happens in `cmd/yaad-index/main.go`; this struct
	// only carries the lowest-priority config layer.
	Auth AuthEntry `yaml:"auth"`

	// PluginStagingDir is the operator-configured base directory for
	// plugin-staged tmp files (per ADR-0014 §6). Plugins that emit
	// `file://<absolute-path>` attachments MUST stage their tmp
	// files under this dir. The daemon's path-traversal guard
	// rejects file URIs whose resolved path isn't a strict
	// descendant of this dir, so a malicious plugin can't write
	// outside the intended staging surface.
	//
	// Resolution chain at server boot, highest
	// priority first:
	//
	//  1. This yaml field, when set to a non-empty value.
	//  2. `YAAD_PLUGIN_STAGING_DIR` env var on the daemon process —
	//     same var name plugins read via the SDK. Lets operators
	//     flip the root without editing yaml (systemd drop-ins,
	//     container deploys).
	//  3. `os.TempDir()` — POSIX-conformant fallback that respects
	//     `$TMPDIR`. Previously hardcoded `/tmp`; the change picks
	//     up containerized runtimes (often `/var/tmp` or a tmpfs
	//     mount) and per-user tempdirs.
	//
	// Validate ensures the value (when non-empty) is absolute and
	// names an existing directory. Empty is permitted at parse —
	// the cmd/yaad-index boot resolves the chain before constructing
	// the attachments dispatcher. Plugins receive the resolved path
	// via the `YAAD_PLUGIN_STAGING_DIR` env (part of the ADR-0014
	// daemon series; same plumbing as `YAAD_TIMEZONE`).
	PluginStagingDir string `yaml:"plugin_staging_dir"`

	// PluginDataRoot is the operator-configured base directory
	// under which the daemon provisions per-(plugin,instance)
	// persistent-state dirs per #284 + #287. When set, the
	// resolver joins it with `yaad-<plugin>/<instance>/` for each
	// instance. When empty, the resolver falls through the
	// environment-driven chain at `internal/plugins/datadir`:
	// `$STATE_DIRECTORY` (systemd `StateDirectory=`-aware) →
	// `os.UserCacheDir()` (XDG default).
	//
	// Production deployments under hardened systemd units
	// (`ProtectHome=read-only`) need this knob because
	// `os.UserCacheDir()` resolves under an unwritable `$HOME`.
	// Operators of those units typically set `StateDirectory=`
	// on the unit (which auto-creates `/var/lib/<unit>/` +
	// exports `$STATE_DIRECTORY`) and the daemon picks it up
	// without yaml changes; this field is the explicit override
	// for deployments that want the root somewhere else.
	//
	// Validate ensures the value (when non-empty) is absolute.
	// The path doesn't have to exist at Load time — the daemon
	// `MkdirAll`s under it at boot per the #284 lifecycle.
	PluginDataRoot string `yaml:"plugin_data_root"`

	// FillInstruction is operator-supplied prose injected verbatim onto
	// every `needs_fill` ingest response under the `instruction` wire
	// field (per ADR-0013 §2). The intent is to give the agent's AI a
	// stable, operator-controlled directive on how to approach gap
	// filling — without requiring per-call API surface changes.
	//
	// Empty / unset → no `instruction` field on the response (omitempty
	// at the wire layer). Non-empty → the string is passed verbatim,
	// byte-identical to this config value; the server does NOT compose
	// or post-process it.
	//
	// Per-kind override + canonical_vocabulary registry are deferred
	// to follow-up PRs per ADR-0013's "Implementation order".
	// (No `omitempty` on the yaml tag — yaml.v3's omitempty affects
	// marshal only; on unmarshal a missing key already leaves the
	// field at its zero value. Keeping the tag bare avoids advertising
	// behavior that doesn't exist on the read path.)
	FillInstruction string `yaml:"fill_instruction"`
}

// UserContentFrontmatterEdgeMapping is one entry in
// Config.UserContentFrontmatterEdges. Mirrors
// the plugin-side `plugins.FrontmatterEdgeMapping` shape — same
// `{edge_type, target_kind}` pair — so the daemon can call the
// shared `appendFrontmatterDerivedCanonicals` helper with operator-
// sourced mappings just like it does with plugin-Capabilities-
// sourced ones for ingest.
//
// Wire YAML shape: `{edge_type: "...", target_kind: "..."}`.
type UserContentFrontmatterEdgeMapping struct {
	EdgeType string `yaml:"edge_type"`
	TargetKind string `yaml:"target_kind"`
}

// AuthEntry is the `auth:` block of the config document.
//
// - KeysDir is the directory holding `private.pem` + `public.pem`.
// Empty / unset → the CLI / env layer resolves the default
// (`/etc/yaad-index/keys/`) at runtime.
// - DefaultTTL is the duration the `yaad-index issue-token`
// subcommand uses when `--ttl` isn't passed. Empty / unset →
// the CLI defaults to `2160h` (90 days). Values use Go's
// `time.ParseDuration` syntax: `ns`/`us`/`ms`/`s`/`m`/`h` only
// (no `d` suffix).
// - Required is the master switch on the HTTP
// middleware. Tri-state on the YAML side so the precedence
// chain (CLI > env > config > default-true) can distinguish
// "operator left it unset" from "operator explicitly false."
// Default at the wire layer is `true` — running production
// unauthenticated requires an explicit opt-out.
//
// All three fields are operational, not vault-readable. Validation
// at Load time is deliberately minimal — empty values are valid
// (the CLI layer fills defaults); a non-empty KeysDir is NOT
// stat'd here because the keygen subcommand may run before the
// directory exists.
type AuthEntry struct {
	KeysDir string `yaml:"keys_dir"`
	DefaultTTL string `yaml:"default_ttl"`
	Required *bool `yaml:"required"`
}

// CanonicalKindConfig is one entry in the operator-config canonical-
// kind registry — per ADR-0013 §1 and ADR-0016 §3.
//
// Gaps maps gap-field-name → GapSpec ({type, description}). Both
// the long-form struct shape AND the pre-ADR-0016 shorthand
// (string description) parse via GapSpec's custom UnmarshalYAML —
// existing operator configs continue to work without rewrite.
//
// Instruction is the AI-fill instruction struct ({enabled, text}).
// Pointer-shape because at the operator-config layer absence is
// distinct from explicit-zero: a per-kind block omitting
// `instruction:` inherits from the canonical_kinds_defaults
// (root) layer; an explicit `instruction: {enabled: false}` opts
// the kind out. The pre-ADR-0016 `instruction: "string"` shape
// also parses via InstructionSpec's custom UnmarshalYAML
// (treated as Enabled=true with the string as Text).
//
// Plugins are FORBIDDEN from contributing to this struct's
// Instruction field per ADR-0016 §2; the daemon's capabilities
// parser strips plugin-supplied instruction with a WARN. Only
// operator config (root + per-kind) feeds Instruction into the
// merged effective registry.
type CanonicalKindConfig struct {
	Gaps        map[string]GapSpec `yaml:"gaps,omitempty" json:"gaps,omitempty"`
	Instruction *InstructionSpec   `yaml:"instruction,omitempty" json:"instruction,omitempty"`
	// ResolverPlugin names the plugin authoritative for entities
	// of this kind, per #276. When set, canonical_type gap fills
	// targeting this kind require the named canonical-id to
	// already exist in the store (the agent should have ingested
	// through the plugin first); fills against an unresolved
	// target return 422 `unresolved_target` with a suggested-
	// action hint naming the resolver plugin. When unset, fills
	// auto-materialize a thin row as today.
	//
	// Operator-fill can bypass the check by passing
	// `?allow_unresolved=true` on the POST /v1/entities/{id}/
	// operator-fill request; the bypass is stamped into the
	// commit message (`... (allow_unresolved)`) for audit.
	// Plugin-emit edge paths are unaffected — the plugin IS
	// the resolver when it emits its own canonical-edge
	// targets.
	ResolverPlugin string `yaml:"resolver_plugin,omitempty" json:"resolver_plugin,omitempty"`
}

// VaultEntry is the `vault:` block of the config document.
// WorkflowEntry bundles per-workflow-engine operator knobs per
// ADR-0027 cut 3.
type WorkflowEntry struct {
	// GraphWalkCap caps the per-call result list size on the
	// graph.in_edges / out_edges / in_neighbors / out_neighbors
	// CEL helpers. Zero / unset → decision.DefaultGraphWalkCap
	// (1000). Operators with dense graphs can raise the cap;
	// the truncation flag on the wrapping struct
	// {items, truncated, total} signals overflow regardless of
	// the cap value.
	GraphWalkCap int `yaml:"graph_walk_cap"`
}

type VaultEntry struct {
	Path string `yaml:"path"`

	// AutoCommit controls vault-write → git-commit. The vault
	// becomes its own audit log: every
	// successful Writer.Write produces a git commit summarizing
	// the operation. Tri-state via *bool:
	// - nil → auto-detect: enabled iff `<vault>/.git/` exists.
	// - &true → enabled (Validate fails if no .git/).
	// - &false → disabled regardless of .git/ presence.
	// Operators with a non-git vault leave the field unset; auto-detect
	// keeps yaad-index commit-free without ceremony.
	AutoCommit *bool `yaml:"auto_commit,omitempty"`

	// AutoCommitDebounceSeconds collapses bursty writes into batched
	// commits. 0 (default) → per-operation commit. >0 → collect writes
	// for N seconds, commit a single rollup with a summarized message
	// (`bulk: ingest 12 entities, fill 3, note 2`). Trade-off:
	// per-operation gives a 1:1 audit; debounce trades 1:1 for fewer
	// process spawns under bulk-import / reindex workloads.
	AutoCommitDebounceSeconds int `yaml:"auto_commit_debounce_seconds,omitempty"`

	// AutoPush runs `git push` after each (debounced or per-operation)
	// commit. Default false: operator pushes via cron / manual. Push
	// failures (network, auth, non-fast-forward) log loudly but do
	// NOT fail the underlying vault write — the local commit always
	// lands; the push is best-effort.
	AutoPush bool `yaml:"auto_push,omitempty"`

	// CommitterName / CommitterEmail are used as both the git committer
	// AND the default author when a write doesn't carry an explicit
	// agent identity. Empty values fall back to "yaad-index" /
	// "yaad-index@localhost". The author CAN be overridden per-write
	// (e.g. fill carries the calling agent's identity), but the
	// committer is always yaad-index — the process that wrote the
	// file IS the committer regardless of which agent triggered it.
	CommitterName string `yaml:"committer_name,omitempty"`
	CommitterEmail string `yaml:"committer_email,omitempty"`
}

// PluginEntry is one entry in Config.Plugins.
//
// Config is the per-plugin operator-supplied structured config
// per ADR-0006 (2026-05-22 amendment / #192). Daemon walks each
// entry at subprocess spawn time, JSON-marshals the whole block,
// and delivers it as a single `YAAD_PLUGIN_CONFIG` env var the
// plugin reads + `json.Unmarshal`s into its own struct on
// startup. Arbitrary YAML structure is allowed (scalars, lists,
// nested maps); the plugin owns its schema + advertises it via
// `--init`'s `config_schema` field so the daemon validates the
// operator input at registry-load time.
//
// Operator keys MUST NOT start with `_` — that prefix is
// reserved for daemon-injected fields (e.g. `_name`, which
// carries the entry's `name:` value through to the subprocess
// for multi-instance plugins).
type PluginEntry struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
	// FetchTimeout, when set, overrides DefaultFetchTimeout on the
	// subprocess wrapper for this plugin's per-request fetches.
	// Format is anything `time.ParseDuration` accepts (e.g. `30s`,
	// `5m`, `1h`). Validate enforces positive parse on non-empty
	// values; empty means "use the daemon default."
	FetchTimeout string         `yaml:"fetch_timeout,omitempty"`
	Config       map[string]any `yaml:"config,omitempty"`

	// Instances declares the per-plugin runtime-config variants per
	// ADR-0028. Each entry is one independent runtime context
	// (e.g. two Gmail accounts, two GitHub identity contexts) sharing
	// the same plugin binary + capability set. Absent / nil → Load
	// synthesizes a single implicit instance named `default` so the
	// rest of the daemon can assume `len(Instances) >= 1` after a
	// successful Load. Empty `instances: []` is a config error.
	//
	// Instance-level `env:` and `config:` values are runtime
	// parameters passed to the subprocess at spawn time; the plugin's
	// `--init` capabilities are plugin-scoped per ADR-0028 §2 and
	// never re-probed per instance.
	//
	// Cross-validation against the plugin's `supports_instances`
	// capability (ADR-0028 §9) lives downstream of config.Load —
	// the daemon performs the fail-fast check after the plugin's
	// `--init` has reported capabilities, since the flag is read
	// from the plugin binary, not the operator config.
	Instances []InstanceEntry `yaml:"instances,omitempty"`
}

// InstanceEntry is one runtime-config variant of a plugin per
// ADR-0028 §1. Each instance has a required Name (operator-chosen,
// unique within the plugin, matching `[a-z0-9_-]+`) plus optional
// per-instance `env:` and `config:` blocks.
//
// The Name is used as the second half of the slash-form
// `<plugin>/<instance>` invocation syntax (ADR-0028 §4) and entity
// `source:` field shape (ADR-0028 §5).
//
// Enabled is the ADR-0028 §7 on/off flag (Cut 5). Nil pointer or
// true → enabled; false → disabled. A disabled instance stays in
// operator config + retains runtime state but is invisible to URL
// routing (Cut 3), command dispatch (Cut 4 fan-out + instance-
// scoped form both skip), and scheduled refresh. /v1/plugins
// surfaces the disabled instance so operators see the full
// configured set. Pointer shape keeps the YAML default-on contract
// (absent → enabled) distinct from explicit `enabled: false`.
type InstanceEntry struct {
	Name    string            `yaml:"name"`
	Env     map[string]string `yaml:"env,omitempty"`
	Config  map[string]any    `yaml:"config,omitempty"`
	Enabled *bool             `yaml:"enabled,omitempty"`
	// DataDir is the operator override for this instance's
	// per-(plugin,instance) persistent-state directory per #284.
	// When set, MUST be an absolute path; the daemon stamps it on
	// YAAD_PLUGIN_DATA_DIR for every subprocess invocation. When
	// empty, the daemon resolves the default
	// `<userCacheDir>/yaad-<plugin>/<instance>/` (see
	// internal/plugins/datadir.Resolve). Validated at Load time.
	DataDir string `yaml:"data_dir,omitempty"`
}

// IsEnabled returns true when this instance is operator-enabled
// per ADR-0028 §7. Nil pointer (operator omitted the flag) → true
// (default-on); explicit `enabled: false` → false.
func (e InstanceEntry) IsEnabled() bool {
	if e.Enabled == nil {
		return true
	}
	return *e.Enabled
}

// FetchTimeoutDuration returns the parsed FetchTimeout, or 0 when the
// operator did not set one (caller falls through to
// subprocess.DefaultFetchTimeout). Safe to call without an error
// check after Validate has passed — Validate rejects any non-empty
// FetchTimeout that does not parse.
func (e PluginEntry) FetchTimeoutDuration() time.Duration {
	if e.FetchTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(e.FetchTimeout)
	if err != nil {
		return 0
	}
	return d
}

// Load reads and validates the config at path. Returns ErrFileMissing
// (wrapped) when the file does not exist; the caller decides whether
// "no config" should be treated as "no plugins" (for development
// convenience) or fatal (for production).
func Load(path string) (*Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileMissing, path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %s: %w", path, err)
	}
	// Synthesize the implicit `default` instance for any plugin that
	// omitted the `instances:` block (ADR-0028 §1). Downstream code
	// can then assume `len(entry.Instances) >= 1` without per-call-
	// site defaulting. Runs after Validate so structural errors
	// surface against the operator's actual input shape, not a
	// post-synthesis shape they didn't write.
	//
	// The synthesized default instance inherits the plugin-level
	// `config:` block so the back-compat single-instance path
	// keeps the operator's existing config visible per-instance.
	// Two consumers:
	//   - per-instance JSON-Schema validation gate in cmd/yaad-index
	//     (Cut 1): without the Config copy, a legacy operator
	//     config (`config: {...}` with no `instances:`) would skip
	//     per-instance schema checks entirely.
	//   - per-call subprocess env splice (Cut 4): the dispatch
	//     layer builds YAAD_PLUGIN_CONFIG from instance.Config
	//     each call. Without the copy, single-instance plugins
	//     would receive an empty YAAD_PLUGIN_CONFIG when the
	//     daemon switched to per-call env (the legacy registry-
	//     build-time configEnv is dropped in favor of the per-
	//     call ctx splice).
	// Explicit `instances:` entries already own their own `config:`
	// + `env:` blocks and are untouched. PluginEntry today has no
	// `env:` field — instance-level env is the only operator-
	// writable env surface (per ADR-0028 §1); the synthesized
	// default's Env stays nil for back-compat single-instance
	// plugins, matching the pre-Cut-4 reality where no per-spawn
	// env-passthrough existed beyond the global pluginEnv() +
	// YAAD_PLUGIN_CONFIG path.
	for i := range c.Plugins {
		if c.Plugins[i].Instances == nil {
			c.Plugins[i].Instances = []InstanceEntry{{
				Name:   defaultInstanceName,
				Config: c.Plugins[i].Config,
			}}
		}
	}
	return &c, nil
}

// Validate enforces the ADR-0006 rules on each plugin entry. Public so
// tests and a future `--check-config` CLI flag can reuse it.
func (c *Config) Validate() error {
	seen := make(map[string]int, len(c.Plugins))
	for i, entry := range c.Plugins {
		if entry.Name == "" {
			return fmt.Errorf("plugin entry %d has empty name", i)
		}
		if prev, dup := seen[entry.Name]; dup {
			return fmt.Errorf("plugin %q: duplicate name (also at entry %d)", entry.Name, prev)
		}
		seen[entry.Name] = i
		if entry.Path == "" {
			return fmt.Errorf("plugin %q: path is empty", entry.Name)
		}
		if !filepath.IsAbs(entry.Path) {
			return fmt.Errorf("plugin %q: path %q is not absolute (ADR-0006: no relative paths or PATH search)",
				entry.Name, entry.Path)
		}
		info, err := os.Stat(entry.Path)
		if err != nil {
			return fmt.Errorf("plugin %q: stat %s: %w", entry.Name, entry.Path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("plugin %q: path %s is a directory, not a binary", entry.Name, entry.Path)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("plugin %q: path %s is not executable", entry.Name, entry.Path)
		}
		if err := validatePluginConfig(entry.Name, entry.Config); err != nil {
			return err
		}
		if err := validateInstances(entry.Name, entry.Instances); err != nil {
			return err
		}
		if entry.FetchTimeout != "" {
			d, err := time.ParseDuration(entry.FetchTimeout)
			if err != nil {
				return fmt.Errorf("plugin %q: fetch_timeout %q: %w",
					entry.Name, entry.FetchTimeout, err)
			}
			if d <= 0 {
				return fmt.Errorf("plugin %q: fetch_timeout %q must be positive",
					entry.Name, entry.FetchTimeout)
			}
		}
	}
	if c.Vault.Path != "" {
		if !filepath.IsAbs(c.Vault.Path) {
			return fmt.Errorf("vault.path %q is not absolute", c.Vault.Path)
		}
		info, err := os.Stat(c.Vault.Path)
		if err != nil {
			return fmt.Errorf("vault.path: stat %s: %w", c.Vault.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("vault.path %s is not a directory", c.Vault.Path)
		}
		if c.Vault.AutoCommitDebounceSeconds < 0 {
			return fmt.Errorf("vault.auto_commit_debounce_seconds: %d is negative; use 0 to disable debounce",
				c.Vault.AutoCommitDebounceSeconds)
		}
		if c.Vault.AutoCommit != nil && *c.Vault.AutoCommit {
			gitDir := filepath.Join(c.Vault.Path, ".git")
			if _, err := os.Stat(gitDir); err != nil {
				return fmt.Errorf("vault.auto_commit=true but %s does not exist: %w", gitDir, err)
			}
		}
		if c.Vault.AutoPush {
			disabled := c.Vault.AutoCommit != nil && !*c.Vault.AutoCommit
			if disabled {
				return fmt.Errorf("vault.auto_push=true requires vault.auto_commit (currently explicitly false)")
			}
		}
	} else {
		if c.Vault.AutoCommit != nil || c.Vault.AutoCommitDebounceSeconds != 0 || c.Vault.AutoPush ||
			c.Vault.CommitterName != "" || c.Vault.CommitterEmail != "" {
			return fmt.Errorf("vault.auto_commit / auto_push / committer_* fields require vault.path")
		}
	}
	if _, err := ParseLogLevel(c.LogLevel); err != nil {
		return err
	}
	// Timezone validation: empty defaults to
	// UTC (matches legacy behavior); non-empty must parse via
	// time.LoadLocation. Operator catches misspellings / bad IANA
	// identifiers at server start rather than seeing every
	// timestamp render in the wrong zone silently.
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("timezone: load %q: %w", c.Timezone, err)
		}
	}
	// Negative cache_ttl_seconds is now valid —
	// it expresses "infinite" at the global level under the three-
	// level sentinel resolution chain (positive=N seconds, 0=no
	// opinion fall through, negative=infinite). Legacy deployments
	// that used 0 to mean "infinite" continue to work because the
	// all-zero default also resolves to "no TTL stamped → cache
	// forever".
	if err := validateCanonicalKinds(c.CanonicalKinds); err != nil {
		return err
	}
	// Root-scoped operator-defaults (canonical_kinds_defaults: top-
	// level sibling per ADR-0016 §3). Same shape rules as a
	// per-kind block; an empty / missing block is the common case.
	if err := validateCanonicalKindConfig("canonical_kinds_defaults", c.CanonicalKindsDefaults); err != nil {
		return err
	}
	if c.PluginStagingDir != "" {
		if !filepath.IsAbs(c.PluginStagingDir) {
			return fmt.Errorf("plugin_staging_dir %q is not absolute", c.PluginStagingDir)
		}
		info, err := os.Stat(c.PluginStagingDir)
		if err != nil {
			return fmt.Errorf("plugin_staging_dir: stat %s: %w", c.PluginStagingDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("plugin_staging_dir %s is not a directory", c.PluginStagingDir)
		}
	}
	if c.PluginDataRoot != "" && !filepath.IsAbs(c.PluginDataRoot) {
		// #287: don't stat — the daemon MkdirAll's under this
		// root at boot per #284's lifecycle, so the path may
		// legitimately not exist at Load time. Only enforce
		// absolute-path.
		return fmt.Errorf("plugin_data_root %q is not absolute", c.PluginDataRoot)
	}
	if err := validateUserContentFrontmatterEdges(c.UserContentFrontmatterEdges); err != nil {
		return err
	}
	return nil
}

// validateUserContentFrontmatterEdges enforces the per-mapping
// shape rules: each mapping must name a
// non-empty edge_type and target_kind. CanonicalGuard's enabled-
// kinds + edge-types gate runs at edge-creation time (same shape
// as the plugin path); the parse-time check here only catches
// outright-empty fields so a typo like
// `mentions: { edge_type: "", target_kind: person }` fails at
// server start instead of silently dropping every mention edge.
//
// Empty / nil map is valid (no derivation runs — legacy behavior).
func validateUserContentFrontmatterEdges(m map[string]UserContentFrontmatterEdgeMapping) error {
	for field, mapping := range m {
		if strings.TrimSpace(field) == "" {
			return fmt.Errorf("user_content_frontmatter_edges: empty field name")
		}
		if strings.TrimSpace(mapping.EdgeType) == "" {
			return fmt.Errorf("user_content_frontmatter_edges.%s: edge_type is required", field)
		}
		if strings.TrimSpace(mapping.TargetKind) == "" {
			return fmt.Errorf("user_content_frontmatter_edges.%s: target_kind is required", field)
		}
	}
	return nil
}

// validateCanonicalKinds enforces the operator-config registry
// shape per ADR-0013 §1 + ADR-0016 §3. Empty map is valid (the
// minimal config relies on code defaults + active-plugin
// auto-activation per ADR-0016).
//
// Per kind:
// - kind name matches [a-z][a-z0-9_]*
// - each declared gap field name matches [a-z][a-z0-9_]*
// - each gap description is non-whitespace-only
// - instruction (when set) accepts both shorthand (string) and
// long-form ({enabled, text}); whitespace-only Text is
// rejected even when Enabled=false (clear false-signal)
//
// Per ADR-0016 the per-kind block can omit `gaps:` entirely (the
// merged effective registry inherits from code defaults + plugin
// extras + operator-defaults). The pre-ADR-0016 "must declare at
// least one gap" check is removed; the merge layer guarantees
// every kind has name/tags/summary regardless of operator block.
//
// Errors are scoped with a dotted path so operators reading the
// failure can navigate directly to the offending key.
func validateCanonicalKinds(reg map[string]CanonicalKindConfig) error {
	for kind, cfg := range reg {
		if !canonicalKindName.MatchString(kind) {
			return fmt.Errorf("canonical_kinds.%s: invalid kind name; must match [a-z][a-z0-9_]*(-[a-z0-9_]+)* (hyphens allowed between alphanumeric groups; no trailing or consecutive hyphens)", kind)
		}
		if err := validateCanonicalKindConfig(fmt.Sprintf("canonical_kinds.%s", kind), cfg); err != nil {
			return err
		}
	}
	return nil
}

// validateCanonicalKindConfig validates a single per-kind (or
// root-defaults) block — gap field names + descriptions, and the
// instruction's whitespace-only-text rejection. Used by both the
// per-kind validator and the canonical_kinds_defaults validator.
func validateCanonicalKindConfig(path string, cfg CanonicalKindConfig) error {
	for field, spec := range cfg.Gaps {
		if !kindOrFieldName.MatchString(field) {
			return fmt.Errorf("%s.gaps.%s: invalid gap field name; must match [a-z][a-z0-9_]*", path, field)
		}
		if strings.TrimSpace(spec.Description) == "" {
			return fmt.Errorf("%s.gaps.%s.description: cannot be empty or whitespace-only", path, field)
		}
		if err := spec.Validate(fmt.Sprintf("%s.gaps.%s", path, field)); err != nil {
			return err
		}
	}
	if cfg.Instruction != nil && cfg.Instruction.Text != "" && strings.TrimSpace(cfg.Instruction.Text) == "" {
		return fmt.Errorf("%s.instruction.text: cannot be whitespace-only", path)
	}
	return nil
}

// ErrFileMissing wraps os.IsNotExist on the config file path. Distinct
// from a YAML parse / validate error so callers can distinguish
// "operator hasn't created the file yet" from "config is broken."
var ErrFileMissing = errors.New("config file does not exist")

// validateInstances enforces the per-plugin `instances:` structural
// rules from ADR-0028 §1: empty array is rejected; each name must
// match instanceName; names must be unique within the plugin. Nil
// slice (no `instances:` block in YAML) is OK — Load synthesizes
// the implicit `default` instance after Validate returns.
//
// The cross-validation gate against the plugin's `supports_instances`
// capability (§9) is NOT performed here — that flag is read from the
// plugin binary's `--init` output, which isn't available until the
// daemon registers the plugin at startup. The gate runs in
// cmd/yaad-index after capability bring-up.
func validateInstances(pluginName string, instances []InstanceEntry) error {
	if instances == nil {
		return nil
	}
	if len(instances) == 0 {
		return fmt.Errorf("plugin %q: instances: must not be empty (omit the block to use the implicit default instance)",
			pluginName)
	}
	seen := make(map[string]int, len(instances))
	for i, inst := range instances {
		if inst.Name == "" {
			return fmt.Errorf("plugin %q: instances[%d] has empty name", pluginName, i)
		}
		if !instanceName.MatchString(inst.Name) {
			return fmt.Errorf("plugin %q: instances[%d].name %q is invalid (must match %s)",
				pluginName, i, inst.Name, instanceName.String())
		}
		if prev, dup := seen[inst.Name]; dup {
			return fmt.Errorf("plugin %q: instances[%d].name %q duplicates instances[%d].name",
				pluginName, i, inst.Name, prev)
		}
		seen[inst.Name] = i
		if inst.DataDir != "" && !filepath.IsAbs(inst.DataDir) {
			return fmt.Errorf("plugin %q: instances[%d].data_dir %q is not absolute (ADR-0028 + #284: operator-override data_dir must be an absolute path)",
				pluginName, i, inst.DataDir)
		}
		// #286: reject any operator env entry that would shadow
		// a daemon-stamped key per #284's last-wins-shadow
		// semantics. ReservedInstanceEnvKeys is the single
		// source of truth; the hint text routes the operator to
		// the correct structured knob.
		for key, hint := range ReservedInstanceEnvKeys {
			if _, shadowed := inst.Env[key]; shadowed {
				return fmt.Errorf("plugin %q: instances[%d].env[%s] is reserved (daemon-owned per #286 — %s)",
					pluginName, i, key, hint)
			}
		}
	}
	return nil
}
