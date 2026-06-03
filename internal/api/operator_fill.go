package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// stubFillProvenanceSource is the fill-provenance Source for a fill
// with no caller claim (auth-disabled dev-mode path) — matches the
// `agent:stub` convention used elsewhere (#358). An authenticated fill
// stamps the real claim subject (e.g. `agent:bob`) instead.
const stubFillProvenanceSource = "agent:stub"

// operatorFillResponse is the 200 envelope on a successful
// operator-fill. Mirrors fillResponse shape so callers can branch
// uniformly on `ok` across the two fill paths.
type operatorFillResponse struct {
	OK bool `json:"ok"`
	Entity entity `json:"entity"`
	Gaps []string `json:"gaps"`
}

// handleEntityOperatorFill implements POST /v1/entities/{id}/operator-fill
// per ADR-0019 step 5.
//
// Body shape: per-field can be one of:
//
// - scalar value (int / bool / string / etc.) → set the field +
// stamp gap_state.source=operator + gap_state.filled_at = now.
// - explicit JSON null → clear: remove from data + remove the
// gap_state entry (back to "untouched").
// - {"defer": true} → mark deferred. Field MUST currently be
// unfilled; a deferred-on-filled combination returns 409
// `deferred_requires_unfilled`.
// - {"defer": false} → un-defer (clears the deferred flag).
//
// Validation runs against the resolved canonical-kind config:
// type, range, max_length, enum values. Fields whose
// fill_strategy is "agent" reject with 400 (operators can't write
// agent-only fields).
//
// Auth: any authenticated, non-anonymous claim per #317. Agent-
// tier tokens (Subject names an agent, Operator may or may not
// be present) are accepted as the right caller for the per-gap
// `fill_strategy` source semantics (ADR-0019): when the gap is
// strategy=operator, the agent surfaces the question to the
// operator OUT-OF-BAND and writes here on the operator's
// confirmed behalf. The per-gap strategy still governs the
// SOURCE of the value; this endpoint is a write-permission
// boundary, not a source-governance gate. Provenance records
// the calling Subject so the audit trail names the agent.
//
// Vault-then-DB ordering per ADR-0008. Auto-commit prefix
// `operator-fill: <id>`.
func handleEntityOperatorFill(
	logger *slog.Logger,
	st store.Store,
	edgeWriter edgewrite.EdgeWriter,
	vaultReader *vault.Reader,
	vaultWriter *vault.Writer,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	writeLocks *writelocks.Manager,
	bus eventbus.Bus,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		id, rerr := resolveEntityID(r.Context(), st, id)
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to resolve entity reference")
			return
		}

		if vaultReader == nil || vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"operator-fill requires vault.path configuration; gap_state lives in vault frontmatter")
			return
		}
		// Per-entity write-lock (yaad-index #23 + ADR-0024).
		release, ok := acquireWriteLock(w, r, writeLocks, id)
		if !ok {
			return
		}
		// #154: release first, then publish — see fill.go for the
		// rationale. Drain defer is declared first; the
		// release defer (declared second) runs first via LIFO.
		var pending eventbus.PendingEvents
		defer pending.Drain(r.Context(), bus)
		defer release()

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			// Unauthenticated dev-mode path keeps the legacy
			// /v1/fill ergonomics — agents iterating against a
			// local daemon without auth wired hit this handler
			// the same way the old /v1/fill accepted them. The
			// strategy gate below treats them as agent-trigger.
			claim = &auth.Claim{}
		}

		var req map[string]json.RawMessage
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON object: %v", err))
			return
		}
		if len(req) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"body must be a non-empty object of field operations")
			return
		}
		// #355 Cut 2 compat: the pre-unification /v1/fill body wrapped
		// fields in a {"fields": {...}} envelope. Detect and unwrap so
		// existing clients (and the broad test corpus) keep working
		// while the unified shape (flat per-field-op map) is preferred.
		// The wrapper is removable once callers migrate.
		if len(req) == 1 {
			if rawInner, ok := req["fields"]; ok {
				var inner map[string]json.RawMessage
				if err := json.Unmarshal(rawInner, &inner); err == nil && len(inner) > 0 {
					req = inner
				}
			}
		}

		// autoMaterialize is set when either the entity row OR the
		// vault file is missing AND the id resolves to a canonical-
		// label kind. The fill then auto-creates whichever piece is
		// missing per ADR-0021's amendment (yaad-index phase D):
		// - vault file at `<vault_root>/ct/<kind>/<slug>.md` (always
		// on the auto-materialize path, since the vault file's
		// absence is what triggers the path).
		// - DB row via UpsertEntity at the end (idempotent: creates
		// missing, updates present).
		var autoMaterialize bool

		got, err := st.GetEntity(r.Context(), id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				logger.ErrorContext(r.Context(), "store.GetEntity from operator-fill", "err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error", "failed to look up entity")
				return
			}
			// No DB row. Auto-materialize ONLY when the id parses as
			// a canonical-label `<canonical_kind>:<slug>` shape. Any
			// other shape (source-namespace prefix, malformed id) →
			// 404 not_found.
			labelKind, _, ok := parseCanonicalLabelID(id, canonicalKindReg)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no entity with id %s", id))
				return
			}
			got = &store.Entity{
				ID: id,
				Kind: labelKind,
				CreatedAt: clock.Now(),
			}
			autoMaterialize = true
		}

		// Resolve the kind config so per-field validation can run
		// against the typed gap shape. Per #353 a missing kindCfg
		// no longer rejects — non-canonical-kind entities (gmail,
		// github, etc.) can have gap_state populated by workflow
		// `add_gap` actions, and the typed gap shape resolves from
		// that gap_state via mergeWorkflowGapSpec / resolveEffectiveGaps.
		// kindCfg falls through as zero value when the kind isn't in
		// the registry; resolveEffectiveGaps below produces the per-
		// field spec map from gap_state alone in that case.
		kindCfg := canonicalKindReg[got.Kind]

		var ve *vault.Entity
		if autoMaterialize {
			ve = canonical.NewCanonicalLabelEntity(got.ID, got.Kind, kindCfg)
		} else {
			ve, err = vaultReader.ReadByID(got.Kind, id)
			if err != nil {
				if !vault.IsNotExist(err) {
					logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from operator-fill", "err", err, "id", id)
					writeError(w, http.StatusInternalServerError, "internal_error", "failed to read vault file")
					return
				}
				// DB row exists (thin label Per the prior design, phase B) but no
				// vault file. Auto-materialize ONLY when the kind is
				// in the canonical-kind registry. Source-shape
				// entities with a missing vault file remain a 404 —
				// the daemon never auto-creates source-shape vault
				// files; that path is plugin-driven.
				if _, ok := canonicalKindReg[got.Kind]; !ok {
					writeError(w, http.StatusNotFound, "not_found",
						fmt.Sprintf("no vault file for id %s (kind=%s)", id, got.Kind))
					return
				}
				ve = canonical.NewCanonicalLabelEntity(got.ID, got.Kind, kindCfg)
				autoMaterialize = true
			}
		}

		// Operator's full canonical_kinds registry — used by the
		// canonical_type fill path's wildcard expansion (`kinds: "*"`).
		// Sorted for deterministic test output + stable error
		// messages.
		operatorAllKinds := make([]string, 0, len(canonicalKindReg))
		for k := range canonicalKindReg {
			operatorAllKinds = append(operatorAllKinds, k)
		}
		sort.Strings(operatorAllKinds)

		// Per #158: effective gap-spec map merges operator-config
		// canonical_kinds with workflow-injected GapStateEntry
		// (per #142). Operator can fill a workflow-injected gap on
		// any entity (subject to fill_strategy audience check); the
		// canonical_type code path needs the merged shape to route
		// correctly.
		effectiveGaps := resolveEffectiveGaps(kindCfg.Gaps, ve.GapState)

		// #355 unified fill: derive trigger-mode from claim and
		// force-overwrite from query param. Trigger-mode replaces the
		// caller-identity gate (was: this URL is operator-fill,
		// therefore caller is operator) with a request-property gate.
		// operator-trigger: an operator-only token (subject == operator)
		// OR a pair-claim token the operator delegated via the agent
		// skill UI (#361 — claim.OperatorDelegated). agent-trigger: any
		// other authenticated claim. The delegation flag is honored only
		// for non-anonymous claims (subject present), so an anonymous /
		// auth-disabled request can't self-promote to operator-trigger.
		triggerMode := "agent"
		if claim.Subject != "" && (claim.Subject == claim.Operator || claim.OperatorDelegated) {
			triggerMode = "operator"
		}
		force := r.URL.Query().Get("force") == "true"
		ops, opErr := parseOperatorFillOps(
			req, effectiveGaps, operatorAllKinds,
			ve.Data, ve.Gaps, triggerMode, force,
		)
		if opErr != nil {
			writeError(w, opErr.status, opErr.code, opErr.message)
			return
		}

		// #276 resolver-plugin gate: when any target kind has a
		// `resolver_plugin:` config set, require the canonical id
		// to already exist in the store. Operator can override
		// per-call by passing `?allow_unresolved=true` — useful
		// for registering homebrew / custom entities that
		// legitimately aren't in the resolver plugin's index.
		// The bypass is recorded in provenance via the commit
		// message suffix below so the audit trail shows the
		// override was intentional.
		allowUnresolved := r.URL.Query().Get("allow_unresolved") == "true"
		// #325: same shared resolver auto-fetch path as agent-fill.
		// When operator passes `allow_unresolved=true`, the
		// auto-fetch is skipped along with the rest of the gate
		// per checkCanonicalTypeResolverPlugins' early return.
		autoFetcher, _ := edgeWriter.(edgewrite.ResolverAutoFetcher)
		if perr := checkCanonicalTypeResolverPlugins(r.Context(), st, autoFetcher, id, canonicalKindReg, ops, allowUnresolved); perr != nil {
			writeError(w, perr.status, perr.code, perr.message)
			return
		}

		// Defer-on-unfilled precondition: a defer op against a
		// currently-filled field rejects with 409 per ADR-0019. We
		// vet here (not in parseOperatorFillOps) because the check
		// reads the entity's current Data shape, which the parser
		// doesn't see.
		if opErr := preDeferCheck(ve, ops); opErr != nil {
			writeError(w, opErr.status, opErr.code, opErr.message)
			return
		}

		// Apply ops to the in-memory vault entity. Mutates ve.Data,
		// ve.Gaps, and ve.GapState. Returns ordered field-name list
		// for the commit message. kindCfg.Gaps is threaded through
		// so opClear can re-insert known-gap fields back into
		// ve.Gaps (a prior PR cold-reviewer carry-over: clearing a previously-set
		// field shouldn't permanently remove it from the open-gap
		// list — the operator should be able to re-fill it).
		fillNow := clock.Now()
		applied := applyOperatorFillOps(ve, ops, fillNow, effectiveGaps)

		commitMsg := operatorFillCommitMessage(ve.ID, applied, claim.Subject)
		if allowUnresolved {
			// Stamp the bypass into the audit trail so a future
			// reader of the vault history sees the override was
			// intentional. #276.
			commitMsg += " (allow_unresolved)"
		}
		commitAuthor := agentAuthorRef(claim.Subject)
		// #358: stamp one fill-provenance entry per call that performs
		// an actual fill (≥1 set op). A FilledAt provenance row on a
		// pure clear/defer call would misrepresent the audit trail, so
		// gate on a set op. Source is the calling agent (claim subject);
		// the dev-mode no-claim path falls back to the documented
		// agent:stub placeholder. Mirrored to the DB after UpsertEntity
		// (below) so the reloaded response + GET surface it.
		var fillEntry *vault.ProvenanceEntry
		for _, op := range ops {
			if op.Kind == opSet {
				src := commitAuthor
				if src == "" {
					src = stubFillProvenanceSource
				}
				fillEntry = &vault.ProvenanceEntry{Source: src, FilledAt: &fillNow, OK: true}
				ve.Provenance = append(ve.Provenance, *fillEntry)
				break
			}
		}
		// ADR-0021 amendment ( phase D): canonical-label
		// auto-materialize lands the vault file at
		// `<vault_root>/ct/<kind>/<slug>.md` rather than the
		// per-kind default. The autoMaterialize flag was set above
		// when either the DB row or the vault file was missing for
		// a canonical-label-shaped id.
		writeErr := error(nil)
		if autoMaterialize {
			writeErr = vaultWriter.WriteCanonicalLabelWithCommit(r.Context(), ve, commitMsg, commitAuthor)
		} else {
			writeErr = vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor)
		}
		if writeErr != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.Write from operator-fill",
				"err", writeErr, "id", id, "auto_materialize", autoMaterialize)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		// Mirror data + gap_state to DB. Auto-materialize path
		// ( phase D) creates the row if missing; the existing
		// path updates Data + GapState in place. UpsertEntity is
		// idempotent on either path.
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: ve.ID,
			Kind: ve.Kind,
			Data: vaultEntityDataForDB(ve),
			GapState: vaultGapStateToStore(ve.GapState),
			CreatedAt: got.CreatedAt,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from operator-fill",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror operator-fill to DB")
			return
		}

		// #358: mirror the fill-provenance row to the DB so the reloaded
		// entity (toAPIEntity(fresh) below) and GET /v1/entities/{id}
		// surface it. AppendProvenance runs after UpsertEntity so the
		// row is guaranteed present.
		if fillEntry != nil {
			if err := st.AppendProvenance(r.Context(), ve.ID, []store.ProvenanceEntry{{
				Source: fillEntry.Source,
				FilledAt: fillEntry.FilledAt,
				OK: fillEntry.OK,
			}}); err != nil {
				logger.ErrorContext(r.Context(), "store.AppendProvenance from operator-fill",
					"err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to mirror fill provenance to DB")
				return
			}
		}

		// Canonical_type edge create/replace per yaad-index.
		// Walks `ops` (not `applied` — applied is just field
		// names) and for each canonical_type set, deletes prior
		// edges of type=field originating from this source, then
		// creates new edges to each label. The label endpoints
		// are auto-materialized as thin entity rows (mirrors the
		// ingest-time path from phase B) so the FK on edges
		// is satisfied.
		if err := applyCanonicalTypeEdges(r.Context(), st, edgeWriter, ve.ID, ops, effectiveGaps, logger, bus, eventbus.SourceOperator, &pending); err != nil {
			logger.ErrorContext(r.Context(), "operator-fill canonical_type edge create/replace",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to materialize canonical_type edges")
			return
		}
		// Dataview-paragraph append per yaad-index #119 — same
		// shape as the agent-fill path. Operator-authored data
		// lands on the target canonical entity; auto-materialize
		// covers a target that has only a thin DB row.
		dataviewDeps := canonical.DataviewAppendDeps{
			Store:       st,
			VaultReader: vaultReader,
			VaultWriter: vaultWriter,
			WriteLocks:  writeLocks,
			KindReg:     canonicalKindReg,
			Bus:         bus,
			Logger:      logger,
		}
		if err := appendDataviewParagraphs(r.Context(), dataviewDeps, ops, eventbus.SourceOperator, "", &pending); err != nil {
			logger.ErrorContext(r.Context(), "operator-fill canonical_type dataview-append",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to append dataview paragraphs")
			return
		}

		// Publish fill.completed per ADR-0024 Phase 2 — one event per
		// `set` op landed. Clear / defer ops aren't fills (they
		// remove or postpone, not write), so they're filtered. The
		// SourceTag is SourceOperator: this is the operator-strategy
		// endpoint, distinguished from the sibling agent-strategy
		// /fill endpoint which emits SourceAgent.
		now := clock.Now().UTC()
		setOps := make([]string, 0, len(ops))
		for _, op := range ops {
			if op.Kind == opSet {
				setOps = append(setOps, op.Field)
			}
		}
		sort.Strings(setOps)
		opFillChain := eventbus.WorkflowChainFromContext(r.Context())
		for _, gap := range setOps {
			// #154: queue for publish-after-unlock.
			eventbus.QueueOrPublish(r.Context(), bus, &pending, eventbus.FillCompletedEvent{
				EntityID:  ve.ID,
				Gap:       gap,
				SourceTag: eventbus.SourceOperator,
				At:        now,
				Chain:     opFillChain,
			})
		}

		fresh, err := st.GetEntity(r.Context(), id)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity post-operator-fill reread",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to reload merged entity")
			return
		}

		remainingGaps := ve.Gaps
		if remainingGaps == nil {
			remainingGaps = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(operatorFillResponse{
			OK: true,
			Entity: toAPIEntity(fresh),
			Gaps: remainingGaps,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities/{id}/operator-fill response",
				"err", err, "id", id)
		}
	}
}

// handleOperatorFillGone implements the ADR-0029 §5 410-gone responder
// for POST /v1/entities/{id}/operator-fill. The unified endpoint
// (/v1/entities/{id}/fill) absorbs the operator-fill body shape
// verbatim, so clients replay the same payload against the new URL.
//
// 410 is chosen over 308 redirect: the request shape is identical, so
// a redirect would add a round-trip without semantic benefit, and 410
// carries a clearer "migrate" signal for downstream agents reading
// status codes deterministically.
func handleOperatorFillGone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Location", "/v1/entities/"+id+"/fill")
	writeError(w, http.StatusGone, "operator_fill_removed",
		"POST /v1/entities/{id}/operator-fill is removed per ADR-0029; replay the same body against POST /v1/entities/{id}/fill")
}

// operatorFillOpKind is the discriminant for parsed body entries.
type operatorFillOpKind int

const (
	opSet operatorFillOpKind = iota
	opClear
	opDefer
	opUndefer
)

// operatorFillOp is one validated body entry. Field is the gap
// name; Kind selects which mutation runs; Value carries the scalar
// for opSet (nil otherwise).
type operatorFillOp struct {
	Field string
	Kind operatorFillOpKind
	Value any
}

type opError struct {
	status int
	code string
	message string
}

// parseOperatorFillOps validates the request body against the
// resolved kind config and returns ordered ops. Every error path
// emits a single opError — per-call atomic, all-or-nothing per
// ADR-0008's existing fill semantics.
//
// operatorAllKinds is the operator's full `canonical_kinds`
// registry (sorted, for deterministic test output). Threaded
// through to parseSingleOp so canonical_type gaps with `kinds:
// "*"` resolve their wildcard against the registry per ADR-0008.
// parseOperatorFillOps routes each request field through one of three
// branches per ADR-0029 §2:
//
//   - Open gap — field appears in currentGaps. Strategy gate fires
//     per the gap's fill_strategy + the request's triggerMode;
//     parseSingleOp validates against the gap's typed spec.
//   - Overwrite — field has a value in currentData but is not in
//     currentGaps. Requires force=true; else 409 already_filled. When
//     the field also appears in gaps (closed gap with retained spec),
//     parseSingleOp validates against that spec; otherwise the
//     overwrite parses with an empty spec (no type check).
//   - Ad-hoc — field has no value AND no gap entry. Requires
//     triggerMode=="operator"; else 400 unknown_field. Parses with an
//     empty spec.
//
// triggerMode is "operator" when the caller's claim subject equals the
// operator claim, "agent" otherwise. force enables overwrite per the
// request's `?force=true` query param.
func parseOperatorFillOps(
	req map[string]json.RawMessage,
	gaps map[string]config.GapSpec,
	operatorAllKinds []string,
	currentData map[string]any,
	currentGaps []string,
	triggerMode string,
	force bool,
) ([]operatorFillOp, *opError) {
	openGap := make(map[string]bool, len(currentGaps))
	for _, g := range currentGaps {
		openGap[g] = true
	}
	out := make([]operatorFillOp, 0, len(req))
	for field, raw := range req {
		spec, hasSpec := gaps[field]
		_, hasValue := currentData[field]
		isOpen := openGap[field]

		switch {
		case isOpen:
			// Open-gap branch: strategy gate per spec + triggerMode.
			if spec.FillStrategy == "agent" && triggerMode != "agent" {
				return nil, &opError{
					status: http.StatusBadRequest,
					code: "agent_only_field",
					message: fmt.Sprintf("field %q has fill_strategy=agent; operator-trigger fill rejected", field),
				}
			}
			if spec.FillStrategy == "operator" && triggerMode != "operator" {
				return nil, &opError{
					status: http.StatusBadRequest,
					code: "operator_only_field",
					message: fmt.Sprintf("field %q has fill_strategy=operator; agent-trigger fill rejected", field),
				}
			}
		case hasValue:
			// Overwrite branch: existing value, not an open gap.
			if !force {
				return nil, &opError{
					status: http.StatusConflict,
					code: "already_filled",
					message: fmt.Sprintf("field %q already has a value; pass ?force=true to overwrite", field),
				}
			}
			// Defer / undefer on a non-open field has no gap_state
			// entry to flip; reject so the request shape stays clean.
			if isDeferEnvelope(raw) {
				return nil, &opError{
					status: http.StatusBadRequest,
					code: "defer_requires_open_gap",
					message: fmt.Sprintf("field %q has no open gap; defer is only valid for open gaps", field),
				}
			}
		default:
			// Ad-hoc branch: no value, no gap. Operator-trigger only.
			if triggerMode != "operator" {
				return nil, &opError{
					status: http.StatusBadRequest,
					code: "unknown_field",
					message: fmt.Sprintf("field %q is not in the entity's effective gap set; ad-hoc writes require operator-trigger", field),
				}
			}
			if isDeferEnvelope(raw) {
				return nil, &opError{
					status: http.StatusBadRequest,
					code: "defer_requires_open_gap",
					message: fmt.Sprintf("field %q has no open gap; defer is only valid for open gaps", field),
				}
			}
		}

		// Spec resolution: open-gap + spec'd-overwrite use the typed
		// spec for validation; bare overwrite + ad-hoc parse with an
		// empty spec (no type check). hasSpec is true for both open
		// gaps and closed-gap overwrites.
		parseSpec := spec
		if !hasSpec {
			parseSpec = config.GapSpec{}
		}
		op, err := parseSingleOp(field, raw, parseSpec, operatorAllKinds)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	// Stable order for deterministic commit messages and tests.
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out, nil
}

// isDeferEnvelope reports whether the raw body looks like a
// `{"defer": true|false}` envelope. Used to reject defer ops on
// non-open-gap fields before parseSingleOp inspects the body.
func isDeferEnvelope(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var env struct {
		Defer *bool `json:"defer"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	return env.Defer != nil
}

// parseSingleOp resolves one body entry into a discriminated op.
// Validates type / range / max_length / enum-values per spec.
func parseSingleOp(field string, raw json.RawMessage, spec config.GapSpec, operatorAllKinds []string) (operatorFillOp, *opError) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return operatorFillOp{Field: field, Kind: opClear}, nil
	}
	// Defer object: {"defer": true|false}. Recognized for ALL
	// gap types (including canonical_type — operator can defer a
	// canonical_type gap the same way they defer any other typed
	// gap). The defer envelope check fires before the
	// canonical_type list-parse so a `{"defer": true}` body
	// works on canonical_type fields.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var deferEnv struct {
			Defer *bool `json:"defer"`
		}
		if err := json.Unmarshal(raw, &deferEnv); err == nil && deferEnv.Defer != nil {
			if *deferEnv.Defer {
				return operatorFillOp{Field: field, Kind: opDefer}, nil
			}
			return operatorFillOp{Field: field, Kind: opUndefer}, nil
		}
		// Object that isn't a `{"defer": ...}` envelope. For
		// canonical_type gaps, fall through to the list parser —
		// it surfaces a type-mismatch with the right "expected
		// array, got object" message. For every other type, this
		// is the legacy "missing defer key" rejection.
		if spec.Type != config.CanonicalTypeName {
			return operatorFillOp{}, &opError{
				status: http.StatusBadRequest,
				code: "invalid_field_op",
				message: fmt.Sprintf("field %q object form requires a `defer` boolean", field),
			}
		}
	}
	// Canonical_type list (per yaad-index). Validates each
	// element's kind against the gap's resolution set, slugifies
	// {name, kind} entries via slug.Slug, and accepts pre-formed
	// labels as-is (operator-fill path only). Stored as a
	// []string of canonical-label ids; the post-write phase
	// creates edges from the source entity to each label.
	if spec.Type == config.CanonicalTypeName {
		labels, perr := parseCanonicalLabelList(field, raw, spec, operatorAllKinds, true)
		if perr != nil {
			return operatorFillOp{}, perr
		}
		return operatorFillOp{Field: field, Kind: opSet, Value: labels}, nil
	}
	// Scalar set: validate against the gap's typed shape.
	val, err := parseAndValidateScalar(field, raw, spec)
	if err != nil {
		return operatorFillOp{}, err
	}
	return operatorFillOp{Field: field, Kind: opSet, Value: val}, nil
}

// parseAndValidateScalar decodes a scalar JSON value against the
// gap's typed shape and returns the Go value to write. Type
// mismatches (e.g. `"9"` for an int gap) reject with 400.
func parseAndValidateScalar(field string, raw json.RawMessage, spec config.GapSpec) (any, *opError) {
	mismatch := func(expect string, got any) *opError {
		return &opError{
			status: http.StatusBadRequest,
			code: "type_mismatch",
			message: fmt.Sprintf("field %q: expected %s, got %T %v", field, expect, got, got),
		}
	}
	switch spec.Type {
	case "int":
		// Reject strings up front: json.Number's UnmarshalJSON
		// accepts quoted numerics ("9"), which would otherwise let
		// `"9"` slip through type-validation as 9. The wire shape
		// for type=int is bare JSON numbers only.
		trimmedRaw := strings.TrimSpace(string(raw))
		if len(trimmedRaw) > 0 && trimmedRaw[0] == '"' {
			return nil, mismatch("int", trimmedRaw)
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, mismatch("int", string(raw))
		}
		i, err := n.Int64()
		if err != nil {
			return nil, mismatch("int", string(raw))
		}
		if len(spec.Range) == 2 {
			if i < int64(spec.Range[0]) || i > int64(spec.Range[1]) {
				return nil, &opError{
					status: http.StatusBadRequest,
					code: "out_of_range",
					message: fmt.Sprintf("field %q: value %d outside range [%d, %d]", field, i, spec.Range[0], spec.Range[1]),
				}
			}
		}
		return i, nil
	case "bool":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, mismatch("bool", string(raw))
		}
		return b, nil
	case "string", "text":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, mismatch(spec.Type, string(raw))
		}
		if spec.MaxLength > 0 && len(s) > spec.MaxLength {
			return nil, &opError{
				status: http.StatusBadRequest,
				code: "max_length_exceeded",
				message: fmt.Sprintf("field %q: length %d > max_length %d", field, len(s), spec.MaxLength),
			}
		}
		return s, nil
	case "enum":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, mismatch("enum string", string(raw))
		}
		for _, v := range spec.Values {
			if s == v {
				return s, nil
			}
		}
		return nil, &opError{
			status: http.StatusBadRequest,
			code: "enum_value_invalid",
			message: fmt.Sprintf("field %q: value %q not in {%s}", field, s, strings.Join(spec.Values, ", ")),
		}
	default:
		// Unknown / pre-ADR-0019 type — pass the raw JSON value
		// through after a permissive decode. The agent-fill path's
		// applyFieldsToVaultEntity already accepts arbitrary `any`
		// shapes for legacy types.
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, &opError{
				status: http.StatusBadRequest,
				code: "invalid_value",
				message: fmt.Sprintf("field %q: %v", field, err),
			}
		}
		return v, nil
	}
}

// applyOperatorFillOps mutates the vault entity in place. Returns
// the ordered list of field names touched (used for the commit
// message). Caller must run preDeferCheck first — this function
// trusts the ops have already passed defer-on-unfilled validation.
//
// `now` is the timestamp to stamp on FilledAt / DeferredAt. Single
// timestamp across the whole call so all touched fields share the
// same fill / defer moment in the audit trail.
//
// `kindGaps` is the resolved canonical-kind gap registry; opClear
// uses it to re-insert known-gap fields into ve.Gaps when the
// caller clears a previously-set value. Without this, a
// `set→clear` sequence would permanently remove the field from
// the open-gap list (a prior PR cold-reviewer carry-over: the operator should
// be able to re-fill the field after clearing it).
func applyOperatorFillOps(ve *vault.Entity, ops []operatorFillOp, now time.Time, kindGaps map[string]config.GapSpec) []string {
	if ve.Data == nil {
		ve.Data = map[string]any{}
	}
	if ve.GapState == nil {
		ve.GapState = map[string]vault.GapStateEntry{}
	}
	gapPresent := make(map[string]bool, len(ve.Gaps))
	for _, g := range ve.Gaps {
		gapPresent[g] = true
	}
	touched := make([]string, 0, len(ops))
	for _, op := range ops {
		switch op.Kind {
		case opSet:
			// canonical_type ops carry a []canonicalLabelEntry —
			// frontmatter records the ID list only; per-entry
			// `data` flows through op.Value to
			// applyCanonicalTypeEdges for dataview-paragraph
			// recording on the target canonical entity per
			// yaad-index #119. Scalar ops keep their natural
			// Go shape.
			// #359: `summary` and `tags` are reserved vault frontmatter
			// fields that live at the top level of the entity, not in
			// `data:`. Route them to the struct fields so the markdown
			// renders them as native frontmatter; vaultEntityDataForDB
			// projects them back into the API entity.data shape on read.
			switch op.Field {
			case "summary":
				ve.Summary, _ = op.Value.(string)
			case "tags":
				ve.Tags = tagsValueToStrings(op.Value)
			default:
				if ids := canonicalLabelEntryIDs(op.Value); ids != nil {
					ve.Data[op.Field] = ids
				} else {
					ve.Data[op.Field] = op.Value
				}
			}
			ve.GapState[op.Field] = vault.GapStateEntry{
				Source: "operator",
				FilledAt: &now,
			}
			// Field is no longer a gap — remove from the open list.
			if gapPresent[op.Field] {
				ve.Gaps = removeStringFromSlice(ve.Gaps, op.Field)
				delete(gapPresent, op.Field)
			}
		case opClear:
			// #359: clearing a reserved top-level field zeroes the
			// struct field, not a `data:` entry.
			switch op.Field {
			case "summary":
				ve.Summary = ""
			case "tags":
				ve.Tags = nil
			default:
				delete(ve.Data, op.Field)
			}
			delete(ve.GapState, op.Field)
			// Re-insert into ve.Gaps if the field is a known gap in
			// the resolved kind config and isn't already present.
			// Append at end (deterministic order); a future
			// canonical-ordering refactor can reshuffle if needed.
			if _, isKnownGap := kindGaps[op.Field]; isKnownGap && !gapPresent[op.Field] {
				ve.Gaps = append(ve.Gaps, op.Field)
				gapPresent[op.Field] = true
			}
		case opDefer:
			ve.GapState[op.Field] = vault.GapStateEntry{
				Deferred: true,
				DeferredAt: &now,
			}
		case opUndefer:
			// Drop the deferred state; if nothing else (no Source),
			// remove the entry entirely so reads see "untouched".
			delete(ve.GapState, op.Field)
		}
		touched = append(touched, op.Field)
	}
	return touched
}

// preDeferCheck vets defer ops against the entity's current data
// shape. ADR-0019: a deferred field MUST be unfilled. A defer-on-
// filled combination (Data has a value AND op is Defer) returns
// 409 deferred_requires_unfilled.
func preDeferCheck(ve *vault.Entity, ops []operatorFillOp) *opError {
	for _, op := range ops {
		if op.Kind != opDefer {
			continue
		}
		if _, has := ve.Data[op.Field]; has {
			return &opError{
				status: http.StatusConflict,
				code: "deferred_requires_unfilled",
				message: fmt.Sprintf("field %q is filled; cannot mark deferred. clear (set to null) first", op.Field),
			}
		}
	}
	return nil
}

// removeStringFromSlice returns a copy of in without the first
// occurrence of s. Stable order otherwise.
func removeStringFromSlice(in []string, s string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == s {
			continue
		}
		out = append(out, v)
	}
	return out
}

// tagsValueToStrings coerces a fill value for the reserved `tags` field
// (#359) into the []string shape vault frontmatter expects. JSON arrays
// decode to []any on the no-spec default path, so unwrap element-wise; a
// bare []string passes through; anything else yields nil.
func tagsValueToStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// vaultGapStateToStore converts the vault-side gap_state map to
// the store-side equivalent. Field-for-field copy across the two
// layered types (vault doesn't import store, store doesn't import
// vault — bridge lives at the api layer per the existing pattern).
func vaultGapStateToStore(in map[string]vault.GapStateEntry) map[string]store.GapStateEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]store.GapStateEntry, len(in))
	for k, e := range in {
		out[k] = store.GapStateEntry{
			Source: e.Source,
			FilledAt: e.FilledAt,
			Deferred: e.Deferred,
			DeferredAt: e.DeferredAt,
			// Workflow-injected GapSpec metadata (#142) that
			// /v1/needs-fill surfaces to the fill-prompt builder.
			// Must carry through every vault→store mirror or a
			// note/fill write would strip the gap's shape from the DB
			// (the #390 review finding — this is the single shared
			// translator, so the fix covers all callers).
			DataSchema: e.DataSchema,
			Type: e.Type,
			Description: e.Description,
			FillStrategy: e.FillStrategy,
			Range: e.Range,
			MaxLength: e.MaxLength,
			Values: e.Values,
			Kinds: e.Kinds,
		}
	}
	return out
}

// parseCanonicalLabelID parses `<kind>:<slug>` into its parts AND
// validates that the kind is in the operator's canonical-kind
// registry. Used by the operator-fill auto-materialize path
// ( phase D): when the DB row OR vault file is missing for a
// canonical-label entity, the daemon synthesizes the missing
// pieces only when the id confirms canonical-kind shape; any
// other id shape (source-namespace prefix, malformed id, kind not
// in registry) keeps the existing 404.
func parseCanonicalLabelID(id string, reg map[string]config.CanonicalKindConfig) (kind, slug string, ok bool) {
	idx := strings.IndexByte(id, ':')
	if idx <= 0 || idx == len(id)-1 {
		return "", "", false
	}
	kind, slug = id[:idx], id[idx+1:]
	if _, found := reg[kind]; !found {
		return "", "", false
	}
	return kind, slug, true
}

// operatorFillCommitMessage produces the audit line for an
// operator-fill write per ADR-0019. Touched lists the affected
// field names (sorted) so the same op set produces the same
// commit message regardless of iteration order.
//
// Templates:
//
//	with author: "operator-fill: <id> [field1, field2, ...] by <author>"
//	no author: "operator-fill: <id> [field1, field2, ...]"
func operatorFillCommitMessage(entityID string, fields []string, author string) string {
	body := strings.Join(fields, ", ")
	if author == "" {
		return fmt.Sprintf("operator-fill: %s [%s]", entityID, body)
	}
	return fmt.Sprintf("operator-fill: %s [%s] by %s", entityID, body, author)
}
