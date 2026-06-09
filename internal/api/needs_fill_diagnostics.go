package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// NeedsFillDiagnosis classifies every entity by whether its open gaps
// surface on GET /v1/needs-fill, split by caller audience. It exists to
// pin the cause of "entities with open gaps that never appear in the
// queue" (#523).
//
// The in-flight `gap_call_done_at` flag is NOT the cause: nothing in
// production sets it (MarkGapCallDone is test-only), so it is always
// NULL and never removes an entity from the queue. The real gate is the
// per-gap filter buildNeedsFillEntry applies — deferral + declared-shape
// + the fill_strategy audience filter (operator-strategy gaps are hidden
// from agent callers, and vice versa). This diagnostic surfaces which of
// those is dropping the open-gap entities.
type NeedsFillDiagnosis struct {
	// TotalEntities scanned (DB rows enumerated).
	TotalEntities int `json:"total_entities"`
	// VaultMissing: a DB row with no vault file (pure-pointer canonical
	// label per ADR-0021) — expected, never surfaces.
	VaultMissing int `json:"vault_missing"`
	// NoOpenGaps: the vault file has an empty `gaps:` list.
	NoOpenGaps int `json:"no_open_gaps"`
	// AgentCallable: surfaces to an agent caller — i.e. it is (or should
	// be) in the live agent queue.
	AgentCallable int `json:"agent_callable"`
	// OperatorOnly: surfaces ONLY to an operator caller — hidden from
	// agents because every open, non-deferred, shaped gap is
	// fill_strategy=operator. The likely #523 culprit.
	OperatorOnly int `json:"operator_only"`
	// HiddenFromBoth: has open gaps but none surface to either audience
	// (all deferred, or no declared shape anywhere).
	HiddenFromBoth int `json:"hidden_from_both"`

	OperatorOnlySample   []string `json:"operator_only_sample,omitempty"`
	HiddenFromBothSample []string `json:"hidden_from_both_sample,omitempty"`
}

const needsFillDiagnosisSampleCap = 25

// DiagnoseNeedsFill classifies entities by whether their open gaps
// surface to an agent and/or operator needs-fill caller. Pure: it only
// reads the vault + the canonical-kind registry, exactly as
// buildNeedsFillEntry does in the live handler.
func DiagnoseNeedsFill(reg map[string]config.CanonicalKindConfig, vaultReader *vault.Reader, entities []store.Entity) NeedsFillDiagnosis {
	var d NeedsFillDiagnosis
	d.TotalEntities = len(entities)
	for _, e := range entities {
		ve, err := vaultReader.ReadByID(e.Kind, e.ID)
		if err != nil {
			if vault.IsNotExist(err) {
				d.VaultMissing++
			}
			continue
		}
		if len(ve.Gaps) == 0 {
			d.NoOpenGaps++
			continue
		}
		_, agentOK := buildNeedsFillEntry(e.ID, e.Kind, ve, "", reg, false)
		_, operatorOK := buildNeedsFillEntry(e.ID, e.Kind, ve, "", reg, true)
		switch {
		case agentOK:
			d.AgentCallable++
		case operatorOK:
			d.OperatorOnly++
			if len(d.OperatorOnlySample) < needsFillDiagnosisSampleCap {
				d.OperatorOnlySample = append(d.OperatorOnlySample, e.ID)
			}
		default:
			d.HiddenFromBoth++
			if len(d.HiddenFromBothSample) < needsFillDiagnosisSampleCap {
				d.HiddenFromBothSample = append(d.HiddenFromBothSample, e.ID)
			}
		}
	}
	return d
}

// handleNeedsFillDiagnostics implements GET /v1/needs-fill/diagnostics
// (#523): a read-only classification of every entity's needs-fill
// surfaceability, so an operator can pin why open-gap entities aren't
// queueing. Enumerates the same candidate set the needs-fill handler
// walks (gap_call_done_at IS NULL — all entities in practice) and
// classifies each.
func handleNeedsFillDiagnostics(
	logger *slog.Logger,
	st store.Store,
	vaultReader *vault.Reader,
	canonicalKindReg map[string]config.CanonicalKindConfig,
) http.HandlerFunc {
	const batch = 500
	return func(w http.ResponseWriter, r *http.Request) {
		if vaultReader == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"needs-fill diagnostics require vault.path configuration")
			return
		}
		kindFilter := r.URL.Query().Get("kind")

		var entities []store.Entity
		after := ""
		for {
			page, err := st.ListGapCallableCandidates(r.Context(), after, batch, kindFilter)
			if err != nil {
				logger.ErrorContext(r.Context(), "needs-fill diagnostics: list candidates", "err", err)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to enumerate entities")
				return
			}
			entities = append(entities, page...)
			if len(page) < batch {
				break
			}
			after = page[len(page)-1].ID
		}

		d := DiagnoseNeedsFill(canonicalKindReg, vaultReader, entities)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(d); err != nil {
			logger.ErrorContext(r.Context(), "needs-fill diagnostics: encode", "err", err)
		}
	}
}
