// Package api wires the v1 HTTP surface defined in ADR-0002. The handler
// returned by NewHandler is the only entry point; routes are registered
// with the Go 1.22+ method-aware net/http.ServeMux pattern (`GET /v1/...`).
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/yaad-index/yaad-index/internal/attachments"
	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// NewHandler returns the v1 API router with an empty plugin registry —
// suitable for tests and dev binaries that don't load a config file.
// Production binaries should call NewHandlerWithRegistry with a
// registry hydrated from the operator's config (per ADR-0006).
func NewHandler(logger *slog.Logger, st store.Store) http.Handler {
	return NewHandlerWithRegistry(logger, st, plugins.NewRegistry())
}

// NewHandlerWithRegistry is the same as NewHandler but takes an
// explicitly-constructed plugin registry. Used by main.go to wire the
// config-allowlisted subprocess plugins, and by tests to register
// fixture plugins without a binary on disk.
//
// Endpoints are registered against a Go 1.22+ method-aware
// net/http.ServeMux.
//
// The store is plumbed through to all handlers; the ingest tracker
// (in-memory state for in-flight /v1/ingest attempts) is constructed
// once here and shared across requests so concurrent ingests of the
// same URL share a single simulator goroutine + persistence call.
//
// Middleware composition (outer → inner):
// - withRequestID: stamp a per-request id on the context + X-Request-Id
// response header so panic recovery and downstream handlers share an
// identifier callers can correlate against.
// - withRecover: catch panics from any handler and emit a canonical
// 500 envelope when the response hasn't been committed yet.
//
// Existing per-handler writeError calls remain the source of every
// non-panic error shape; the middleware never re-wraps them.
func NewHandlerWithRegistry(logger *slog.Logger, st store.Store, registry *plugins.Registry, opts ...HandlerOption) http.Handler {
	cfg := handlerConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	// Default-init the write-lock manager when no opt provided. Every
	// handler that mutates the vault expects a non-nil manager; a
	// fresh empty manager is the right zero-value for tests + dev
	// deployments.
	if cfg.writeLocks == nil {
		cfg.writeLocks = writelocks.New()
	}
	// Default-init the event bus when no opt provided. Every mutation
	// handler expects a non-nil bus to publish events on; a fresh
	// in-memory bus with zero subscribers is the right zero-value
	// (Publish is a no-op when nothing subscribes — see
	// internal/eventbus per ADR-0024 Phase 2.1).
	if cfg.eventBus == nil {
		cfg.eventBus = eventbus.NewMemoryBus()
	}

	mux := http.NewServeMux()
	tracker := newIngestTracker(logger, st, cfg.vaultWriter, cfg.vaultReader, cfg.canonicalGuard, cfg.cacheTTLSeconds, cfg.attachmentsDispatcher, cfg.writeLocks, cfg.eventBus)

	// Per yaad-index a prior PR: the protect wrapper enforces Bearer-JWT
	// auth on every protected route. When auth.required=false the
	// AnonymousAuth bypass attaches a synthetic claim instead — handlers
	// dereferencing ClaimFromContext continue to work either way.
	//
	// Public routes (health, structure, cv-status, future jwks) are wired
	// without protect() and stay accessible without a token by design —
	// system metadata, not vault data. Every other route (data reads +
	// data writes + reindex) goes through protect(). The split is
	// documented in adr/0002-api-surface.md Per the prior design,.
	protect := buildAuthMiddleware(logger, cfg)

	// Public — no auth.
	mux.HandleFunc("GET /v1/health", handleHealth(logger))
	mux.HandleFunc("GET /v1/structure", handleStructure(logger, registry, cfg.canonicalKindReg, cfg.canonicalEdgeTypes))
	mux.HandleFunc("GET /v1/cv-status", handleCVStatus(logger, st, cfg.canonicalKindReg, cfg.canonicalEdgeTypes))
	if len(cfg.jwks) > 0 {
		mux.HandleFunc("GET /v1/jwks", handleJWKS(logger, cfg.jwks))
	}

	// Protected — Bearer JWT (or anonymous bypass when auth.required=false).
	mux.Handle("GET /v1/kinds", protect(http.HandlerFunc(handleKinds(logger, registry))))
	mux.Handle("GET /v1/plugins", protect(http.HandlerFunc(handlePlugins(logger, registry))))
	mux.Handle("POST /v1/entities/batch", protect(http.HandlerFunc(handleEntitiesBatch(logger, st))))
	// /v1/entities/batch is a method-target path, not an entity id. The
	// explicit GET handler below carves it out from the {id} matcher so a
	// stray GET on this path returns the canonical 405 envelope rather than
	// a confusing "no entity with id batch" 404. See rejectGetOnBatch.
	mux.Handle("GET /v1/entities/batch", protect(http.HandlerFunc(rejectGetOnBatch())))
	mux.Handle("GET /v1/entities/{id}", protect(http.HandlerFunc(handleEntity(logger, st, cfg.vaultReader))))
	mux.Handle("DELETE /v1/entities/{id}", protect(http.HandlerFunc(handleEntityDelete(logger, st, cfg.vaultWriter, cfg.writeLocks))))
	// Archive lifecycle per ADR-0018 step 2. Same vault-then-DB
	// ordering as DELETE; non-destructive transitions toggling the
	// `archived_at` column + `_archive/<kind>/<slug>.md` placement.
	mux.Handle("POST /v1/entities/{id}/archive", protect(http.HandlerFunc(handleEntityArchive(logger, st, cfg.vaultWriter, cfg.writeLocks))))
	mux.Handle("POST /v1/entities/{id}/restore", protect(http.HandlerFunc(handleEntityRestore(logger, st, cfg.vaultWriter, cfg.writeLocks))))
	mux.Handle("GET /v1/entities/{id}/context", protect(http.HandlerFunc(handleEntityContext(logger, st, cfg.vaultReader))))
	mux.Handle("GET /v1/entities/{id}/attachments/{name}", protect(http.HandlerFunc(handleEntityAttachment(logger, st, cfg.vaultReader))))
	mux.Handle("POST /v1/edges", protect(http.HandlerFunc(handleCreateEdge(logger, st, registry, cfg.eventBus))))
	mux.Handle("GET /v1/edges", protect(http.HandlerFunc(handleListEdges(logger, st))))
	mux.Handle("GET /v1/search", protect(http.HandlerFunc(handleSearch(logger, st, registry))))
	mux.Handle("POST /v1/search/upstream", protect(http.HandlerFunc(handleSearchUpstream(logger, registry))))
	mux.Handle("POST /v1/ingest", protect(http.HandlerFunc(handleIngest(logger, st, tracker, registry, cfg.vaultReader, cfg.fillInstruction, cfg.canonicalKindReg))))
	mux.Handle("GET /v1/needs-fill", protect(http.HandlerFunc(handleNeedsFill(logger, st, cfg.vaultReader, cfg.fillInstruction, cfg.canonicalKindReg))))
	mux.Handle("POST /v1/entities/{id}/fill", protect(http.HandlerFunc(handleFill(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.canonicalKindReg, cfg.writeLocks, cfg.eventBus))))
	mux.Handle("POST /v1/entities/{id}/operator-fill", protect(http.HandlerFunc(handleEntityOperatorFill(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.canonicalKindReg, cfg.writeLocks, cfg.eventBus))))
	mux.Handle("POST /v1/entities/{id}/comments", protect(http.HandlerFunc(handleComments(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.canonicalKindReg))))

	// User-content (UGC) read + write surface per yaad-index
	// (PR-B added the GETs; PR-C added the writes).
	mux.Handle("GET /v1/user-content/{id}", protect(http.HandlerFunc(handleUserContentRead(logger, st, cfg.vaultReader))))
	mux.Handle("GET /v1/user-content/{id}/sections", protect(http.HandlerFunc(handleUserContentSectionsList(logger, st, cfg.vaultReader))))
	mux.Handle("GET /v1/user-content/{id}/sections/{sec}", protect(http.HandlerFunc(handleUserContentSection(logger, st, cfg.vaultReader))))
	mux.Handle("POST /v1/user-content", protect(http.HandlerFunc(handleUserContentCreate(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.canonicalKindReg, cfg.userContentFrontmatterEdges, cfg.writeLocks, cfg.eventBus))))
	mux.Handle("PUT /v1/user-content/{id}/sections/{sec}", protect(http.HandlerFunc(handleUserContentSectionReplace(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.writeLocks))))
	mux.Handle("PUT /v1/user-content/{id}/frontmatter", protect(http.HandlerFunc(handleUserContentFrontmatterEdit(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.canonicalKindReg, cfg.userContentFrontmatterEdges, cfg.eventBus, cfg.writeLocks))))
	mux.Handle("DELETE /v1/user-content/{id}", protect(http.HandlerFunc(handleUserContentDelete(logger, st, cfg.vaultReader, cfg.vaultWriter, cfg.writeLocks))))
	// Archive lifecycle for user-content per ADR-0018 step 2. Same
	// shared handler as the entity routes — kind-aware via the row
	// loaded from store. UGC entities live under their own DB IDs +
	// vault path namespace, but the archive transition is uniform.
	mux.Handle("POST /v1/user-content/{id}/archive", protect(http.HandlerFunc(handleEntityArchive(logger, st, cfg.vaultWriter, cfg.writeLocks))))
	mux.Handle("POST /v1/user-content/{id}/restore", protect(http.HandlerFunc(handleEntityRestore(logger, st, cfg.vaultWriter, cfg.writeLocks))))

	if cfg.reindexHandler != nil {
		mux.Handle("POST /v1/reindex", protect(cfg.reindexHandler))
	}
	if cfg.workflowEngine != nil {
		mux.Handle("POST /v1/workflows/trigger", protect(http.HandlerFunc(handleWorkflowTrigger(logger, cfg.workflowEngine))))
	}
	return withRequestID(withRecover(logger)(mux))
}

// buildAuthMiddleware picks between RequireAuth and AnonymousAuth based
// on the operator's `auth.required` resolution from main.go. Default
// (no verifier wired, no required flag set) is AnonymousAuth — tests
// constructed via NewHandler() get the dev-mode pass-through so the
// unauth'd test corpus keeps working without per-test plumbing.
//
// Production main.go always wires WithAuthVerifier + WithAuthRequired,
// so the production server runs RequireAuth unless the operator
// explicitly opts out via `auth.required=false`.
//
// The (authRequired=true, authVerifier=nil) combination panics — the cold-reviewer's
// a prior PR review note 2: silently falling to AnonymousAuth would mask a
// bad test setup or a missing wire-up in main.go and ship an unauth'd
// production server. A construction-time panic surfaces the misuse
// where it can still be fixed.
func buildAuthMiddleware(logger *slog.Logger, cfg handlerConfig) func(http.Handler) http.Handler {
	if cfg.authRequired {
		if cfg.authVerifier == nil {
			panic("api: WithAuthRequired(true) without WithAuthVerifier(...) — caller must wire both")
		}
		return RequireAuth(logger, cfg.authVerifier)
	}
	return AnonymousAuth()
}

// HandlerOption configures the v1 API router. Use WithReindexHandler
// to wire the POST /v1/reindex route in production binaries; tests
// that don't exercise reindex omit it.
type HandlerOption func(*handlerConfig)

type handlerConfig struct {
	reindexHandler http.Handler
	vaultWriter *vault.Writer
	vaultReader *vault.Reader
	canonicalGuard *config.CanonicalGuard
	cacheTTLSeconds int
	fillInstruction string
	canonicalKindReg map[string]config.CanonicalKindConfig
	canonicalEdgeTypes []string
	userContentFrontmatterEdges map[string]config.UserContentFrontmatterEdgeMapping
	authVerifier auth.Verifier
	authRequired bool
	jwks []auth.JWK
	attachmentsDispatcher *attachments.Dispatcher
	// writeLocks is the per-artifact daemon write-lock manager
	// (yaad-index #23 + ADR-0024). Acquired before any vault
	// mutation surface (ingest, fill, archive/restore, delete, UGC
	// section, UGC frontmatter); skipped for additive surfaces
	// (comments, edges). NewHandlerWithRegistry constructs a fresh
	// Manager when this is nil so tests + dev deployments don't
	// have to wire one explicitly.
	writeLocks *writelocks.Manager
	// eventBus is the daemon-internal pub-sub substrate per ADR-0024
	// (workflow engine Phase 2). Mutation handlers publish events
	// here so workflow subscribers (Phases 3-6) can react to graph
	// changes without coupling to the API layer. Default-constructed
	// to an in-memory bus with no subscribers when no opt provided —
	// Publish is a no-op in that state, so tests + dev deployments
	// don't have to wire one explicitly.
	eventBus eventbus.Bus
	// workflowEngine, when non-nil, exposes the manual-trigger
	// endpoint POST /v1/workflows/trigger per ADR-0024 §"Agent
	// surface". The endpoint stays unregistered (404) when this
	// option isn't wired — useful for tests + dev binaries
	// without a vault, where no workflow engine runs.
	workflowEngine *engine.Engine
}

// WithReindexHandler registers a handler for POST /v1/reindex. When
// omitted, the route is unregistered — a request to it surfaces as a
// 404 from the default mux. Production main.go passes a handler
// constructed by internal/reindex.HTTPHandler.
func WithReindexHandler(h http.Handler) HandlerOption {
	return func(c *handlerConfig) { c.reindexHandler = h }
}

// WithWriteLocks wires an externally-constructed write-lock Manager
// (per yaad-index #23). When unset, NewHandlerWithRegistry
// constructs a fresh empty Manager so tests + dev binaries don't
// have to wire one. Production main.go wires a single Manager
// shared across the daemon so all write handlers consult the same
// lock map.
func WithWriteLocks(m *writelocks.Manager) HandlerOption {
	return func(c *handlerConfig) { c.writeLocks = m }
}

// WithVaultIO wires a vault.Writer + vault.Reader into the ingest
// tracker so successful ingests write a markdown file to the vault
// before updating the DB (per ADR-0008 / a prior PR). Both arguments must
// be non-nil — a nil writer with a non-nil reader (or vice versa)
// panics on tracker construction. Tests that don't exercise the
// vault path omit this option entirely; the tracker then falls back
// to DB-only persistence (the pre-a prior PR behavior).
func WithVaultIO(w *vault.Writer, r *vault.Reader) HandlerOption {
	return func(c *handlerConfig) {
		c.vaultWriter = w
		c.vaultReader = r
	}
}

// WithCanonicalGuard wires the operator-config-derived canonical
// kinds / edge types validator into the ingest path (per ADR-0008 /
// a prior PR). When a plugin emits canonical-shape stubs alongside its
// source-shape entity, yaad-index filters them through this guard;
// only kinds in the operator's `canonical_kinds:` config materialize.
//
// Omitting this option is observationally equivalent to passing a
// guard built from empty slices: no canonical layer materializes.
// Tests that don't exercise the canonical path may pass nil or omit
// the option.
func WithCanonicalGuard(g *config.CanonicalGuard) HandlerOption {
	return func(c *handlerConfig) { c.canonicalGuard = g }
}

// WithCacheTTL bounds how long a notation cache hit is considered
// fresh (per yaad-index the source issue a prior PR; reshaped under PR for
// the three-level resolution chain). The argument is taken as the
// global-level input to resolveCacheTTL at ingest time — sentinel
// rules apply (positive = N seconds, negative = infinite, zero =
// no opinion / fall through to all-zero default).
//
// Per the prior design, the lookup-side TTL check reads from the entity's vault
// frontmatter (`cache_ttl_seconds:`), NOT from this option — the
// global value participates only at ingest-time resolution. The
// API surface is preserved for backward compat with existing tests
// that pass a time.Duration; negative durations get sign-preserved
// through the int conversion.
//
// force_refetch=true skips the cache lookup entirely (orthogonal
// to TTL).
func WithCacheTTL(ttl time.Duration) HandlerOption {
	return func(c *handlerConfig) {
		// Preserve sign so negative durations round-trip as negative
		// int seconds (= "infinite" sentinel post-). Round-trip
		// via .Seconds() loses no precision because every operator-
		// settable TTL is a whole-second value (cache_ttl_seconds:
		// is an integer-only YAML int).
		c.cacheTTLSeconds = int(ttl / time.Second)
	}
}

// WithFillInstruction wires the operator's `fill_instruction:` config
// onto every needs_fill ingest response (per ADR-0013 §2 a prior PR). The
// string is passed verbatim — no composition, no post-processing.
// Empty / unset → no `instruction` field on the wire (omitempty).
//
// Per-kind `instruction:` override + canonical_vocabulary registry
// land via WithCanonicalKindRegistry (a prior PR per ADR-0013).
func WithFillInstruction(text string) HandlerOption {
	return func(c *handlerConfig) {
		c.fillInstruction = text
	}
}

// WithCanonicalEdgeTypes surfaces the operator's
// `canonical_edge_types` list onto introspection endpoints (per
// yaad-index / ADR-0013 §7). The list is read-only on the
// handler side — the same caller-must-not-mutate contract as the
// registry map. Empty / nil → empty list on the wire.
func WithCanonicalEdgeTypes(edgeTypes []string) HandlerOption {
	return func(c *handlerConfig) {
		c.canonicalEdgeTypes = edgeTypes
	}
}

// WithCanonicalKindRegistry wires the operator's `canonical_kinds:`
// registry (per ADR-0013 §1 + §2 a prior PR) onto needs_fill responses:
//
// - Per-kind `instruction:` overrides the global `fill_instruction`
// for entities whose kind appears in the registry. Resolution
// order: per-kind set → per-kind wins; per-kind unset → global
// wins; both unset → field omitted.
// - The full registry is surfaced verbatim under
// `canonical_vocabulary` on every needs_fill response. Empty /
// nil registry → field omitted (omitempty).
//
// Operator-config-only: plugins never control the registry contents
// (prompt-injection guardrail per ADR-0013 §2).
//
// **Caller must not mutate the map after passing it.** The handler
// retains the reference and surfaces it verbatim on every
// needs_fill response — concurrent mutation under load races the
// JSON encoder. yaad-index v1 treats config as immutable post-
// startup so this is safe today; documenting the contract pre-
// empts a config-hot-reload regression. Defensive-copy is a future
// option if the contract has to relax.
func WithCanonicalKindRegistry(reg map[string]config.CanonicalKindConfig) HandlerOption {
	return func(c *handlerConfig) {
		c.canonicalKindReg = reg
	}
}

// WithUserContentFrontmatterEdges wires the operator's
// `user_content_frontmatter_edges:` config block per yaad-index
// (re-implementation of on the ADR-0021 contract). When
// set, the UGC create handler walks each declared
// frontmatter-field name on the request body's `data` map and
// derives canonical-label edges from the values via the shared
// applyCanonicalTypeEdges helper. UGC is operator-authored, so
// pre-formed canonical-label strings are accepted same as on
// operator-fill.
//
// Empty / nil → UGC frontmatter-edge derivation is a no-op; the
// dead config field from/a prior PR stays parseable but inert
// (mirrors current behavior on operators who haven't declared
// any mappings).
func WithUserContentFrontmatterEdges(m map[string]config.UserContentFrontmatterEdgeMapping) HandlerOption {
	return func(c *handlerConfig) {
		c.userContentFrontmatterEdges = m
	}
}

// WithAuthVerifier wires the auth.Verifier (constructed in main.go from
// `<keys_dir>/public.pem`) onto the protected-route middleware (per
// yaad-index a prior PR). When this option is omitted, protected routes
// fall through to AnonymousAuth — useful for tests that don't exercise
// the auth path. Production main.go always wires both this and
// WithAuthRequired.
func WithAuthVerifier(v auth.Verifier) HandlerOption {
	return func(c *handlerConfig) {
		c.authVerifier = v
	}
}

// WithAuthRequired toggles the Bearer-JWT enforcement on protected
// routes (per yaad-index a prior PR). True → RequireAuth; false →
// AnonymousAuth bypass with a synthetic claim. Default false so tests
// that don't construct a verifier continue working; production main.go
// resolves the precedence chain (CLI > env > config > default-true)
// and passes the result here.
func WithAuthRequired(required bool) HandlerOption {
	return func(c *handlerConfig) {
		c.authRequired = required
	}
}

// WithAttachmentsDispatcher wires the ADR-0014 attachment dispatcher
// onto the ingest path. When a plugin emits FetchResult.Attachments,
// the tracker calls Dispatch (3-scheme dispatch + path-traversal
// guard + tmp cleanup) and stamps the resulting (role, uri) pairs
// onto the new provenance row's `fetch_attachments` field.
//
// Omitting this option leaves attachments unhandled — plugins that
// emit attachments see them silently dropped (with a debug log per
// ingest). Tests that don't exercise the attachment path may pass
// nil or omit the option. Production main.go always wires this when
// vault.path is configured.
func WithAttachmentsDispatcher(d *attachments.Dispatcher) HandlerOption {
	return func(c *handlerConfig) { c.attachmentsDispatcher = d }
}

// WithJWKS publishes the verifier's public key on `GET /v1/jwks` per
// yaad-index a prior PR. The slice is constructed at startup by
// auth.LoadJWKS and cached for the server's lifetime — clients
// (peer agents, future yaad-mcp) cache for one hour via the
// `Cache-Control: public, max-age=3600` response header.
//
// Omitting this option leaves /v1/jwks unregistered (404) — useful
// when the operator runs in dev-mode without keys on disk. When
// keys are loaded but auth.required=false, the JWKS endpoint can
// still serve them; the document is independent of enforcement.
func WithJWKS(keys []auth.JWK) HandlerOption {
	return func(c *handlerConfig) {
		c.jwks = keys
	}
}

// WithEventBus wires the daemon-internal pub-sub substrate (per
// ADR-0024 workflow engine Phase 2) into the API mutation
// handlers. POST /v1/edges, POST /v1/entities/{id}/fill, and POST
// /v1/entities/{id}/operator-fill publish entity.edge_added and
// fill.completed events for any subscriber the operator has
// registered (workflow engine, future audit log, etc.).
//
// Omitting this option leaves the handlers wired to a fresh
// in-memory Bus with no subscribers — Publish is a no-op in that
// state, so existing surfaces see no behavior change.
func WithEventBus(b eventbus.Bus) HandlerOption {
	return func(c *handlerConfig) { c.eventBus = b }
}

// WithWorkflowEngine wires the workflow engine into the API
// so POST /v1/workflows/trigger is registered (per ADR-0024
// §"Agent surface"). Omitting this option leaves the route
// unregistered (404) — appropriate for tests + dev binaries
// that don't run the workflow engine. Production main.go
// wires this when cfg.Vault.Path is configured (the same
// gating used for the loader + reconcile loop).
func WithWorkflowEngine(eng *engine.Engine) HandlerOption {
	return func(c *handlerConfig) { c.workflowEngine = eng }
}
