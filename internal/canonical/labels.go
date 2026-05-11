// Package canonical centralises the cross-package helpers for the
// canonical-label edge model. The two shared utilities — splitting
// `<kind>:<slug>` ids and ensuring a thin entity row exists for an
// edge endpoint — are imported by the daemon's ingest path
// (internal/api), the canonical_type fill paths
// (internal/api/canonical_edges.go), and the reindex walker
// (internal/reindex). Living in a leaf package avoids the import
// cycle that would result if reindex tried to call back into
// internal/api.
//
// Scope is intentionally narrow: just the label-id split,
// thin-row-ensure helper, and the system-reserved kind constant.
// AllowKind / AllowEdgeType gating lives at the call site so each
// caller can wire its own drop-counter behavior (per-plugin at
// ingest time vs the no-counter reindex/backfill paths).
package canonical

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yaad-index/yaad-index/internal/store"
)

// SourceTypeKind is the system-reserved kind for canonical-label
// edge targets that label a source's source-type — e.g. an `is_a`
// edge from a `bgg:<slug>` source node points at
// `source-type:bgg-record`. source-type labels bypass the
// operator's canonical_kinds gate (an operator never enumerates
// source-types in canonical_kinds), but they DO get a thin entity
// row so the edges-table FK constraint is satisfied. source-type
// rows are filtered out of operator-facing list / search /
// needs_fill surfaces.
const SourceTypeKind = "source-type"

// SplitLabelID parses `<kind>:<slug>` into its parts. Returns
// ok=false when the id has no separator, an empty kind, or an
// empty slug — so callers can route malformed inputs to a debug
// log without surfacing them as failures.
func SplitLabelID(id string) (kind, slug string, ok bool) {
	idx := strings.IndexByte(id, ':')
	if idx <= 0 || idx == len(id)-1 {
		return "", "", false
	}
	return id[:idx], id[idx+1:], true
}

// EnsureLabelRow inserts a thin entity row for the canonical-label
// `<kind>:<slug>` if one is not already present. The row carries
// just (Kind + ID), no Data and no vault file — callers that need
// the operator-facing vault file rely on the auto-materialize-on-
// first-fill path. Skips when a row already exists so prior
// operator-fill values + Data are preserved.
//
// label is `<kind>:<slug>`; the helper splits + UpsertEntity-s
// the thin row. Returns an error wrapping the underlying store
// failure on UpsertEntity / probe; a malformed label returns an
// error so callers can log + skip.
//
// AllowKind / source-type-bypass gating is the caller's
// responsibility: this helper only ensures the row exists.
func EnsureLabelRow(ctx context.Context, st store.Store, label string, logger *slog.Logger) error {
	kind, _, ok := SplitLabelID(label)
	if !ok {
		return fmt.Errorf("malformed canonical-label id %q", label)
	}
	if _, err := st.GetEntity(ctx, label); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("probe %q: %w", label, err)
	}
	if err := st.UpsertEntity(ctx, &store.Entity{ID: label, Kind: kind}); err != nil {
		return fmt.Errorf("upsert thin row %q: %w", label, err)
	}
	if logger != nil {
		logger.Debug("auto-materialized thin canonical-label row", "id", label, "kind", kind)
	}
	return nil
}
