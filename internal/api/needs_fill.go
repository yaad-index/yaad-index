// Pull-based batch gap-call surface (per ADR-0013 §6 / yaad-index).
//
// Returns entities that are currently gap-callable — the AI has not yet
// been called for the entity's current fetch-cycle (DB
// `gap_call_done_at IS NULL` per a prior PR) AND the entity carries unfilled
// gaps in vault frontmatter. Each entry on the response carries the
// full needs_fill payload used by the cache-hit ingest envelope:
// resolved `instruction` (per-kind override → global → omit per
// ADR-0013 §2), `canonical_vocabulary` registry verbatim, gap-name
// → AI-prompt map, and the `clean_content` body.
//
// Pagination uses an opaque base64(last-seen-id) cursor over an
// `id ASC` ordering. v1's id-only cursor is sufficient; future
// (created_at, id) compound is deferred per the dispatch.
//
// Use cases: cron-driven batch fills, multi-agent coordinator
// dispatch. Direct AI failure recovery (single-entity case) does NOT
// require this endpoint — a failed fill leaves the flag unset, so a
// direct ingest still returns needs_fill.

package api

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

const (
	needsFillDefaultLimit = 50
	needsFillMaxLimit = 200
	// needsFillCandidateBatch is how many DB rows the handler asks
	// for in one ListGapCallableCandidates call when it's still
	// hunting for fillable entities. Sized larger than typical agent
	// limits so DBs full of non-fillable thin labels don't force an
	// extra DB round-trip per page of vault-filtered emptiness.
	needsFillCandidateBatch = 200
	// needsFillMaxCandidateScan bounds the total DB rows the handler
	// examines per HTTP call. When the agent's `limit` is small and
	// the DB has long runs of non-fillable rows, the handler keeps
	// fetching batches until it fills the page OR scans this many
	// rows. The cap makes each response bounded — a 325-entity DB
	// resolves to one round-trip end-to-end; a 100k DB caps at this
	// many rows per call, with the cursor advancing the rest.
	needsFillMaxCandidateScan = 1000
)

// needsFillGapMeta carries the typed metadata for one gap per
// ADR-0019 step 6 — the typed fill metadata the operator-fill
// surface introduced for +. Each gap in the response
// now surfaces its FillStrategy + Type (and optional shape fields)
// alongside the existing name → prompt map. New field on the
// `gap_metadata` key; legacy callers that only read `gaps` keep
// working unchanged.
type needsFillGapMeta struct {
	Type string `json:"type,omitempty"`
	FillStrategy string `json:"fill_strategy,omitempty"`
	Range []int `json:"range,omitempty"`
	MaxLength int `json:"max_length,omitempty"`
	Values []string `json:"values,omitempty"`
	// Kinds carries the canonical-kind allowlist for
	// `type: canonical_type` gaps per yaad-index. Surfaced
	// here (yaad-index) so the agent's UI sees the
	// resolution set at fill-prompt construction time. Wildcard
	// `["*"]` round-trips verbatim; downstream callers expand
	// against the operator's full canonical_kinds registry per
	// ADR-0008.
	Kinds []string `json:"kinds,omitempty"`
	// DataSchema is the optional per-key extraction guidance a
	// workflow `add_gap` action injected for canonical_type
	// gaps carrying per-entry `data` (#117). Key = data-field
	// name; value = natural-language extraction instruction.
	// Surfaced so the agent's fill-prompt builder can include
	// the per-key guidance inline. Empty / nil drops via
	// `omitempty` for gaps without workflow-injected schema.
	DataSchema map[string]string `json:"data_schema,omitempty"`
}

// needsFillEntry is one entry on the `entities` array of the
// `GET /v1/needs-fill` response. Mirrors the cache-hit needs_fill
// envelope's per-entity shape verbatim — instruction resolution,
// canonical_vocabulary, gaps map, clean_content all match
// `ingestNeedsFillResponse`. Wire-shape parity is asserted in
// tests so a future refactor that drifts one shape breaks the
// other's regression suite immediately.
type needsFillEntry struct {
	ID string `json:"id"`
	Kind string `json:"kind"`
	Gaps map[string]string `json:"gaps"`
	// GapMetadata is the ADR-0019 step 6 typed-metadata sibling to
	// Gaps. Same key set; values carry FillStrategy + Type + the
	// optional range/max_length/values shape. Empty / nil omits
	// the wire field via omitempty so legacy deployments keep
	// the leaner response shape on entities with no typed gaps.
	GapMetadata map[string]needsFillGapMeta `json:"gap_metadata,omitempty"`
	// clean_content + clean_content_truncated are always present on
	// the wire — same as `ingestNeedsFillResponse` — so a single
	// agent-side decoder can read both surfaces with one shape. Alice2's
	// a prior PR review caught the omitempty + missing-truncated drift;
	// fold-in restores parity. clean_content_truncated is hardcoded
	// to false here exactly as `respondFromCacheHit` does — the
	// cache-hit path doesn't have plugin-side truncation context to
	// surface, and neither do we (the same vault file feeds both).
	CleanContent string `json:"clean_content"`
	CleanContentTruncated bool `json:"clean_content_truncated"`
	Instruction string `json:"instruction,omitempty"`
	// CanonicalVocabulary moved to the response-root per #275; the
	// per-entity field used to repeat the full operator config on
	// every entry, blowing agent-context windows when the kind set
	// grew past a handful. omitempty keeps the wire surface clean
	// for the now-default empty case.
}

type needsFillResponse struct {
	OK bool `json:"ok"`
	// Total reflects the DB-side gap-callable queue depth
	// (entities with gap_call_done_at IS NULL) per #338. It
	// over-estimates the final-page entries by the count of
	// pure-pointer canonical rows + entities whose vault gaps
	// were entirely auth-filtered. Useful as a queue-depth
	// anchor for agents pacing the fill loop without needing
	// to paginate to exhaustion.
	Total      int              `json:"total"`
	Entities   []needsFillEntry `json:"entities"`
	NextCursor string           `json:"next_cursor,omitempty"`
	// CanonicalVocabulary lives at response-root per #275: one
	// copy per response rather than one per entry. Omitted when
	// the operator's request strips it via `?exclude=canonical_vocabulary`.
	CanonicalVocabulary map[string]config.LegacyCanonicalKindConfig `json:"canonical_vocabulary,omitempty"`
}

func handleNeedsFill(
	logger *slog.Logger,
	st store.Store,
	vaultReader *vault.Reader,
	fillInstruction string,
	canonicalKindReg map[string]config.CanonicalKindConfig,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseNeedsFillLimit(r.URL.Query().Get("limit"))
		afterID, ok := decodeNeedsFillCursor(r.URL.Query().Get("cursor"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"cursor: malformed or invalid base64 encoding")
			return
		}

		// Vault is the canonical source for `Gaps`-non-empty per
		// ADR-0008; without a vault reader we can't filter
		// candidates and have nothing meaningful to surface.
		// Short-circuit before the DB query so a DB-only deploy
		// returns an honest empty result on every call instead of
		// looping through pages of empty bodies with non-empty
		// next_cursor (the cold-reviewer's a prior PR catch on the prior shape that
		// advanced lastConsidered without emitting entries).
		if vaultReader == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// No vault = no entries; canonical_vocabulary still
			// surfaces at top level so callers can read the
			// operator config off this endpoint when /v1/structure
			// isn't wired. Honors `?exclude=` the same way the
			// happy path does.
			noVaultExcluded := parseNeedsFillExclude(r.URL.Query().Get("exclude"))
			// total stays 0 on the no-vault path — gap-callable
			// rows in the DB are meaningless without a vault to
			// resolve their gaps.
			noVaultResp := needsFillResponse{
				OK:       true,
				Total:    0,
				Entities: []needsFillEntry{},
			}
			if !noVaultExcluded[needsFillFieldCanonicalVocabulary] {
				noVaultResp.CanonicalVocabulary = config.LegacyRegistryWireShape(canonicalKindReg)
			}
			if err := json.NewEncoder(w).Encode(noVaultResp); err != nil {
				logger.ErrorContext(r.Context(), "encode /v1/needs-fill (no-vault) response", "err", err)
			}
			return
		}

		// ADR-0019 step 6: auth-aware filtering. Operator caller
		// (Subject == Operator) skips agent-only fields; agent caller
		// (Subject != Operator) skips operator-only fields.
		// Anonymous claims are treated as agent (the existing
		// fill-loop ergonomics — operators authenticate explicitly,
		// dev-mode unauthenticated requests are agent-shaped).
		isOperator := false
		if claim, ok := ClaimFromContext(r.Context()); ok && claim != nil &&
			!IsAnonymousClaim(claim) && claim.Subject != "" &&
			claim.Subject == claim.Operator {
			isOperator = true
		}

		// Scan-until-found-or-exhausted: a DB full of source-shape
		// and thin-canonical rows can have long runs where every
		// candidate fails the vault-side gaps filter or the
		// audience-aware buildNeedsFillEntry filter. Pre-#112 each
		// such run cost the client one HTTP call per `limit` rows
		// returning `entities: []` with an advancing cursor. The
		// inner loop now batches at needsFillCandidateBatch and
		// keeps pulling until it fills the agent's `limit`, scans
		// needsFillMaxCandidateScan rows total (the per-request
		// bound), or the DB stream exhausts.
		entries := make([]needsFillEntry, 0, limit)
		lastConsidered := afterID
		scanned := 0
		exhausted := false
		for len(entries) < limit && scanned < needsFillMaxCandidateScan {
			batch := needsFillCandidateBatch
			if remaining := needsFillMaxCandidateScan - scanned; remaining < batch {
				batch = remaining
			}
			candidates, err := st.ListGapCallableCandidates(r.Context(), lastConsidered, batch)
			if err != nil {
				logger.ErrorContext(r.Context(), "store.ListGapCallableCandidates",
					"err", err, "after_id", lastConsidered, "batch", batch)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to enumerate gap-callable entities")
				return
			}
			if len(candidates) == 0 {
				exhausted = true
				break
			}
			earlyBreak := false
			for i := range candidates {
				cand := candidates[i]
				lastConsidered = cand.ID
				scanned++

				ve, err := vaultReader.ReadByID(cand.Kind, cand.ID)
				if err != nil {
					// Pure-pointer canonical-label rows (per ADR-0021)
					// live in the DB with `gap_call_done_at = NULL`
					// but have no vault file — ReadByID returns
					// IsNotExist after probing all three layouts
					// (active, ct/, _archive). This is the expected
					// shape, not a fault; log at debug to keep the
					// scan quiet. Per #156.
					if vault.IsNotExist(err) {
						logger.DebugContext(r.Context(),
							"needs-fill candidate has no vault file (pure-pointer row); skipping",
							"id", cand.ID, "kind", cand.Kind)
						continue
					}
					logger.WarnContext(r.Context(),
						"vault read for needs-fill candidate errored; skipping",
						"id", cand.ID, "err", err)
					continue
				}
				if len(ve.Gaps) == 0 {
					continue
				}
				entry, ok := buildNeedsFillEntry(
					cand.ID, cand.Kind, ve, fillInstruction, canonicalKindReg, isOperator)
				if !ok {
					// All gaps for this entity were filtered out
					// (deferred or wrong fill_strategy for caller).
					continue
				}
				entries = append(entries, entry)
				if len(entries) >= limit {
					earlyBreak = true
					break
				}
			}
			// Exhaustion only when we walked every candidate in the
			// batch (no early break) AND the batch came back short
			// (fewer rows than requested = SQLite drained the
			// `id > lastConsidered` range). An early break leaves
			// unread rows past lastConsidered in this batch, so the
			// cursor still needs to advance for the client.
			if !earlyBreak && len(candidates) < batch {
				exhausted = true
				break
			}
		}

		// next_cursor: omit when the DB stream exhausted (clients
		// know to stop). Emit at lastConsidered otherwise — even
		// when the page is full of vault-filtered emptiness, the
		// cursor advances so the next call resumes past the rows
		// we already considered.
		var nextCursor string
		if !exhausted {
			nextCursor = encodeNeedsFillCursor(lastConsidered)
		}

		// #275: build the top-level response with canonical_vocabulary
		// included by default unless the caller passed
		// `?exclude=canonical_vocabulary`. Same opt-out shape covers
		// per-entry `clean_content` for callers that already cached
		// the body.
		excluded := parseNeedsFillExclude(r.URL.Query().Get("exclude"))
		if excluded[needsFillFieldCleanContent] {
			for i := range entries {
				entries[i].CleanContent = ""
			}
		}
		// #338: queue-depth anchor for the response. DB-side
		// count over the same gap-callable predicate as the
		// listing query; cheap COUNT(*) regardless of cursor
		// position. Logs at WARN on failure and surfaces total=0
		// rather than failing the whole response — the entries
		// payload is the load-bearing surface.
		total, err := st.CountGapCallableCandidates(r.Context())
		if err != nil {
			logger.WarnContext(r.Context(), "store.CountGapCallableCandidates failed; surfacing total=0",
				"err", err)
			total = 0
		}

		resp := needsFillResponse{
			OK:         true,
			Total:      total,
			Entities:   entries,
			NextCursor: nextCursor,
		}
		if !excluded[needsFillFieldCanonicalVocabulary] {
			resp.CanonicalVocabulary = config.LegacyRegistryWireShape(canonicalKindReg)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/needs-fill response", "err", err)
		}
	}
}

// needsFillFieldCanonicalVocabulary and needsFillFieldCleanContent
// are the two field-names `?exclude=` accepts per #275. Centralized
// as constants so the parser + the ingest cache-hit emit point speak
// the same vocabulary.
const (
	needsFillFieldCanonicalVocabulary = "canonical_vocabulary"
	needsFillFieldCleanContent        = "clean_content"
)

// parseNeedsFillExclude decodes the `?exclude=field1,field2` query
// param value into a set of field names the caller wants stripped
// from the response. Unknown / unsupported field names are silently
// ignored (forward-compatible with future fields). Empty input
// returns an empty set — default behavior is to include everything.
//
// Used by `/v1/needs-fill` and `/v1/ingest` (cache-hit needs-fill
// response) so caching agents can opt out of receiving the
// `canonical_vocabulary` registry on every page when they've already
// fetched it via `/v1/structure` or `/v1/kinds`, and / or the
// `clean_content` body when they've cached it via
// `/v1/entities/<id>`.
func parseNeedsFillExclude(raw string) map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = true
	}
	return out
}

// buildNeedsFillEntry shapes a single entity's gap-call payload.
// Returns (entry, true) when at least one gap survives the
// audience-aware filter (defer + fill_strategy + registry
// presence); returns (zero, false) when all gaps are filtered out
// — caller skips the entry entirely so an entity whose only open
// gap is deferred, wrong-audience, or not in the registry doesn't
// surface as an empty-gaps row.
//
// ADR-0019 step 6 audience filtering:
//
// - isOperator=true (operator caller): skip fields whose
// fill_strategy=="agent" (don't waste operator attention on
// fields the agent should derive from clean_content).
// - isOperator=false (agent caller): skip fields whose
// fill_strategy=="operator" (don't have agent waste cycles
// trying to fill operator-only fields like rating/owned).
// - fill_strategy="" or "both": included for both audiences.
//
// Defer filter (both audiences): skip fields with
// ve.GapState[field].Deferred=true. The operator can un-defer via
// operator-fill {"defer": false} to bring them back.
//
// Per yaad-index #4 / ADR-0013 §1: the canonical-kind registry is
// the canonical source for AI-prompts. A gap whose kind isn't in
// the registry OR whose name isn't in the registry's per-kind
// Gaps map is dropped (no plugin-side prompt fallback). Operators
// who want to surface fill prompts MUST enable the kind in
// `canonical_kinds:` config and declare the gap there.
func buildNeedsFillEntry(
	id, kind string,
	ve *vault.Entity,
	fillInstruction string,
	reg map[string]config.CanonicalKindConfig,
	isOperator bool,
) (needsFillEntry, bool) {
	// kind-not-in-registry was a strict-mode early-return until
	// #142 added workflow-injected GapStateEntry — a workflow can
	// now inject the full gap spec inline on a source-shape entity
	// whose kind is plugin-emitted but not in the canonical-kind
	// registry (the registry is canonical-LABEL-side per ADR-0016,
	// source kinds aren't carried there). The per-gap loop below
	// already enforces strict-mode per-gap via the
	// `!hasCfgSpec && !workflowGapEntryHasShape(entry)` check, so
	// an entity whose gaps have NO declared shape anywhere still
	// drops via len(gaps)==0; a source-shape entity with at least
	// one workflow-injected gap shape surfaces (per #156). kindCfg
	// is the zero-value `config.CanonicalKindConfig` when the kind
	// isn't in `reg`; Gaps is nil and lookups return zero/false
	// safely.
	kindCfg := reg[kind]
	gaps := make(map[string]string, len(ve.Gaps))
	meta := make(map[string]needsFillGapMeta, len(ve.Gaps))
	for _, g := range ve.Gaps {
		// Defer filter: a deferred gap doesn't surface to either
		// audience. Operator un-defers via operator-fill to bring
		// it back.
		if entry, ok := ve.GapState[g]; ok && entry.Deferred {
			continue
		}
		// Per-gap spec resolution layers per #142:
		//   1. Operator-config canonical_kinds.<kind>.gaps.<g>
		//      (the historic source).
		//   2. Workflow-injected GapStateEntry fields (#142).
		// When both are present, the workflow's inline shape
		// overlays the operator-config shape per-field (any
		// non-empty workflow field wins). When only the
		// workflow has declared the gap, the workflow-injected
		// shape stands alone.
		cfgSpec, hasCfgSpec := kindCfg.Gaps[g]
		entry, hasEntry := ve.GapState[g]
		spec := mergeWorkflowGapSpec(cfgSpec, entry)
		if !hasCfgSpec && !workflowGapEntryHasShape(entry) {
			// Neither operator-config nor workflow declared a
			// shape — strict-mode skip (#4): there's nothing to
			// surface as a prompt.
			continue
		}
		// Audience filter: skip fields whose fill_strategy doesn't
		// match this caller's audience.
		if isOperator && spec.FillStrategy == "agent" {
			continue
		}
		if !isOperator && spec.FillStrategy == "operator" {
			continue
		}
		gaps[g] = spec.Description
		gm := needsFillGapMeta{
			Type: spec.Type,
			FillStrategy: spec.FillStrategy,
			Range: spec.Range,
			MaxLength: spec.MaxLength,
			Values: spec.Values,
			Kinds: spec.Kinds,
		}
		if hasEntry && len(entry.DataSchema) > 0 {
			gm.DataSchema = entry.DataSchema
		}
		meta[g] = gm
	}
	if len(gaps) == 0 {
		return needsFillEntry{}, false
	}
	if len(meta) == 0 {
		meta = nil
	}
	// `reg` is shared by reference here, same as ingest.go's
	// cache-hit emit. The mutation contract on
	// `WithCanonicalKindRegistry` (added in a prior PR: "Caller must
	// not mutate the map after passing it") covers the map-level
	// reference, not just the per-entry struct — operators set
	// canonical_kinds at startup and the handler retains the
	// reference. Defensive-copy is a future option if the contract
	// ever has to relax (e.g., config hot-reload). The cold-reviewer's a prior PR
	// review note pre-emptively confirmed this is non-regression.
	return needsFillEntry{
		ID:                    id,
		Kind:                  kind,
		Gaps:                  gaps,
		GapMetadata:           meta,
		CleanContent:          ve.CleanContent,
		CleanContentTruncated: false,
		Instruction:           resolveInstruction(kind, fillInstruction, reg),
	}, true
}

// parseNeedsFillLimit clamps the query-param limit to [1, max] with
// silent fallback on out-of-range values per the dispatch's
// "lenient on bad values" policy. Bad strings, missing values,
// non-positive integers all default to 50; values > 200 cap at 200.
func parseNeedsFillLimit(raw string) int {
	if raw == "" {
		return needsFillDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return needsFillDefaultLimit
	}
	if n > needsFillMaxLimit {
		return needsFillMaxLimit
	}
	return n
}

// encodeNeedsFillCursor produces the opaque cursor string the wire
// emits. Internally base64(last-seen-id); the outer client never
// inspects the encoding.
func encodeNeedsFillCursor(lastID string) string {
	if lastID == "" {
		return ""
	}
	return base64.URLEncoding.EncodeToString([]byte(lastID))
}

// decodeNeedsFillCursor reverses the encoding. Empty input → empty
// afterID (first page); valid base64 → decoded id; invalid base64
// → reject with the second return false so the handler can emit
// 400 invalid_argument. v1's id-only cursor is sufficient; if the
// future evolves to a (created_at, id) compound, this is the one
// place to extend.
func decodeNeedsFillCursor(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// workflowGapEntryHasShape reports whether the GapStateEntry
// carries enough workflow-injected GapSpec metadata to stand
// alone as a gap spec (without an operator-config registration
// per ADR-0013). At minimum a Type is required; the other
// fields are individually optional. Used by buildNeedsFillEntry
// to decide whether a gap with no canonical_kinds registration
// should still surface on /v1/needs-fill via its workflow-
// injected shape (per #142).
func workflowGapEntryHasShape(entry vault.GapStateEntry) bool {
	return entry.Type != ""
}

// mergeWorkflowGapSpec layers the workflow-injected GapStateEntry
// fields onto the operator-config GapSpec, returning the
// effective spec /v1/needs-fill should surface. Non-empty
// workflow fields win per-field; empty workflow fields fall
// through to the operator-config value. When the operator
// config doesn't carry the gap at all, the workflow-injected
// fields stand alone.
func mergeWorkflowGapSpec(cfg config.GapSpec, entry vault.GapStateEntry) config.GapSpec {
	out := cfg
	if entry.Type != "" {
		out.Type = entry.Type
	}
	if entry.Description != "" {
		out.Description = entry.Description
	}
	if entry.FillStrategy != "" {
		out.FillStrategy = entry.FillStrategy
	}
	if len(entry.Range) > 0 {
		out.Range = append([]int(nil), entry.Range...)
	}
	if entry.MaxLength != 0 {
		out.MaxLength = entry.MaxLength
	}
	if len(entry.Values) > 0 {
		out.Values = append([]string(nil), entry.Values...)
	}
	if len(entry.Kinds) > 0 {
		out.Kinds = append([]string(nil), entry.Kinds...)
	}
	return out
}

// resolveEffectiveGaps builds the per-field gap spec map the
// fill / operator-fill / canonical-edge derivation paths route
// against. Layers per-#142 + #158:
//
//  1. Operator config canonical_kinds.<kind>.gaps.<g> — the
//     historic source.
//  2. Workflow-injected GapStateEntry on the entity — when the
//     workflow's `add_gap` action carries inline Type / Kinds /
//     FillStrategy / Description / etc.
//
// A gap surfaces in the result when EITHER layer carries it.
// When both carry it, mergeWorkflowGapSpec layers workflow non-
// empty fields atop the operator config per-field. Gaps with no
// shape from either layer don't appear — same strict-mode skip
// semantics as buildNeedsFillEntry's per-gap loop.
//
// Used by the write side (#158) so source-shape entities with
// workflow-injected canonical_type gaps route through the
// canonical_type code path (label parse, edge create, dataview
// append) rather than the legacy untyped-data path that just
// stores the JSON verbatim in entity.data.
func resolveEffectiveGaps(
	kindGaps map[string]config.GapSpec,
	gapState map[string]vault.GapStateEntry,
) map[string]config.GapSpec {
	if len(kindGaps) == 0 && len(gapState) == 0 {
		return nil
	}
	out := make(map[string]config.GapSpec, len(kindGaps)+len(gapState))
	for field, spec := range kindGaps {
		entry := gapState[field]
		out[field] = mergeWorkflowGapSpec(spec, entry)
	}
	for field, entry := range gapState {
		if _, alreadyMerged := out[field]; alreadyMerged {
			continue
		}
		if !workflowGapEntryHasShape(entry) {
			continue
		}
		out[field] = mergeWorkflowGapSpec(config.GapSpec{}, entry)
	}
	return out
}
