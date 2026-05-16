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
// the thin row. Returns (created, err): `created` is true when
// this call inserted a new row, false when an existing row was
// reused; on error `created` is false and the err wraps the
// underlying store failure (or names the malformed label).
//
// The `created` return lets callers emit lifecycle events (e.g.
// the workflow-engine eventbus.entity.created event per ADR-0024
// Phase 2) only on the first-time-seen path. AllowKind /
// source-type-bypass gating remains the caller's responsibility:
// this helper only ensures the row exists.
func EnsureLabelRow(ctx context.Context, st store.Store, label string, logger *slog.Logger) (bool, error) {
	kind, _, ok := SplitLabelID(label)
	if !ok {
		return false, fmt.Errorf("malformed canonical-label id %q", label)
	}
	if _, err := st.GetEntity(ctx, label); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("probe %q: %w", label, err)
	}
	if err := st.UpsertEntity(ctx, &store.Entity{ID: label, Kind: kind}); err != nil {
		return false, fmt.Errorf("upsert thin row %q: %w", label, err)
	}
	if logger != nil {
		logger.Debug("auto-materialized thin canonical-label row", "id", label, "kind", kind)
	}
	return true, nil
}
