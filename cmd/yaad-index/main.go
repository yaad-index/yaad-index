// Command yaad-index is the HTTP server entry point.
//
// See ADR-0001 (AI-first remote API), ADR-0002 (v1 endpoint surface),
// ADR-0003 (kong CLI library), ADR-0004 (slog logging), and ADR-0006
// (plugin discovery via config allowlist) under adr/.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/yaad-index/yaad-index/internal/api"
	"github.com/yaad-index/yaad-index/internal/buildinfo"
	"github.com/yaad-index/yaad-index/internal/attachments"
	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/datadir"
	"github.com/yaad-index/yaad-index/internal/plugins/subprocess"
	"github.com/yaad-index/yaad-index/internal/reindex"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/loader"
	wftasks "github.com/yaad-index/yaad-index/internal/workflow/tasks"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// loaderRegistryAdapter bridges *plugins.Registry to
// loader.PluginRegistry — the loader's interface deliberately
// returns `any` so internal/workflow/loader doesn't depend on
// internal/plugins. The adapter is a one-method passthrough.
type loaderRegistryAdapter struct {
	registry *plugins.Registry
}

func (a loaderRegistryAdapter) LookupByName(name string) (any, bool) {
	return a.registry.LookupByName(name)
}

// canonicalLoaderRegistry satisfies loader.CanonicalRegistry
// against the operator's merged canonical-kinds registry +
// declared canonical_edge_types config. The loader uses this to
// reject workflow files at load time that name an unknown
// edge_type or target.kind in an add_canonical_edge action.
type canonicalLoaderRegistry struct {
	kinds     map[string]config.CanonicalKindConfig
	edgeTypes []string
}

func (r canonicalLoaderRegistry) KindExists(kind string) bool {
	_, ok := r.kinds[kind]
	return ok
}

func (r canonicalLoaderRegistry) EdgeTypeExists(edgeType string) bool {
	for _, t := range r.edgeTypes {
		if t == edgeType {
			return true
		}
	}
	return false
}

func (r canonicalLoaderRegistry) RegisteredGapTypes(gap string) []string {
	seen := make(map[string]struct{})
	for _, kindCfg := range r.kinds {
		spec, ok := kindCfg.Gaps[gap]
		if !ok {
			continue
		}
		t := spec.Type
		if t == "" {
			t = "string"
		}
		seen[t] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out
}

// storeEntityResolver satisfies engine.EntityResolver against
// the production store.Store. Returns a nested map:
//   - top-level `id` + `kind` so CEL predicates can reference
//     entity.id and entity.kind directly.
//   - top-level `data` carrying the entity's frontmatter Data
//     sub-map so predicates can read entity.data.<field>
//     (e.g. entity.data.subject for a gmail entity).
//
// The nested shape matches docs/workflows.md §11's worked
// example and the canonical CEL access pattern operators
// declare in their workflow files. The previous shape flattened
// Data into the top level — entity.data.subject failed CEL
// evaluation with `no such key: data` even though the entity's
// vault frontmatter clearly had `data: {subject, date}`
// populated. Bug fix per #145.
type storeEntityResolver struct {
	st store.Store
}

func (r *storeEntityResolver) Resolve(ctx context.Context, id string) (map[string]any, error) {
	e, err := r.st.GetEntity(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, decision.ErrEntityNotFound
	}
	if err != nil {
		return nil, err
	}
	return entityToCELMap(e), nil
}

// entityToCELMap is the single source-of-truth shape for entities
// crossing into the workflow CEL evaluator. Both storeEntityResolver.Resolve
// (single-entity Get path) and storeGraphWalker.walkNeighbors (batch GetEntities
// path) route through this helper so the CEL-side `entity.X` access pattern
// stays consistent — adding a field here surfaces it everywhere at once,
// avoiding the resolver/walker divergence that hand-built map shapes invite.
//
// `data` may be nil for canonical-label thin rows (no plugin-emitted Data);
// CEL `has(entity.data)` returns false against an absent key cleanly, but
// operators commonly write `entity.data.X` — a nil sub-map produces
// `no such key: X` on the inner access, which is the expected failure shape
// (the data field genuinely isn't there). Workflows that want to guard the
// missing-data case use `has(entity.data) && entity.data.X != ""`.
//
// `slug` is the after-colon suffix of the canonical id (`<kind>:<slug>` per
// ADR-0021), derived rather than stored. Surfaced because the documented
// `subject: '{{ entity.slug }}'` shape would otherwise fail with `no such
// key: slug`. For an id missing the colon separator the slug degrades to
// the full id — safe shape for malformed/legacy rows.
func entityToCELMap(e *store.Entity) map[string]any {
	out := make(map[string]any, 4)
	out["id"] = e.ID
	out["kind"] = e.Kind
	out["slug"] = strings.TrimPrefix(e.ID, e.Kind+":")
	if len(e.Data) > 0 {
		dataMap := make(map[string]any, len(e.Data))
		for k, v := range e.Data {
			dataMap[k] = v
		}
		out["data"] = dataMap
	}
	return out
}

// storeGraphWalker satisfies decision.GraphWalker against the
// production store. Edge walks call GetEdgesTo / GetEdgesFor
// with the optional edge-type filter; neighbor walks chain the
// edge fetch with a GetEntities batch so the returned list<Entity>
// arrives in two round trips (edge query + batch entity fetch)
// rather than the N+1 shape a CEL-side
// `graph.in_edges(id).map(e, graph.get(e.from))` would produce.
//
// Per-call cap is enforced after the edge fetch in-memory; the
// store-level GetEdgesFor / GetEdgesTo don't currently take a
// LIMIT (the existing surface walks a single entity's outbound
// edges, bounded by per-entity edge counts in practice). When
// graph density justifies it, push the LIMIT into SQL — for v1
// the in-memory cap matches the operator-observed performance
// shape on day-anchor walks.
//
// nil store → every method returns the empty result; useful for
// dev binaries that don't wire a store.
type storeGraphWalker struct {
	st       store.Store
	resolver *storeEntityResolver
}

func (w *storeGraphWalker) Get(ctx context.Context, id string) (map[string]any, error) {
	if w.resolver == nil {
		return nil, decision.ErrEntityNotFound
	}
	return w.resolver.Resolve(ctx, id)
}

func (w *storeGraphWalker) InEdges(ctx context.Context, toID, edgeType string, limit int) ([]decision.WalkEdge, int, error) {
	return w.walkEdges(ctx, edgeType, limit, func(types []string) ([]store.Edge, error) {
		return w.st.GetEdgesTo(ctx, toID, types)
	})
}

func (w *storeGraphWalker) OutEdges(ctx context.Context, fromID, edgeType string, limit int) ([]decision.WalkEdge, int, error) {
	return w.walkEdges(ctx, edgeType, limit, func(types []string) ([]store.Edge, error) {
		return w.st.GetEdgesFor(ctx, fromID, types)
	})
}

func (w *storeGraphWalker) InNeighbors(ctx context.Context, toID, edgeType string, limit int) ([]map[string]any, int, error) {
	return w.walkNeighbors(ctx, edgeType, limit,
		func(types []string) ([]store.Edge, error) { return w.st.GetEdgesTo(ctx, toID, types) },
		func(e store.Edge) string { return e.From },
	)
}

func (w *storeGraphWalker) OutNeighbors(ctx context.Context, fromID, edgeType string, limit int) ([]map[string]any, int, error) {
	return w.walkNeighbors(ctx, edgeType, limit,
		func(types []string) ([]store.Edge, error) { return w.st.GetEdgesFor(ctx, fromID, types) },
		func(e store.Edge) string { return e.To },
	)
}

func (w *storeGraphWalker) walkEdges(_ context.Context, edgeType string, limit int, fetch func([]string) ([]store.Edge, error)) ([]decision.WalkEdge, int, error) {
	var typeFilter []string
	if edgeType != "" {
		typeFilter = []string{edgeType}
	}
	edges, err := fetch(typeFilter)
	if err != nil {
		return nil, 0, err
	}
	total := len(edges)
	if limit > 0 && total > limit {
		edges = edges[:limit]
	}
	out := make([]decision.WalkEdge, len(edges))
	for i, e := range edges {
		out[i] = decision.WalkEdge{
			From:     e.From,
			To:       e.To,
			Type:     e.Type,
			Metadata: e.Metadata,
		}
	}
	return out, total, nil
}

func (w *storeGraphWalker) walkNeighbors(ctx context.Context, edgeType string, limit int, fetch func([]string) ([]store.Edge, error), endpoint func(store.Edge) string) ([]map[string]any, int, error) {
	var typeFilter []string
	if edgeType != "" {
		typeFilter = []string{edgeType}
	}
	edges, err := fetch(typeFilter)
	if err != nil {
		return nil, 0, err
	}
	total := len(edges)
	if limit > 0 && total > limit {
		edges = edges[:limit]
	}
	if len(edges) == 0 {
		return nil, total, nil
	}
	// De-dupe endpoint ids before the batch fetch — a single
	// source-side entity may appear on multiple inbound edges
	// (e.g. references_day + due_on to the same day from one
	// task).
	idSet := make(map[string]struct{}, len(edges))
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		id := endpoint(e)
		if _, dup := idSet[id]; dup {
			continue
		}
		idSet[id] = struct{}{}
		ids = append(ids, id)
	}
	matched, _, err := w.st.GetEntities(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[string]map[string]any, len(matched))
	for i := range matched {
		// Loop-variable address capture is fine here because
		// entityToCELMap reads-and-copies fields; the entity
		// pointer doesn't escape the call.
		byID[matched[i].ID] = entityToCELMap(&matched[i])
	}
	// Preserve edge order in the output — operator semantics
	// expect "neighbors in the order their edges land in the
	// store" (matches the existing GetEdges* ordering).
	out := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		if m, ok := byID[endpoint(e)]; ok {
			out = append(out, m)
		}
	}
	return out, total, nil
}

// ServeCmd implements `yaad-index serve`.
type ServeCmd struct {
	Bind string `name:"bind" env:"YAAD_INDEX_BIND" default:"localhost:7433" help:"host:port to bind the HTTP server to."`
	DBPath string `name:"db-path" env:"YAAD_INDEX_DB_PATH" default:"~/.local/share/yaad-index/yaad.db" help:"path to the SQLite database file (auto-created on first run)."`
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/yaad-index/config.yaml" help:"path to the yaad-index config file (per ADR-0006). Missing file → no plugins; broken file → fail-fast."`
	AuthRequired *bool `name:"auth-required" env:"YAAD_INDEX_AUTH_REQUIRED" help:"require Bearer JWT on protected routes (per yaad-index). Default true; pass --auth-required=false (or set YAAD_INDEX_AUTH_REQUIRED=false) for dev mode. Lowest priority is auth.required in the config file."`
	KeysDir string `name:"keys-dir" env:"YAAD_INDEX_KEYS_DIR" help:"override auth.keys_dir from the config file. Public-key (public.pem) is loaded from here at startup to verify Bearer JWTs. Default /etc/yaad-index/keys/."`
}

// Run starts the HTTP server and blocks until SIGINT/SIGTERM, then performs
// a bounded graceful shutdown.
func (s *ServeCmd) Run() error {
	// Per the prior design,: the slog handler's level threshold is operator-
	// controlled via `log_level:` in the config. Use a slog.LevelVar
	// so a single logger instance carries through the whole startup
	// sequence — pre-config log lines render at the default (info),
	// the level swaps post-config-load to whatever the operator
	// asked for. This avoids the chicken-and-egg of "logger needs
	// the level, level needs the config, config load might log."
	levelVar := new(slog.LevelVar)
	levelVar.Set(config.DefaultLogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar, ReplaceAttr: clock.LogTimeAttr}))
	slog.SetDefault(logger)

	dbPath, err := expandPath(s.DBPath)
	if err != nil {
		return fmt.Errorf("expand --db-path %q: %w", s.DBPath, err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("close store", "err", err)
		}
	}()
	logger.Info("store ready", "db_path", dbPath)

	cfg, err := loadConfigOptional(logger, s.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg != nil {
		// ParseLogLevel was already called inside Validate during Load,
		// so this can't fail here — but defensive re-parse keeps the
		// wiring honest if Validate ever loosens.
		level, perr := config.ParseLogLevel(cfg.LogLevel)
		if perr != nil {
			return fmt.Errorf("parse log_level: %w", perr)
		}
		levelVar.Set(level)
		// Operator-configured timezone per yaad-index. Parsed +
		// validated in cfg.Validate; defensive re-parse here keeps
		// the wiring honest if Validate ever loosens. Empty / unset
		// → leave clock package on its default (UTC), matching pre-
		// behavior.
		if cfg.Timezone != "" {
			tzLoc, perr := time.LoadLocation(cfg.Timezone)
			if perr != nil {
				return fmt.Errorf("parse timezone %q: %w", cfg.Timezone, perr)
			}
			clock.SetLocation(tzLoc)
			logger.Info("operator timezone active",
				"timezone", cfg.Timezone, "offset", tzLoc.String())
		}
	}
	registry, err := buildPluginRegistry(logger, st, cfg)
	if err != nil {
		return fmt.Errorf("build plugin registry: %w", err)
	}

	// Build the per-plugin instance lookups per ADR-0028 §1 + §3
	// (Cuts 1 + 3). Two parallel maps with different consumers:
	//   - pluginInstances (name-only) → ingest tracker's
	//     resolveInstanceName + /v1/plugins handler's instances
	//     surface.
	//   - pluginInstanceConfigs (full per-instance Config block)
	//     → /v1/ingest URL routing layer for glob-match against
	//     each instance's `config[<config_field>]`.
	// Cut 1's Load synthesis ensures every PluginEntry has at
	// least one instance (synthesized `default` or explicit
	// operator-named); we surface those lists here. nil cfg path
	// (dev binaries without an operator config) leaves both maps
	// empty — resolveInstanceName falls back to `default` and
	// pickInstance falls back to the single-instance fast path.
	pluginInstances := map[string][]string{}
	pluginInstanceConfigs := map[string][]config.InstanceEntry{}
	if cfg != nil {
		for _, entry := range cfg.Plugins {
			if len(entry.Instances) > 0 {
				names := make([]string, 0, len(entry.Instances))
				for _, inst := range entry.Instances {
					names = append(names, inst.Name)
				}
				pluginInstances[entry.Name] = names
				pluginInstanceConfigs[entry.Name] = append([]config.InstanceEntry(nil), entry.Instances...)
			}
		}
	}

	// ADR-0028 §3 Cut 3 — emit a startup warning for any
	// multi-instance plugin whose operator-config instance globs
	// overlap. Per the §3 amendment locked in PR-242, first-match
	// wins on the resolution path; the overlap warning is a
	// diagnostic so the operator notices ambiguous routing rather
	// than discover it through misattributed ingest.
	if cfg != nil {
		capsByName := map[string]plugins.Capabilities{}
		for _, p := range registry.Plugins() {
			capsByName[p.Name()] = p.Capabilities()
		}
		api.WarnInstanceRoutingOverlap(logger, pluginInstanceConfigs, capsByName)
		// #256: fail-fast on any unresolved `${NAME}` reference
		// inside an instance.env value so the operator sees the
		// missing-secret error at boot rather than at first
		// dispatch. Empty-resolution refs warn but proceed.
		if err := validatePluginInstanceEnvReferences(pluginInstanceConfigs, logger); err != nil {
			return fmt.Errorf("validate plugin instance env: %w", err)
		}
		// #287: stamp the operator's `plugin_data_root` (if any) on
		// the datadir resolver BEFORE ensurePluginInstanceDataDirs
		// so the startup MkdirAll and the per-dispatch
		// buildInstanceEnv stamp the same resolved path. Calling
		// SetRoot after ensure would create dirs at the env-driven
		// fallback while subsequent buildInstanceEnv stamped the
		// plugin_data_root-derived path — handing the plugin a
		// path the daemon didn't MkdirAll. Empty leaves the
		// resolver on the env-driven precedence chain
		// ($STATE_DIRECTORY → UserCacheDir).
		datadir.SetRoot(cfg.PluginDataRoot)
		if cfg.PluginDataRoot != "" {
			logger.Info("plugin data dir root configured", "plugin_data_root", cfg.PluginDataRoot)
		}
		// #284: provision per-(plugin,instance) persistent-state
		// directories with 0700 perms before any plugin subprocess
		// spawns. Resolves operator override / default, MkdirAll
		// the path, fails fast if a non-dir squats the resolved
		// path. buildInstanceEnv stamps the same resolved path on
		// YAAD_PLUGIN_DATA_DIR for every per-call invocation; both
		// resolve from the same Resolve() so paths match.
		if err := ensurePluginInstanceDataDirs(pluginInstanceConfigs); err != nil {
			return fmt.Errorf("ensure plugin instance data dirs: %w", err)
		}
	}

	// Construct the daemon-internal event bus (per ADR-0024
	// Phase 2). The API mutation handlers publish to + the
	// workflow engine subscribes from this same bus, so
	// instance-sharing is essential: a separate default-bus
	// inside the api package would leave the engine
	// subscribing to a phantom bus that never sees any
	// events.
	bus := eventbus.NewMemoryBus()

	// pluginInstances is the plugin-name → ordered instance-name
	// list per ADR-0028 §1. Cut 1's Load synthesis ensures every
	// PluginEntry has at least one instance (synthesized `default`
	// or explicit operator-named); we surface the full list here so
	// the ingest tracker (active instance = index 0) and
	// /v1/plugins (full list exposure) each get what they need.
	// Cuts 3 + 4 will widen tracker usage to per-invocation
	// routing. nil cfg path (dev binaries without an operator
	// config) leaves the map empty — the tracker's resolver falls
	// back to `default` and /v1/plugins synthesizes the implicit
	// instance per ADR-0028 §1.
	var (
		handlerOpts    = []api.HandlerOption{api.WithEventBus(bus), api.WithPluginInstances(pluginInstances), api.WithPluginInstanceConfigs(pluginInstanceConfigs)}
		guard          *config.CanonicalGuard
		mergedRegistry           map[string]config.CanonicalKindConfig
		canonicalKindProvenance  config.RegistryProvenance
		wfEngine       *engine.Engine
		// enabledEdgeTypes is computed inside the `if cfg != nil`
		// block (plugin+operator union) and re-used by the reindex
		// wiring further below so the alias-rederive path can
		// classify each frontmatter alias as bare vs typed (per
		// #3). nil/empty stays permissive — every alias bare.
		enabledEdgeTypes []string
		// edgeService is the centralized edge-write service per
		// #304 Cut C1; built inside the `if cfg != nil` block
		// when canonicalKindResolvers is available, then reused
		// by the reindexer + workflow runner constructors that
		// land later in this function. Outside the cfg-loaded
		// path it stays nil and downstream constructors default
		// to a passthrough Service over the store.
		edgeService *edgewrite.Service
	)
	if cfg != nil {
		// ADR-0016 §4: build the merged effective canonical-kind
		// registry from {code defaults, plugin extras, operator
		// defaults, operator per-kind}. The merged result drives
		// the runtime — guard, needs_fill responses, structure,
		// AI-fill prompts. Plugin-emitted canonical_kinds_emitted
		// auto-activate via the merge (no operator re-declaration).
		pluginGaps, pluginEmittedKinds := collectPluginCanonicalContributions(registry)
		mergedRegistry, canonicalKindProvenance = config.MergeCanonicalRegistryWithProvenance(
			pluginGaps,
			pluginEmittedKinds,
			cfg.CanonicalKindsDefaults,
			cfg.CanonicalKinds,
			logger,
		)
		// #48 slice 4 — boot-time signal-only audit: one INFO line
		// per kind whose merge produced at least one plugin /
		// operator / operator-defaults layer contribution; full
		// per-(kind, field) provenance dumped at DEBUG. Vanilla
		// install with code/builtin defaults only stays silent.
		config.LogCanonicalRegistryBootAudit(logger, mergedRegistry, canonicalKindProvenance)

		// Per ADR-0013 §1: the canonical-kinds set drives the
		// CanonicalGuard. After ADR-0016, "kinds set" is the keys
		// of the merged registry — code defaults are always-on for
		// every plugin-emitted kind, so plugin auto-activation
		// flows through here too.
		//
		// Edge types follow the same ADR-0016 §plugin-driven-
		// activation semantic: a plugin's Capabilities.
		// CanonicalEdgeTypesEmitted auto-activate via union with the
		// operator's canonical_edge_types. Without this, plugins like
		// yaad-wikipedia emitting is_about would drop edges silently
		// on every ingest until an operator copied the type into
		// config — which contradicts the ADR-0016 plugin-driven-
		// activation promise and surfaces as cv-status drift
		// (yaad-index #9).
		//
		// The kind arg derives from mergedRegistry while the edge
		// arg derives from the bare registry — same plugin source
		// either way. mergedRegistry's KEY SET is built by
		// MergeCanonicalRegistry from (pluginEmittedKinds collected
		// via collectPluginCanonicalContributions(registry)) UNION
		// (operator cfg.CanonicalKinds keys); the edge path here
		// mirrors that exact union shape via unionEdgeTypes — only
		// the kind-side has a kind-keyed config struct to bind gaps,
		// so the edge path doesn't go through a merge intermediate.
		// Effective enabled sets are symmetric.
		enabledEdgeTypes = unionEdgeTypes(cfg.CanonicalEdgeTypes, collectPluginEmittedEdgeTypes(registry))
		guard = canonical.NewGuardWithDaemonDefaults(canonicalKindNames(mergedRegistry), enabledEdgeTypes)
		handlerOpts = append(handlerOpts, api.WithCanonicalGuard(guard))
		warnCanonicalEmissionGaps(logger, registry, guard)
		// Always pass the configured cache_ttl_seconds through (even
		// when 0 or negative) — yaad-index's resolveCacheTTL
		// uses sentinel rules: positive=N seconds, 0=no opinion fall
		// through, negative=infinite. Legacy the option was
		// gated on > 0 because negative was a config-validation
		// error; that gate is now lifted.
		ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
		handlerOpts = append(handlerOpts, api.WithCacheTTL(ttl))
		if cfg.CacheTTLSeconds != 0 {
			logger.Info("global cache_ttl_seconds set", "cache_ttl_seconds", cfg.CacheTTLSeconds)
		}
		if cfg.FillInstruction != "" {
			handlerOpts = append(handlerOpts, api.WithFillInstruction(cfg.FillInstruction))
			logger.Info("fill_instruction configured (per ADR-0013)",
				"len", len(cfg.FillInstruction))
		}
		handlerOpts = append(handlerOpts, api.WithMCPServerVersion(buildinfo.Version))
		if len(mergedRegistry) > 0 {
			handlerOpts = append(handlerOpts, api.WithCanonicalKindRegistry(mergedRegistry))
			handlerOpts = append(handlerOpts, api.WithCanonicalKindProvenance(canonicalKindProvenance))
			logger.Info("canonical_kinds registry merged + surfaced (per ADR-0013 §2 a prior PR, ADR-0016 §4 four-layer merge)",
				"kinds", len(mergedRegistry),
				"plugin_emitted", len(pluginEmittedKinds),
				"operator_per_kind", len(cfg.CanonicalKinds))
		}

		// Build the canonical-kind → resolver-plugin ownership map per
		// #304 Cut A. Strict per-plugin subset validation (error on
		// declares-but-doesn't-emit); lenient cross-plugin coverage
		// (WARN on emitted-without-resolver, upgraded to ERROR in
		// Cut C's centralized edge-write).
		canonicalKindResolvers, err := buildCanonicalKindResolvers(registry, pluginEmittedKinds, logger)
		if err != nil {
			return fmt.Errorf("canonical kind resolvers: %w", err)
		}
		if len(canonicalKindResolvers) > 0 {
			handlerOpts = append(handlerOpts, api.WithCanonicalKindResolvers(canonicalKindResolvers))
			logger.Info("canonical_kind_resolvers ownership map built (per #304 Cut A)",
				"kinds_with_resolver", len(canonicalKindResolvers))
		}

		// Construct the centralized edge-write service per #304
		// Cut C1. Every edge-create entry point in the daemon
		// routes through this single Service so Cut C2 + C3 can
		// add caller-mode + resolver-aware behavior in one
		// place. Cardinality enforcement (≤1 resolver per kind)
		// happens inside edgewrite.New — Cut A built a map[][]string
		// to defer the cardinality decision; Cut C1 upgrades the
		// multi-resolver case to a config-load ERROR.
		edgeService, err = edgewrite.New(st, canonicalKindResolvers)
		if err != nil {
			return fmt.Errorf("edge-write service: %w", err)
		}
		handlerOpts = append(handlerOpts, api.WithEdgeWriter(edgeService))
		// Pass-through unconditionally — the build helper produces a
		// `[]` wire shape on empty input regardless, but dropping the
		// len-guard here means the option is always wired in a single
		// place. The cold-reviewer's a prior PR catch on the upstream null-empty drift.
		handlerOpts = append(handlerOpts, api.WithCanonicalEdgeTypes(cfg.CanonicalEdgeTypes))

		// Per yaad-index: thread the operator-config UGC
		// frontmatter-edge mappings into the UGC create handler so
		// declared fields trigger canonical-edge derivation. Empty/
		// nil mapping → derivation is a no-op (the dead config field
		// from/a prior PR stays parseable but inert).
		if len(cfg.UserContentFrontmatterEdges) > 0 {
			handlerOpts = append(handlerOpts, api.WithUserContentFrontmatterEdges(cfg.UserContentFrontmatterEdges))
			logger.Info("user_content_frontmatter_edges configured (per yaad-index)",
				"mappings", len(cfg.UserContentFrontmatterEdges))
		}
	}

	// Per yaad-index a prior PR: resolve auth.required (CLI > env > config
	// > default-true) and load the public-key verifier when enforcement
	// is on. `auth.required=false` skips the verifier load entirely so
	// dev binaries don't need a keypair to start.
	authRequired, authVerifier, err := resolveAuth(logger, s.AuthRequired, s.KeysDir, cfg)
	if err != nil {
		return fmt.Errorf("init auth: %w", err)
	}
	handlerOpts = append(handlerOpts,
		api.WithAuthRequired(authRequired),
		api.WithAuthVerifier(authVerifier),
	)
	if !authRequired {
		logger.Warn("auth disabled (auth.required=false) — dev mode; protected routes pass through with synthetic anonymous claim",
			"hint", "drop --auth-required / YAAD_INDEX_AUTH_REQUIRED / auth.required to enable Bearer-JWT enforcement")
	}

	// Per yaad-index a prior PR: serve /v1/jwks whenever a keypair is
	// reachable on disk — independent of whether enforcement is on.
	// Peers verifying yaad-index-issued tokens need the public key
	// regardless of how this server is enforcing on inbound requests.
	// Dev-mode without keys → endpoint stays unregistered (404).
	if jwksKeys := loadJWKSIfAvailable(logger, s.KeysDir, cfg); len(jwksKeys) > 0 {
		handlerOpts = append(handlerOpts, api.WithJWKS(jwksKeys))
		logger.Info("/v1/jwks active",
			"keys", len(jwksKeys))
	}
	if cfg != nil && cfg.Vault.Path != "" {
		reindexer, err := reindex.NewWithOptions(st, cfg.Vault.Path, guard, logger, enabledEdgeTypes)
		if err != nil {
			return fmt.Errorf("init reindex (vault.path=%s): %w", cfg.Vault.Path, err)
		}
		// Route reindex's edge-restore path through the centralized
		// edge-write service per #304 Cut C1 — keeps every edge-
		// create call site on the same service so Cut C2 + C3 can
		// add routing behavior in one place.
		reindexer.SetEdgeWriter(edgeService)
		handlerOpts = append(handlerOpts, api.WithReindexHandler(api.HandleReindex(logger, reindexer)))

		// Auto-commit (per yaad-index the source issue): construct a
		// GitCommitter when the vault root is a git working tree and
		// the operator hasn't opted out via vault.auto_commit: false.
		// The plain Writer call ignores the committer; only handlers
		// invoking WriteWithCommit produce git commits.
		committer, autoCommitOn := buildAutoCommitter(logger, cfg.Vault)
		writerOpts := []vault.WriterOption{
			vault.WithCanonicalKinds(guard.EnabledKinds()),
			vault.WithLogger(logger),
		}
		if committer != nil {
			writerOpts = append(writerOpts, vault.WithCommitter(committer))
			defer func() {
				if err := committer.Close(); err != nil {
					logger.Warn("auto-commit Close failed", "err", err)
				}
			}()
		}
		writer, err := vault.NewWriter(cfg.Vault.Path, writerOpts...)
		if err != nil {
			return fmt.Errorf("init vault writer (vault.path=%s): %w", cfg.Vault.Path, err)
		}
		reader, err := vault.NewReader(cfg.Vault.Path)
		if err != nil {
			return fmt.Errorf("init vault reader (vault.path=%s): %w", cfg.Vault.Path, err)
		}
		handlerOpts = append(handlerOpts, api.WithVaultIO(writer, reader))
		// Per-entity write-lock manager shared between the
		// HTTP handlers (operator-fill etc.) and the workflow
		// vault-backed writers (Phase 4.B.2). Sharing the same
		// Manager means concurrent operator + workflow writes
		// against the same entity respect each other's locks
		// rather than racing on the vault file.
		wfWriteLocks := writelocks.New()
		handlerOpts = append(handlerOpts, api.WithWriteLocks(wfWriteLocks))
		logger.Info("vault wiring active", "vault_path", cfg.Vault.Path, "auto_commit", autoCommitOn)

		// Attachments dispatcher (per ADR-0014). Plugins emitting
		// FetchResult.Attachments route through here for scheme
		// dispatch + vault placement + tmp cleanup. The staging
		// dir is resolved via the three-layer chain in
		// resolvePluginStagingDir below (yaad-index #33): operator
		// yaml > YAAD_PLUGIN_STAGING_DIR env > os.TempDir().
		stagingDir := resolvePluginStagingDir(cfg.PluginStagingDir, os.Getenv("YAAD_PLUGIN_STAGING_DIR"))
		dispatcher, err := attachments.New(stagingDir, attachments.WithLogger(logger))
		if err != nil {
			return fmt.Errorf("init attachments dispatcher (plugin_staging_dir=%s): %w", stagingDir, err)
		}
		handlerOpts = append(handlerOpts, api.WithAttachmentsDispatcher(dispatcher))

		// Shared SyncIngester per ADR-0024 §"workflow.trigger(input)
		// input semantics". One tracker drives both /v1/ingest and
		// the workflow engine's URL-shape input path so job-map
		// dedup + cache-TTL gate coordinate across surfaces.
		//
		// canonicalEdgeTypes is the operator∪plugin-emitted union
		// (same set the reindex re-derive path uses) — keeping the
		// ingest classifier and the reindex classifier on one
		// effective set prevents plugin-auto-activated prefixes
		// from drifting bare→typed between fresh-write and
		// reindex passes.
		//
		// canonicalKinds is the same set the vault writer holds
		// (guard.EnabledKinds()) so the ingest path can compute the
		// title-synthesized-merged alias list that vault.Marshal
		// would write — mirroring the same merged list into
		// entity_aliases keeps /v1/search and the vault frontmatter
		// in lockstep on the first ingest pass.
		syncIngester := api.NewSyncIngester(
			logger, st, edgeService, registry, writer, reader,
			guard, cfg.CacheTTLSeconds, dispatcher, wfWriteLocks, bus,
			pluginInstances,
			pluginInstanceConfigs,
			enabledEdgeTypes,
			guard.EnabledKinds(),
		)
		handlerOpts = append(handlerOpts, api.WithSyncIngester(syncIngester))

		// Propagate the staging dir to subprocess plugins via
		// YAAD_PLUGIN_STAGING_DIR (per ADR-0014 §6 PR-B). Mirrors
		// clock.SetLocation's plumbing — package-level set-once at
		// boot, lock-free read on every subprocess spawn.
		subprocess.SetStagingDir(stagingDir)
		logger.Info("attachments dispatcher active", "plugin_staging_dir", stagingDir)

		// (datadir.SetRoot is called earlier, before
		// ensurePluginInstanceDataDirs, so the boot-time MkdirAll
		// and the per-dispatch buildInstanceEnv stamp the same
		// resolved path. See the SetRoot block above the
		// validate/ensure pair.)

		// Workflow loader (per ADR-0024 Phase 1.B). Scans
		// <vault>/workflows/ for operator-authored workflow files,
		// validates them, builds an in-memory registry, and
		// hot-reloads on mtime change. The loader uses the live
		// plugin registry to validate each workflow's
		// allowed_plugins list at load time. Phase 1 ships only
		// the parser + registry — Phase 3+ wires the registry's
		// workflows into the event-bus subscriber path.
		workflowDir := filepath.Join(cfg.Vault.Path, "workflows")
		wfLoader := loader.New(loader.Options{
			Paths:             []string{workflowDir},
			PluginRegistry:    loaderRegistryAdapter{registry: registry},
			CanonicalRegistry: canonicalLoaderRegistry{kinds: mergedRegistry, edgeTypes: cfg.CanonicalEdgeTypes},
			PollInterval:      loader.DefaultPollInterval,
			Logger:            logger,
		})
		wfCtx, wfCancel := context.WithCancel(context.Background())
		defer wfCancel()
		go func() {
			if err := wfLoader.Run(wfCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("workflow loader: run terminated unexpectedly", "err", err)
			}
		}()
		logger.Info("workflow loader active",
			"workflow_dir", workflowDir,
			"poll_interval", loader.DefaultPollInterval.String())

		// Workflow engine (per ADR-0024 Phase 3). Wires the
		// loader's registry × the shared event bus × the
		// decision evaluator. The Reconcile goroutine below
		// polls the loader's current workflow snapshot on
		// the same cadence as the loader's mtime poll, so
		// hot-reloaded edits surface as fresh engine
		// registrations without a daemon restart. Phase 3
		// records decisions but does NOT execute actions;
		// Phase 4 wires action runners against the engine's
		// decision output.
		wfResolver := &storeEntityResolver{st: st}
		// Phase 4.B.2 vault-backed writers. Share the same
		// writelocks.Manager wired into the api handlers above
		// so operator + workflow writes against the same
		// entity coordinate on a single lock map.
		wfWriterBackend := &actions.VaultWriterBackend{
			Store:       st,
			EdgeWriter:  edgeService,
			VaultReader: reader,
			VaultWriter: writer,
			WriteLocks:  wfWriteLocks,
			Logger:      logger,
			Kinds:       mergedRegistry,
		}
		wfPluginDispatcher, err := actions.NewRegistryPluginDispatcher(registry)
		if err != nil {
			return fmt.Errorf("init workflow plugin dispatcher: %w", err)
		}
		wfRunner := actions.New(actions.Options{
			TaskWriter:       actions.NewFileTaskWriter(cfg.Vault.Path, mergedRegistry, st, edgeService, logger),
			NoteWriter:       actions.NewVaultNoteWriter(wfWriterBackend),
			GapWriter:        actions.NewVaultGapWriter(wfWriterBackend),
			PropertyWriter:   actions.NewVaultPropertyWriter(wfWriterBackend),
			EdgeWriter: actions.NewVaultEdgeWriter(
				st, edgeService, reader, writer, wfWriteLocks,
				mergedRegistry, bus, logger,
			),
			ArchiveWriter:    actions.NewVaultArchiveWriter(wfWriterBackend),
			RestoreWriter:    actions.NewVaultRestoreWriter(wfWriterBackend),
			PluginDispatcher: wfPluginDispatcher,
			Bus:              bus,
			// Phase 5.B err-task pattern — systemic failures
			// (condition-eval, subject-render, action-runner
			// non-MissingRef errors) accumulate into the
			// workflow's err task at tasks/<workflow>-err.md.
			ErrTaskWriter: actions.NewFileErrTaskWriter(cfg.Vault.Path, st, logger),
			// Receives the rendered-template drift Warn when
			// the engine ships a non-nil RenderedTemplates map
			// that lacks an expected (idx, field) entry —
			// surfaces engine drift at execute time rather
			// than silently using raw CEL source.
			Logger: logger,
		})
		wfWalker := &storeGraphWalker{st: st, resolver: wfResolver}
		wfEngine, err = engine.New(engine.Options{
			Bus:          bus,
			Resolver:     wfResolver,
			Runner:       wfRunner,
			IngestRouter: syncIngester,
			Logger:       logger,
			Walker:       wfWalker,
			GraphWalkCap: cfg.Workflow.GraphWalkCap,
		})
		if err != nil {
			return fmt.Errorf("init workflow engine: %w", err)
		}
		// Synchronous initial Load + Reconcile before launching
		// the polling goroutines — PR-80 review fold-in. The
		// prior shape used a 100ms sleep as a hopeful sync,
		// which races on slow filesystems / large workflow
		// directories. The loader.Run goroutine ALSO does an
		// initial Load (per loader.Run's contract); doing it
		// once here keeps the reconcile fed without depending
		// on goroutine-startup timing.
		if err := wfLoader.Load(wfCtx); err != nil {
			logger.Warn("workflow loader: initial load failed; engine will see empty registry until next tick",
				"err", err)
		}
		if err := wfEngine.Reconcile(wfLoader.Workflows()); err != nil {
			logger.Warn("workflow engine: initial reconcile failed", "err", err)
		}
		go func() {
			ticker := time.NewTicker(loader.DefaultPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-wfCtx.Done():
					return
				case <-ticker.C:
					if err := wfEngine.Reconcile(wfLoader.Workflows()); err != nil {
						logger.Warn("workflow engine: reconcile failed; will retry next tick", "err", err)
					}
				}
			}
		}()
		handlerOpts = append(handlerOpts, api.WithWorkflowEngine(wfEngine))
		// #277: per-workflow CRUD surface. workflowDir is the
		// same on-disk path the loader polls; mutations write the
		// file, the loader reconciles engine state on the next
		// poll (vault-as-truth per ADR-0008). MkdirAll here so
		// the first PUT in a vault without a pre-existing
		// `workflows/` subdir succeeds — the loader is happy with
		// a missing dir (treats it as "no workflows") but
		// atomicWriteFile's tmpfile path is not. Boot-time
		// fail-fast at uncreatable paths matches the #284 plugin
		// data-dir pattern.
		if err := os.MkdirAll(workflowDir, 0o755); err != nil {
			return fmt.Errorf("ensure workflow dir %s: %w", workflowDir, err)
		}
		handlerOpts = append(handlerOpts, api.WithWorkflowDir(workflowDir))
		// Phase 6.B/C task surface — filesystem-walk reader +
		// writer rooted at the same vault path the action
		// runners write tasks under. Registers GET /v1/tasks
		// + GET /v1/tasks/{id} + POST /v1/tasks/{id}/resolve
		// per ADR-0024 §"Agent surface".
		handlerOpts = append(handlerOpts, api.WithTasksReader(wftasks.NewReader(cfg.Vault.Path)))
		handlerOpts = append(handlerOpts, api.WithTasksWriter(wftasks.NewWriter(cfg.Vault.Path)))
		logger.Info("workflow engine active",
			"reconcile_interval", loader.DefaultPollInterval.String(),
			"http_trigger_path", "/v1/workflows/trigger")
	} else {
		logger.Info("vault.path not configured; ingest stays DB-only and POST /v1/reindex is unregistered (404)")
	}

	srv := &http.Server{
		Addr: s.Bind,
		Handler: api.NewHandlerWithRegistry(logger, st, registry, handlerOpts...),
		ReadHeaderTimeout: 10 * time.Second,
	}

	listenErr := make(chan error, 1)
	go func() {
		logger.Info("yaad-index listening", "bind", s.Bind)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-listenErr:
		if err != nil {
			return fmt.Errorf("listen on %s: %w", s.Bind, err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// Drain the workflow engine's #169 event queue before
	// exit. The worker stops accepting new events + finishes
	// processing whatever was already buffered. HTTP server
	// shutdown above already terminated the request paths
	// that publish bus events; the queue should drain
	// quickly.
	if wfEngine != nil {
		wfEngine.Shutdown()
	}
	logger.Info("shutdown complete")
	return nil
}

// CLI is the top-level command tree. New subcommands land alongside Serve.
type CLI struct {
	Serve ServeCmd `cmd:"" help:"Run the yaad-index HTTP server."`
	Plugins PluginsCmd `cmd:"" help:"Operator-facing plugin management."`
	Reindex ReindexCmd `cmd:"" help:"Walk the markdown vault and rebuild the derived index."`
	Cache CacheCmd `cmd:"" help:"Operator-facing cache lifecycle management (per yaad-index)."`
	Keygen KeygenCmd `cmd:"" help:"Generate the RS256 keypair under keys_dir (per yaad-index)."`
	IssueToken IssueTokenCmd `cmd:"issue-token" help:"Issue a pair-claim JWT for an operator/agent pair (per yaad-index)."`
	Command CommandCmd `cmd:"" help:"Dispatch a command-shape plugin invocation against the running daemon (per ADR-0022 +)."`
	Fetch FetchCmd `cmd:"" help:"Dispatch a URL-shape plugin invocation against the running daemon (per ADR-0022 +)."`
	Workflow WorkflowCmd `cmd:"" help:"Workflow engine surface — trigger workflows manually (per ADR-0024 §Agent surface)."`
	Task     TaskCmd     `cmd:"" help:"Task surface — list / load workflow-produced tasks (per ADR-0024 §Agent surface)."`
}

// ReindexCmd implements `yaad-index reindex`. Walks the vault root
// and (re)builds the SQLite index. Default mode is incremental
// (mtime + content hash per file); --full forces a complete drop +
// rebuild. Mirrors POST /v1/reindex.
type ReindexCmd struct {
	DBPath string `name:"db-path" env:"YAAD_INDEX_DB_PATH" default:"~/.local/share/yaad-index/yaad.db" help:"path to the SQLite database file (matches the serve subcommand)."`
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/yaad-index/config.yaml" help:"path to the yaad-index config file. vault.path is read from here unless --vault-path overrides."`
	VaultPath string `name:"vault-path" env:"YAAD_INDEX_VAULT_PATH" help:"override vault root (otherwise read from config.vault.path). Must be absolute."`
	Full bool `name:"full" help:"drop every entity, edge, and provenance row before rebuilding (default: incremental)."`
}

// Run executes the reindex CLI subcommand.
func (c *ReindexCmd) Run() error {
	// Same level-swappable logger pattern as ServeCmd .
	levelVar := new(slog.LevelVar)
	levelVar.Set(config.DefaultLogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar, ReplaceAttr: clock.LogTimeAttr}))

	dbPath, err := expandPath(c.DBPath)
	if err != nil {
		return fmt.Errorf("expand --db-path %q: %w", c.DBPath, err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	vaultPath := c.VaultPath
	// Always load the config (even if --vault-path overrides) so the
	// log_level setting takes effect. The unused-config case (vault
	// path overridden) doesn't change semantics; we just ignore the
	// vault sub-key in cfg and use c.VaultPath.
	cfg, err := loadConfigOptional(logger, c.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg != nil {
		level, perr := config.ParseLogLevel(cfg.LogLevel)
		if perr != nil {
			return fmt.Errorf("parse log_level: %w", perr)
		}
		levelVar.Set(level)
		if vaultPath == "" {
			vaultPath = cfg.Vault.Path
		}
		// Operator-configured timezone (per yaad-index PR-C) —
		// reindex re-walks the vault and renders provenance in summary
		// logs; without this its log lines would print in UTC even
		// when the operator runs every other binary in their local TZ.
		if cfg.Timezone != "" {
			tzLoc, perr := time.LoadLocation(cfg.Timezone)
			if perr != nil {
				return fmt.Errorf("parse timezone %q: %w", cfg.Timezone, perr)
			}
			clock.SetLocation(tzLoc)
		}
	}
	if vaultPath == "" {
		return errors.New("vault path is required (set vault.path in config or pass --vault-path)")
	}
	expanded, err := expandPath(vaultPath)
	if err != nil {
		return fmt.Errorf("expand vault path %q: %w", vaultPath, err)
	}

	// CLI-invoked reindex passes nil guard — gating depends on the
	// merged plugin+config registry, which would require loading the
	// plugin set just to run a manual reindex. The HTTP reindex
	// handler (daemon path) does wire the guard since the registry is
	// already loaded. Permissive CLI behavior matches the legacy
	// shape.
	reindexer, err := reindex.New(st, expanded, nil, logger)
	if err != nil {
		return fmt.Errorf("init reindex: %w", err)
	}

	mode := reindex.Incremental
	if c.Full {
		mode = reindex.Full
	}

	summary, err := reindexer.Run(context.Background(), mode)
	if err != nil {
		return fmt.Errorf("reindex %s: %w", mode, err)
	}

	out, mErr := json.MarshalIndent(summary, "", " ")
	if mErr != nil {
		return fmt.Errorf("marshal summary: %w", mErr)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	if _, err := os.Stdout.WriteString("\n"); err != nil {
		return fmt.Errorf("write summary newline: %w", err)
	}
	return nil
}

// PluginsCmd groups operator-only plugin tooling. Deliberately CLI-
// only — these touch the capability cache (server-startup state) and
// don't belong on the agent-facing /v1 API surface (Per the prior design, the v1
// HTTP surface is intentionally unauthenticated).
type PluginsCmd struct {
	ClearCache PluginsClearCacheCmd `cmd:"clear-cache" help:"Drop cached plugin capabilities so the next server start re-runs --init."`
	Reprobe PluginsReprobeCmd `cmd:"reprobe" help:"Force a re-probe of plugin capabilities (--init re-runs, fresh row written). Use after a Capabilities-shape change where the plugin author forgot to bump --version. Restart required to load the new shape into the running daemon."`
}

// PluginsClearCacheCmd implements `yaad-index plugins clear-cache`.
// With no flags, drops all plugin_capabilities rows. With --name <n>,
// drops the single named row. The next server start re-inits each
// plugin from scratch and re-populates the cache.
type PluginsClearCacheCmd struct {
	DBPath string `name:"db-path" env:"YAAD_INDEX_DB_PATH" default:"~/.local/share/yaad-index/yaad.db" help:"path to the SQLite database file (matches the serve subcommand)."`
	Name string `name:"name" help:"clear only the named plugin's cache row; omit to clear all."`
}

// Run executes the clear-cache subcommand. Opens the store, dispatches
// to clearPluginCache, and writes the operator-facing confirmation
// line. The split lets clearPluginCache unit-test cleanly without
// going through kong + store.New + os.Stderr.
func (c *PluginsClearCacheCmd) Run() error {
	dbPath, err := expandPath(c.DBPath)
	if err != nil {
		return fmt.Errorf("expand --db-path %q: %w", c.DBPath, err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()
	return clearPluginCache(context.Background(), st, c.Name, os.Stderr)
}

// PluginsReprobeCmd implements `yaad-index plugins reprobe [--name X]`
// per yaad-index. With no --name, walks every plugin entry in
// the operator's config and re-runs --init for each. With --name X,
// targets just that plugin.
//
// **Why this exists.** The startup cache-aware loader trusts the
// `--version` string: same-version → skip --init, trust cache. If a
// plugin ships a Capabilities-shape change WITHOUT bumping --version
// (e.g., adds `frontmatter_edges` while keeping version "0.2.0"),
// the daemon never sees the new field on subsequent restarts. This
// command bypasses the cache: clears the row, runs --init, writes
// fresh data.
//
// **Daemon restart still required.** The daemon's in-memory plugin
// registry is built at server start from the cache. This command
// only refreshes the SQLite row — operators MUST restart the daemon
// to load the new Capabilities into the running registry. Hot-
// reload via an admin route is out of scope for v1 Per the prior design,.
type PluginsReprobeCmd struct {
	DBPath string `name:"db-path" env:"YAAD_INDEX_DB_PATH" default:"~/.local/share/yaad-index/yaad.db" help:"path to the SQLite database file (matches the serve subcommand)."`
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/yaad-index/config.yaml" help:"path to the yaad-index config file. Required — reprobe needs to know each plugin's binary path."`
	Name string `name:"name" help:"reprobe only the named plugin; omit to reprobe all configured plugins."`
}

// Run executes the reprobe subcommand. Loads the operator's config
// to resolve plugin binary paths, opens the store, and dispatches
// to reprobePlugins.
func (c *PluginsReprobeCmd) Run() error {
	dbPath, err := expandPath(c.DBPath)
	if err != nil {
		return fmt.Errorf("expand --db-path %q: %w", c.DBPath, err)
	}
	configPath, err := expandPath(c.ConfigPath)
	if err != nil {
		return fmt.Errorf("expand --config %q: %w", c.ConfigPath, err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", configPath, err)
	}
	if cfg == nil || len(cfg.Plugins) == 0 {
		return fmt.Errorf("no plugins configured at %s; nothing to reprobe", configPath)
	}
	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	// stderr-only logger so the per-plugin progress + per-plugin
	// failure detail land outside the stdout summary stream the
	// caller pipes to less or scripts on.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return reprobePlugins(context.Background(), logger, st, cfg.Plugins, c.Name, os.Stdout)
}

// reprobePlugins is the inner of PluginsReprobeCmd.Run, factored so
// unit tests can inject an in-memory store + a buffer + a fake
// plugin entry list. Walks the requested plugin set, deletes each
// cached row, re-runs --init via subprocess.New (which does the
// --init handshake + parse), and upserts the fresh capabilities.
//
// Output (stdout): one line per plugin, terse table-style:
//
//	<name>: <old-version> -> <new-version> [shape: <changed|unchanged>]
//
// On per-plugin failure: emits a "<name>: ERROR <msg>" line and
// continues with the next plugin (so a single broken binary
// doesn't block reprobing the others). Exit code:
//
// - 0 if every targeted plugin succeeded.
// - 1 if any failed (or the named plugin wasn't in config).
//
// Returning an error here propagates that to the os.Exit(1) path
// kong wires up around Run().
func reprobePlugins(ctx context.Context, logger *slog.Logger, st store.Store, configured []config.PluginEntry, only string, out io.Writer) error {
	// Filter to the targeted set. With --name, exactly one plugin
	// — error if it's not in config so the operator gets a clear
	// "you typoed the plugin name" signal instead of a silent
	// no-op.
	var targets []config.PluginEntry
	if only == "" {
		targets = configured
	} else {
		for _, e := range configured {
			if e.Name == only {
				targets = append(targets, e)
				break
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("plugin %q not found in config; available: %v",
				only, pluginEntryNames(configured))
		}
	}

	var failures []string
	for _, entry := range targets {
		oldRow, _, gErr := st.GetPluginCapabilities(ctx, entry.Name)
		oldVersion := ""
		var oldCaps plugins.Capabilities
		if gErr == nil {
			oldVersion = oldRow.Version
			// Best-effort: malformed cached JSON shouldn't block
			// the reprobe; we just won't be able to compute the
			// shape-changed signal for this row.
			_ = json.Unmarshal(oldRow.CapabilitiesJSON, &oldCaps)
		}

		// Clear the cached row. Failure here is logged + we
		// continue (the upsert below will overwrite anyway, so a
		// stale row won't survive).
		if _, dErr := st.DeletePluginCapabilities(ctx, entry.Name); dErr != nil {
			logger.Warn("reprobe: delete cached row failed; continuing", "name", entry.Name, "err", dErr)
		}

		// Force a fresh --init via subprocess.New. The constructor
		// reads --init, parses, and returns the constructed
		// plugin; we capture caps for the fresh upsert below.
		// Per-plugin JSON config env (#192) threaded through so
		// reprobe matches the boot path.
		configEnv, cfgErr := config.PluginConfigEnv(entry.Name, entry.Config)
		if cfgErr != nil {
			_, _ = fmt.Fprintf(out, "%s: ERROR marshal config: %v\n", entry.Name, cfgErr)
			failures = append(failures, entry.Name)
			continue
		}
		p, err := subprocess.New(entry.Name, entry.Path,
			appendFetchTimeoutOpt(entry,
				subprocess.WithLogger(logger),
				subprocess.WithConfigEnv(configEnv))...)
		if err != nil {
			_, _ = fmt.Fprintf(out, "%s: ERROR --init failed: %v\n", entry.Name, err)
			failures = append(failures, entry.Name)
			continue
		}
		newCaps := p.Capabilities()
		capsJSON, mErr := json.Marshal(newCaps)
		if mErr != nil {
			_, _ = fmt.Fprintf(out, "%s: ERROR marshal capabilities failed: %v\n", entry.Name, mErr)
			failures = append(failures, entry.Name)
			continue
		}
		if uErr := st.UpsertPluginCapabilities(ctx, entry.Name, newCaps.Version, capsJSON); uErr != nil {
			_, _ = fmt.Fprintf(out, "%s: ERROR cache upsert failed: %v\n", entry.Name, uErr)
			failures = append(failures, entry.Name)
			continue
		}

		shapeNote := "unchanged"
		// Shape comparison: marshal old + new and string-compare.
		// Reasonable for a single-line summary; not a deep
		// structural diff. False-positive on key-order shuffle
		// would surface as `changed` even when semantically same;
		// json.Marshal in Go is deterministic on map iteration
		// order via sorted keys for the same input, so this is
		// stable in practice.
		if oldRow.Version != "" {
			if !sameCapabilitiesShape(oldRow.CapabilitiesJSON, capsJSON) {
				shapeNote = "changed"
			}
		} else {
			shapeNote = "first-write"
		}

		oldDisplay := oldVersion
		if oldDisplay == "" {
			oldDisplay = "(none)"
		}
		_, _ = fmt.Fprintf(out, "%s: %s -> %s [shape: %s]\n",
			entry.Name, oldDisplay, newCaps.Version, shapeNote)
	}

	if len(failures) > 0 {
		return fmt.Errorf("reprobe failed for %d plugin(s): %v", len(failures), failures)
	}
	return nil
}

// sameCapabilitiesShape returns true when the two raw capabilities
// JSON blobs deserialize + re-serialize to byte-identical streams.
// Used by the reprobe summary line to flag a shape-change when the
// version string didn't move but the wire shape did (the ADR-0014/
// 0015/0016 wave had several such cases — exactly the gap reprobe
// exists to recover from).
func sameCapabilitiesShape(oldJSON, newJSON []byte) bool {
	var oldCaps, newCaps plugins.Capabilities
	if err := json.Unmarshal(oldJSON, &oldCaps); err != nil {
		return false
	}
	if err := json.Unmarshal(newJSON, &newCaps); err != nil {
		return false
	}
	oldRe, err := json.Marshal(oldCaps)
	if err != nil {
		return false
	}
	newRe, err := json.Marshal(newCaps)
	if err != nil {
		return false
	}
	return string(oldRe) == string(newRe)
}

// appendFetchTimeoutOpt extends the caller's subprocess.Option slice
// with WithFetchTimeout only when the operator set fetch_timeout on
// the PluginEntry. Unset / zero-duration entries get the daemon
// default (DefaultFetchTimeout) untouched. Centralizes the override
// decision so registerPlugin's cache-hit and --init paths and the
// reprobePlugins re-init path stay in sync.
func appendFetchTimeoutOpt(entry config.PluginEntry, opts ...subprocess.Option) []subprocess.Option {
	if d := entry.FetchTimeoutDuration(); d > 0 {
		opts = append(opts, subprocess.WithFetchTimeout(d))
	}
	return opts
}

// pluginEntryNames extracts plugin names from a config slice for
// the --name-not-found error message.
func pluginEntryNames(entries []config.PluginEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// clearPluginCache is the inner of PluginsClearCacheCmd.Run, factored
// so unit tests can inject an in-memory store + a buffer in place of
// the real store and os.Stderr. name=="" means "clear all";
// name!="" means "clear that one row."
func clearPluginCache(ctx context.Context, st store.Store, name string, w io.Writer) error {
	if name != "" {
		dropped, err := st.DeletePluginCapabilities(ctx, name)
		if err != nil {
			return fmt.Errorf("delete plugin_capabilities %q: %w", name, err)
		}
		if dropped {
			_, _ = fmt.Fprintf(w, "cleared 1 plugin_capabilities row for %q\n", name)
		} else {
			_, _ = fmt.Fprintf(w, "no plugin_capabilities row for %q (already absent)\n", name)
		}
		return nil
	}
	n, err := st.ClearAllPluginCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("clear plugin_capabilities: %w", err)
	}
	_, _ = fmt.Fprintf(w, "cleared %d plugin_capabilities row(s)\n", n)
	return nil
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("yaad-index"),
		kong.Description("yaad-index — knowledge index server."),
		kong.UsageOnError(),
	)
	if err := ctx.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "yaad-index:", err)
		os.Exit(1)
	}
}

// loadConfigOptional resolves the config file path, expands `~/`, and
// returns the parsed Config — or (nil, nil) if the file is absent
// (operator hasn't created it yet; dev binaries without a config still
// work). A broken-but-present config returns an error so server start
// fails per ADR-0006.
func loadConfigOptional(logger *slog.Logger, configPath string) (*config.Config, error) {
	expanded, err := expandPath(configPath)
	if err != nil {
		return nil, fmt.Errorf("expand --config %q: %w", configPath, err)
	}
	cfg, err := config.Load(expanded)
	if err != nil {
		if errors.Is(err, config.ErrFileMissing) {
			logger.Info("no config file; running with zero plugins and no vault",
				"path", expanded)
			return nil, nil
		}
		return nil, err
	}
	return cfg, nil
}

// pluginCacheOutcome categorises which path served a single
// plugin's bring-up. Used by buildPluginRegistry to tally the
// startup summary line and by registerPlugin to emit per-plugin
// log lines at the right severity (Per the prior design, + the agreed
// log-level map):
//
// - cacheHit — INFO; cached capabilities served the plugin.
// - cacheMissFirstStart — INFO; no cache row, --init runs.
// - cacheMissVersionChanged — INFO; cached version != probed version,
// --init re-runs (operator-initiated
// routine deploy, not an alarm).
// - cacheFailure — WARN/ERROR; the cache COULD have served
// this plugin (version probe matched the
// cached row) but something on the
// cache-read or decode path went wrong
// and we fell back to --init. Operator
// should investigate; the per-path log
// carries the err detail.
type pluginCacheOutcome int

const (
	cacheHit pluginCacheOutcome = iota
	cacheMissFirstStart
	cacheMissVersionChanged
	cacheFailure
)

// String returns the label surfaced on the existing
// `plugin registered` INFO line (`source=cache` or `source=init`).
// Operators have been reading those for several PRs; the new outcome
// classification is additive on top.
func (o pluginCacheOutcome) String() string {
	if o == cacheHit {
		return "cache"
	}
	return "init"
}

// buildPluginRegistry hydrates a subprocess plugin per cfg.Plugins
// entry and returns the registry. Nil cfg → empty registry; a config
// without plugins → empty registry. Failed --init → fail-fast per
// ADR-0006.
//
// **Cache-aware** : for each plugin, probe `--version` first
// (cheap) and check the store-backed plugin_capabilities cache. On a
// version match, build the plugin from the cached capabilities and
// skip the --init handshake. On a miss / version mismatch / probe
// failure, fall through to a full --init load and upsert the result.
// Plugins without --version support never benefit from the cache but
// continue to register normally.
//
// **Observability** : each registerPlugin call returns a
// pluginCacheOutcome that buildPluginRegistry tallies into a single
// startup summary line ("plugin cache summary") with cache_hits /
// cache_misses (first-start + version-change lumped) / cache_failures
// (WARN/ERROR paths) / failed_plugins (names that hit the failure
// path). Skipped when no plugins registered (don't spam empty).
func buildPluginRegistry(logger *slog.Logger, st store.Store, cfg *config.Config) (*plugins.Registry, error) {
	registry := plugins.NewRegistry()
	if cfg == nil || len(cfg.Plugins) == 0 {
		return registry, nil
	}

	// Short, fixed-budget context for version probes — long enough for
	// a sane plugin's hello-and-exit but tight enough that broken
	// plugins don't block startup forever. Independent of subprocess's
	// per-fetch timeout (5s), which is for the longer ingest path.
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		hits, misses, failures int
		failedPlugins []string
	)

	// Iterate the slice in YAML order — first-match-wins dispatch
	// priority depends on it (ADR-0006). Don't rewrite as a map range.
	for _, entry := range cfg.Plugins {
		p, outcome, err := registerPlugin(probeCtx, logger, st, entry)
		if err != nil {
			return nil, fmt.Errorf("plugin %q at %s: %w", entry.Name, entry.Path, err)
		}
		// ADR-0028 §9 cross-validation gate: a plugin whose binary
		// declares supports_instances=false (the default, including
		// the zero-value for plugins predating ADR-0028) cannot run
		// with more than one operator-configured instance. Two-plus
		// entries would silently break the plugin's data shape
		// (e.g. two yaad-bgg instances sharing one API key would
		// double-write identical entities). Fail fast at startup so
		// the operator fixes the config rather than discovers the
		// breakage at first ingest. Runs after registerPlugin
		// because the flag lives in the plugin's `--init`
		// capabilities, which Load can't reach.
		if !p.Capabilities().SupportsInstances && len(entry.Instances) >= 2 {
			return nil, fmt.Errorf(
				"plugin %q does not support multi-instance config (capability supports_instances=false); "+
					"reduce instances: to one entry or remove the block (got %d instances)",
				entry.Name, len(entry.Instances))
		}
		// ADR-0028 §7 Cut 5: a single-instance plugin
		// (supports_instances=false) with its sole instance
		// disabled is operator-mistake — the plugin would
		// silently never run. Fail-fast at config load.
		if !p.Capabilities().SupportsInstances && len(entry.Instances) == 1 && !entry.Instances[0].IsEnabled() {
			return nil, fmt.Errorf(
				"plugin %q has its only configured instance %q disabled (enabled: false); "+
					"the plugin will never run — remove the instance entry or set enabled: true",
				entry.Name, entry.Instances[0].Name)
		}
		// ADR-0028 §7 Cut 5: warn (not error) when every
		// configured instance of a multi-instance plugin is
		// disabled. Likely operator mistake (plugin is fully
		// shut off) but not load-fatal; the plugin stays
		// registered, dispatch surfaces the "no_enabled_
		// instances" error per call. Covers both the 1-instance
		// case (operator disabled the lone configured instance
		// on a supports_instances=true plugin — silent
		// registration without this WARN) and the multi-instance
		// case (operator disabled everything during maintenance).
		if p.Capabilities().SupportsInstances && len(entry.Instances) >= 1 {
			anyEnabled := false
			for _, inst := range entry.Instances {
				if inst.IsEnabled() {
					anyEnabled = true
					break
				}
			}
			if !anyEnabled {
				logger.Warn("plugin has no enabled instances",
					"name", entry.Name,
					"configured_instances", len(entry.Instances),
					"note", "all instances declare enabled: false; plugin will reject every invocation with no_enabled_instances")
			}
		}
		// ADR-0028 §1 + §2: validate every instance's `config:`
		// block against the plugin's declared `config_schema`. The
		// schema is plugin-scoped (one `--init`, one cache row), but
		// each instance carries its own operator-supplied config so
		// each one needs the gate. Fail-fast surfaces operator
		// typos / shape mismatches at startup instead of at first
		// fetch.
		//
		// This is the SINGLE validation path. The plugin-level
		// entry.Config validation was removed from registerPlugin
		// (both cache-hit and fresh-init paths) because that call
		// would always reject required-field schemas once an
		// operator declared explicit `instances:` — entry.Config
		// goes empty in that case while the operator's actual
		// config lives per-instance. Back-compat for legacy
		// single-instance configs (no `instances:` block) flows
		// through here via Load's synthesis-copy: the synthesized
		// `default` instance inherits entry.Config into its own
		// Config field, so the legacy operator's config still gets
		// schema-validated — through this loop, not the removed
		// registerPlugin call.
		for i, inst := range entry.Instances {
			if vErr := config.ValidatePluginConfigAgainstSchema(
				entry.Name, inst.Config, p.Capabilities().ConfigSchema,
			); vErr != nil {
				return nil, fmt.Errorf(
					"plugin %q instance %q (index %d): %w",
					entry.Name, inst.Name, i, vErr)
			}
		}
		registry.Register(p)
		logger.Info("plugin registered",
			"name", entry.Name, "path", entry.Path, "source", outcome.String(),
			"capabilities_name", p.Capabilities().Name,
			"capabilities_version", p.Capabilities().Version,
			"supports_instances", p.Capabilities().SupportsInstances,
			"instances", len(entry.Instances))

		switch outcome {
		case cacheHit:
			hits++
		case cacheMissFirstStart, cacheMissVersionChanged:
			misses++
		case cacheFailure:
			failures++
			failedPlugins = append(failedPlugins, entry.Name)
		}
	}

	logger.Info("plugin cache summary",
		"cache_hits", hits,
		"cache_misses", misses,
		"cache_failures", failures,
		"failed_plugins", failedPlugins,
		"registered", len(cfg.Plugins))

	return registry, nil
}

// registerPlugin executes the cache-aware bring-up for one plugin
// and returns the constructed *subprocess.Plugin plus a
// pluginCacheOutcome that buildPluginRegistry uses for the startup
// summary tally. The version probe is best-effort: any failure
// (binary doesn't support --version, exits non-zero, times out,
// etc.) silently falls through to the full --init path with
// outcome=cacheMissFirstStart (the cache wasn't consultable, so
// from a tally perspective it's a miss, not a failure).
//
// Per the log-level map agreed the operators 07:25Z, plus the
// probe-failure path added Per the prior design, (the seventh implicit path
// yaad's a prior PR review flagged):
//
// - INFO cache hit — `plugin registered` line at
// INFO already emitted by the caller; no extra line here.
// - INFO cache miss (first-start) — "no cached capabilities, running --init"
// - INFO cache miss (version) — "plugin version changed, re-running --init"
// with cached_version + probed_version.
// Version bumps are operator-initiated
// routine deploys, not alarms.
// - INFO probe failed — "plugin version probe failed; falling
// through to --init" with name + err + probed_version. Lumps with
// first-start in the summary tally (the cache wasn't consultable, so
// it's a miss, not a failure).
// - WARN cache lookup error — DB transient, recoverable.
// - ERROR cached caps malformed — corrupt DB row, operator-actionable.
// - ERROR Plugin construction fail — corrupt-cached-state, operator-actionable.
func registerPlugin(ctx context.Context, logger *slog.Logger, st store.Store, entry config.PluginEntry) (*subprocess.Plugin, pluginCacheOutcome, error) {
	name := entry.Name
	path := entry.Path
	outcome := cacheMissFirstStart // overwritten below as paths discriminate

	// Build per-plugin env-var slice from operator yaml `config:`
	// sub-block per #192. The block JSON-marshals into a single
	// YAAD_PLUGIN_CONFIG env var the subprocess reads on startup;
	// passed to both --init and cache-hit construction paths so
	// plugins reading config at --init time see the same env
	// regardless of cache state.
	configEnv, configEnvErr := config.PluginConfigEnv(name, entry.Config)
	if configEnvErr != nil {
		return nil, cacheFailure, fmt.Errorf("plugin %q: marshal config: %w", name, configEnvErr)
	}

	probedVersion, probeErr := subprocess.RunVersion(ctx, path, 2*time.Second)
	if probeErr == nil && probedVersion != "" {
		cached, found, err := st.GetPluginCapabilities(ctx, name)
		switch {
		case err != nil:
			logger.Warn("plugin capabilities cache read failed; falling through to --init",
				"err", err, "name", name)
			outcome = cacheFailure
		case !found:
			logger.Info("no cached capabilities, running --init",
				"name", name)
			outcome = cacheMissFirstStart
		case subprocess.VersionCacheKey(cached.Version) != subprocess.VersionCacheKey(probedVersion):
			// Per yaad-index: compare on the cache-key prefix
			// (stripped of the `+<build-hash>` suffix) so a fresh
			// rebuild with the same semver tag reuses the cache.
			// Logged versions surface the FULL strings the plugin
			// emitted so operators tailing logs can correlate to a
			// specific build artifact.
			logger.Info("plugin version changed, re-running --init",
				"name", name,
				"cached_version", cached.Version,
				"probed_version", probedVersion,
				"cached_cache_key", subprocess.VersionCacheKey(cached.Version),
				"probed_cache_key", subprocess.VersionCacheKey(probedVersion))
			outcome = cacheMissVersionChanged
		default:
			// Version match — try to serve from cache.
			var caps plugins.Capabilities
			if uErr := json.Unmarshal(cached.CapabilitiesJSON, &caps); uErr != nil {
				logger.Error("cached caps malformed, ignoring cache",
					"err", uErr, "name", name)
				outcome = cacheFailure
			} else {
				// Per-instance config_schema validation moved to
				// buildPluginRegistry per ADR-0028 §1 + §2 — the
				// schema is plugin-scoped but config is per-instance,
				// so the right gate is downstream where we have both
				// caps and the resolved entry.Instances list. The
				// legacy plugin-level entry.Config validation here
				// would always reject required-field schemas once
				// the operator migrates their config to per-instance,
				// because entry.Config is empty when explicit
				// `instances:` are declared. Load's synthesis-copy
				// preserves back-compat: an absent `instances:`
				// block still routes entry.Config through schema
				// validation via the synthesized default instance's
				// Config field.
				p, ctorErr := subprocess.NewWithCapabilities(name, path, caps,
				appendFetchTimeoutOpt(entry,
					subprocess.WithLogger(logger),
					subprocess.WithConfigEnv(configEnv))...)
				if ctorErr != nil {
					logger.Error("Plugin construction from cached caps failed, ignoring cache",
						"err", ctorErr, "name", name)
					outcome = cacheFailure
				} else {
					return p, cacheHit, nil
				}
			}
		}
	} else {
		// Probe failed (binary lacks --version, exited non-zero, or
		// timed out). Cache lookup never runs; outcome stays at the
		// initial cacheMissFirstStart so the summary tally lumps this
		// with first-start (the cache wasn't consultable). The INFO
		// breadcrumb lets operators distinguish "no cache row" from
		// "probe broken" when tailing logs — closes the seventh
		// implicit path yaad's a prior PR review flagged.
		logger.Info("plugin version probe failed; falling through to --init",
			"name", name,
			"err", probeErr,
			"probed_version", probedVersion,
		)
	}

	// Fall-through: full --init load + cache upsert.
	p, err := subprocess.New(name, path,
		appendFetchTimeoutOpt(entry,
			subprocess.WithLogger(logger),
			subprocess.WithConfigEnv(configEnv))...)
	if err != nil {
		return nil, outcome, err
	}
	caps := p.Capabilities()
	// Per-instance config_schema validation moved to
	// buildPluginRegistry per ADR-0028 §1 + §2 (see the comment
	// near the cross-validation gate). Same rationale as the
	// cache-hit branch above.
	if caps.Version != "" {
		capsJSON, mErr := json.Marshal(caps)
		if mErr != nil {
			logger.Warn("plugin capabilities marshal for cache failed; skipping cache write",
				"err", mErr, "name", name)
		} else if uErr := st.UpsertPluginCapabilities(ctx, name, caps.Version, capsJSON); uErr != nil {
			logger.Warn("plugin capabilities cache upsert failed; continuing without cache",
				"err", uErr, "name", name)
		}
	}
	return p, outcome, nil
}

// warnCanonicalEmissionGaps surfaces the cold-reviewer's a prior PR review note 2 at
// startup: when a registered plugin declares a canonical kind /
// edge type it MAY emit (via Capabilities.CanonicalKindsEmitted /
// CanonicalEdgeTypesEmitted) that the operator hasn't enabled in
// the corresponding config slice, log a structured warning so the
// operator can decide whether to enable it or accept the silent
// drop.
//
// Per yaad's review on the plan: skip the positive-confirmation
// case (plugin declares X, operator has X enabled) — operators
// don't need the noise. Only emit on gaps.
func warnCanonicalEmissionGaps(logger *slog.Logger, registry *plugins.Registry, guard *config.CanonicalGuard) {
	for _, p := range registry.Plugins() {
		caps := p.Capabilities()
		for _, k := range caps.CanonicalKindsEmitted {
			if k == "" || guard.AllowKind(k) {
				continue
			}
			logger.Warn("plugin declares canonical kind not enabled in config",
				"plugin", caps.Name, "kind", k,
				"hint", "add to canonical_kinds in config.yaml to materialize stubs of this kind")
		}
		for _, t := range caps.CanonicalEdgeTypesEmitted {
			if t == "" || guard.AllowEdgeType(t) {
				continue
			}
			logger.Warn("plugin declares canonical edge type not enabled in config",
				"plugin", caps.Name, "edge_type", t,
				"hint", "add to canonical_edge_types in config.yaml to materialize edges of this type")
		}
	}
}

// resolvePluginStagingDir picks the daemon's attachment staging root
// per ADR-0014 + yaad-index #33. Resolution chain, highest priority
// first:
//
//  1. Operator yaml (`cfg.PluginStagingDir`) — validated absolute +
//     existing-directory at config load.
//  2. `YAAD_PLUGIN_STAGING_DIR` env var on the daemon process — the
//     same var name plugins read via the SDK. Lets operators flip
//     the staging root without editing yaml (useful for systemd
//     drop-ins + containerized deploys).
//  3. `os.TempDir()` — POSIX-conformant fallback that respects
//     `$TMPDIR`. Previously hardcoded `/tmp`; the change picks up
//     containerized runtimes (often `/var/tmp` or a tmpfs mount)
//     and per-user tempdirs in development.
//
// Empty strings at any layer are skipped — the chain falls through.
// The selected value is propagated to subprocess plugins via the
// same env var name (`subprocess.SetStagingDir` plumbing) so the
// daemon-side dispatcher and the plugin-side SDK see one consistent
// path.
func resolvePluginStagingDir(yamlValue, envValue string) string {
	if yamlValue != "" {
		return yamlValue
	}
	if envValue != "" {
		return envValue
	}
	return os.TempDir()
}

// expandPath resolves a leading "~/" against the current user's home
// directory. kong's default tag is a literal string and does not expand
// shell variables; doing the expansion here keeps the default ergonomic
// (`~/.local/share/yaad-index/yaad.db`) without pulling kong into a
// custom mapper.
func expandPath(p string) (string, error) {
	if !strings.HasPrefix(p, "~/") && p != "~" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~/")), nil
}

// buildAutoCommitter resolves the operator's auto-commit configuration
// (per yaad-index the source issue) into a vault.Committer. Returns
// (nil, false) when auto-commit is off — the Writer's nil-committer
// path takes over and behaves identically to legacy (plain atomic
// write, no git invocation). Returns (committer, true) when the
// vault root is a git working tree AND the operator hasn't opted
// out via auto_commit: false.
//
// Tri-state on cfg.AutoCommit:
// - nil (default) → auto-detect: enabled iff <vault>/.git/ exists.
// - &true → enabled (Validate has already verified .git/).
// - &false → disabled regardless of .git/ presence.
func buildAutoCommitter(logger *slog.Logger, vc config.VaultEntry) (*vault.GitCommitter, bool) {
	if vc.AutoCommit != nil && !*vc.AutoCommit {
		logger.Info("auto-commit disabled via config", "vault_path", vc.Path)
		return nil, false
	}
	if vc.AutoCommit == nil {
		gitDir := filepath.Join(vc.Path, ".git")
		if _, err := os.Stat(gitDir); err != nil {
			logger.Debug("auto-commit auto-detect: vault is not a git working tree",
				"vault_path", vc.Path, "git_dir", gitDir)
			return nil, false
		}
	}
	committer, err := vault.NewGitCommitter(vc.Path, vault.GitCommitterOptions{
		CommitterName: vc.CommitterName,
		CommitterEmail: vc.CommitterEmail,
		DebounceSeconds: vc.AutoCommitDebounceSeconds,
		AutoPush: vc.AutoPush,
		Logger: logger,
	})
	if err != nil {
		logger.Warn("auto-commit setup failed; continuing without auto-commit",
			"err", err, "vault_path", vc.Path)
		return nil, false
	}
	logger.Info("auto-commit active",
		"vault_path", vc.Path,
		"debounce_seconds", vc.AutoCommitDebounceSeconds,
		"auto_push", vc.AutoPush)
	return committer, true
}

// resolveAuth applies the precedence chain CLI > env > config >
// default-true to `auth.required`, then — when enforcement is on —
// loads the verifier from the resolved keys directory. Returns
// (required, verifier, err) where verifier is nil when required is
// false (the AnonymousAuth bypass doesn't need a verifier).
//
// Path precedence chain (locked, per yaad-index):
// - CLI flag --auth-required (kong-populated *bool)
// - YAAD_INDEX_AUTH_REQUIRED env (kong-populated into the same field)
// - cfg.Auth.Required (config field, *bool tri-state)
// - default true
//
// Same chain for --keys-dir / YAAD_INDEX_KEYS_DIR / auth.keys_dir /
// /etc/yaad-index/keys default.
func resolveAuth(logger *slog.Logger, cliRequired *bool, cliKeysDir string, cfg *config.Config) (bool, auth.Verifier, error) {
	required := true
	switch {
	case cliRequired != nil:
		required = *cliRequired
	case cfg != nil && cfg.Auth.Required != nil:
		required = *cfg.Auth.Required
	}
	if !required {
		return false, nil, nil
	}
	keysDir := cliKeysDir
	if keysDir == "" && cfg != nil {
		keysDir = cfg.Auth.KeysDir
	}
	if keysDir == "" {
		keysDir = authDefaultKeysDir
	}
	verifier, err := auth.LoadVerifier(keysDir)
	if err != nil {
		return false, nil, fmt.Errorf("load verifier from %s: %w", keysDir, err)
	}
	logger.Info("auth enabled — protected routes require Bearer JWT",
		"keys_dir", keysDir)
	return true, verifier, nil
}

// loadJWKSIfAvailable resolves the keys directory the same way
// resolveAuth does (CLI > config > default) and returns the JWKS
// payload when public.pem is readable. A missing or unreadable
// keypair is treated as "endpoint not configured" — the operator
// running in dev-mode without keys gets /v1/jwks unregistered
// (404), which is the right v1 behavior; advertising an empty key
// set would be more confusing than absent.
//
// The loader is best-effort (logs a Warn on parse failure) so a
// malformed public.pem doesn't block server start; auth enforcement
// is a separate path and surfaces its own error in resolveAuth when
// required=true.
func loadJWKSIfAvailable(logger *slog.Logger, cliKeysDir string, cfg *config.Config) []auth.JWK {
	keysDir := cliKeysDir
	if keysDir == "" && cfg != nil {
		keysDir = cfg.Auth.KeysDir
	}
	if keysDir == "" {
		keysDir = authDefaultKeysDir
	}
	keys, err := auth.LoadJWKS(keysDir)
	if err != nil {
		logger.Info("/v1/jwks not registered — public key not available",
			"keys_dir", keysDir, "err", err.Error())
		return nil
	}
	return keys
}

// canonicalKindNames extracts the enabled-kinds set from the new
// map-shape `canonical_kinds:` config (per ADR-0013 §1). Result is
// the slice of kind names in arbitrary order — CanonicalGuard
// dedupes via its internal map, so call-site order doesn't matter.
// (EnabledKinds returns unsorted output, by design — the guard's
// callers don't depend on order.)
func canonicalKindNames(reg map[string]config.CanonicalKindConfig) []string {
	if len(reg) == 0 {
		return nil
	}
	out := make([]string, 0, len(reg))
	for k := range reg {
		out = append(out, k)
	}
	return out
}

// collectPluginCanonicalContributions walks the loaded plugins and
// gathers Layer-2 inputs to ADR-0016's four-layer merge:
//
// - Per-kind gap additions/overrides from each plugin's
// `canonical_kinds_extras` capabilities field. Per-plugin gap-
// description conflicts (two plugins describing the same field
// differently for the same kind) are resolved by last-loaded-
// plugin-wins per ADR-0016 §5.
// - The union of every plugin's `canonical_kinds_emitted` list —
// these kinds AUTO-ACTIVATE in the merged registry without
// operator re-declaration per ADR-0016 §2.
//
// Returns (pluginGaps, pluginEmittedKinds): the merge function
// consumes both. Unknown / un-typed gaps fall through with
// Type=string default (matches GapSpec's UnmarshalYAML shorthand).
func collectPluginCanonicalContributions(registry *plugins.Registry) (map[string]map[string]config.GapSpec, []string) {
	logger := slog.Default()
	pluginGaps := make(map[string]map[string]config.GapSpec)
	emittedSet := make(map[string]struct{})
	for _, p := range registry.Plugins() {
		caps := p.Capabilities()

		// Build the per-plugin emitted set so the §6.5 consistency
		// check (extras must reference only emitted kinds) is
		// scoped to THIS plugin — a different plugin's emitted
		// set doesn't help cover this plugin's typo.
		perPluginEmitted := make(map[string]struct{}, len(caps.CanonicalKindsEmitted))
		for _, kind := range caps.CanonicalKindsEmitted {
			perPluginEmitted[kind] = struct{}{}
			emittedSet[kind] = struct{}{}
		}

		for kind, extras := range caps.CanonicalKindsExtras {
			// ADR-0016 §6.5: a plugin's canonical_kinds_extras may
			// only reference kinds also listed in
			// canonical_kinds_emitted. Extras for an un-emitted
			// kind is almost certainly a copy-paste typo; the
			// canonical declaration wins (silently), the typo
			// surfaces as a WARN naming plugin + kind.
			if _, ok := perPluginEmitted[kind]; !ok {
				logger.Warn("plugin canonical_kinds_extras references a kind absent from canonical_kinds_emitted; ignored — likely typo (per ADR-0016 §6.5)",
					"plugin", p.Name(), "kind", kind)
				continue
			}
			if pluginGaps[kind] == nil {
				pluginGaps[kind] = make(map[string]config.GapSpec, len(extras.Gaps))
			}
			for fieldName, spec := range extras.Gaps {
				// Convert plugins.GapSpec to config.GapSpec —
				// same shape, separate types so the plugins
				// package doesn't import internal/config.
				typ := spec.Type
				if typ == "" {
					typ = "string"
				}
				pluginGaps[kind][fieldName] = config.GapSpec{
					Type: typ,
					Description: spec.Description,
				}
			}
		}
	}
	emitted := make([]string, 0, len(emittedSet))
	for k := range emittedSet {
		emitted = append(emitted, k)
	}
	return pluginGaps, emitted
}

// collectPluginEmittedEdgeTypes is the edge-type half of ADR-0016
// §plugin-driven-activation: walks the loaded plugins and unions
// every Capabilities.CanonicalEdgeTypesEmitted value into a single
// slice. Mirrors the kind-side path in
// collectPluginCanonicalContributions so a plugin declaring it MAY
// emit a canonical edge type auto-activates that type without
// operator opt-in. Empty strings (sloppy capabilities declarations)
// are dropped silently — NewCanonicalGuard already skips them too.
func collectPluginEmittedEdgeTypes(registry *plugins.Registry) []string {
	emittedSet := make(map[string]struct{})
	for _, p := range registry.Plugins() {
		caps := p.Capabilities()
		for _, t := range caps.CanonicalEdgeTypesEmitted {
			if t == "" {
				continue
			}
			emittedSet[t] = struct{}{}
		}
	}
	emitted := make([]string, 0, len(emittedSet))
	for t := range emittedSet {
		emitted = append(emitted, t)
	}
	return emitted
}

// buildCanonicalKindResolvers walks the loaded plugins, validates
// each plugin's ResolvesCanonicalKinds subset constraint per #304
// Cut A, and returns the static `kind → []plugin-name` ownership
// map later cuts consume.
//
// **Per-plugin validation (ERROR):** every entry in a plugin's
// ResolvesCanonicalKinds MUST also appear in its
// CanonicalKindsEmitted. A typo here would silently route
// resolution to a plugin that doesn't emit the kind it claims to
// resolve — fail-fast at config-load is safer than a runtime
// 404. Mirrors the §6.5 typo gate's intent, but ERROR not WARN.
//
// **Cross-plugin coverage (WARN):** every kind in
// pluginEmittedKinds SHOULD have at least one resolver. Lone
// emitters surface as WARN here. Cut C's centralized edge-write
// falls these through to the existing edge-write path (no
// resolution attempt; legacy pass-through behavior). Operators
// see the gap before workflows that expect auto-resolution land.
//
// **Cardinality:** Cut A does NOT enforce one-resolver-per-kind.
// The returned map shape is `kind → []plugin` so multi-resolver
// kinds (e.g. yaad-wikipedia + yaad-bgg both resolving
// `boardgame`) survive into Cut C, which is where routing
// consumption needs to pick a single plugin and may reject
// ambiguity.
//
// Returns the ownership map on success. Returns error only on
// the per-plugin subset violation — cross-plugin coverage gaps
// log + return without error per the lenient policy.
func buildCanonicalKindResolvers(registry *plugins.Registry, pluginEmittedKinds []string, logger *slog.Logger) (map[string][]string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	resolvers := make(map[string][]string)
	for _, p := range registry.Plugins() {
		caps := p.Capabilities()
		if len(caps.ResolvesCanonicalKinds) == 0 {
			continue
		}
		emittedSet := make(map[string]struct{}, len(caps.CanonicalKindsEmitted))
		for _, k := range caps.CanonicalKindsEmitted {
			emittedSet[k] = struct{}{}
		}
		for _, k := range caps.ResolvesCanonicalKinds {
			if k == "" {
				continue
			}
			if _, ok := emittedSet[k]; !ok {
				return nil, fmt.Errorf("plugin %q declares resolves_canonical_kinds entry %q which is not in its canonical_kinds_emitted (#304 Cut A subset constraint)", p.Name(), k)
			}
			resolvers[k] = append(resolvers[k], p.Name())
		}
	}
	// Cross-plugin coverage WARN: a plugin-emitted kind with no
	// resolver claim from any plugin will reject auto-mode edges
	// in Cut C — surface the gap at startup so operators can
	// declare the resolver before deploying workflows that need
	// it.
	for _, k := range pluginEmittedKinds {
		if k == "" {
			continue
		}
		if _, ok := resolvers[k]; !ok {
			logger.Warn("canonical kind has plugin emitters but no resolver claim (per #304 Cut A): Cut C will fall edges to this kind through to the existing edge-write path without resolution — declare resolves_canonical_kinds on the relevant plugin to opt into name-resolution + disambiguation-task routing",
				"kind", k)
		}
	}
	return resolvers, nil
}

// unionEdgeTypes returns the deduped union of two edge-type slices
// in arbitrary order. Used to merge operator-config canonical_edge_
// types with plugin-declared CanonicalEdgeTypesEmitted before
// constructing the guard.
func unionEdgeTypes(a, b []string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for _, t := range a {
		if t == "" {
			continue
		}
		set[t] = struct{}{}
	}
	for _, t := range b {
		if t == "" {
			continue
		}
		set[t] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	return out
}

// validatePluginInstanceEnvReferences walks every configured
// plugin instance's env map and probes each value for
// `${NAME}` reference resolution against the daemon's process
// env per #256. Missing references → fail-fast error naming the
// plugin + instance + key so the operator sees the gap at boot;
// empty references (env var present but value is "") warn but
// proceed so the operator can intentionally have a placeholder
// during setup.
//
// The probe re-runs in `buildInstanceEnv` at dispatch time
// (`os.LookupEnv` is the same source either way) so a change to
// `yaad-index.env` between dispatches isn't picked up without a
// daemon restart — by design per the issue body.
// ensurePluginInstanceDataDirs walks every configured plugin
// instance, resolves its per-(plugin,instance) persistent-state
// directory (operator override or
// `<userCacheDir>/yaad-<plugin>/<instance>/` default), and
// MkdirAll-creates it at 0700 perms per #284. Idempotent — an
// existing dir is left in place (operator-owned state is never
// re-permed or deleted by the daemon). Fails fast when a
// non-directory squats the resolved path so the operator sees
// the misconfig at boot.
func ensurePluginInstanceDataDirs(pluginInstanceConfigs map[string][]config.InstanceEntry) error {
	for pluginName, instances := range pluginInstanceConfigs {
		for _, instance := range instances {
			resolved, err := datadir.Resolve(pluginName, instance.Name, instance.DataDir)
			if err != nil {
				return fmt.Errorf("resolve data dir for plugin %q instance %q: %w",
					pluginName, instance.Name, err)
			}
			if err := datadir.Ensure(resolved); err != nil {
				return fmt.Errorf("ensure data dir for plugin %q instance %q: %w",
					pluginName, instance.Name, err)
			}
		}
	}
	return nil
}

func validatePluginInstanceEnvReferences(pluginInstanceConfigs map[string][]config.InstanceEntry, logger *slog.Logger) error {
	for pluginName, instances := range pluginInstanceConfigs {
		for _, instance := range instances {
			for k, v := range instance.Env {
				_, emptyRefs, err := config.ExpandEnvReferences(v)
				if err != nil {
					return fmt.Errorf("plugin %q instance %q env[%s]: %w",
						pluginName, instance.Name, k, err)
				}
				for _, refName := range emptyRefs {
					logger.Warn("plugin instance env: reference resolved to empty value",
						"plugin", pluginName,
						"instance", instance.Name,
						"env_key", k,
						"reference", refName)
				}
			}
		}
	}
	return nil
}

