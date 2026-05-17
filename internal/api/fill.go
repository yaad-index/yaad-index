package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// stubFillProvenanceSource matches edges.go's `agent:stub` convention —
// the same placeholder identity for any agent-authored write while authn
// is not yet wired. Real provenance will carry the authenticated agent
// id (e.g. `agent:bob`, `agent:carol`); the swap-in is one symbol.
const stubFillProvenanceSource = "agent:stub"

// fillRequest is the POST /v1/entities/{id}/fill body. Per ADR-0008
// (a prior PR) the request no longer carries a fill_token — the entity
// ID in the URL path IS the durable callback handle. The agent
// supplies one or more gap-field values; partial fills are permitted,
// and remaining gaps stay open for a future call.
//
// The request body's wire shape is `{"fields": {<name>: <value>}}`.
// handleFill decodes via an inline `map[string]json.RawMessage` so
// canonical_type fields per yaad-index can be re-decoded
// against the shared parseCanonicalLabelList helper while other
// fields land their natural Go shape via applyFieldsToVaultEntity.

// fillResponse is the 200 envelope on a successful fill. `gaps`
// surfaces the remaining unfilled gap field names from the vault
// frontmatter post-write — empty list when this call closed every
// open gap, otherwise the agent can chain another partial fill
// without first re-fetching via GET /v1/entities/{id}. Per
// ADR-0008's multi-call-fill model.
//
// Always non-nil — JSON-encodes as `[]` rather than `null` so
// agents have a stable schema (no presence vs. emptiness ambiguity).
type fillResponse struct {
	OK bool `json:"ok"`
	Entity entity `json:"entity"`
	Gaps []string `json:"gaps"`
}

// fillConflictResponse is the 409 envelope emitted when one or more
// submitted field names are not in the entity's current gap set
// (already filled, never were a gap, etc.). Per ADR-0008's "Callback
// ID = entity ID" subsection: per-call atomic — one rejected field
// fails the whole call; no partial success. The `rejected` array
// names the offending fields so the agent can decide whether to
// re-fetch the current entity and overwrite (a future endpoint) or
// accept that its fill was redundant.
type fillConflictResponse struct {
	OK bool `json:"ok"`
	Error string `json:"error"`
	Message string `json:"message"`
	Rejected []string `json:"rejected"`
}

// handleFill implements POST /v1/entities/{id}/fill.
//
// Vault is authoritative for gap state per ADR-0008: the handler
// reads the entity's current `gaps:` list from the vault file,
// validates submitted field names against it, and (on success)
// rewrites the vault file with the filled values + reduced gap set
// before mirroring the change to the DB.
//
// Asymmetry with the ingest path (intentional, do not "fix"):
// a prior PR made ingest's vault wiring optional — without `vault.path`
// configured, ingest stays DB-only. Fill cannot do that: the gap
// set lives in vault frontmatter, so a fill call against an
// unconfigured vault has no source of truth to validate against.
// This handler returns 503 `vault_required` in that case.
//
// DB-row lookup for the entity Kind is done first (cheap; gives the
// vault file path `<vault>/<kind>/<slug>.md`). The DB row is
// presumed in sync with the vault for the recently-ingested-entity
// case that the agent flow targets. A stale DB → 404 not_found is
// acceptable behavior — operator runs `yaad-index reindex` to
// repair drift.
func handleFill(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, canonicalKindReg map[string]config.CanonicalKindConfig, writeLocks *writelocks.Manager, bus eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Decode once into json.RawMessage values so canonical_type
		// fields per yaad-index can re-decode against the
		// shared parseCanonicalLabelList helper. Other fields decode
		// to their natural Go shape via applyFieldsToVaultEntity.
		var rawReq struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rawReq); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		if len(rawReq.Fields) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"fields is required and must be non-empty")
			return
		}

		id := r.PathValue("id")

		if vaultReader == nil || vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"fill requires vault.path configuration; the gap set lives in vault frontmatter")
			return
		}
		// Per-entity write-lock (yaad-index #23 + ADR-0024).
		release, ok := acquireWriteLock(w, r, writeLocks, id)
		if !ok {
			return
		}
		defer release()

		got, err := st.GetEntity(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no entity with id %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "store.GetEntity from fill", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to look up entity")
			return
		}

		ve, err := vaultReader.ReadByID(got.Kind, id)
		if err != nil {
			if vault.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for id %s (kind=%s)", id, got.Kind))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from fill", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to read vault file")
			return
		}

		// Validation: every submitted field name must appear in the
		// entity's current gap set. Per-call atomic — collect every
		// rejection, then return 409 if any.
		gapSet := make(map[string]struct{}, len(ve.Gaps))
		for _, g := range ve.Gaps {
			gapSet[g] = struct{}{}
		}
		var rejected []string
		for key := range rawReq.Fields {
			if _, ok := gapSet[key]; !ok {
				rejected = append(rejected, key)
			}
		}
		if len(rejected) > 0 {
			sort.Strings(rejected)
			respondFillConflict(w, r, logger, id, rejected)
			return
		}

		// Resolve the entity's typed gap config (when present).
		// Workflow-injected canonical_type gaps live on the
		// entity's kind in the operator's canonical-kind registry.
		// Missing entry → no canonical_type gates fire and the
		// fields fall through to the untyped legacy applyFields
		// path (existing agent-fill behavior preserved for
		// summary / tags / etc. on source-shape entities).
		kindCfg := canonicalKindReg[got.Kind]

		// Operator's full canonical_kinds registry — wildcard
		// expansion source for canonical_type gaps with
		// `kinds: "*"` per ADR-0008.
		operatorAllKinds := make([]string, 0, len(canonicalKindReg))
		for k := range canonicalKindReg {
			operatorAllKinds = append(operatorAllKinds, k)
		}
		sort.Strings(operatorAllKinds)

		// Walk fields once: split into canonical_type ops (which
		// validate via parseCanonicalLabelList — agent path
		// rejects pre-formed labels per spec) and legacy untyped
		// fields. Any parse error short-circuits the whole call;
		// per-call atomic.
		canonicalTypeOps := make([]operatorFillOp, 0)
		legacyFields := make(map[string]any, len(rawReq.Fields))
		for field, raw := range rawReq.Fields {
			spec, hasSpec := kindCfg.Gaps[field]
			if hasSpec && spec.Type == config.CanonicalTypeName {
				labels, perr := parseCanonicalLabelList(field, raw, spec, operatorAllKinds, false)
				if perr != nil {
					writeError(w, perr.status, perr.code, perr.message)
					return
				}
				canonicalTypeOps = append(canonicalTypeOps, operatorFillOp{
					Field: field,
					Kind: opSet,
					Value: labels,
				})
				// Frontmatter stores label IDs only; the per-
				// entry `data` payload flows through Value to
				// applyCanonicalTypeEdges as the dataview-append
				// source per yaad-index #119.
				continue
			}
			// Legacy untyped path — decode the raw JSON into the
			// loose `any` shape applyFieldsToVaultEntity expects.
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_argument",
					fmt.Sprintf("field %q: invalid JSON value: %v", field, err))
				return
			}
			legacyFields[field] = v
		}
		sort.Slice(canonicalTypeOps, func(i, j int) bool {
			return canonicalTypeOps[i].Field < canonicalTypeOps[j].Field
		})

		// Apply both buckets to the vault entity. Canonical_type
		// ops land their []string of canonical-label ids in
		// ve.Data[<field>]; legacy fields land per
		// applyFieldsToVaultEntity (summary / tags / data
		// subkeys).
		for _, op := range canonicalTypeOps {
			if ve.Data == nil {
				ve.Data = make(map[string]any, 1)
			}
			// Frontmatter persists label IDs only; the per-entry
			// `data` payload is recorded as a dataview paragraph
			// on the target canonical entity after the source-
			// side vault write (applyCanonicalTypeEdges).
			ve.Data[op.Field] = canonicalLabelEntryIDs(op.Value)
		}
		applyFieldsToVaultEntity(ve, legacyFields)

		// Drop every applied field from the open-gap list. Both
		// buckets contribute; the resulting list excludes
		// everything the agent just touched.
		applied := make(map[string]any, len(rawReq.Fields))
		for field := range rawReq.Fields {
			applied[field] = struct{}{}
		}
		ve.Gaps = removeKeysFromList(ve.Gaps, applied)

		now := clock.Now()
		fillEntry := vault.ProvenanceEntry{
			Source: stubFillProvenanceSource,
			FilledAt: &now,
			OK: true,
		}
		ve.Provenance = append(ve.Provenance, fillEntry)

		commitMsg := fillCommitMessage(ve.ID, applied)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, stubFillProvenanceSource); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.Write from fill", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		// Mirror to DB. Vault is authoritative — the DB upsert is
		// best-effort with the same merged shape. UpsertEntity carries
		// data; AppendProvenance adds the fill row.
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: ve.ID,
			Kind: ve.Kind,
			Data: vaultEntityDataForDB(ve),
			CreatedAt: got.CreatedAt,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from fill (vault already written)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror fill to DB")
			return
		}
		if err := st.AppendProvenance(r.Context(), ve.ID,
			[]store.ProvenanceEntry{toStoreProvenance(fillEntry)},
		); err != nil {
			logger.ErrorContext(r.Context(), "store.AppendProvenance from fill (vault already written)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror fill provenance to DB")
			return
		}

		// Canonical_type edge create/replace per yaad-index
		// (mirror of the operator-fill path's post-write step).
		// Walks the canonical_type ops collected above; for each,
		// deletes prior edges of type=field originating from this
		// source, ensures thin label rows for new endpoints, then
		// CreateEdge for each.
		if len(canonicalTypeOps) > 0 {
			if err := applyCanonicalTypeEdges(r.Context(), st, ve.ID, canonicalTypeOps, kindCfg.Gaps, logger, bus, eventbus.SourceAgent); err != nil {
				logger.ErrorContext(r.Context(), "fill canonical_type edge create/replace",
					"err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to materialize canonical_type edges")
				return
			}
			// Dataview-paragraph append per yaad-index #119:
			// for each entry with non-empty `data`, record it on
			// the target canonical entity's body. Auto-materializes
			// the target vault file when missing.
			dataviewDeps := dataviewAppendDeps{
				Store:       st,
				VaultReader: vaultReader,
				VaultWriter: vaultWriter,
				WriteLocks:  writeLocks,
				KindReg:     canonicalKindReg,
				Bus:         bus,
				Logger:      logger,
			}
			if err := appendDataviewParagraphs(r.Context(), dataviewDeps, canonicalTypeOps, eventbus.SourceAgent, ""); err != nil {
				logger.ErrorContext(r.Context(), "fill canonical_type dataview-append",
					"err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to append dataview paragraphs")
				return
			}
		}

		// Mark gap-call done for this fetch-cycle (per ADR-0013 §4 +
		// §5 / yaad-index). Reaching this point means we just
		// returned 2xx on the fill — full or partial, both count.
		// The flag suppresses the cache-hit needs_fill payload on
		// subsequent ingests until a refetch clears it. Best-effort
		// on the DB write: a failure here is logged but doesn't fail
		// the user-visible fill response (the vault + DB data
		// already landed; flag-state regen is the safety net).
		if err := st.MarkGapCallDone(r.Context(), ve.ID); err != nil {
			logger.WarnContext(r.Context(), "store.MarkGapCallDone after fill (best-effort)",
				"err", err, "id", id)
		}

		// Publish fill.completed for every gap that landed (per
		// ADR-0024 Phase 2). One event per gap so subscribers can
		// trigger on a specific gap surfacing. SourceAgent is the
		// agent-strategy fill path — operator-strategy is the
		// sibling /operator-fill endpoint and emits
		// SourceOperator.
		fillAt := clock.Now().UTC()
		filledGaps := make([]string, 0, len(rawReq.Fields))
		for k := range rawReq.Fields {
			filledGaps = append(filledGaps, k)
		}
		sort.Strings(filledGaps)
		for _, gap := range filledGaps {
			bus.Publish(r.Context(), eventbus.FillCompletedEvent{
				EntityID:  ve.ID,
				Gap:       gap,
				SourceTag: eventbus.SourceAgent,
				At:        fillAt,
			})
		}

		// Re-read the merged entity so the response includes the
		// freshly-appended provenance row and the canonical data shape.
		fresh, err := st.GetEntity(r.Context(), id)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity post-fill reread", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to reload merged entity")
			return
		}

		// Surface the remaining gap field-name set from the in-memory
		// vault entity post-write . The vault is canonical
		// for gaps; the DB row doesn't track them. Always emit a
		// non-nil slice so the JSON encoding is `[]` not `null` when
		// this call closed every open gap.
		remainingGaps := ve.Gaps
		if remainingGaps == nil {
			remainingGaps = []string{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(fillResponse{
			OK: true,
			Entity: toAPIEntity(fresh),
			Gaps: remainingGaps,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities/{id}/fill response", "err", err)
		}
	}
}

func respondFillConflict(w http.ResponseWriter, r *http.Request, logger *slog.Logger, id string, rejected []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	if err := json.NewEncoder(w).Encode(fillConflictResponse{
		OK: false,
		Error: "conflict",
		Message: fmt.Sprintf("entity %s: %d field(s) not in current gap set; see rejected",
			id, len(rejected)),
		Rejected: rejected,
	}); err != nil {
		logger.ErrorContext(r.Context(), "encode fill conflict response", "err", err)
	}
}

// applyFieldsToVaultEntity merges submitted field values into the
// vault entity. summary + tags are schema-special (top-level
// frontmatter fields); other field names land under data. Mutates
// the receiver in place.
func applyFieldsToVaultEntity(e *vault.Entity, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "summary":
			if s, ok := v.(string); ok {
				e.Summary = s
			} else {
				// Anything non-string for summary is a wire-shape
				// mismatch; coerce via fmt for forwards-compat with
				// agents that send numbers etc. Unlikely path.
				e.Summary = fmt.Sprintf("%v", v)
			}
		case "tags":
			e.Tags = coerceTags(v)
		default:
			if e.Data == nil {
				e.Data = make(map[string]any, 1)
			}
			e.Data[k] = v
		}
	}
}

// coerceTags accepts the JSON-decoded shape of a tags field — most
// commonly []any of strings — and returns []string for vault.Tags.
// Single string inputs land as a one-element slice; nil → nil.
func coerceTags(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			} else {
				out = append(out, fmt.Sprintf("%v", x))
			}
		}
		return out
	case string:
		return []string{t}
	case nil:
		return nil
	default:
		return []string{fmt.Sprintf("%v", v)}
	}
}

// removeKeysFromList returns a copy of in with any element that
// matches a key in the keys map dropped. Order is preserved among
// the kept elements; empty input produces nil (not []string{}).
func removeKeysFromList(in []string, keys map[string]any) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, k := range in {
		if _, drop := keys[k]; drop {
			continue
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// vaultEntityDataForDB projects a vault entity into the data map the
// store sees. Top-level vault fields that the DB tracks (summary,
// tags, notes) are folded into data so that GET /v1/entities/{id}
// returns them via the entity data field — preserving the existing
// wire shape. Future PRs may adjust the API surface to expose these
// as top-level fields too.
//
// `notes_text` is a derived concatenation of every note's Text
// joined by newlines — stored under that key so the DB-side
// LIKE-on-data search (a prior PR's snippet-from-summary contract) can
// find note content. The actual note list (with date + author)
// stays in the vault file's frontmatter, which is the canonical
// source.
func vaultEntityDataForDB(e *vault.Entity) map[string]any {
	out := make(map[string]any, len(e.Data)+3)
	for k, v := range e.Data {
		out[k] = v
	}
	if e.Summary != "" {
		out["summary"] = e.Summary
	}
	if len(e.Tags) > 0 {
		out["tags"] = e.Tags
	}
	if len(e.Notes) > 0 {
		parts := make([]string, 0, len(e.Notes))
		for _, c := range e.Notes {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			out["notes_text"] = strings.Join(parts, "\n")
		}
	}
	return out
}

func toStoreProvenance(p vault.ProvenanceEntry) store.ProvenanceEntry {
	return store.ProvenanceEntry{
		Source: p.Source,
		FetchedAt: p.FetchedAt,
		FilledAt: p.FilledAt,
		OK: p.OK,
		Error: p.Error,
		ErrorMessage: p.ErrorMessage,
	}
}
