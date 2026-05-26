package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Ingest tunables per ADR-0002 lines 72–85.
const (
	ingestDefaultWaitSeconds = 60
	ingestMinWaitSeconds = 0 // 0 means async-only mode (ADR line 85).
	ingestMaxWaitSeconds = 300 // ADR line 85.
	// stubNeedsFillEntityID is the placeholder id the needs-fill-test
	// URL fixture persists. The fill handler doesn't depend on this
	// const any more (it owns its own end-to-end fixture), but ingest
	// still needs a stable id for the simulator to write to + the
	// long-poll response to surface.
	stubNeedsFillEntityID = "boardgame:needs-fill-stub"
)

// ingestRequest mirrors the POST /v1/ingest body (ADR-0002 lines 72–79).
//
// WaitSeconds is `*int` (not `int`) so the handler can distinguish
// "field absent" (apply the 60s default) from "field present with value
// 0" (caller intentionally requested async-only mode).
type ingestRequest struct {
	URL string `json:"url"`
	Hint string `json:"hint,omitempty"`
	ForceRefetch bool `json:"force_refetch,omitempty"`
	WaitSeconds *int `json:"wait_seconds,omitempty"`
}

// Four discrete response shapes per ADR-0002 (universal-state amendment
// + ADR-0006 disambiguation extension). Each carries `state` (the
// canonical discriminator going forward) AND the legacy `status` field
// (same value) so existing clients reading either field keep working.
//
// `state` is yaad-index inference, NOT plugin-emitted — the plugin's
// FetchResult populates Entity/Options/Gaps and the tracker labels
// the response per ADR-0006.

type ingestCompleteResponse struct {
	OK bool `json:"ok"`
	State string `json:"state"` // "complete"
	Status string `json:"status"` // legacy alias, same value
	Entity entity `json:"entity"`
}

type ingestQueuedResponse struct {
	OK bool `json:"ok"`
	State string `json:"state"` // "queued"
	Status string `json:"status"` // legacy alias, same value
	EstimatedEntityID string `json:"estimated_entity_id,omitempty"`
}

type ingestNeedsFillResponse struct {
	OK bool `json:"ok"`
	State string `json:"state"` // "needs_fill"
	Status string `json:"status"` // legacy alias, same value
	Entity entity `json:"entity"`
	CleanContent string `json:"clean_content"`
	CleanContentTruncated bool `json:"clean_content_truncated"`
	// Gaps is a {field-name → description} object — keys are the
	// data field names the agent's AI must fill, values are short
	// descriptions guiding the AI's output (per ADR-0002 universal-
	// state amendment).
	//
	// Per ADR-0008 (a prior PR): the entity ID itself is the durable
	// callback handle for POST /v1/entities/{id}/fill — no separate
	// fill_token + fill_token_expires_at fields are emitted, and
	// agents can fill arbitrarily late or across multiple partial
	// calls.
	Gaps map[string]string `json:"gaps"`

	// Instruction is operator-supplied prose for the AI fill pass.
	// Resolution order at response-build time (per ADR-0013 §2):
	// 1. If the entity's kind is registered in `canonical_kinds:`
	// with a non-empty per-kind `instruction:` → that wins.
	// 2. Else, if global `fill_instruction:` is set → that wins.
	// 3. Else → the field is omitted entirely (omitempty).
	// For source-shape entities (kind NOT in the registry), only
	// path (2) fires — the per-kind override path is skipped. The
	// server does NOT compose or post-process the chosen value;
	// it's byte-identical to the config string.
	Instruction string `json:"instruction,omitempty"`

	// CanonicalVocabulary is the operator's CV registry surfaced
	// verbatim from `cfg.CanonicalKinds` (per ADR-0013 §2 a prior PR).
	// Each entry carries the kind's gap-set vocabulary + optional
	// per-kind instruction; the agent reads this alongside the
	// resolved Instruction when deciding whether to extract
	// canonical companions from `clean_content`.
	//
	// Empty / nil registry → field omitted (omitempty).
	// Operator-config-only — plugins never control these contents
	// (prompt-injection guardrail per ADR-0013 §2).
	CanonicalVocabulary map[string]config.LegacyCanonicalKindConfig `json:"canonical_vocabulary,omitempty"`
}

// ingestDisambiguationResponse is the new 200 shape (per ADR-0006).
// The plugin returned Options instead of a single entity; the caller
// picks one option by its key (the plugin's canonical id) and
// re-invokes /v1/ingest with the plugin's shorthand input shape
// (`<plugin>: <id>`) to fetch the chosen candidate. No `entity`
// field — the canonical id isn't resolved yet. Single-option
// responses are valid ("is this what you meant?" confirmation) and
// are not auto-resolved.
type ingestDisambiguationResponse struct {
	OK bool `json:"ok"`
	State string `json:"state"` // "disambiguation"
	Status string `json:"status"` // legacy alias, same value
	// Options is keyed by the plugin's canonical id. Empty options
	// is impossible at this layer — the tracker maps an empty
	// FetchResult to 404 not_found before reaching this shape.
	Options map[string]ingestDisambiguationOption `json:"options"`
}

type ingestDisambiguationOption struct {
	Label string `json:"label"`
	Summary string `json:"summary,omitempty"`
}

// cacheNotationsSource is the canonical `provenance.source` value
// surfaced on a cache-hit response so the agent can distinguish
// "served from the index cache" from "served from a fresh upstream
// fetch." Per yaad-index the source issue a prior PR — these entries ride on
// the response only; the entity's persistent provenance stays
// untouched (cache hits aren't fetches).
const cacheNotationsSource = "cache:notations"

func handleIngest(logger *slog.Logger, st store.Store, tracker *ingestTracker, registry *plugins.Registry, vaultReader *vault.Reader, fillInstruction string, canonicalKindReg map[string]config.CanonicalKindConfig, pluginInstanceConfigs map[string][]config.InstanceEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		if strings.TrimSpace(req.URL) == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "url is required")
			return
		}

		// Routing-time validation per ADR-0022 §4 + yaad-index.
		// Cheap shape-check that rejects inputs naming a registered
		// plugin whose declared url_patterns / commands don't accept
		// the input — saving subprocess wall-clock on the unhappy
		// path. Inputs without a recognized plugin namespace fall
		// through to the existing first-match-wins registry walk
		// below.
		//
		// Pre-PR- there was an http/https-only scheme check + a
		// strict url.ParseRequestURI parse here; both rejected
		// legitimate disambiguation re-invocations before the plugin
		// matcher saw them. Routing-time validation is plugin-aware
		// so it doesn't repeat that mistake — it only rejects when
		// the named plugin's own declared shape contradicts the
		// input.
		if vErr := validateRouting(registry, req.URL); vErr != nil {
			writeError(w, vErr.Status, vErr.Code, vErr.Message)
			return
		}

		// Per-command authorization gate per ADR-0022 §5.3 (2026-05-22
		// amendment for #107). The blanket-operator-only rule has been
		// replaced with a per-command flag: each CommandSpec declares
		// `operator_only: true|false`, defaulting false (agent-callable).
		// Pair-claim tokens may invoke any command whose spec does NOT
		// set operator_only; operator-only tokens may invoke any
		// command. Anonymous dev-mode claims continue to pass — they
		// satisfy ClaimIsOperatorOnly so `auth.required=false`
		// deployments don't suddenly need real tokens.
		//
		// Audit trail: JWT.sub continues to distinguish agent
		// invocations (`sub=<agent>`) from operator invocations
		// (`sub=<operator>`) — no provenance shape change needed.
		if inv := plugins.ParseInvocation(req.URL); inv.Shape == plugins.InvocationCommand {
			c, _ := ClaimFromContext(r.Context())
			if !ClaimIsOperatorOnly(c) && commandRequiresOperator(registry, inv) {
				writeError(w, http.StatusForbidden, "operator_only_required",
					fmt.Sprintf("command %q on plugin %q declares operator_only; pair-claim tokens cannot invoke it",
						inv.Command, inv.Plugin))
				return
			}
		}

		waitSeconds, err := boundIntPtr(req.WaitSeconds,
			ingestDefaultWaitSeconds, ingestMinWaitSeconds, ingestMaxWaitSeconds)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("wait_seconds: %v", err))
			return
		}

		// Lookup-first per the source issue a prior PR: probe the index (the
		// cache) before invoking the plugin. The notation table maps
		// every input form (URL, shorthand `<plugin>: <id>`, future
		// shapes) to the canonical entity slug; a hit short-circuits
		// the entire plugin spawn + upstream fetch + persist
		// pipeline. force_refetch=true skips the lookup.
		var ttlExpired bool
		if !req.ForceRefetch {
			hit, expired := tryNotationCacheHit(r.Context(), logger, st, vaultReader, req.URL)
			if hit != nil {
				respondFromCacheHit(w, r, logger, hit, fillInstruction, canonicalKindReg)
				return
			}
			ttlExpired = expired
		}

		// Plugin path. Two dispatch shapes per ADR-0022:
		//
		//   - URL-shape: walk the registry in registration order and
		//     take the first plugin whose Match accepts the input.
		//     Fixture sentinels are the fallback so brass-birmingham
		//     / queued-test / needs-fill-test still drive long-poll
		//     tests without a network or a registered plugin.
		//
		//   - Command-shape (`<plugin>: !<command>`): the Match-based
		//     walk can't see commands — a plugin with empty url_patterns
		//     but non-empty commands (e.g. gmail) is invisible there.
		//     validateRouting upstream already confirmed the named
		//     plugin exists + advertises the command, so the dispatch
		//     here is a direct LookupByName.
		//
		// If neither hits → 422 unsupported_url (ADR-0002 / ADR-0006):
		// the request is well-formed, just not actionable with the
		// currently-loaded plugins.
		var att ingestAttempt
		inv := plugins.ParseInvocation(req.URL)
		if inv.Shape == plugins.InvocationCommand {
			// validateRouting passed → plugin exists. LookupByName
			// can't miss; the fallthrough is defensive.
			plugin, ok := registry.LookupByName(inv.Plugin)
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, "unsupported_url",
					fmt.Sprintf("no plugin handles URL %s", req.URL))
				return
			}
			// ADR-0028 §4 (Cut 4) command-shape dispatch:
			//   - `<plugin>/<instance>: !<cmd>` → single-attempt
			//     against the named instance (validated below).
			//   - bare `<plugin>: !<cmd>` → fan out serially across
			//     every enabled instance in declaration order.
			//
			// Both shapes route through handleCommandFanOut. The
			// single-instance case (instance-scoped form OR a
			// plugin with only one configured instance) collapses
			// to the regular single-attempt response shape; the
			// multi-instance bare case emits an aggregate
			// fan-out response with per-instance status.
			instances := pluginInstanceConfigs[plugin.Name()]
			handleCommandFanOut(w, r, logger, st, tracker, plugin, req, inv, instances, waitSeconds, fillInstruction, canonicalKindReg)
			return
		} else if plugin, matched := registry.Lookup(req.URL); matched {
			// ADR-0028 §3 Cut 3: pick the active instance for
			// URL-shape ingest by walking the plugin's
			// instance_routing capability against the matched
			// URL. Fail-fast on unrouted (no glob matches) so
			// the operator (or agent) sees the exact URL that
			// failed routing and can fix the missing glob entry
			// — no silent fallback to the first-declared instance.
			instanceName, perr := pickInstance(plugin, pluginInstanceConfigs[plugin.Name()], req.URL)
			if perr != nil {
				switch {
				case errors.Is(perr, ErrUnroutedURL):
					writeUnroutedError(w, plugin.Name(), req.URL, perr)
					return
				case errors.Is(perr, ErrNoURLRouting):
					writeError(w, http.StatusBadRequest, "no_url_routing", perr.Error())
					return
				case errors.Is(perr, ErrUnsupportedRoutingStrategy):
					writeError(w, http.StatusInternalServerError, "unsupported_routing_strategy", perr.Error())
					return
				default:
					writeError(w, http.StatusInternalServerError, "instance_routing_failed", perr.Error())
					return
				}
			}
			// ADR-0028 §4 (Cut 4): build the per-instance
			// subprocess env splice and thread it through the
			// ingest attempt. The simulator stamps it into the
			// invocation ctx so subprocess.Plugin spawns this
			// call with the active instance's
			// YAAD_PLUGIN_CONFIG + InstanceEntry.Env entries.
			extraEnv, envErr := buildInstanceEnvForName(plugin.Name(), pluginInstanceConfigs[plugin.Name()], instanceName)
			if envErr != nil {
				writeError(w, http.StatusInternalServerError, "instance_env_failed", envErr.Error())
				return
			}
			att = ingestAttemptForPlugin(plugin, req.URL, instanceName)
			att.simulation.extraEnv = extraEnv
		} else {
			fixtureAtt, err := ingestAttemptForURL(req.URL)
			if errors.Is(err, errNoFixtureMatch) {
				writeError(w, http.StatusUnprocessableEntity, "unsupported_url",
					fmt.Sprintf("no plugin handles URL %s", req.URL))
				return
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
				return
			}
			att = fixtureAtt
		}
		att.forceRefetch = req.ForceRefetch
		att.ttlExpired = ttlExpired

		// `hint` is accepted-but-not-acted-on. The real plugin runtime
		// (ADR-0005) will dispatch against `hint`. `force_refetch` is
		// honored by the lookup-first guard above (the source issue a prior PR) —
		// when true, the cache lookup is skipped unconditionally and
		// ingest always invokes the plugin.
		_ = req.Hint

		rec := tracker.beginAttempt(att)

		// plannedID is what we surface on the queued path. For fixtures
		// it's the known id; for plugins it's empty (data-derived ids
		// per ADR-0002 lines 138–145 — estimated_entity_id is omitted
		// via omitempty when empty).
		plannedID := att.plannedEntityID
		if plannedID == "" && att.entity != nil {
			plannedID = att.entity.ID
		}

		if waitSeconds == 0 {
			// Async-only mode (ADR line 85). Don't block; surface the
			// queued shape immediately. The simulator continues in the
			// background and persistence happens whether or not this
			// caller comes back to poll.
			respondIngestQueued(w, r, logger, plannedID)
			return
		}

		timeout := time.Duration(waitSeconds) * time.Second
		snap, err := tracker.wait(r.Context(), rec, timeout)
		switch {
		case errors.Is(err, errIngestTimeout):
			respondIngestQueued(w, r, logger, plannedID)
			return
		case err != nil:
			// ctx cancelled (client dropped connection) — best effort
			// log, then return; the response writer is likely already
			// detached.
			logger.WarnContext(r.Context(), "ingest long-poll abandoned",
				"err", err, "id", plannedID)
			return
		}

		switch snap.state {
		case ingestStateComplete:
			respondIngestComplete(w, r, logger, st, snap.entityID)
		case ingestStateNeedsFill:
			respondIngestNeedsFill(w, r, logger, st, snap, fillInstruction, canonicalKindReg)
		case ingestStateDisambiguation:
			respondIngestDisambiguation(w, r, logger, snap.options)
		case ingestStateNotFound:
			writeError(w, http.StatusNotFound, "not_found",
				orDefault(snap.failureMessage, "no result for the requested URL"))
		case ingestStateFailed:
			writeError(w, http.StatusInternalServerError,
				orDefault(snap.failureCode, "internal_error"),
				orDefault(snap.failureMessage, "ingest failed"))
		default:
			// Defensive — wait should only return a non-pending snapshot.
			logger.ErrorContext(r.Context(), "ingest tracker: wait returned unexpected pending state",
				"id", plannedID, "state", int(snap.state))
			writeError(w, http.StatusInternalServerError, "internal_error",
				"ingest tracker returned an unexpected state")
		}
	}
}

// notationCacheHit captures the data needed to respond from the
// store without invoking a plugin (per the source issue a prior PR). State is
// inferred from openGaps: empty → complete, non-empty → needs_fill.
//
// vaultEntity (when non-nil) carries the single-hop body fields
// the cache-hit response surfaces via mergeVaultEntity (per issue
// a prior PR addendum) — clean_content, summary, tags, gaps,
// aliases, plugin, notations, notes. nil when WithVaultIO isn't
// wired or the vault read errored; the response then falls back to
// the DB-only shape.
type notationCacheHit struct {
	entity *store.Entity
	vaultEntity *vault.Entity
	openGaps []string
	matched store.Notation
}

// tryNotationCacheHit implements lookup-first dispatch (per issue
// a prior PR) with TTL freshness gating reshaped under to read
// the per-entity TTL from vault frontmatter (`cache_ttl_seconds:`)
// rather than a global config knob. Returns a non-nil hit when:
// - The notation table has a row for the request URL/shorthand.
// - The store has the entity that notation points at.
// - The vault (when wired) has the entity's gap state for
// surfacing on the needs_fill response.
// - When the vault entity's `cache_ttl_seconds` is positive: the
// entity's freshest successful provenance.fetched_at is within
// now-ttl. Past TTL → fall through to plugin path. nil/zero =
// no opinion (cache forever). Negative = infinite (cache forever).
//
// TTL is resolved at INGEST time (per the operator's clarification —
// resolveCacheTTL walks {entry > plugin > global}, the resolved value
// is baked into vault frontmatter). The lookup path is just
// `provenance[last].fetched_at + frontmatter.cache_ttl_seconds vs
// now` — no three-level walk here.
//
// Legacy entities (whose vault files predate the resolution PR
// and have no `cache_ttl_seconds` field) cache forever until they
// re-ingest naturally OR force_refetch is invoked. To bring them
// into the new TTL system, operators force-refetch each.
//
// On any miss / error, returns nil and the orchestrator falls
// through to the existing plugin Fetch path. Errors below the
// cache-miss threshold (vault read failure on a hit) downgrade to a
// "served as complete" rather than blocking ingest — the cache hit
// itself is still valuable.
// commandRequiresOperator looks up the dispatched command's spec on
// the registered plugin and returns its OperatorOnly flag. Unknown
// plugin or unknown command → false (the routing-time validator
// rejects those paths earlier with a structured error; falling
// through here as "agent-callable" keeps the auth gate from
// double-reporting the same failure).
//
// Per ADR-0022 §5.3 (2026-05-22 amendment for #107): operator_only is
// a per-command property, not a per-plugin or per-shape one.
func commandRequiresOperator(registry *plugins.Registry, inv plugins.Invocation) bool {
	p, ok := registry.LookupByName(inv.Plugin)
	if !ok {
		return false
	}
	for _, c := range p.Capabilities().Commands {
		if c.Name == inv.Command {
			return c.OperatorOnly
		}
	}
	return false
}

// Returns the cache hit (when fresh) or nil + a bool flagging whether
// the miss was specifically a TTL fall-through (so the caller can
// surface `re-ingest: ... [ttl_expired]` on the auto-commit message,
// per yaad-index the source issue).
func tryNotationCacheHit(
	ctx context.Context,
	logger *slog.Logger,
	st store.Store,
	vaultReader *vault.Reader,
	notation string,
) (*notationCacheHit, bool) {
	matched, err := st.GetNotation(ctx, notation)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false
	}
	if err != nil {
		logger.WarnContext(ctx, "store.GetNotation for cache lookup errored; falling through to plugin",
			"notation", notation, "err", err)
		return nil, false
	}
	got, err := st.GetEntity(ctx, matched.EntityID)
	if errors.Is(err, store.ErrNotFound) {
		// Stale notation row pointing at a gone entity. Don't try to
		// repair from this hot path — log + fall through; reindex
		// will reconcile.
		logger.WarnContext(ctx, "notation cache hit but entity missing; falling through to plugin",
			"notation", notation, "entity_id", matched.EntityID)
		return nil, false
	}
	if err != nil {
		logger.WarnContext(ctx, "store.GetEntity after notation hit errored; falling through to plugin",
			"notation", notation, "entity_id", matched.EntityID, "err", err)
		return nil, false
	}

	// Open gaps + per-entity TTL live in the vault frontmatter
	// (ADR-0008). Read the vault first — both the TTL gate
	// and the response enrichment (the source issue a prior PR addendum:
	// clean_content, summary, tags, aliases, plugin, notations,
	// notes) need it. DB-only mode (no vault wiring) skips the
	// TTL gate entirely; cache hits forever, matching the legacy
	// fallback contract.
	var (
		openGaps []string
		vaultEntity *vault.Entity
	)
	if vaultReader != nil {
		ve, err := vaultReader.ReadByID(got.Kind, got.ID)
		if err != nil {
			logger.WarnContext(ctx,
				"vault read for cache-hit enrichment errored; serving DB-only shape",
				"id", got.ID, "err", err)
		} else {
			openGaps = ve.Gaps
			vaultEntity = ve
		}
	}

	// TTL gate (per yaad-index). The vault entity's
	// `cache_expires:` is the SOLE freshness signal post-PR-B.
	// Legacy entries that carried only `cache_ttl_seconds:`
	// no longer participate in the gate — they cache forever
	// until force_refetch re-ingests and stamps cache_expires.
	// The migration is operator-driven;'s `cache refetch`
	// CLI bulk-applies it across the vault.
	if vaultEntity != nil && vaultEntity.CacheExpires.Expired(time.Now()) {
		logger.InfoContext(ctx,
			"notation cache hit: past cache_expires; falling through to refresh",
			"notation", notation, "entity_id", matched.EntityID,
			"cache_expires", vaultEntity.CacheExpires.Time)
		return nil, true
	}

	return &notationCacheHit{entity: got, vaultEntity: vaultEntity, openGaps: openGaps, matched: matched}, false
}

// (freshestPersistentFetch was removed in PR-B alongside the
// cache_ttl_seconds legacy fallback. The lookup-side freshness gate
// is now `vault.CacheExpires.Expired(now)` — no provenance scan
// needed. The CLI's freshestPersistentFetchVault — which mirrors
// the same shape on vault.Entity for the operator-facing list-
// expired age column — still lives in cmd/yaad-index/cache.go.)

// resolveInstruction picks the right `instruction` value to surface
// on a needs_fill response per ADR-0013 §2's resolution order:
//
// 1. If the entity's kind is registered in `canonical_kinds:` and
// that kind's per-kind `instruction:` is non-empty → use it.
// 2. Else fall back to the global `fill_instruction:`.
// 3. Else (both unset) → return "" so the wire field's omitempty
// tag drops it entirely.
//
// Source-shape entities (kind not in the registry) skip step (1) by
// virtue of the registry-lookup miss; only the global path applies.
// The returned string is byte-identical to whichever config field
// won — no composition, no post-processing.
func resolveInstruction(kind, global string, reg map[string]config.CanonicalKindConfig) string {
	// Post-ADR-0016: per-kind instruction is an *InstructionSpec
	// pointer. Effective per-kind text wins when set + non-empty;
	// otherwise we fall through to the global fill_instruction
	// (legacy ADR-0013 §2 behavior).
	if cfg, ok := reg[kind]; ok && cfg.Instruction != nil && cfg.Instruction.Text != "" {
		return cfg.Instruction.Text
	}
	return global
}

// respondFromCacheHit writes the cache-hit response without
// invoking the plugin or the tracker. complete vs needs_fill is
// inferred from the open-gap list surfaced from the vault.
//
// An ephemeral provenance entry tagged `cache:notations` is
// injected onto the response so the agent can distinguish a
// cache-served entity from a freshly-fetched one. The entity's
// PERSISTENT provenance is NOT touched here — cache hits aren't
// fetches; the persistence-side provenance still records only real
// upstream attempts.
//
// The wire entity carries the same single-hop body fields
// (clean_content / summary / tags / gaps / aliases / plugin /
// notations / notes) that GetEntity surfaces, via the
// mergeVaultEntity overlay (per yaad-index the source issue a prior PR
// addendum). Closes the gap a prior PR left where cache hits returned
// only the DB-mirrored slice.
func respondFromCacheHit(w http.ResponseWriter, r *http.Request, logger *slog.Logger, hit *notationCacheHit, fillInstruction string, canonicalKindReg map[string]config.CanonicalKindConfig) {
	wireEntity := toAPIEntity(hit.entity)
	if hit.vaultEntity != nil {
		wireEntity = mergeVaultEntity(wireEntity, hit.vaultEntity)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	wireEntity.Provenance = append(wireEntity.Provenance, provenanceEntry{
		Source: cacheNotationsSource,
		FetchedAt: now,
		OK: true,
	})

	// Per ADR-0013 §4 + §5 / yaad-index: even when gaps remain
	// open, the gap-call-done flag suppresses the needs_fill payload
	// for the rest of the current fetch-cycle. The AI couldn't satisfy
	// the gap-set with the prior `clean_content`; re-prompting against
	// the same content is wasteful. The agent receives the entity in
	// its current (partially-filled) shape under the `complete` state
	// instead — same envelope as a fully-filled entity. The only way
	// to re-issue the gap-call is a refetch (force_refetch=true or
	// TTL-driven), which clears the flag.
	//
	// `hit.entity` is hydrated by `tryNotationCacheHit` via
	// `store.GetEntity` (see ingest.go::tryNotationCacheHit) — that
	// query reads the full entity row including `gap_call_done_at`
	// per migration 008's column. Any future change to the cache-hit
	// lookup must keep populating this field; otherwise the
	// suppression silently never fires (per the cold-reviewer's a prior PR review).
	if len(hit.openGaps) == 0 || hit.entity.GapCallDoneAt != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(ingestCompleteResponse{
			OK: true,
			State: "complete",
			Status: "complete",
			Entity: wireEntity,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/ingest cache-hit complete response", "err", err)
		}
		return
	}
	// Per yaad-index #4 / ADR-0013 §1: the canonical-kind registry
	// is the canonical source for AI-prompts. On the cache-hit
	// path:
	//
	//   - Kind present in registry: surface the registry's per-gap
	//     Description as the prompt. Gaps not in the registry's
	//     per-kind Gaps map drop (no plugin-side fallback).
	//   - Kind NOT in registry: return needs_fill with empty gaps
	//     so the agent receives the entity but no fill work to do.
	//     Operator must enable the kind in `canonical_kinds:` to
	//     surface prompts.
	kindCfg, kindInRegistry := canonicalKindReg[wireEntity.Kind]
	gaps := make(map[string]string, len(hit.openGaps))
	if kindInRegistry {
		for _, g := range hit.openGaps {
			spec, hasSpec := kindCfg.Gaps[g]
			if !hasSpec {
				continue
			}
			gaps[g] = spec.Description
		}
	}
	cleanContent := ""
	if hit.vaultEntity != nil {
		cleanContent = hit.vaultEntity.CleanContent
	}
	// #275: honor `?exclude=` for canonical_vocabulary +
	// clean_content so the cache-hit response shape stays
	// symmetric with /v1/needs-fill.
	excluded := parseNeedsFillExclude(r.URL.Query().Get("exclude"))
	if excluded[needsFillFieldCleanContent] {
		cleanContent = ""
	}
	resp := ingestNeedsFillResponse{
		OK:                    true,
		State:                 "needs_fill",
		Status:                "needs_fill",
		Entity:                wireEntity,
		CleanContent:          cleanContent,
		CleanContentTruncated: false,
		Gaps:                  gaps,
		Instruction:           resolveInstruction(wireEntity.Kind, fillInstruction, canonicalKindReg),
	}
	if !excluded[needsFillFieldCanonicalVocabulary] {
		resp.CanonicalVocabulary = config.LegacyRegistryWireShape(canonicalKindReg)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.ErrorContext(r.Context(), "encode /v1/ingest cache-hit needs_fill response", "err", err)
	}
}

// ingestAttemptForPlugin builds an ingest attempt that delegates to a
// plugin's Fetch. plannedEntityID is empty — the canonical id is
// data-derived (ADR-0002 lines 138–145), so the queued response omits
// estimated_entity_id for these attempts. instanceName carries the
// ADR-0028 §3 picked instance (Cut 3); empty means the caller didn't
// route through the per-instance picker and the tracker's
// resolveInstanceName fallback applies.
func ingestAttemptForPlugin(plugin plugins.Plugin, rawURL, instanceName string) ingestAttempt {
	return ingestAttempt{
		plannedEntityID: "",
		simulation: ingestSimulation{
			plugin: plugin,
			rawURL: rawURL,
			instanceName: instanceName,
		},
	}
}

func respondIngestQueued(w http.ResponseWriter, r *http.Request, logger *slog.Logger, entityID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(ingestQueuedResponse{
		OK: true,
		State: "queued",
		Status: "queued",
		EstimatedEntityID: entityID,
	}); err != nil {
		logger.ErrorContext(r.Context(), "encode /v1/ingest queued response", "err", err)
	}
}

func respondIngestComplete(w http.ResponseWriter, r *http.Request, logger *slog.Logger, st store.Store, entityID string) {
	got, err := st.GetEntity(r.Context(), entityID)
	if err != nil {
		logger.ErrorContext(r.Context(), "store.GetEntity for complete ingest", "err", err, "id", entityID)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"failed to load completed entity")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(ingestCompleteResponse{
		OK: true,
		State: "complete",
		Status: "complete",
		Entity: toAPIEntity(got),
	}); err != nil {
		logger.ErrorContext(r.Context(), "encode /v1/ingest complete response", "err", err)
	}
}

func respondIngestNeedsFill(w http.ResponseWriter, r *http.Request, logger *slog.Logger, st store.Store, snap *ingestRecord, fillInstruction string, canonicalKindReg map[string]config.CanonicalKindConfig) {
	got, err := st.GetEntity(r.Context(), snap.entityID)
	if err != nil {
		logger.ErrorContext(r.Context(), "store.GetEntity for needs_fill ingest", "err", err, "id", snap.entityID)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"failed to load partial entity")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	gapsCopy := make(map[string]string, len(snap.gaps))
	for k, v := range snap.gaps {
		gapsCopy[k] = v
	}
	// #275: same `?exclude=` honoring as the cache-hit emit.
	excluded := parseNeedsFillExclude(r.URL.Query().Get("exclude"))
	cleanContent := snap.cleanContent
	if excluded[needsFillFieldCleanContent] {
		cleanContent = ""
	}
	resp := ingestNeedsFillResponse{
		OK:                    true,
		State:                 "needs_fill",
		Status:                "needs_fill",
		Entity:                toAPIEntity(got),
		CleanContent:          cleanContent,
		CleanContentTruncated: snap.cleanContentTruncated,
		Gaps:                  gapsCopy,
		Instruction:           resolveInstruction(got.Kind, fillInstruction, canonicalKindReg),
	}
	if !excluded[needsFillFieldCanonicalVocabulary] {
		resp.CanonicalVocabulary = config.LegacyRegistryWireShape(canonicalKindReg)
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.ErrorContext(r.Context(), "encode /v1/ingest needs_fill response", "err", err)
	}
}

// respondIngestDisambiguation emits the 200 disambiguation shape
// added in ADR-0006. Options are carried inline keyed by the plugin's
// canonical id; no entity is persisted (the caller picks one key and
// re-invokes ingest via the plugin's shorthand-by-id input shape).
func respondIngestDisambiguation(w http.ResponseWriter, r *http.Request, logger *slog.Logger, options map[string]plugins.DisambiguationOption) {
	wire := make(map[string]ingestDisambiguationOption, len(options))
	for id, o := range options {
		wire[id] = ingestDisambiguationOption{
			Label: o.Label,
			Summary: o.Summary,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(ingestDisambiguationResponse{
		OK: true,
		State: "disambiguation",
		Status: "disambiguation",
		Options: wire,
	}); err != nil {
		logger.ErrorContext(r.Context(), "encode /v1/ingest disambiguation response", "err", err)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// errNoFixtureMatch is returned by ingestAttemptForURL when the URL
// matches none of the development-only sentinel substrings. The
// handler maps this to 422 unsupported_url (per ADR-0002 / ADR-0006).
// A typed sentinel — not a generic error — keeps the 422 path from
// being conflated with future "fixture matched but the entity-build
// failed" errors that would still warrant 400 invalid_argument.
var errNoFixtureMatch = errors.New("no plugin or fixture pattern matches URL")

// ingestAttemptForURL maps a request URL to the fixture entity, the
// provenance entry to record this attempt, and the simulation that
// drives the in-flight tracker. Phase B handles three sentinel URL
// substrings; real collector dispatch lands in a later phase. Each
// call returns a freshly-built ProvenanceEntry stamped with `now` so
// re-ingesting the same URL produces distinguishable rows.
//
// Simulation delays are tuned for production semantics:
// - brass-birmingham fixture completes "fast" (50ms) — most callers
// with the default 60s wait_seconds get the inline complete shape.
// - queued-test fixture intentionally exceeds typical wait_seconds
// (60s simulated extraction) so callers exercise the 202 queued
// timeout path.
// - needs-fill-test fixture completes fast (50ms) and transitions
// to needs_fill so callers exercise the inline needs_fill +
// fill_token path.
//
// Tests that need fast feedback pass small wait_seconds and rely on
// the 50ms-fast fixtures completing within their window; the timeout
// test uses a small wait_seconds against the 60s queued-test fixture
// to fire the timeout path quickly.
func ingestAttemptForURL(rawURL string) (ingestAttempt, error) {
	now := clock.Now()
	switch {
	case strings.Contains(rawURL, "brass-birmingham"):
		return ingestAttempt{
			entity: &store.Entity{
				ID: "boardgame:brass-birmingham",
				Kind: "boardgame",
				Data: map[string]any{
					"title": "Brass: Birmingham",
					"year": float64(2018),
				},
			},
			provenance: store.ProvenanceEntry{
				Source: "bgg:14",
				FetchedAt: &now,
				OK: true,
			},
			simulation: ingestSimulation{
				delay: 50 * time.Millisecond,
				transitionTo: ingestStateComplete,
			},
		}, nil
	case strings.Contains(rawURL, "queued-test"):
		return ingestAttempt{
			entity: &store.Entity{
				ID: "boardgame:queued-test-stub",
				Kind: "boardgame",
				Data: map[string]any{
					"title": "Queued Test Stub",
				},
			},
			provenance: store.ProvenanceEntry{
				Source: "ingest:queued-test",
				FetchedAt: &now,
				OK: true,
			},
			simulation: ingestSimulation{
				// Long enough to time out a default 60s wait_seconds —
				// the queued-test fixture exists specifically to exercise
				// that path.
				delay: 60 * time.Second,
				transitionTo: ingestStateComplete,
			},
		}, nil
	case strings.Contains(rawURL, "needs-fill-test"):
		return ingestAttempt{
			entity: &store.Entity{
				ID: stubNeedsFillEntityID,
				Kind: "boardgame",
				Data: map[string]any{
					"title": "Stub Game (partial)",
				},
			},
			provenance: store.ProvenanceEntry{
				Source: "bgg:stub",
				FetchedAt: &now,
				OK: true,
			},
			simulation: ingestSimulation{
				delay: 50 * time.Millisecond,
				transitionTo: ingestStateNeedsFill,
				cleanContent: "<stub-cleaned content>",
				cleanContentTruncated: false,
				// Map keys are the data fields the agent must fill;
				// values are short descriptions guiding the AI per
				// ADR-0002 universal-state amendment.
				gaps: map[string]string{
					"summary": "one paragraph summary of the game",
					"tags": "topic tags relevant to this entry",
					"complexity_assessment": "qualitative complexity ranking from the description",
				},
			},
		}, nil
	default:
		return ingestAttempt{}, errNoFixtureMatch
	}
}
