// Canonical-vocabulary drift status endpoint (per ADR-0013 §3 /
// alice2-index a prior PR). Surfaces:
//
// - `config_hash`: deterministic SHA over canonical_kinds +
// canonical_edge_types (a prior PR's `config.ConfigHash`).
// - `drift.kinds_emitted_not_enabled[]`: per-(plugin, kind)
// counter of canonical entity stubs the plugin emitted but
// the operator's config dropped at the orchestrator filter.
// Sourced from a prior PR's `dropped_canonical_kinds` table.
// - `drift.edge_types_emitted_not_enabled[]`: same axis for
// canonical edge types.
// - `kinds_enabled_not_emitted[]` / `edge_types_enabled_not_emitted[]`:
// stubbed empty for v1 per the issue spec — cleanly
// detecting "operator enabled X but no plugin emits it" is
// a different signal that lands in a follow-up.
// - `last_reindex_at`: MAX(last_indexed_at) across reindex_files;
// null when no reindex has ever run.
// - `reindex_hint`: static guidance string per the spec.
//
// Pairs with `/v1/structure` (a prior PR) on the introspection axis:
// structure says "what's configured / what's loaded?", cv-status
// says "given that, what's drifting?".

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
)

// cvStatusReindexHint is the static guidance string per ADR-0013
// §3. Operators read drift counts → enable kinds in config →
// call POST /v1/reindex to materialize the stubs from the vault
// (force-refetch is too aggressive per the ADR; reindex re-
// derives without re-fetching upstream).
const cvStatusReindexHint = "POST /v1/reindex to materialize stubs after enabling kinds/edges in config"

type cvDriftKindRow struct {
	Plugin string `json:"plugin"`
	Kind string `json:"kind"`
	WouldMaterializeCount int64 `json:"would_materialize_count"`
}

type cvDriftEdgeRow struct {
	Plugin string `json:"plugin"`
	EdgeType string `json:"edge_type"`
	WouldMaterializeCount int64 `json:"would_materialize_count"`
}

type cvDrift struct {
	KindsEmittedNotEnabled []cvDriftKindRow `json:"kinds_emitted_not_enabled"`
	KindsEnabledNotEmitted []cvDriftKindRow `json:"kinds_enabled_not_emitted"`
	EdgeTypesEmittedNotEnabled []cvDriftEdgeRow `json:"edge_types_emitted_not_enabled"`
	EdgeTypesEnabledNotEmitted []cvDriftEdgeRow `json:"edge_types_enabled_not_emitted"`
}

type cvStatusResponse struct {
	OK bool `json:"ok"`
	ConfigHash string `json:"config_hash"`
	Drift cvDrift `json:"drift"`
	LastReindexAt *string `json:"last_reindex_at"`
	ReindexHint string `json:"reindex_hint"`
}

func handleCVStatus(
	logger *slog.Logger,
	st store.Store,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	canonicalEdgeTypes []string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash, err := config.ConfigHash(canonicalKindReg, canonicalEdgeTypes)
		if err != nil {
			logger.ErrorContext(r.Context(), "config.ConfigHash", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to compute config_hash")
			return
		}

		droppedKinds, err := st.ListDroppedCanonicalKinds(r.Context())
		if err != nil {
			logger.ErrorContext(r.Context(), "store.ListDroppedCanonicalKinds", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load drift counters (kinds)")
			return
		}
		droppedEdges, err := st.ListDroppedCanonicalEdges(r.Context())
		if err != nil {
			logger.ErrorContext(r.Context(), "store.ListDroppedCanonicalEdges", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load drift counters (edges)")
			return
		}

		lastReindex, hasReindex, err := st.LastReindexAt(r.Context())
		if err != nil {
			logger.ErrorContext(r.Context(), "store.LastReindexAt", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load last_reindex_at")
			return
		}
		var lastReindexWire *string
		if hasReindex {
			s := lastReindex.UTC().Format("2006-01-02T15:04:05Z")
			lastReindexWire = &s
		}

		// `make([]T, 0, ...)` rather than nil slices so empty
		// drift sections serialize as `[]` on the wire (not
		// `null`). Same parity contract as `/v1/needs-fill`'s
		// edge_types fix from alice2-index a prior PR.
		out := cvStatusResponse{
			OK: true,
			ConfigHash: hash,
			Drift: cvDrift{
				KindsEmittedNotEnabled: make([]cvDriftKindRow, 0, len(droppedKinds)),
				KindsEnabledNotEmitted: []cvDriftKindRow{},
				EdgeTypesEmittedNotEnabled: make([]cvDriftEdgeRow, 0, len(droppedEdges)),
				EdgeTypesEnabledNotEmitted: []cvDriftEdgeRow{},
			},
			LastReindexAt: lastReindexWire,
			ReindexHint: cvStatusReindexHint,
		}
		for _, r := range droppedKinds {
			out.Drift.KindsEmittedNotEnabled = append(out.Drift.KindsEmittedNotEnabled, cvDriftKindRow{
				Plugin: r.Plugin,
				Kind: r.Kind,
				WouldMaterializeCount: r.Count,
			})
		}
		for _, r := range droppedEdges {
			out.Drift.EdgeTypesEmittedNotEnabled = append(out.Drift.EdgeTypesEmittedNotEnabled, cvDriftEdgeRow{
				Plugin: r.Plugin,
				EdgeType: r.EdgeType,
				WouldMaterializeCount: r.Count,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/cv-status response", "err", err)
		}
	}
}
