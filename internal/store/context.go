package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GetEdgesForMany returns outbound edges from any of the given source
// ids, optionally filtered by edge type. One SQL query — bounded by
// the IN-list size. SQLite's default `SQLITE_MAX_VARIABLE_NUMBER` is
// 32766, more than enough for the BFS frontiers we expect (capped by
// max_results = 1000 → frontier ≤ 1000).
//
// Empty fromIDs → []Edge{}, no query (no SQL with empty IN-list).
// Empty types → no type filter.
func (s *sqliteStore) GetEdgesForMany(ctx context.Context, fromIDs []string, types []string) ([]Edge, error) {
	if len(fromIDs) == 0 {
		return []Edge{}, nil
	}

	idPlaceholders := make([]string, len(fromIDs))
	args := make([]any, 0, len(fromIDs)+len(types))
	for i, id := range fromIDs {
		idPlaceholders[i] = "?"
		args = append(args, id)
	}

	query := `
		SELECT type, from_id, to_id, metadata, created_at, updated_at
		FROM edges
		WHERE from_id IN (` + strings.Join(idPlaceholders, ",") + `)
	`
	if len(types) > 0 {
		typePlaceholders := make([]string, len(types))
		for i, t := range types {
			typePlaceholders[i] = "?"
			args = append(args, t)
		}
		query += " AND type IN (" + strings.Join(typePlaceholders, ",") + ")"
	}
	query += " ORDER BY from_id ASC, created_at ASC, type ASC, to_id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges for %d sources: %w", len(fromIDs), err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Edge, 0)
	for rows.Next() {
		var (
			edge Edge
			metadataRaw sql.NullString
			createdStr string
			updatedStr string
		)
		if err := rows.Scan(&edge.Type, &edge.From, &edge.To, &metadataRaw, &createdStr, &updatedStr); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		if metadataRaw.Valid && metadataRaw.String != "" {
			if err := json.Unmarshal([]byte(metadataRaw.String), &edge.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal metadata for edge %s→%s: %w", edge.From, edge.To, err)
			}
		}
		if edge.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
			return nil, fmt.Errorf("parse edge created_at: %w", err)
		}
		if edge.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
			return nil, fmt.Errorf("parse edge updated_at: %w", err)
		}
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate edges: %w", err)
	}
	return out, nil
}

// GetContextNeighbors implements the BFS-with-cycle-detection traversal
// behind `GET /v1/entities/{id}/context`.
//
// Algorithm:
//
// 1. Load the root entity via GetEntity. ErrNotFound bubbles up.
// 2. For depth = 1..maxDepth:
// a. GetEdgesForMany over the previous depth's frontier.
// b. For each edge whose `to` is unvisited, mark it as a candidate
// neighbor at this depth. Edges to already-visited entities are
// dropped (the entity is already in the result set).
// c. If candidates would exceed maxResults, truncate to fit and
// stop the traversal.
// d. Resolve all candidate-`to` ids in one GetEntities call.
// e. Append candidates (in edge-iteration order) to the neighbors
// slice; the new frontier is the candidates' ids.
// 3. Return root + neighbors + truncated.
//
// Each level is one SQL query for edges + one SQL query for entities,
// so total is 2 * actual-depth queries (capped at depth=3 → 6 SQL
// round-trips worst case). No recursive CTE — keeps the SQL plain and
// the Go side enforces ordering / cycle detection.
func (s *sqliteStore) GetContextNeighbors(
	ctx context.Context,
	rootID string,
	maxDepth int,
	edgeTypes []string,
	maxResults int,
) (*Entity, []ContextNeighbor, bool, error) {
	if maxDepth < 0 {
		return nil, nil, false, fmt.Errorf("maxDepth must be >= 0, got %d", maxDepth)
	}
	if maxResults < 0 {
		return nil, nil, false, fmt.Errorf("maxResults must be >= 0, got %d", maxResults)
	}

	root, err := s.GetEntity(ctx, rootID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, false, fmt.Errorf("context root %s: %w", rootID, ErrNotFound)
		}
		return nil, nil, false, fmt.Errorf("load context root %s: %w", rootID, err)
	}

	visited := map[string]struct{}{root.ID: {}}
	neighbors := make([]ContextNeighbor, 0)
	frontier := []string{root.ID}
	truncated := false

	for depth := 1; depth <= maxDepth && len(frontier) > 0 && !truncated; depth++ {
		edges, err := s.GetEdgesForMany(ctx, frontier, edgeTypes)
		if err != nil {
			return nil, nil, false, fmt.Errorf("context traversal depth %d: %w", depth, err)
		}

		// Collect candidate (edge, to-id) pairs in iteration order, dedupe by to-id.
		type pending struct {
			edge Edge
			to string
		}
		candidates := make([]pending, 0, len(edges))
		seenInLevel := make(map[string]struct{}, len(edges))
		for _, e := range edges {
			if _, vis := visited[e.To]; vis {
				continue
			}
			if _, dup := seenInLevel[e.To]; dup {
				// Multiple edges into the same neighbor at this depth: keep
				// the first one as the "introducing" edge. Future iterations
				// could surface multi-edge metadata; v1 keeps the wire shape
				// simple.
				continue
			}
			seenInLevel[e.To] = struct{}{}
			candidates = append(candidates, pending{edge: e, to: e.To})
		}

		// Truncate to fit max_results before resolving entities — no point
		// loading entity rows we'd discard.
		remaining := maxResults - len(neighbors)
		if remaining <= 0 {
			truncated = true
			break
		}
		if len(candidates) > remaining {
			candidates = candidates[:remaining]
			truncated = true
		}
		if len(candidates) == 0 {
			break
		}

		toIDs := make([]string, len(candidates))
		for i, c := range candidates {
			toIDs[i] = c.to
		}
		matched, _, err := s.GetEntities(ctx, toIDs)
		if err != nil {
			return nil, nil, false, fmt.Errorf("context resolve depth %d: %w", depth, err)
		}
		entityByID := make(map[string]Entity, len(matched))
		for _, e := range matched {
			entityByID[e.ID] = e
		}

		newFrontier := make([]string, 0, len(candidates))
		for _, c := range candidates {
			ent, ok := entityByID[c.to]
			if !ok {
				// Edge points at an entity that's gone — schema-level FK
				// should prevent this, but if it ever happens (e.g. a
				// concurrent delete-cascade racing the BFS), drop the
				// neighbor cleanly. The edge is already gone; the agent
				// just sees a smaller result set.
				continue
			}
			visited[ent.ID] = struct{}{}
			neighbors = append(neighbors, ContextNeighbor{
				Edge: c.edge,
				Entity: ent,
				Depth: depth,
			})
			newFrontier = append(newFrontier, ent.ID)
		}
		frontier = newFrontier
	}

	return root, neighbors, truncated, nil
}
