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
	"github.com/yaad-index/yaad-index/internal/attachments"
	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/subprocess"
	"github.com/yaad-index/yaad-index/internal/reindex"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/loader"
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

// storeEntityResolver satisfies engine.EntityResolver against
// the production store.Store. Returns the entity's Data map
// plus id + kind fields injected so CEL predicates can
// reference entity.id and entity.kind without operators
// having to materialize them inside Data.
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
	out := make(map[string]any, len(e.Data)+2)
	for k, v := range e.Data {
		out[k] = v
	}
	out["id"] = e.ID
	out["kind"] = e.Kind
	return out, nil
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

	// Construct the daemon-internal event bus (per ADR-0024
	// Phase 2). The API mutation handlers publish to + the
	// workflow engine subscribes from this same bus, so
	// instance-sharing is essential: a separate default-bus
	// inside the api package would leave the engine
	// subscribing to a phantom bus that never sees any
	// events.
	bus := eventbus.NewMemoryBus()

	var (
		handlerOpts = []api.HandlerOption{api.WithEventBus(bus)}
		guard       *config.CanonicalGuard
	)
	if cfg != nil {
		// ADR-0016 §4: build the merged effective canonical-kind
		// registry from {code defaults, plugin extras, operator
		// defaults, operator per-kind}. The merged result drives
		// the runtime — guard, needs_fill responses, structure,
		// AI-fill prompts. Plugin-emitted canonical_kinds_emitted
		// auto-activate via the merge (no operator re-declaration).
		pluginGaps, pluginEmittedKinds := collectPluginCanonicalContributions(registry)
		mergedRegistry := config.MergeCanonicalRegistry(
			pluginGaps,
			pluginEmittedKinds,
			cfg.CanonicalKindsDefaults,
			cfg.CanonicalKinds,
			logger,
		)

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
		enabledEdgeTypes := unionEdgeTypes(cfg.CanonicalEdgeTypes, collectPluginEmittedEdgeTypes(registry))
		guard = config.NewCanonicalGuard(canonicalKindNames(mergedRegistry), enabledEdgeTypes)
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
		if len(mergedRegistry) > 0 {
			handlerOpts = append(handlerOpts, api.WithCanonicalKindRegistry(mergedRegistry))
			logger.Info("canonical_kinds registry merged + surfaced (per ADR-0013 §2 a prior PR, ADR-0016 §4 four-layer merge)",
				"kinds", len(mergedRegistry),
				"plugin_emitted", len(pluginEmittedKinds),
				"operator_per_kind", len(cfg.CanonicalKinds))
		}
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
		reindexer, err := reindex.New(st, cfg.Vault.Path, guard, logger)
		if err != nil {
			return fmt.Errorf("init reindex (vault.path=%s): %w", cfg.Vault.Path, err)
		}
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
		// Propagate the staging dir to subprocess plugins via
		// YAAD_PLUGIN_STAGING_DIR (per ADR-0014 §6 PR-B). Mirrors
		// clock.SetLocation's plumbing — package-level set-once at
		// boot, lock-free read on every subprocess spawn.
		subprocess.SetStagingDir(stagingDir)
		logger.Info("attachments dispatcher active", "plugin_staging_dir", stagingDir)

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
			Paths:          []string{workflowDir},
			PluginRegistry: loaderRegistryAdapter{registry: registry},
			PollInterval:   loader.DefaultPollInterval,
			Logger:         logger,
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
		wfRunner := actions.New(actions.Options{
			TaskWriter: actions.NewFileTaskWriter(cfg.Vault.Path),
			// Phase 4.B stubs — surface clear "vault-backed
			// impl pending" errors so operators see the gap
			// at execute time. Phase 4.B.2 follow-up replaces
			// these with real vault.Writer-backed impls.
			CommentWriter: actions.StubCommentWriter{},
			GapWriter:     actions.StubGapWriter{},
		})
		wfEngine, err := engine.New(engine.Options{
			Bus:      bus,
			Resolver: wfResolver,
			Runner:   wfRunner,
			Logger:   logger,
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
		// Per-plugin config env (#7) threaded through so reprobe
		// matches the boot path.
		p, err := subprocess.New(entry.Name, entry.Path,
			subprocess.WithLogger(logger),
			subprocess.WithConfigEnv(config.PluginConfigEnvVars(entry.Name, entry.Config)))
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
		registry.Register(p)
		logger.Info("plugin registered",
			"name", entry.Name, "path", entry.Path, "source", outcome.String(),
			"capabilities_name", p.Capabilities().Name,
			"capabilities_version", p.Capabilities().Version)

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
	// sub-block per yaad-index #7. Passed to both --init and
	// cache-hit construction paths so plugins reading config at
	// --init time see the same env regardless of cache state.
	configEnv := config.PluginConfigEnvVars(name, entry.Config)

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
				p, ctorErr := subprocess.NewWithCapabilities(name, path, caps,
				subprocess.WithLogger(logger),
				subprocess.WithConfigEnv(configEnv))
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
		subprocess.WithLogger(logger),
		subprocess.WithConfigEnv(configEnv))
	if err != nil {
		return nil, outcome, err
	}
	caps := p.Capabilities()
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
