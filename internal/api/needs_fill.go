// Pull-based batch gap-call surface (per ADR-0013 §6 / alice2-index).
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

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

const (
	needsFillDefaultLimit = 50
	needsFillMaxLimit = 200
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
	// `type: canonical_type` gaps per alice2-index. Surfaced
	// here (alice2-index) so the agent's UI sees the
	// resolution set at fill-prompt construction time. Wildcard
	// `["*"]` round-trips verbatim; downstream callers expand
	// against the operator's full canonical_kinds registry per
	// ADR-0008.
	Kinds []string `json:"kinds,omitempty"`
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
	CanonicalVocabulary map[string]config.LegacyCanonicalKindConfig `json:"canonical_vocabulary,omitempty"`
}

type needsFillResponse struct {
	OK bool `json:"ok"`
	Entities []needsFillEntry `json:"entities"`
	NextCursor string `json:"next_cursor,omitempty"`
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
			if err := json.NewEncoder(w).Encode(needsFillResponse{
				OK: true,
				Entities: []needsFillEntry{},
			}); err != nil {
				logger.ErrorContext(r.Context(), "encode /v1/needs-fill (no-vault) response", "err", err)
			}
			return
		}

		candidates, err := st.ListGapCallableCandidates(r.Context(), afterID, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.ListGapCallableCandidates",
				"err", err, "after_id", afterID, "limit", limit)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to enumerate gap-callable entities")
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

		entries := make([]needsFillEntry, 0, len(candidates))
		// lastConsidered tracks the id of the last DB candidate we
		// looked at — the cursor advances past it regardless of
		// whether vault filtering kept it. This means subsequent
		// pages don't re-consider the same id and pagination always
		// makes forward progress.
		lastConsidered := afterID
		for i := range candidates {
			cand := candidates[i]
			lastConsidered = cand.ID

			ve, err := vaultReader.ReadByID(cand.Kind, cand.ID)
			if err != nil {
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
		}

		// next_cursor logic: SQLite returns at most `limit` rows
		// for the LIMIT clause, so `len(candidates) == limit`
		// means the candidate stream isn't exhausted and we emit
		// a cursor at the last considered id. Fewer rows means
		// the stream is fully drained — omit the cursor
		// (omitempty drops it from the wire) so clients know to
		// stop iterating. Tightened from `>=` to `==` per the cold-reviewer's
		// a prior PR readability note — they're equivalent here, but
		// `==` reads more precisely.
		var nextCursor string
		if len(candidates) == limit {
			nextCursor = encodeNeedsFillCursor(lastConsidered)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(needsFillResponse{
			OK: true,
			Entities: entries,
			NextCursor: nextCursor,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/needs-fill response", "err", err)
		}
	}
}

// buildNeedsFillEntry shapes a single entity's gap-call payload.
// Returns (entry, true) when at least one gap survives the
// audience-aware filter (defer + fill_strategy); returns
// (zero, false) when all gaps are filtered out — caller skips the
// entry entirely so an entity whose only open gap is deferred or
// wrong-audience doesn't surface as an empty-gaps row.
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
// Resolved per-field metadata (Type, FillStrategy, Range, etc.)
// is sourced from the canonical-kind registry and surfaced on the
// new GapMetadata wire field. Operator-set FillStrategy from the
// kind config wins; entity-emitted gaps without a config entry get
// no metadata (the field is omitted from GapMetadata for that key).
func buildNeedsFillEntry(
	id, kind string,
	ve *vault.Entity,
	fillInstruction string,
	reg map[string]config.CanonicalKindConfig,
	isOperator bool,
) (needsFillEntry, bool) {
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
		// Audience filter: skip fields whose fill_strategy doesn't
		// match this caller's audience.
		spec, hasSpec := kindCfg.Gaps[g]
		if hasSpec {
			if isOperator && spec.FillStrategy == "agent" {
				continue
			}
			if !isOperator && spec.FillStrategy == "operator" {
				continue
			}
		}
		// The vault frontmatter carries the gap-name list; the AI
		// prompts originated from the plugin emit and aren't stored
		// in the vault. The cache-hit ingest path emits empty-string
		// values as the documented "prompt unavailable" sentinel —
		// mirror that here so the wire shape stays identical.
		gaps[g] = ""
		// Surface typed metadata when the kind config has it. Empty
		// when the gap has no resolved spec (e.g. plugin-emitted gap
		// with no canonical-kind config entry).
		if hasSpec {
			meta[g] = needsFillGapMeta{
				Type: spec.Type,
				FillStrategy: spec.FillStrategy,
				Range: spec.Range,
				MaxLength: spec.MaxLength,
				Values: spec.Values,
				Kinds: spec.Kinds,
			}
		}
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
		ID: id,
		Kind: kind,
		Gaps: gaps,
		GapMetadata: meta,
		CleanContent: ve.CleanContent,
		CleanContentTruncated: false,
		Instruction: resolveInstruction(kind, fillInstruction, reg),
		CanonicalVocabulary: config.LegacyRegistryWireShape(reg),
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
