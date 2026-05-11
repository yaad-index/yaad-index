// Per-(plugin, kind/edge_type) drop counters — the durable signal
// behind `/v1/cv-status` (per ADR-0013 §3 / alice2-index a prior PR).
// The orchestrator increments these at the existing config-filter
// drop site (the startup-WARN site) so operators see an aggregate
// count of "you would have materialized N more entities" rather
// than scrolling logs.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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
