// Per-(plugin, kind/edge_type) drop counters — the durable signal
// behind `/v1/cv-status` (per ADR-0013 §3).
// The orchestrator increments these at the existing config-filter
// drop site (the startup-WARN site) so operators see an aggregate
// count of "you would have materialized N more entities" rather
// than scrolling logs.
//
// Per #48 slice 1 the increment also fires a per-(plugin,
// kind|edge_type) WARN-once at the first hit in this process
// lifetime. The aggregate counter in `/v1/cv-status` answers
// "how many drops since last reindex"; the WARN-once answers
// "did anything start dropping silently this process" so the
// operator sees the problem in their startup log without having
// to poll the endpoint.

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// droppedWarnGate dedups the per-(plugin, kind|edge_type) WARN-
// once log per process lifetime. The aggregate counter rows
// already live in the `dropped_canonical_kinds` /
// `dropped_canonical_edges` tables; this in-memory gate exists
// purely to prevent log spam on the second + later drops of the
// same key.
//
// Composite-key shape: `<axis>|<plugin>|<kind|edge_type>` where
// axis is `kind` or `edge`. The axis prefix prevents collision
// between a plugin's kind and edge_type that happen to share a
// name (defensive — the two carry independent vocabularies in
// practice).
var droppedWarnGate sync.Map // map[string]struct{}

// warnDroppedOnce logs a WARN at the first observation of the
// (axis, plugin, key) tuple per process lifetime. Subsequent
// calls with the same key are silent — the counter table is the
// per-event-count surface, this gate is the at-least-once log
// surface. The caller's `axisName` is included in the message so
// operators can correlate to the right `/v1/cv-status` drift
// section without ambiguity.
func warnDroppedOnce(axis, plugin, key, axisName string) {
	gateKey := axis + "|" + plugin + "|" + key
	if _, loaded := droppedWarnGate.LoadOrStore(gateKey, struct{}{}); loaded {
		return
	}
	slog.Default().Warn("canonical "+axisName+" dropped by config filter (first occurrence this process); aggregate counts at /v1/cv-status",
		"plugin", plugin,
		axisName, key,
	)
}

// resetDroppedWarnGate clears the WARN-once dedup map. Test-only
// hook — production never resets in-process (a reindex would
// reset the durable counter, not this in-memory gate).
func resetDroppedWarnGate() {
	droppedWarnGate.Range(func(k, _ any) bool {
		droppedWarnGate.Delete(k)
		return true
	})
}

// IncDroppedCanonicalKind upserts the (plugin, kind) row in the
// drop counter table — first call inserts count=1; subsequent
// calls increment and refresh `last_seen_at`. `first_seen_at`
// stays pinned to the earliest observation.
//
// Empty plugin / kind reject up front rather than producing a
// dead row that silently bloats the drift surface; this mirrors
// the existing AppendProvenance / ReplaceNotations input
// validation pattern.
func (s *sqliteStore) IncDroppedCanonicalKind(ctx context.Context, plugin, kind string) error {
	if plugin == "" {
		return errors.New("IncDroppedCanonicalKind: empty plugin")
	}
	if kind == "" {
		return errors.New("IncDroppedCanonicalKind: empty kind")
	}
	warnDroppedOnce("kind", plugin, kind, "kind")
	now := time.Now().UTC().Format(sqliteTimeFormat)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO dropped_canonical_kinds (plugin, kind, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(plugin, kind) DO UPDATE SET
			count = count + 1,
			last_seen_at = excluded.last_seen_at
	`, plugin, kind, now, now); err != nil {
		return fmt.Errorf("inc dropped canonical kind (%s, %s): %w", plugin, kind, err)
	}
	return nil
}

// IncDroppedCanonicalEdge is the edge-type counterpart —
// (plugin, edge_type) row in the sibling counter table.
func (s *sqliteStore) IncDroppedCanonicalEdge(ctx context.Context, plugin, edgeType string) error {
	if plugin == "" {
		return errors.New("IncDroppedCanonicalEdge: empty plugin")
	}
	if edgeType == "" {
		return errors.New("IncDroppedCanonicalEdge: empty edge_type")
	}
	warnDroppedOnce("edge", plugin, edgeType, "edge_type")
	now := time.Now().UTC().Format(sqliteTimeFormat)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO dropped_canonical_edges (plugin, edge_type, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(plugin, edge_type) DO UPDATE SET
			count = count + 1,
			last_seen_at = excluded.last_seen_at
	`, plugin, edgeType, now, now); err != nil {
		return fmt.Errorf("inc dropped canonical edge (%s, %s): %w", plugin, edgeType, err)
	}
	return nil
}

// ListDroppedCanonicalKinds returns every (plugin, kind) row in
// the counter table, ordered by (plugin, kind) for deterministic
// output (the cv-status endpoint surfaces the rows directly so
// stable ordering across calls keeps client-side diffs sane).
func (s *sqliteStore) ListDroppedCanonicalKinds(ctx context.Context) ([]DroppedCanonicalKindCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT plugin, kind, count, first_seen_at, last_seen_at
		FROM dropped_canonical_kinds
		ORDER BY plugin ASC, kind ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query dropped canonical kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]DroppedCanonicalKindCount, 0)
	for rows.Next() {
		var (
			r DroppedCanonicalKindCount
			firstStr string
			lastStr string
		)
		if err := rows.Scan(&r.Plugin, &r.Kind, &r.Count, &firstStr, &lastStr); err != nil {
			return nil, fmt.Errorf("scan dropped canonical kind: %w", err)
		}
		if r.FirstSeenAt, err = parseSQLiteTime(firstStr); err != nil {
			return nil, fmt.Errorf("parse first_seen_at for (%s, %s): %w", r.Plugin, r.Kind, err)
		}
		if r.LastSeenAt, err = parseSQLiteTime(lastStr); err != nil {
			return nil, fmt.Errorf("parse last_seen_at for (%s, %s): %w", r.Plugin, r.Kind, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dropped canonical kinds: %w", err)
	}
	return out, nil
}

// ClearDroppedCanonicalKinds wipes every row from the
// dropped_canonical_kinds table. Called by reindex.Run after a
// successful walk per #31 — reindex is the operator's
// "consume drift signal" action, so post-reindex drift resets to
// zero and any new drops from subsequent ingest accumulate under
// the originating plugin's tag (preserving attribution that would
// otherwise be blurred by clearing during the pass).
//
// Atomicity: single DELETE; SQL-level atomic. A concurrent ingest
// that fires between reindex-walk-end and this call sees its
// IncDroppedCanonicalKind row wiped by the clear — acceptable per
// the v1 semantic (drift means "since last reindex"; the next
// ingest re-emits).
func (s *sqliteStore) ClearDroppedCanonicalKinds(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dropped_canonical_kinds`); err != nil {
		return fmt.Errorf("clear dropped canonical kinds: %w", err)
	}
	return nil
}

// ClearDroppedCanonicalEdges is the edge-type counterpart to
// ClearDroppedCanonicalKinds (#31).
func (s *sqliteStore) ClearDroppedCanonicalEdges(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dropped_canonical_edges`); err != nil {
		return fmt.Errorf("clear dropped canonical edges: %w", err)
	}
	return nil
}

// ListDroppedCanonicalEdges is the edge-type counterpart.
func (s *sqliteStore) ListDroppedCanonicalEdges(ctx context.Context) ([]DroppedCanonicalEdgeCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT plugin, edge_type, count, first_seen_at, last_seen_at
		FROM dropped_canonical_edges
		ORDER BY plugin ASC, edge_type ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query dropped canonical edges: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]DroppedCanonicalEdgeCount, 0)
	for rows.Next() {
		var (
			r DroppedCanonicalEdgeCount
			firstStr string
			lastStr string
		)
		if err := rows.Scan(&r.Plugin, &r.EdgeType, &r.Count, &firstStr, &lastStr); err != nil {
			return nil, fmt.Errorf("scan dropped canonical edge: %w", err)
		}
		if r.FirstSeenAt, err = parseSQLiteTime(firstStr); err != nil {
			return nil, fmt.Errorf("parse first_seen_at for (%s, %s): %w", r.Plugin, r.EdgeType, err)
		}
		if r.LastSeenAt, err = parseSQLiteTime(lastStr); err != nil {
			return nil, fmt.Errorf("parse last_seen_at for (%s, %s): %w", r.Plugin, r.EdgeType, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dropped canonical edges: %w", err)
	}
	return out, nil
}
